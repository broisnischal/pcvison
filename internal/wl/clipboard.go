package wl

import (
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// Clipboard reads and owns the selection through (ext|zwlr)_data_control,
// which works without keyboard focus. The daemon is long-lived, so it can
// serve what it puts on the clipboard itself: no wl-copy child to babysit.
type Clipboard struct {
	c      *Conn
	mgr    *Object
	dev    *Object
	prefix string // interface prefix: ext_data_control or zwlr_data_control

	mu      sync.Mutex
	offers  map[uint32][]string // offer id -> mime types
	current uint32              // offer currently holding the selection (0 = empty)
	ours    *Object             // our source while we own the selection
}

// Snapshot is what a selection held, so it can be put back.
type Snapshot struct {
	Data map[string][]byte
}

func NewClipboard(c *Conn) (*Clipboard, error) {
	seat, err := c.BindIface("wl_seat", 1, nil)
	if err != nil {
		return nil, err
	}
	cb := &Clipboard{c: c, offers: map[uint32][]string{}}
	for _, p := range []string{"ext_data_control", "zwlr_data_control"} {
		iface := p + "_manager_v1"
		if _, ok := c.Find(iface); !ok {
			continue
		}
		cb.mgr, err = c.BindIface(iface, 1, nil)
		if err != nil {
			return nil, err
		}
		cb.prefix = p
		break
	}
	if cb.mgr == nil {
		return nil, errors.New("compositor offers no data-control protocol")
	}
	cb.dev = c.NewObject(cb.prefix+"_device_v1", cb.onDevice)
	if err := cb.mgr.Req(1, NewMsg().U(cb.dev.ID).U(seat.ID)); err != nil {
		return nil, err
	}
	return cb, c.Roundtrip(2 * time.Second)
}

func (cb *Clipboard) onDevice(op uint16, e *Event) {
	switch op {
	case 0: // data_offer(new_id)
		id := e.Uint()
		cb.mu.Lock()
		cb.offers[id] = nil
		cb.mu.Unlock()
		cb.c.Adopt(id, cb.prefix+"_offer_v1", func(op uint16, e *Event) {
			if op == 0 {
				mime := e.String()
				cb.mu.Lock()
				cb.offers[id] = append(cb.offers[id], mime)
				cb.mu.Unlock()
			}
		})
	case 1: // selection(offer or null)
		id := e.Uint()
		cb.mu.Lock()
		old := cb.current
		cb.current = id
		for o := range cb.offers {
			if o != id {
				delete(cb.offers, o)
			}
		}
		cb.mu.Unlock()
		if old != 0 && old != id {
			_ = cb.c.Send(old, 1, NewMsg()) // destroy the stale offer
			cb.c.Forget(old)
		}
	case 3: // primary_selection: we do not track it; drop the offer
		if id := e.Uint(); id != 0 {
			cb.mu.Lock()
			stale := id != cb.current
			if stale {
				delete(cb.offers, id)
			}
			cb.mu.Unlock()
			if stale {
				_ = cb.c.Send(id, 1, NewMsg())
				cb.c.Forget(id)
			}
		}
	}
}

func (cb *Clipboard) receive(offer uint32, mime string) ([]byte, error) {
	var p [2]int
	if err := unix.Pipe2(p[:], unix.O_CLOEXEC); err != nil {
		return nil, err
	}
	r := os.NewFile(uintptr(p[0]), "clip-r")
	defer r.Close()
	err := cb.c.Send(offer, 0, NewMsg().S(mime).FD(p[1]))
	unix.Close(p[1])
	if err != nil {
		return nil, err
	}
	_ = r.SetReadDeadline(time.Now().Add(1500 * time.Millisecond))
	data, err := io.ReadAll(io.LimitReader(r, 32<<20))
	if err != nil && !errors.Is(err, os.ErrDeadlineExceeded) {
		return data, err
	}
	return data, nil
}

func textMime(mimes []string) string {
	for _, want := range []string{"text/plain;charset=utf-8", "UTF8_STRING", "text/plain", "STRING", "TEXT"} {
		for _, m := range mimes {
			if m == want {
				return m
			}
		}
	}
	return ""
}

// Text returns the clipboard as text ("" if it holds something else).
func (cb *Clipboard) Text() (string, error) {
	cb.mu.Lock()
	cur, mimes := cb.current, cb.offers[cb.current]
	cb.mu.Unlock()
	if cur == 0 {
		return "", nil
	}
	m := textMime(mimes)
	if m == "" {
		return "", nil
	}
	b, err := cb.receive(cur, m)
	return string(b), err
}

// Save captures every type the selection offers (capped), so a paste can put
// the user's clipboard back afterwards, images included.
func (cb *Clipboard) Save() *Snapshot {
	cb.mu.Lock()
	cur, mimes := cb.current, append([]string(nil), cb.offers[cb.current]...)
	cb.mu.Unlock()
	if cur == 0 || len(mimes) == 0 {
		return nil
	}
	snap := &Snapshot{Data: map[string][]byte{}}
	total := 0
	for _, m := range mimes {
		if strings.HasPrefix(m, "x-special/") || strings.Contains(m, "chromium/x-") {
			continue
		}
		b, err := cb.receive(cur, m)
		if err != nil || len(b) == 0 {
			continue
		}
		total += len(b)
		if total > 48<<20 {
			break
		}
		snap.Data[m] = b
	}
	if len(snap.Data) == 0 {
		return nil
	}
	return snap
}

// SetText puts text on the clipboard.
func (cb *Clipboard) SetText(text string) error {
	data := []byte(text)
	return cb.set(map[string][]byte{
		"text/plain;charset=utf-8": data, "text/plain": data, "UTF8_STRING": data,
		"TEXT": data, "STRING": data,
	})
}

// Restore puts a saved selection back.
func (cb *Clipboard) Restore(s *Snapshot) error {
	if s == nil || len(s.Data) == 0 {
		return nil
	}
	return cb.set(s.Data)
}

func (cb *Clipboard) set(data map[string][]byte) error {
	var src *Object
	src = cb.c.NewObject(cb.prefix+"_source_v1", func(op uint16, e *Event) {
		switch op {
		case 0: // send(mime, fd)
			mime := e.String()
			fd := e.FD()
			if fd < 0 {
				return
			}
			payload := data[mime]
			go func() {
				f := os.NewFile(uintptr(fd), "clip-w")
				_, _ = f.Write(payload)
				f.Close()
			}()
		case 1: // cancelled: someone else owns the clipboard now
			cb.mu.Lock()
			if cb.ours == src {
				cb.ours = nil
			}
			cb.mu.Unlock()
			_ = src.Req(1, NewMsg())
		}
	})
	if err := cb.mgr.Req(0, NewMsg().U(src.ID)); err != nil {
		return err
	}
	for m := range data {
		_ = src.Req(0, NewMsg().S(m))
	}
	if err := cb.dev.Req(0, NewMsg().U(src.ID)); err != nil {
		return err
	}
	cb.mu.Lock()
	cb.ours = src
	cb.mu.Unlock()
	return cb.c.Roundtrip(time.Second)
}

// Owned reports whether the clipboard still holds what we last set.
func (cb *Clipboard) Owned() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.ours != nil
}
