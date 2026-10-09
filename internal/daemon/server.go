// Package daemon is the resident half of pc. It owns everything that should
// not be paid for on every call: the Wayland connection and its virtual
// devices, the agent's on-screen cursor, a warm process sampler (so CPU% is
// instant), the event log, and the optional screen recorder. The CLI and the
// MCP server are thin clients that send it one JSON line per request.
package daemon

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"pc/internal/rpc"
	"pc/internal/wl"
)

type handler func(d *Daemon, a Args) (*rpc.Response, error)

var handlers = map[string]handler{}

func register(name string, h handler) { handlers[name] = h }

// Daemon is the process-wide state.
type Daemon struct {
	started time.Time
	lastUse atomic.Int64

	sessMu sync.Mutex
	sess   *Session

	act       sync.Mutex  // one gesture at a time
	borrowing atomic.Bool // the user's pointer is lent to a gesture right now
	ghost     *Ghost
	shots     *Shots
	mon       *Monitor
	watch     *Watch

	clipMu    sync.Mutex
	clipGen   int
	clipSaved *clipState

	kmap keymapCache

	lastTarget struct {
		sync.Mutex
		address string
		at      time.Time
	}
}

type clipState struct{ snap *wl.Snapshot }

// CacheDir holds screenshots, the action log and the screen-watch ring.
func CacheDir() string {
	if d := os.Getenv("PC_CACHE"); d != "" {
		return d
	}
	base := os.Getenv("XDG_CACHE_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".cache")
	}
	return filepath.Join(base, "pc")
}

// Run serves until killed.
func Run() error {
	if rpc.Running() {
		return errors.New("a pc daemon is already running")
	}
	if err := os.MkdirAll(rpc.Dir(), 0o700); err != nil {
		return err
	}
	_ = os.MkdirAll(CacheDir(), 0o700)
	_ = os.Remove(rpc.SocketPath())
	ln, err := net.Listen("unix", rpc.SocketPath())
	if err != nil {
		return err
	}
	_ = os.Chmod(rpc.SocketPath(), 0o600)
	_ = os.WriteFile(filepath.Join(rpc.Dir(), "daemon.pid"), []byte(strconv.Itoa(os.Getpid())), 0o600)
	log.SetFlags(log.LstdFlags)
	log.Printf("pc daemon %d up", os.Getpid())

	exeOnce.Do(stampExe)
	d := &Daemon{started: time.Now()}
	d.touch()
	recoverInputState()
	d.shots = newShots()
	d.ghost = newGhost(d)
	go d.ghost.appear()
	d.mon = newMonitor(d)
	d.watch = newWatch(d)
	go d.mon.loop()
	d.watch.resume()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	go func() {
		<-sig
		d.shutdown(ln)
		os.Exit(0)
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if closing.Load() {
				select {} // shutdown is exiting the process
			}
			return err
		}
		go d.serve(conn)
	}
}

var closing atomic.Bool

func (d *Daemon) shutdown(ln net.Listener) {
	closing.Store(true)
	log.Printf("shutting down")
	// Unlink before closing: once the listener is gone a client may start a
	// new daemon, and its fresh socket must not be removed by this one.
	_ = os.Remove(rpc.SocketPath())
	_ = ln.Close()
	pidFile := filepath.Join(rpc.Dir(), "daemon.pid")
	if b, err := os.ReadFile(pidFile); err == nil && strings.TrimSpace(string(b)) == strconv.Itoa(os.Getpid()) {
		_ = os.Remove(pidFile)
	}
	d.watch.halt()
	d.ghost.hideNow()
	recoverInputState()
}

func (d *Daemon) touch() { d.lastUse.Store(time.Now().UnixNano()) }

func (d *Daemon) idleFor() time.Duration {
	return time.Since(time.Unix(0, d.lastUse.Load()))
}

func (d *Daemon) serve(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReaderSize(conn, 1<<20)
	line, err := r.ReadBytes('\n')
	if err != nil {
		return
	}
	var req rpc.Request
	resp := &rpc.Response{}
	if err := json.Unmarshal(line, &req); err != nil {
		resp.Error = "bad request: " + err.Error()
	} else {
		resp = d.Handle(req)
	}
	b, _ := json.Marshal(resp)
	_, _ = conn.Write(append(b, '\n'))
	d.exitIfRebuilt()
}

var (
	exeOnce  sync.Once
	exePath  string
	exeStamp time.Time
	exeCheck atomic.Int64
)

func stampExe() {
	exePath, _ = os.Executable()
	if st, err := os.Stat(exePath); err == nil {
		exeStamp = st.ModTime()
	}
}

