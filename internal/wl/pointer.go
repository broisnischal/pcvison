package wl

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// Buttons, from linux/input-event-codes.h.
var Buttons = map[string]uint32{
	"left": 0x110, "right": 0x111, "middle": 0x112,
	"side": 0x113, "extra": 0x114, "forward": 0x115, "back": 0x116,
}

// Pointer is a zwlr_virtual_pointer_v1. On a single-seat compositor this
// drives the one real cursor, so the caller is responsible for putting it
// back where the human left it. Methods ending in Call only encode, so a
// whole gesture can go out in one write (see Conn.SendAll).
type Pointer struct {
	c   *Conn
	obj *Object
	t0  time.Time
}

func NewPointer(c *Conn) (*Pointer, error) {
	mgr, err := c.BindIface("zwlr_virtual_pointer_manager_v1", 2, nil)
	if err != nil {
		return nil, err
	}
	obj := c.NewObject("zwlr_virtual_pointer_v1", nil)
	if err := mgr.Req(0, NewMsg().U(0).U(obj.ID)); err != nil { // seat: null
		return nil, err
	}
	return &Pointer{c: c, obj: obj, t0: time.Now()}, nil
}

func (p *Pointer) now() uint32 { return uint32(time.Since(p.t0).Milliseconds()) }

// Conn is the connection the pointer lives on.
func (p *Pointer) Conn() *Conn { return p.c }

// MoveCall places the cursor at (x, y) inside a w×h space that the compositor
// maps onto the bounding box of all monitors. Coordinates are scaled by 16 so
// fractional logical positions survive the integer protocol.
func (p *Pointer) MoveCall(x, y, w, h float64) []Call {
	const k = 16
	ew, eh := math.Round(w*k), math.Round(h*k)
	ix := math.Max(0, math.Min(math.Round(x*k), ew-1))
	iy := math.Max(0, math.Min(math.Round(y*k), eh-1))
	return []Call{
		{p.obj.ID, 1, NewMsg().U(p.now()).U(uint32(ix)).U(uint32(iy)).U(uint32(ew)).U(uint32(eh))},
		p.frame(),
	}
}

// ButtonCall presses or releases a button by name.
func (p *Pointer) ButtonCall(name string, down bool) ([]Call, error) {
	code, ok := Buttons[strings.ToLower(name)]
	if !ok {
		return nil, fmt.Errorf("unknown button %q (left, right, middle, back, forward)", name)
	}
	state := uint32(0)
	if down {
		state = 1
	}
	return []Call{{p.obj.ID, 2, NewMsg().U(p.now()).U(code).U(state)}, p.frame()}, nil
}

// ScrollCall sends wheel notches; positive is down (or right).
func (p *Pointer) ScrollCall(notches int, horizontal bool) []Call {
	axis := uint32(0)
	if horizontal {
		axis = 1
	}
	dir := 1
	if notches < 0 {
		dir, notches = -1, -notches
	}
	var out []Call
	for i := 0; i < notches && i < 100; i++ {
		out = append(out,
			Call{p.obj.ID, 5, NewMsg().U(0)}, // axis_source: wheel
			Call{p.obj.ID, 7, NewMsg().U(p.now()).U(axis).F(15 * float64(dir)).I(int32(dir))}, // axis_discrete
			p.frame())
	}
	return out
}

func (p *Pointer) frame() Call { return Call{p.obj.ID, 4, NewMsg()} }

// Send writes a gesture in one go.
func (p *Pointer) Send(calls ...[]Call) error {
	var all []Call
	for _, c := range calls {
		all = append(all, c...)
	}
	return p.c.SendAll(all...)
}
