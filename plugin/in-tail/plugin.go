package in_tail

import (
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/yosisa/fluxion/message"
	"github.com/yosisa/fluxion/parser"
	"github.com/yosisa/fluxion/plugin"
	"gopkg.in/fsnotify.v1"
)

var posFiles = make(map[string]*PositionFile)

type Config struct {
	Tag          string `toml:"tag"`
	Path         string `toml:"path"`
	PosFile      string `toml:"pos_file"`
	Format       string `toml:"format"`
	TimeKey      string `toml:"time_key"`
	TimeFormat   string `toml:"time_format"`
	TimeZone     string `toml:"timezone"`
	RecordKey    string `toml:"record_key"`
	RecordFormat string `toml:"record_format"`
	ReadFromHead bool   `toml:"read_from_head"`
}

type TailInput struct {
	env        *plugin.Env
	conf       *Config
	parser     parser.Parser
	timeParser parser.TimeParser
	rparser    parser.Parser
	pf         *PositionFile
	fsw        *fsnotify.Watcher
	watchers   map[string]*Watcher
}

func (i *TailInput) Init(env *plugin.Env) (err error) {
	i.env = env
	i.conf = &Config{}
	i.watchers = make(map[string]*Watcher)
	if err = env.ReadConfig(i.conf); err != nil {
		return
	}
	if i.conf.TimeKey == "" {
		i.conf.TimeKey = "time"
	}
	i.parser, i.timeParser, err = parser.Get(i.conf.Format, i.conf.TimeFormat, i.conf.TimeZone)
	if err != nil {
		return
	}
	if i.conf.RecordKey != "" {
		if i.rparser, _, err = parser.Get(i.conf.RecordFormat, "", ""); err != nil {
			return
		}
	}

	pf, ok := posFiles[i.conf.PosFile]
	if !ok {
		if pf, err = NewPositionFile(i.conf.PosFile); err != nil {
			return
		}
		posFiles[i.conf.PosFile] = pf
	}
	i.pf = pf
	return
}

func (i *TailInput) Start() (err error) {
	i.fsw, err = fsnotify.NewWatcher()
	if err != nil {
		return
	}
	go i.fsEventHandler()
	go i.pathWatcher()
	return
}

func (i *TailInput) Close() error {
	return i.fsw.Close()
}

func (i *TailInput) fsEventHandler() {
	for {
		select {
		case ev, ok := <-i.fsw.Events:
			if !ok {
				return
			}
			if w, ok := i.watchers[ev.Name]; ok {
				select {
				case w.FSEventC <- ev:
				default:
				}
			} else {
				i.env.Log.Warningf("FSEvent received for closed watcher: %v", ev)
			}
		case err, ok := <-i.fsw.Errors:
			if !ok {
				return
			}
			i.env.Log.Warning(err)
		}
	}
}

func (i *TailInput) pathWatcher() {
	tick := time.Tick(1 * time.Minute)
	for {
		files, err := filepath.Glob(i.conf.Path)
		if err != nil {
			i.env.Log.Error(err)
			return
		}

		changes := make(map[string]bool)
		for f := range i.watchers {
			changes[f] = false
		}
		for _, f := range files {
			f = filepath.Clean(f)
			if _, ok := changes[f]; ok {
				delete(changes, f)
			} else {
				changes[f] = true
			}
		}

		for f, added := range changes {
			if added {
				i.env.Log.Info("Start watching file: ", f)
				pe := i.pf.Get(f)
				pe.ReadFromHead = i.conf.ReadFromHead
				lp := &LineParser{
					env:        i.env,
					tag:        realTag(i.conf.Tag, pe.Path),
					parser:     i.parser,
					timeParser: i.timeParser,
					timeKey:    i.conf.TimeKey,
					rkey:       i.conf.RecordKey,
					rparser:    i.rparser,
				}
				i.watchers[f] = NewWatcher(pe, i.env, lp.parseLine, i.fsw)
				i.fsw.Add(f)
			} else {
				i.env.Log.Info("Stop watching file: ", f)
				i.fsw.Remove(f)
				i.watchers[f].Close()
				delete(i.watchers, f)
			}
		}

		<-tick
	}
}

