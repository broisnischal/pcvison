package wl

import (
	"bytes"
	"errors"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// SeatKeymap follows the keymap the compositor hands keyboard clients: the
// layout the user's keys are read with right now. pc never takes keyboard
// focus, so the only events it sees here are keymap changes.
type SeatKeymap struct {
	mu    sync.Mutex
	text  string
	gen   int
	ready chan struct{}
	once  sync.Once
}

func WatchKeymap(c *Conn) (*SeatKeymap, error) {
	seat, err := c.BindIface("wl_seat", 5, nil)
	if err != nil {
		return nil, err
	}
	k := &SeatKeymap{ready: make(chan struct{})}
	kb := c.NewObject("wl_keyboard", func(op uint16, e *Event) {
		if op != 0 { // keymap; enter, leave, key never come to a client without focus
			return
		}
		format, fd, size := e.Uint(), e.FD(), e.Uint()
		if fd < 0 {
			return
		}
		defer unix.Close(fd)
		if format != 1 || size == 0 || size > 16<<20 { // XKB_V1
			return
		}
		data, err := unix.Mmap(fd, 0, int(size), unix.PROT_READ, unix.MAP_PRIVATE)
		if err != nil {
			return
		}
		text := string(bytes.TrimRight(data, "\x00"))
		_ = unix.Munmap(data)
		k.mu.Lock()
		k.text = text
		k.gen++
		k.mu.Unlock()
		k.once.Do(func() { close(k.ready) })
	})
	if err := seat.Req(1, NewMsg().U(kb.ID)); err != nil { // get_keyboard
		return nil, err
	}
	return k, nil
}

// Text is the current keymap and a generation number that changes with it.
func (k *SeatKeymap) Text(wait time.Duration) (string, int, error) {
	select {
	case <-k.ready:
	case <-time.After(wait):
		return "", 0, errors.New("the compositor sent no keymap")
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.text, k.gen, nil
}
