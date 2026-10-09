package sys

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The agent shares one seat with the human, so before it acts it needs to
// know what their hands are doing: which keys and buttons are down right now
// (EVIOCGKEY, read-only, no grab; needs the input group), and when they last
// typed, clicked or moved (the event stream of the same devices).
//
// The devices stay open: opening a USB input device can take 15 ms (it wakes
// the device), which made the modifier check cost 60 ms per action when it
// reopened them every time. Reading the key state of an open one takes
// microseconds. pc's own virtual pointer and keyboard are Wayland objects,
// not evdev devices, so nothing here ever sees the agent's input.

var modCodes = map[int]string{
	29: "Ctrl", 97: "Ctrl", 42: "Shift", 54: "Shift", 56: "Alt", 100: "AltGr", 125: "Super", 126: "Super",
}

func ioc(dir, typ, nr, size uintptr) uintptr { return dir<<30 | size<<16 | typ<<8 | nr }

const (
	iocRead = 2
	keyMax  = 0x2ff

	evKey = 1
	evRel = 2
	evAbs = 3

	btnMouse = 0x110 // BTN_LEFT; mouse buttons run to 0x117
	btnTouch = 0x14a
)

type device struct {
	f        *os.File
	name     string
	keyboard bool // has letter keys
	pointer  bool // moves a cursor: mouse, touchpad, tablet, trackpoint
}

var (
	devMu   sync.Mutex
	devOpen = map[string]*device{} // path -> open input device
	devSkip = map[string]bool{}    // paths that are neither
	devAt   time.Time
)

func bits(f *os.File, ev uintptr, n int) []byte {
	b := make([]byte, n/8+1)
	_, _, e := unix.Syscall(unix.SYS_IOCTL, f.Fd(), ioc(iocRead, 'E', 0x20+ev, uintptr(len(b))), uintptr(unsafe.Pointer(&b[0])))
	if e != 0 {
		return nil
	}
	return b
}