func realTag(tag, path string) string {
	if !strings.Contains(tag, "*") {
		return tag
	}
	path = strings.Trim(path, "/")
	return strings.Replace(tag, "*", strings.Replace(path, "/", ".", -1), -1)
}

type LineParser struct {
	env        *plugin.Env
	tag        string
	parser     parser.Parser
	timeParser parser.TimeParser
	timeKey    string
	rkey       string
	rparser    parser.Parser
}

func (l *LineParser) parseLine(b []byte) {
	line := string(b)
	v, err := l.parser.Parse(line)
	if err != nil {
		l.env.Log.Warningf("Line parser failed: %v, use default parser: %s", err, line)
		v, _ = parser.DefaultParser.Parse(line)
	}
	l.env.Emit(l.makeEvent(v))
}

func (l *LineParser) makeEvent(v map[string]interface{}) *message.Event {
	if l.rkey != "" && l.rparser != nil {
		if s, ok := v[l.rkey].(string); ok {
			if record, err := l.rparser.Parse(s); err == nil {
				v = record
			} else {
				l.env.Log.Warningf("Record parser failed: %v", err)
			}
		} else {
			l.env.Log.Warning("Record key configured, but not exists")
		}
	}
	if l.timeKey != "" && l.timeParser != nil {
		if val, ok := v[l.timeKey]; ok {
			t, err := l.timeParser.Parse(val)
			if err == nil {
				return message.NewEventWithTime(l.tag, t, v)
			}
			l.env.Log.Warningf("Time parser failed: %v", err)
		} else {
			l.env.Log.Warning("Time key configured, but not exists")
		}
	}
	return message.NewEvent(l.tag, v)
}

type TailHandler func([]byte)

type Watcher struct {
	pe       *PositionEntry
	fsw      *fsnotify.Watcher
	r        *PositionReader
	handler  TailHandler
	rotating bool
	m        sync.Mutex
	FSEventC chan fsnotify.Event
	notifyC  chan bool
	env      *plugin.Env
}

func NewWatcher(pe *PositionEntry, env *plugin.Env, h TailHandler, fsw *fsnotify.Watcher) *Watcher {
	w := &Watcher{
		pe:       pe,
		fsw:      fsw,
		handler:  h,
		FSEventC: make(chan fsnotify.Event, 100),
		notifyC:  make(chan bool, 1),
		env:      env,
	}
	w.open()
	go w.eventLoop()
	return w
}

func (w *Watcher) Close() {
	close(w.FSEventC)
	close(w.notifyC)
}

func (w *Watcher) open() {
	w.m.Lock()
	defer w.m.Unlock()

	w.rotating = false
	if w.r != nil {
		w.r.Close()
	}

	r, err := NewPositionReader(w.pe)
	if err != nil {
		w.env.Log.Warning(err, ", wait for creation")
	} else {
		w.r = r
		w.fsw.Add(w.pe.Path)
	}
	w.notify()
}

func (w *Watcher) eventLoop() {
	tick := time.Tick(10 * time.Second)
	for {
		select {
		case _, ok := <-w.notifyC:
			if !ok {
				return
			}
		case ev := <-w.FSEventC:
			if ev.Op&fsnotify.Create == 0 && ev.Op&fsnotify.Write == 0 {
				continue
			}
		case <-tick:
		}

		if err := w.Scan(); err != nil {
			w.env.Log.Warning(err)
		}
	}
}

func (w *Watcher) Scan() error {
	// To make Scan run only one thread at a time.
	// Also used to block rotation until current scanning completed.
	w.m.Lock()
	defer w.m.Unlock()

	if !w.rotating {
		rotated, truncated := w.pe.IsRotated()
		if rotated {
			w.env.Log.Infof("Rotation detected: %s", w.pe.Path)
			var wait time.Duration
			if w.r != nil {
				wait = 5 * time.Second
			}
			w.rotating = true
			time.AfterFunc(wait, w.open)
		} else if truncated {
			w.env.Log.Infof("Truncation detected: %s", w.pe.Path)
			w.rotating = true
			go w.open()
			return nil
		}
	}

	if w.r == nil {
		return nil
	}

	for {
		line, err := w.r.ReadLine()
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		w.handler(line)
	}
	return nil
}

func (w *Watcher) notify() {
	select {
	case w.notifyC <- true:
	default:
	}
}

func Factory() plugin.Plugin {
	return &TailInput{}
}
