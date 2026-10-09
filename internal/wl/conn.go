// Package wl is a small Wayland client: just the wire protocol and the handful
// of interfaces pc needs (virtual pointer and keyboard, screencopy, layer
// shell, data control). No libwayland, no cgo, so the binary stays static and
// a request costs a write on a socket.
package wl

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"
)

// Global is one entry the compositor advertised in the registry.
type Global struct {
	Name    uint32
	Iface   string
	Version uint32
}

// Object is a client-side proxy. Handler runs on the reader goroutine, so it
// must not block; signal a channel instead.
type Object struct {
	ID      uint32
	Iface   string
	Handler func(op uint16, e *Event)
	c       *Conn
}

// Conn is one connection to a compositor.
type Conn struct {
	sock *net.UnixConn
	wmu  sync.Mutex

	mu      sync.Mutex
	objs    map[uint32]*Object
	free    []uint32
	next    uint32
	err     error
	globals []Global
	onGlob  func(g Global, added bool)

	fds    []int // received descriptors, consumed in order by FD()
	closed chan struct{}

	registry *Object
	Display  string
}

const displayID = 1

// SocketPath turns a WAYLAND_DISPLAY value into a socket path.
func SocketPath(display string) string {
	if display == "" {
		display = os.Getenv("WAYLAND_DISPLAY")
	}
	if display == "" {
		display = "wayland-0"
	}
	if filepath.IsAbs(display) {
		return display
	}
	rt := os.Getenv("XDG_RUNTIME_DIR")
	if rt == "" {
		rt = fmt.Sprintf("/run/user/%d", os.Getuid())
	}
	return filepath.Join(rt, display)
}

// Connect dials the compositor and reads its registry.
func Connect(display string) (*Conn, error) {
	path := SocketPath(display)
	raw, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("wayland %s: %w", path, err)
	}
	c := &Conn{sock: raw, objs: map[uint32]*Object{}, next: 2, closed: make(chan struct{}), Display: display}
	c.objs[displayID] = &Object{ID: displayID, Iface: "wl_display", c: c}
	go c.readLoop()

	c.registry = c.NewObject("wl_registry", func(op uint16, e *Event) {
		switch op {
		case 0: // global
			g := Global{Name: e.Uint(), Iface: e.String(), Version: e.Uint()}
			c.mu.Lock()
			c.globals = append(c.globals, g)
			cb := c.onGlob
			c.mu.Unlock()
			if cb != nil {
				cb(g, true)
			}
		case 1: // global_remove
			name := e.Uint()
			c.mu.Lock()
			var gone *Global
			for i, g := range c.globals {
				if g.Name == name {
					gg := g
					gone = &gg
					c.globals = append(c.globals[:i], c.globals[i+1:]...)
					break
				}
			}
			cb := c.onGlob
			c.mu.Unlock()
			if cb != nil && gone != nil {
				cb(*gone, false)
			}
		}
	})
	if err := c.Send(displayID, 1, NewMsg().U(c.registry.ID)); err != nil {
		c.Close()
		return nil, err
	}
	if err := c.Roundtrip(2 * time.Second); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// OnGlobal registers a hotplug callback (outputs come and go).
func (c *Conn) OnGlobal(fn func(g Global, added bool)) {
	c.mu.Lock()
	c.onGlob = fn
	c.mu.Unlock()
}

// Globals returns every advertised global.
func (c *Conn) Globals() []Global {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := append([]Global(nil), c.globals...)
	sort.Slice(out, func(i, j int) bool { return out[i].Iface < out[j].Iface })
	return out
}

// Find returns the first global with that interface name.
func (c *Conn) Find(iface string) (Global, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, g := range c.globals {
		if g.Iface == iface {
			return g, true
		}
	}
	return Global{}, false
}

// Bind binds a global at min(version, advertised).
func (c *Conn) Bind(g Global, version uint32, h func(op uint16, e *Event)) (*Object, error) {
	if g.Version < version {
		version = g.Version
	}
	o := c.NewObject(g.Iface, h)
	err := c.Send(c.registry.ID, 0, NewMsg().U(g.Name).S(g.Iface).U(version).U(o.ID))
	return o, err
}

// BindIface binds the first global named iface.
func (c *Conn) BindIface(iface string, version uint32, h func(op uint16, e *Event)) (*Object, error) {
	g, ok := c.Find(iface)
	if !ok {
		return nil, fmt.Errorf("compositor does not offer %s", iface)
	}
	return c.Bind(g, version, h)
}

