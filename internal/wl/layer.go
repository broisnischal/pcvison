package wl

import (
	"errors"
	"image"
	"sync"
	"time"
)

// LayerSurface is a small wlr-layer-shell surface on the overlay layer with an
// empty input region: drawn above everything, and clicks pass straight through
// it. It is positioned by margins from the output's top-left corner, so moving
// it costs one set_margin + commit and no redraw.
type LayerSurface struct {
	c      *Conn
	surf   *Object
	layer  *Object
	shm    *Object
	Out    *Output
	W, H   int // logical size
	Scale  int // buffer scale
	mu     sync.Mutex
	serial uint32
	ack    bool
	closed bool
	bufs   []*Buffer
	shown  image.Rectangle // what the last committed buffer painted
}

// NewLayerSurface maps nothing yet: it does the initial commit and waits for
// the compositor's first configure.
func NewLayerSurface(c *Conn, compositor, shm, shell *Object, out *Output, namespace string, w, h, scale int, top, left int) (*LayerSurface, error) {
	l := &LayerSurface{c: c, shm: shm, Out: out, W: w, H: h, Scale: scale}
	configured := make(chan struct{}, 1)
	l.surf = c.NewObject("wl_surface", nil)
	if err := compositor.Req(0, NewMsg().U(l.surf.ID)); err != nil {
		return nil, err
	}
	region := c.NewObject("wl_region", nil)
	if err := compositor.Req(1, NewMsg().U(region.ID)); err != nil {
		return nil, err
	}
	_ = l.surf.Req(5, NewMsg().U(region.ID)) // set_input_region: empty = click-through
	_ = region.Req(0, NewMsg())
	if scale > 1 {
		_ = l.surf.Req(8, NewMsg().I(int32(scale)))
	}
	l.layer = c.NewObject("zwlr_layer_surface_v1", func(op uint16, e *Event) {
		switch op {
		case 0: // configure
			s := e.Uint()
			l.mu.Lock()
			l.serial, l.ack = s, true
			l.mu.Unlock()
			select {
			case configured <- struct{}{}:
			default:
			}
		case 1: // closed
			l.mu.Lock()
			l.closed = true
			l.mu.Unlock()
		}
	})
	if err := shell.Req(0, NewMsg().U(l.layer.ID).U(l.surf.ID).U(out.Obj.ID).U(3).S(namespace)); err != nil {
		return nil, err
	}
	_ = l.layer.Req(0, NewMsg().U(uint32(w)).U(uint32(h))) // set_size
	_ = l.layer.Req(1, NewMsg().U(1|4))                    // anchor top|left
	_ = l.layer.Req(2, NewMsg().I(-1))                     // exclusive zone: ignore bars
	_ = l.layer.Req(4, NewMsg().U(0))                      // no keyboard, ever
	_ = l.layer.Req(3, NewMsg().I(int32(top)).I(0).I(0).I(int32(left)))
	if err := l.surf.Req(6, NewMsg()); err != nil {
		return nil, err
	}
	select {
	case <-configured:
	case <-c.Done():
		return nil, c.Err()
	case <-time.After(2 * time.Second):
		l.Destroy()
		return nil, errors.New("layer surface was never configured")
	}
	return l, nil
}

func (l *LayerSurface) ackPending() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ack {
		_ = l.layer.Req(6, NewMsg().U(l.serial))
		l.ack = false
	}
}

// Closed reports whether the compositor withdrew the surface (output gone).
func (l *LayerSurface) Closed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closed
}

// Move repositions the surface without redrawing it.
func (l *LayerSurface) Move(top, left int) error {
	_, err := l.move(top, left, false)
	return err
}

// MoveFrame repositions the surface and returns a channel that fires when the
// compositor wants the next frame, so an animation advances exactly once per
// display refresh instead of on a timer that drifts against it.
func (l *LayerSurface) MoveFrame(top, left int) (<-chan struct{}, error) {
	return l.move(top, left, true)
}

func (l *LayerSurface) move(top, left int, frame bool) (<-chan struct{}, error) {
	l.ackPending()
	var ch chan struct{}
	if frame {
		ch = make(chan struct{})
		cb := l.c.NewObject("wl_callback", func(op uint16, e *Event) {
			if op == 0 {
				close(ch)
			}
		})
		if err := l.surf.Req(3, NewMsg().U(cb.ID)); err != nil {
			return nil, err
		}
	}
	if err := l.layer.Req(3, NewMsg().I(int32(top)).I(0).I(0).I(int32(left))); err != nil {
		return nil, err
	}
	return ch, l.surf.Req(6, NewMsg())
}

// Draw renders into a free buffer (premultiplied ARGB8888) and presents it,
// optionally moving the surface in the same commit. paint returns the box of
// buffer pixels it touched: only that box and the one the buffer held before
// are cleared, and only what changed since the last frame is damaged, so a
// small cursor on a large surface costs a small upload. Like MoveFrame, the
// returned channel fires when the compositor is ready for the next frame.
func (l *LayerSurface) Draw(paint func(pix []byte, stride, w, h int) image.Rectangle, top, left int, move bool) (<-chan struct{}, error) {
	l.ackPending()
	pw, ph := l.W*l.Scale, l.H*l.Scale
	var b *Buffer
	for _, cand := range l.bufs {
		if !cand.Busy() {
			b = cand
			break
		}
	}
	if b == nil {
		if len(l.bufs) >= 3 {
			b = l.bufs[0] // the compositor is slow to release; reuse rather than grow
		} else {
			var err error
			b, err = NewBuffer(l.c, l.shm, pw, ph, pw*4, FmtARGB8888)
			if err != nil {
				return nil, err
			}
			b.Dirty = image.Rect(0, 0, pw, ph) // fresh memfd pages are zero, but be sure
			l.bufs = append(l.bufs, b)
		}
	}
	stride := pw * 4
	if d := b.Dirty.Intersect(image.Rect(0, 0, pw, ph)); !d.Empty() {
		for y := d.Min.Y; y < d.Max.Y; y++ {
			clear(b.Data[y*stride+d.Min.X*4 : y*stride+d.Max.X*4])
		}
	}
	painted := paint(b.Data, stride, pw, ph).Intersect(image.Rect(0, 0, pw, ph))
	b.Dirty = painted
	damage := painted.Union(l.shown)
	l.shown = painted
	ch := make(chan struct{})
	cb := l.c.NewObject("wl_callback", func(op uint16, e *Event) {
		if op == 0 {
			close(ch)
		}
	})
	_ = l.surf.Req(3, NewMsg().U(cb.ID)) // frame
	if move {
		_ = l.layer.Req(3, NewMsg().I(int32(top)).I(0).I(0).I(int32(left)))
	}
	_ = l.surf.Req(1, NewMsg().U(b.Obj.ID).I(0).I(0)) // attach
	if !damage.Empty() {
		_ = l.surf.Req(9, NewMsg().I(int32(damage.Min.X)).I(int32(damage.Min.Y)).I(int32(damage.Dx())).I(int32(damage.Dy()))) // damage_buffer
	}
	b.MarkBusy()
	return ch, l.surf.Req(6, NewMsg())
}

// Destroy unmaps and frees everything.
func (l *LayerSurface) Destroy() {
	_ = l.layer.Req(7, NewMsg())
	_ = l.surf.Req(0, NewMsg())
	for _, b := range l.bufs {
		b.Destroy()
	}
	l.bufs = nil
}
