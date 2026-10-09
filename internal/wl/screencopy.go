package wl

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// Raw is one captured frame, still in the compositor's pixel format.
type Raw struct {
	W, H    int
	Stride  int
	Format  uint32
	YInvert bool
	Pix     []byte
}

// Screencopy captures outputs with zwlr_screencopy_manager_v1. Captures are
// serialized; the shm buffers are cached per size so a steady stream of
// captures (the screen watch) allocates nothing.
type Screencopy struct {
	c   *Conn
	mgr *Object
	shm *Object
	mu  sync.Mutex
	buf map[[4]int]*Buffer
}

func NewScreencopy(c *Conn, shm *Object) (*Screencopy, error) {
	mgr, err := c.BindIface("zwlr_screencopy_manager_v1", 3, nil)
	if err != nil {
		return nil, err
	}
	return &Screencopy{c: c, mgr: mgr, shm: shm, buf: map[[4]int]*Buffer{}}, nil
}

func supportedFormat(f uint32) bool {
	switch f {
	case FmtARGB8888, FmtXRGB8888, FmtABGR8888, FmtXBGR8888,
		FmtXRGB2101010, FmtARGB2101010, FmtXBGR2101010, FmtABGR2101010:
		return true
	}
	return false
}

// Capture grabs an output, or a region of it in output-logical coordinates
// when region is non-nil ([x, y, w, h]).
func (s *Screencopy) Capture(out *Output, region *[4]int, cursor bool) (*Raw, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	type info struct{ fmt, w, h, stride uint32 }
	var (
		offers   []info
		yinvert  bool
		bufDone  = make(chan struct{}, 1)
		finished = make(chan error, 1)
	)
	frame := s.c.NewObject("zwlr_screencopy_frame_v1", func(op uint16, e *Event) {
		switch op {
		case 0: // buffer
			offers = append(offers, info{e.Uint(), e.Uint(), e.Uint(), e.Uint()})
		case 1: // flags
			yinvert = e.Uint()&1 != 0
		case 2: // ready
			finished <- nil
		case 3: // failed
			finished <- errors.New("the compositor refused the capture (screen locked, output off, or protected content)")
		case 6: // buffer_done
			bufDone <- struct{}{}
		}
	})
	defer frame.Req(1, NewMsg()) // destroy

	overlay := int32(0)
	if cursor {
		overlay = 1
	}
	var err error
	if region != nil {
		r := *region
		err = s.mgr.Req(1, NewMsg().U(frame.ID).I(overlay).U(out.Obj.ID).I(int32(r[0])).I(int32(r[1])).I(int32(r[2])).I(int32(r[3])))
	} else {
		err = s.mgr.Req(0, NewMsg().U(frame.ID).I(overlay).U(out.Obj.ID))
	}
	if err != nil {
		return nil, err
	}

	timeout := time.After(3 * time.Second)
	select {
	case <-bufDone:
	case err := <-finished:
		if err == nil {
			err = errors.New("capture finished before a buffer was offered")
		}
		return nil, err
	case <-s.c.Done():
		return nil, s.c.Err()
	case <-timeout:
		return nil, errors.New("screencopy: no buffer offer from the compositor")
	}
	var pick *info
	for i := range offers {
		if supportedFormat(offers[i].fmt) {
			pick = &offers[i]
			break
		}
	}
	if pick == nil {
		return nil, fmt.Errorf("screencopy offered no usable shm format (%v)", offers)
	}
	key := [4]int{int(pick.w), int(pick.h), int(pick.stride), int(pick.fmt)}
	b := s.buf[key]
	if b == nil {
		if len(s.buf) > 6 { // monitors changed mode; drop the old sizes
			for k, old := range s.buf {
				old.Destroy()
				delete(s.buf, k)
			}
		}
		b, err = NewBuffer(s.c, s.shm, int(pick.w), int(pick.h), int(pick.stride), pick.fmt)
		if err != nil {
			return nil, err
		}
		s.buf[key] = b
	}
	if err := frame.Req(0, NewMsg().U(b.Obj.ID)); err != nil { // copy
		return nil, err
	}
	select {
	case err := <-finished:
		if err != nil {
			return nil, err
		}
	case <-s.c.Done():
		return nil, s.c.Err()
	case <-timeout:
		return nil, errors.New("screencopy: the compositor never finished the copy")
	}
	pix := make([]byte, len(b.Data))
	copy(pix, b.Data)
	return &Raw{W: b.W, H: b.H, Stride: b.Stride, Format: b.Format, YInvert: yinvert, Pix: pix}, nil
}
