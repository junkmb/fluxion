package main

import (
	"net"
	"time"

	"github.com/ugorji/go/codec"
	"github.com/yosisa/fluxion/buffer"
	"github.com/yosisa/fluxion/event"
	"github.com/yosisa/fluxion/plugin"
)

var mh = &codec.MsgpackHandle{}

type Config struct {
	Server string `codec:"server"`
}

type ForwardOutput struct {
	env  *plugin.Env
	conf *Config
	conn net.Conn
}

func (o *ForwardOutput) Name() string {
	return "out-forward"
}

func (o *ForwardOutput) Init(env *plugin.Env) error {
	o.env = env
	o.conf = &Config{}
	return env.ReadConfig(o.conf)
}

func (o *ForwardOutput) Start() (err error) {
	return nil
}

func (o *ForwardOutput) Encode(r *event.Record) (buffer.Sizer, error) {
	var b []byte
	v := []interface{}{r.Tag, r.Time.Unix(), r.Value}
	if err := codec.NewEncoderBytes(&b, mh).Encode(v); err != nil {
		return nil, err
	}
	return buffer.BytesItem(b), nil
}

func (o *ForwardOutput) Write(l []buffer.Sizer) (int, error) {
	if o.conn == nil {
		conn, err := net.DialTimeout("tcp", o.conf.Server, 5*time.Second)
		if err != nil {
			return 0, err
		}
		o.conn = conn
	}

	for i, b := range l {
		if _, err := o.conn.Write(b.(buffer.BytesItem)); err != nil {
			o.conn.Close()
			o.conn = nil
			return i, err
		}
	}

	return len(l), nil
}

func main() {
	plugin.New(func() plugin.Plugin {
		return &ForwardOutput{}
	}).Run()
}
