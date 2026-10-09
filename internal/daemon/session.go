package daemon

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"pc/internal/hypr"
	"pc/internal/wl"
)

// Session is the daemon's Wayland connection and everything bound on it. It
// is created on first use and rebuilt if the compositor goes away (a
// Hyprland restart hands out a new socket).
type Session struct {
	c     *wl.Conn
	outs  *wl.Outputs
	comp  *wl.Object
	shm   *wl.Object
	shell *wl.Object
	sc    *wl.Screencopy
	ptr   *wl.Pointer
	km    *wl.SeatKeymap // the user's layout, for keys sent through the compositor
	clip  *wl.Clipboard
	errs  map[string]string // what failed to bind, for doctor
}

// waylandDisplay finds the user's compositor socket even when the daemon was
// started without a login environment (MCP servers are): Hyprland writes its
// socket name into the instance lock file.
func waylandDisplay() string {
	if sig := hypr.Signature(); sig != "" {
		rt := os.Getenv("XDG_RUNTIME_DIR")
		if rt == "" {
			rt = fmt.Sprintf("/run/user/%d", os.Getuid())
		}
		b, _ := os.ReadFile(filepath.Join(rt, "hypr", sig, "hyprland.lock"))
		lines := strings.Fields(string(b))
		if len(lines) >= 2 && strings.HasPrefix(lines[1], "wayland-") {
			if _, err := os.Stat(wl.SocketPath(lines[1])); err == nil {
				return lines[1]
			}
		}
	}
	if d := os.Getenv("WAYLAND_DISPLAY"); d != "" {
		if _, err := os.Stat(wl.SocketPath(d)); err == nil {
			return d
		}
	}
	rt := filepath.Dir(wl.SocketPath("x"))
	socks, _ := filepath.Glob(filepath.Join(rt, "wayland-*"))
	var live []string
	for _, s := range socks {
		if !strings.HasSuffix(s, ".lock") {
			live = append(live, s)
		}
	}
	sort.Slice(live, func(i, j int) bool {
		a, _ := os.Stat(live[i])
		b, _ := os.Stat(live[j])
		return a != nil && b != nil && a.ModTime().After(b.ModTime())
	})
	if len(live) > 0 {
		return filepath.Base(live[0])
	}
	return ""
}

func (d *Daemon) session() (*Session, error) {
	d.sessMu.Lock()
	defer d.sessMu.Unlock()
	if d.sess != nil {
		select {
		case <-d.sess.c.Done():
			log.Printf("wayland connection lost: %v; reconnecting", d.sess.c.Err())
			d.sess = nil
			hypr.ResetDialect()
			d.ghost.lost()
		default:
			return d.sess, nil
		}
	}
	display := waylandDisplay()
	if display == "" {
		return nil, errors.New("no Wayland session found (is the desktop running?)")
	}
	c, err := wl.Connect(display)
	if err != nil {
		return nil, err
	}
	s := &Session{c: c, errs: map[string]string{}}
	s.outs = wl.TrackOutputs(c)
	note := func(what string, err error) {
		if err != nil {
			s.errs[what] = err.Error()
		}
	}
	s.comp, err = c.BindIface("wl_compositor", 4, nil)
	note("compositor", err)
	s.shm, err = c.BindIface("wl_shm", 1, nil)
	note("shm", err)
	s.shell, err = c.BindIface("zwlr_layer_shell_v1", 4, nil)
	note("layer-shell", err)
	if err := c.Roundtrip(2 * time.Second); err != nil {
		c.Close()
		return nil, err
	}
	if s.shm != nil {
		s.sc, err = wl.NewScreencopy(c, s.shm)
		note("screencopy", err)
	}
	s.ptr, err = wl.NewPointer(c)
	note("virtual-pointer", err)
	s.km, err = wl.WatchKeymap(c)
	note("keymap", err)
	s.clip, err = wl.NewClipboard(c)
	note("clipboard", err)
	if err := c.Roundtrip(2 * time.Second); err != nil {
		c.Close()
		return nil, fmt.Errorf("compositor rejected pc's devices: %w", err)
	}
	if hypr.Available() {
		hypr.NoAnimLayer(ghostNamespace)
	}
	log.Printf("wayland session on %s (%d outputs)", display, len(s.outs.All()))
	d.sess = s
	return s, nil
}

func (s *Session) need(what string, ok bool) error {
	if ok {
		return nil
	}
	if e := s.errs[what]; e != "" {
		return fmt.Errorf("%s unavailable: %s", what, e)
	}
	return fmt.Errorf("%s unavailable on this compositor", what)
}