// exitIfRebuilt ends the daemon after a request if its binary was replaced
// (make build, a plugin update), so the next call starts the new one instead
// of an old daemon serving old code indefinitely.
func (d *Daemon) exitIfRebuilt() {
	exeOnce.Do(stampExe)
	now := time.Now().UnixNano()
	if last := exeCheck.Load(); now-last < int64(5*time.Second) || !exeCheck.CompareAndSwap(last, now) {
		return
	}
	st, err := os.Stat(exePath)
	if err != nil || exeStamp.IsZero() || st.ModTime().Equal(exeStamp) {
		return
	}
	log.Printf("binary %s was rebuilt; exiting so the next call runs the new one", exePath)
	exeStamp = time.Time{} // exit once
	go func() {
		time.Sleep(100 * time.Millisecond)
		p, _ := os.FindProcess(os.Getpid())
		_ = p.Signal(syscall.SIGTERM)
	}()
}

// Handle runs one request; panics become errors so one bad request never
// takes the daemon (and the agent's cursor) down.
func (d *Daemon) Handle(req rpc.Request) (resp *rpc.Response) {
	d.touch()
	defer func() {
		if p := recover(); p != nil {
			log.Printf("panic in %s: %v\n%s", req.Cmd, p, debug.Stack())
			resp = &rpc.Response{Error: fmt.Sprintf("internal error in %s: %v", req.Cmd, p)}
		}
	}()
	h, ok := handlers[req.Cmd]
	if !ok {
		return &rpc.Response{Error: "unknown command " + strconv.Quote(req.Cmd)}
	}
	if label := statusOf[req.Cmd]; label != "" { // input commands set their own
		d.ghost.act(label)
		defer d.ghost.done(600 * time.Millisecond)
	}
	if isInput(req.Cmd) {
		if err := inputAllowed(); err != nil {
			return &rpc.Response{Error: err.Error()}
		}
		d.act.Lock()
		defer d.act.Unlock()
	}
	r, err := h(d, Args(req.Args))
	if err != nil {
		if r == nil {
			r = &rpc.Response{}
		}
		r.OK = false
		r.Error = err.Error()
		return r
	}
	if r == nil {
		r = &rpc.Response{}
	}
	r.OK = true
	return r
}

// statusOf is what the agent's cursor tag says while a read-only command runs.
var statusOf = map[string]string{
	"shot": "looking", "windows": "looking", "wait": "waiting", "watch": "watching",
	"state": "checking the system", "sys": "checking the system", "ps": "checking processes",
	"proc": "checking processes", "events": "reading events", "units": "checking services",
	"logs": "reading logs", "ports": "checking ports",
}

func init() {
	register("ping", func(d *Daemon, a Args) (*rpc.Response, error) {
		return text(fmt.Sprintf("pc daemon %d, up %s", os.Getpid(), human(time.Since(d.started)))), nil
	})
	register("shutdown", func(d *Daemon, a Args) (*rpc.Response, error) {
		go func() {
			time.Sleep(50 * time.Millisecond)
			p, _ := os.FindProcess(os.Getpid())
			_ = p.Signal(syscall.SIGTERM)
		}()
		return text("pc daemon stopping"), nil
	})
}

func text(s string) *rpc.Response { return &rpc.Response{Text: s} }

// Args is a request's argument map with typed getters.
type Args map[string]any

func (a Args) Has(k string) bool { _, ok := a[k]; return ok }

func (a Args) Str(k string) string {
	switch v := a[k].(type) {
	case string:
		return v
	case []any:
		return strings.Join(a.Strs(k), " ")
	case []string:
		return strings.Join(v, " ")
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(v)
	}
	return ""
}

func (a Args) Strs(k string) []string {
	switch v := a[k].(type) {
	case string:
		return []string{v}
	case []any:
		var out []string
		for _, x := range v {
			out = append(out, fmt.Sprint(x))
		}
		return out
	case []string:
		return v
	}
	return nil
}

func (a Args) Num(k string) (float64, bool) {
	switch v := a[k].(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case string:
		f, err := strconv.ParseFloat(v, 64)
		return f, err == nil
	}
	return 0, false
}

func (a Args) Int(k string, def int) int {
	if v, ok := a.Num(k); ok {
		return int(v)
	}
	return def
}

func (a Args) Bool(k string) bool {
	switch v := a[k].(type) {
	case bool:
		return v
	case string:
		return v == "true" || v == "1" || v == "yes"
	case float64:
		return v != 0
	}
	return false
}

// BoolDef is Bool with a default when the key is absent.
func (a Args) BoolDef(k string, def bool) bool {
	if !a.Has(k) {
		return def
	}
	return a.Bool(k)
}

// Dur reads "30s", "15m", "2h", "1d" or a bare number of seconds.
func (a Args) Dur(k string, def time.Duration) time.Duration {
	s := strings.TrimSpace(a.Str(k))
	if s == "" {
		return def
	}
	return parseDur(s, def)
}

func parseDur(s string, def time.Duration) time.Duration {
	if strings.HasSuffix(s, "d") {
		if n, err := strconv.ParseFloat(strings.TrimSuffix(s, "d"), 64); err == nil {
			return time.Duration(n * float64(24*time.Hour))
		}
	}
	if n, err := strconv.ParseFloat(s, 64); err == nil {
		return time.Duration(n * float64(time.Second))
	}
	if v, err := time.ParseDuration(s); err == nil {
		return v
	}
	return def
}