// rescan picks up plugged and unplugged devices; it only opens paths it has
// not seen before. Caller holds devMu.
func rescan(force bool) {
	if !force && time.Since(devAt) < 10*time.Second {
		return
	}
	devAt = time.Now()
	paths, _ := filepath.Glob("/dev/input/event*")
	present := map[string]bool{}
	for _, p := range paths {
		present[p] = true
		if devOpen[p] != nil || devSkip[p] {
			continue
		}
		f, err := os.OpenFile(p, os.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if err != nil {
			devSkip[p] = true
			continue
		}
		keys := bits(f, evKey, keyMax)
		rel := bits(f, evRel, 0x0f)
		abs := bits(f, evAbs, 0x3f)
		d := &device{f: f}
		d.keyboard = hasBit(keys, 29) && hasBit(keys, 30) // Ctrl and A
		d.pointer = hasBit(rel, 0) && hasBit(rel, 1) && hasBit(keys, btnMouse) || // a mouse: REL_X, REL_Y, BTN_LEFT
			hasBit(abs, 0) && hasBit(abs, 1) && (hasBit(keys, btnTouch) || hasBit(keys, btnMouse)) // touchpad, tablet
		if !d.keyboard && !d.pointer {
			f.Close()
			devSkip[p] = true
			continue
		}
		name := make([]byte, 128)
		unix.Syscall(unix.SYS_IOCTL, f.Fd(), ioc(iocRead, 'E', 0x06, uintptr(len(name))), uintptr(unsafe.Pointer(&name[0])))
		d.name = strings.TrimRight(string(name), "\x00")
		devOpen[p] = d
	}
	for p, d := range devOpen {
		if !present[p] {
			d.f.Close()
			delete(devOpen, p)
		}
	}
	for p := range devSkip {
		if !present[p] {
			delete(devSkip, p)
		}
	}
}

func hasBit(b []byte, n int) bool { return n/8 < len(b) && b[n/8]&(1<<(n%8)) != 0 }

// keyState reads which keys and buttons a device has down. Caller holds devMu.
func keyState(p string, d *device, state []byte) bool {
	clear(state)
	_, _, e := unix.Syscall(unix.SYS_IOCTL, d.f.Fd(), ioc(iocRead, 'E', 0x18, uintptr(len(state))), uintptr(unsafe.Pointer(&state[0])))
	if e != 0 { // unplugged: forget it, the next rescan reopens what is there
		d.f.Close()
		delete(devOpen, p)
		return false
	}
	return true
}

// HeldModifiers lists modifier keys currently held down on any keyboard, as
// "Ctrl on <device>". Empty when nothing is held or the devices are unreadable.
func HeldModifiers() []string {
	devMu.Lock()
	defer devMu.Unlock()
	rescan(false)
	var out []string
	state := make([]byte, keyMax/8+1)
	for p, d := range devOpen {
		if !d.keyboard || !keyState(p, d, state) {
			continue
		}
		for code, mod := range modCodes {
			if hasBit(state, code) {
				out = append(out, mod+" on "+d.name)
			}
		}
	}
	return out
}

// CanReadKeyboards reports whether the hand checks work here.
func CanReadKeyboards() bool {
	devMu.Lock()
	defer devMu.Unlock()
	rescan(false)
	for _, d := range devOpen {
		if d.keyboard {
			return true
		}
	}
	return false
}

// Hands is what the human is doing with their own keyboard and mouse.
type Hands struct {
	KeysDown    int // keys held on physical keyboards right now, modifiers included
	ModsDown    int
	ButtonsDown int // mouse buttons held, or a finger on a touchpad
	LastKey     time.Time
	LastMotion  time.Time
	LastButton  time.Time
	Keyboards   int
	Pointers    int
	Watching    bool // the event stream is being read, so the Last* times are live
}

// Since is how long ago their hands last did anything.
func (h Hands) Since() time.Duration {
	last := h.LastKey
	for _, t := range []time.Time{h.LastMotion, h.LastButton} {
		if t.After(last) {
			last = t
		}
	}
	if last.IsZero() {
		return time.Hour
	}
	return time.Since(last)
}

var (
	lastKey, lastMotion, lastButton atomic.Int64 // unix nanos
	watchUse                        atomic.Int64
	watchOn                         atomic.Bool
)

// UserHands reads the held keys and buttons now, and starts the event watch
// if it is not running (it stops itself after a few idle minutes).
func UserHands() Hands {
	WatchHands()
	devMu.Lock()
	defer devMu.Unlock()
	rescan(false)
	h := Hands{Watching: watchOn.Load()}
	state := make([]byte, keyMax/8+1)
	for p, d := range devOpen {
		if !keyState(p, d, state) {
			continue
		}
		if d.keyboard {
			h.Keyboards++
			for c := 1; c < btnMouse; c++ {
				if hasBit(state, c) {
					h.KeysDown++
					if _, ok := modCodes[c]; ok {
						h.ModsDown++
					}
				}
			}
		}
		if d.pointer {
			h.Pointers++
			for c := btnMouse; c < btnMouse+8; c++ {
				if hasBit(state, c) {
					h.ButtonsDown++
				}
			}
			if hasBit(state, btnTouch) {
				h.ButtonsDown++
			}
		}
	}
	h.LastKey = nanos(lastKey.Load())
	h.LastMotion = nanos(lastMotion.Load())
	h.LastButton = nanos(lastButton.Load())
	return h
}

func nanos(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

// WatchHands keeps a reader on every keyboard and pointing device so the
// last-activity times are live. One goroutine polls them all; it exits after
// three minutes without a caller, so an idle daemon reads nothing.
func WatchHands() {
	watchUse.Store(time.Now().UnixNano())
	if !watchOn.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer watchOn.Store(false)
		buf := make([]byte, 24*64)
		for time.Since(nanos(watchUse.Load())) < 3*time.Minute {
			devMu.Lock()
			rescan(false)
			var fds []unix.PollFd
			var devs []*device
			for _, d := range devOpen {
				fds = append(fds, unix.PollFd{Fd: int32(d.f.Fd()), Events: unix.POLLIN})
				devs = append(devs, d)
			}
			devMu.Unlock()
			if len(fds) == 0 {
				time.Sleep(time.Second)
				continue
			}
			n, err := unix.Poll(fds, 500)
			if err != nil && err != unix.EINTR {
				time.Sleep(100 * time.Millisecond)
				continue
			}
			if n <= 0 {
				continue
			}
			now := time.Now().UnixNano()
			for i, pf := range fds {
				if pf.Revents&unix.POLLIN == 0 {
					continue
				}
				for {
					m, err := unix.Read(int(pf.Fd), buf)
					if m <= 0 || err != nil {
						break
					}
					readEvents(buf[:m], devs[i], now)
					if m < len(buf) {
						break
					}
				}
			}
		}
	}()
}

// readEvents files a batch of struct input_event (24 bytes on 64-bit).
func readEvents(b []byte, d *device, now int64) {
	for i := 0; i+24 <= len(b); i += 24 {
		typ := binary.LittleEndian.Uint16(b[i+16:])
		code := binary.LittleEndian.Uint16(b[i+18:])
		switch {
		case typ == evKey && code >= btnMouse && code < btnMouse+8, typ == evKey && code == btnTouch:
			lastButton.Store(now)
		case typ == evKey && d.keyboard:
			lastKey.Store(now)
		case typ == evRel || typ == evAbs:
			if d.pointer {
				lastMotion.Store(now)
			}
		}
	}
}