// NewObject allocates a client id. Freed ids are reused: libwayland servers
// only accept a new id that is free or exactly one past the end, and never
// reusing would grow the compositor's object table without bound.
func (c *Conn) NewObject(iface string, h func(op uint16, e *Event)) *Object {
	c.mu.Lock()
	defer c.mu.Unlock()
	var id uint32
	if n := len(c.free); n > 0 {
		sort.Slice(c.free, func(i, j int) bool { return c.free[i] < c.free[j] })
		id, c.free = c.free[0], c.free[1:]
	} else {
		id = c.next
		c.next++
	}
	o := &Object{ID: id, Iface: iface, Handler: h, c: c}
	c.objs[id] = o
	return o
}

// adopt registers a server-created object (data_offer and friends).
func (c *Conn) Adopt(id uint32, iface string, h func(op uint16, e *Event)) *Object {
	c.mu.Lock()
	defer c.mu.Unlock()
	o := &Object{ID: id, Iface: iface, Handler: h, c: c}
	c.objs[id] = o
	return o
}

// Forget drops a server-created object after we destroyed it; the server
// never sends delete_id for ids it allocated.
func (c *Conn) Forget(id uint32) {
	c.mu.Lock()
	delete(c.objs, id)
	c.mu.Unlock()
}

// Req sends a request on this object.
func (o *Object) Req(op uint16, m *Msg) error { return o.c.Send(o.ID, op, m) }

