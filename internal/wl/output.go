package wl

import (
	"fmt"
	"sync"
)

// Output is one wl_output, tracked so screencopy and the cursor overlay can
// target a monitor by the same name Hyprland uses.
type Output struct {
	Obj       *Object
	GlobalID  uint32
	Name      string
	Desc      string
	Scale     int32
	ModeW     int32
	ModeH     int32
	Transform int32
	ready     bool
}

// Outputs keeps the live set of outputs, following hotplug.
type Outputs struct {
	c     *Conn
	mu    sync.Mutex
	byGID map[uint32]*Output
	cond  *sync.Cond
}

// TrackOutputs binds every wl_output now and any that appear later.
func TrackOutputs(c *Conn) *Outputs {
	o := &Outputs{c: c, byGID: map[uint32]*Output{}}
	o.cond = sync.NewCond(&o.mu)
	for _, g := range c.Globals() {
		if g.Iface == "wl_output" {
			o.add(g)
		}
	}
	prev := c.onGlob
	c.OnGlobal(func(g Global, added bool) {
		if prev != nil {
			prev(g, added)
		}
		if g.Iface != "wl_output" {
			return
		}
		if added {
			o.add(g)
			return
		}
		o.mu.Lock()
		if out := o.byGID[g.Name]; out != nil {
			delete(o.byGID, g.Name)
			_ = out.Obj.Req(0, NewMsg()) // release (v3+)
		}
		o.mu.Unlock()
	})
	return o
}

func (o *Outputs) add(g Global) {
	out := &Output{GlobalID: g.Name, Scale: 1}
	obj, err := o.c.Bind(g, 4, func(op uint16, e *Event) {
		o.mu.Lock()
		defer o.mu.Unlock()
		switch op {
		case 0: // geometry
			e.Int()
			e.Int()
			e.Int()
			e.Int()
			e.Int()
			_ = e.String()
			_ = e.String()
			out.Transform = e.Int()
		case 1: // mode
			flags := e.Uint()
			w, h := e.Int(), e.Int()
			if flags&1 != 0 { // current
				out.ModeW, out.ModeH = w, h
			}
		case 2: // done
			out.ready = true
			o.cond.Broadcast()
		case 3:
			out.Scale = e.Int()
		case 4:
			out.Name = e.String()
		case 5:
			out.Desc = e.String()
		}
	})
	if err != nil {
		return
	}
	out.Obj = obj
	o.mu.Lock()
	o.byGID[g.Name] = out
	o.mu.Unlock()
}

// ByName finds an output by connector name (HDMI-A-1, DP-2, ...).
func (o *Outputs) ByName(name string) (*Output, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	var names []string
	for _, out := range o.byGID {
		if out.Name == name {
			return out, nil
		}
		names = append(names, out.Name)
	}
	return nil, fmt.Errorf("no output named %q (have %v)", name, names)
}

// All returns the outputs that finished their initial burst of events.
func (o *Outputs) All() []*Output {
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []*Output
	for _, x := range o.byGID {
		if x.ready {
			out = append(out, x)
		}
	}
	return out
}