// Err is the protocol error that killed the connection, if any.
func (c *Conn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Done is closed when the connection dies.
func (c *Conn) Done() <-chan struct{} { return c.closed }

// Close tears the connection down.
func (c *Conn) Close() {
	c.fail(errors.New("closed"))
}

func (c *Conn) fail(err error) {
	c.mu.Lock()
	if c.err == nil {
		c.err = err
		close(c.closed)
		_ = c.sock.Close()
	}
	c.mu.Unlock()
}

// Roundtrip waits until the compositor has processed everything sent so far.
func (c *Conn) Roundtrip(timeout time.Duration) error {
	done := make(chan struct{})
	cb := c.NewObject("wl_callback", func(op uint16, e *Event) {
		if op == 0 {
			close(done)
		}
	})
	if err := c.Send(displayID, 0, NewMsg().U(cb.ID)); err != nil {
		return err
	}
	select {
	case <-done:
		return c.Err()
	case <-c.closed:
		return c.Err()
	case <-time.After(timeout):
		return fmt.Errorf("compositor did not answer within %v", timeout)
	}
}

// Send encodes and writes one request.
func (c *Conn) Send(id uint32, op uint16, m *Msg) error {
	if err := c.Err(); err != nil {
		return err
	}
	size := 8 + len(m.b)
	if size > 4096 {
		return fmt.Errorf("wayland request too large (%d bytes)", size)
	}
	buf := make([]byte, size)
	binary.LittleEndian.PutUint32(buf[0:], id)
	binary.LittleEndian.PutUint32(buf[4:], uint32(size)<<16|uint32(op))
	copy(buf[8:], m.b)
	c.wmu.Lock()
	defer c.wmu.Unlock()
	var err error
	if len(m.fds) > 0 {
		_, _, err = c.sock.WriteMsgUnix(buf, syscall.UnixRights(m.fds...), nil)
	} else {
		_, err = c.sock.Write(buf)
	}
	if err != nil {
		c.fail(err)
	}
	return err
}

// Call is one encoded request, for SendAll.
type Call struct {
	ID uint32
	Op uint16
	M  *Msg
}

// SendAll writes several requests with a single write, so the compositor
// reads and dispatches them in one go, with no frame rendered in between.
// Requests carrying file descriptors are not allowed here.
func (c *Conn) SendAll(calls ...Call) error {
	if err := c.Err(); err != nil {
		return err
	}
	var buf []byte
	for _, k := range calls {
		size := 8 + len(k.M.b)
		buf = binary.LittleEndian.AppendUint32(buf, k.ID)
		buf = binary.LittleEndian.AppendUint32(buf, uint32(size)<<16|uint32(k.Op))
		buf = append(buf, k.M.b...)
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err := c.sock.Write(buf)
	if err != nil {
		c.fail(err)
	}
	return err
}

func (c *Conn) readLoop() {
	buf := make([]byte, 1<<16)
	oob := make([]byte, syscall.CmsgSpace(28*4))
	var inbox []byte
	for {
		n, oobn, _, _, err := c.sock.ReadMsgUnix(buf, oob)
		if err != nil {
			c.fail(fmt.Errorf("compositor connection lost: %w", err))
			return
		}
		if n == 0 {
			c.fail(errors.New("compositor closed the connection"))
			return
		}
		if oobn > 0 {
			if msgs, err := syscall.ParseSocketControlMessage(oob[:oobn]); err == nil {
				for _, m := range msgs {
					if fds, err := syscall.ParseUnixRights(&m); err == nil {
						c.fds = append(c.fds, fds...)
					}
				}
			}
		}
		inbox = append(inbox, buf[:n]...)
		for len(inbox) >= 8 {
			id := binary.LittleEndian.Uint32(inbox[0:])
			word := binary.LittleEndian.Uint32(inbox[4:])
			size, op := int(word>>16), uint16(word&0xffff)
			if size < 8 {
				c.fail(fmt.Errorf("malformed wayland message (size %d)", size))
				return
			}
			if len(inbox) < size {
				break
			}
			c.dispatch(id, op, inbox[8:size])
			inbox = inbox[size:]
		}
		if len(inbox) == 0 {
			inbox = nil
		} else {
			inbox = append([]byte(nil), inbox...)
		}
	}
}

func (c *Conn) dispatch(id uint32, op uint16, body []byte) {
	e := &Event{b: body, c: c}
	if id == displayID {
		switch op {
		case 0: // error
			obj, code, msg := e.Uint(), e.Uint(), e.String()
			c.mu.Lock()
			iface := "?"
			if o := c.objs[obj]; o != nil {
				iface = o.Iface
			}
			c.mu.Unlock()
			c.fail(fmt.Errorf("wayland protocol error on %s#%d (code %d): %s", iface, obj, code, msg))
		case 1: // delete_id
			gone := e.Uint()
			c.mu.Lock()
			delete(c.objs, gone)
			if gone < 0xff000000 {
				c.free = append(c.free, gone)
			}
			c.mu.Unlock()
		}
		return
	}
	c.mu.Lock()
	o := c.objs[id]
	var h func(uint16, *Event)
	if o != nil {
		h = o.Handler
	}
	c.mu.Unlock()
	if h != nil {
		h(op, e)
	}
}

// Msg builds request arguments.
type Msg struct {
	b   []byte
	fds []int
}

func NewMsg() *Msg { return &Msg{} }

func (m *Msg) U(v uint32) *Msg {
	m.b = binary.LittleEndian.AppendUint32(m.b, v)
	return m
}

func (m *Msg) I(v int32) *Msg { return m.U(uint32(v)) }

// F encodes 24.8 fixed point.
func (m *Msg) F(v float64) *Msg { return m.I(int32(math.Round(v * 256))) }

func (m *Msg) S(s string) *Msg {
	m.U(uint32(len(s) + 1))
	m.b = append(m.b, s...)
	m.b = append(m.b, 0)
	for len(m.b)%4 != 0 {
		m.b = append(m.b, 0)
	}
	return m
}

// FD attaches a descriptor; it travels out of band.
func (m *Msg) FD(fd int) *Msg {
	m.fds = append(m.fds, fd)
	return m
}

// Event decodes event arguments in order.
type Event struct {
	b []byte
	c *Conn
}

func (e *Event) Uint() uint32 {
	if len(e.b) < 4 {
		return 0
	}
	v := binary.LittleEndian.Uint32(e.b)
	e.b = e.b[4:]
	return v
}

func (e *Event) Int() int32 { return int32(e.Uint()) }

func (e *Event) Fixed() float64 { return float64(e.Int()) / 256 }

func (e *Event) String() string {
	n := int(e.Uint())
	if n == 0 || n > len(e.b) {
		return ""
	}
	s := string(e.b[:n-1])
	pad := (n + 3) &^ 3
	if pad > len(e.b) {
		pad = len(e.b)
	}
	e.b = e.b[pad:]
	return s
}

func (e *Event) Array() []byte {
	n := int(e.Uint())
	if n > len(e.b) {
		n = len(e.b)
	}
	a := append([]byte(nil), e.b[:n]...)
	pad := (n + 3) &^ 3
	if pad > len(e.b) {
		pad = len(e.b)
	}
	e.b = e.b[pad:]
	return a
}

// FD pops the next received descriptor (-1 if none arrived).
func (e *Event) FD() int {
	if len(e.c.fds) == 0 {
		return -1
	}
	fd := e.c.fds[0]
	e.c.fds = e.c.fds[1:]
	return fd
}
