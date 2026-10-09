package daemon

import (
	"bufio"
	"encoding/binary"
	"net"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"pc/internal/hypr"
	"pc/internal/rpc"
)

// TestTwoUsers runs the user and the agent at the same time on the real
// desktop (PC_LIVE=1). "The user" is a uinput keyboard and mouse, so its
// input takes the same path as real hardware (kernel, libinput, the input
// method, Hyprland). The agent is the running pc daemon. Test windows: one
// the user types into (focused, as theirs would be), one the agent types into
// (on a hidden workspace), one the agent clicks (floating). It stops at once
// if focus leaves the user's test window, so a slip can only type into it.
//
// PC_OUT=dir keeps the logs.
func TestTwoUsers(t *testing.T) {
	if os.Getenv("PC_LIVE") == "" {
		t.Skip("PC_LIVE=1")
	}
	dir := os.Getenv("PC_OUT")
	if dir == "" {
		dir = t.TempDir()
	}
	kb, err := newUinput("pc-test-user-keyboard", false)
	if err != nil {
		t.Fatal(err)
	}
	defer kb.Close()
	mouse, err := newUinput("pc-test-user-mouse", true)
	if err != nil {
		t.Fatal(err)
	}
	defer mouse.Close()

	real := watchRealHands(t)
	defer real.stop()
	events := watchHyprEvents()
	defer func() {
		if t.Failed() {
			t.Logf("hyprland events during the test:\n%s", events())
		}
	}()
	only := os.Getenv("PC_STEP")

	userLog, agentLog, clickLog := filepath.Join(dir, "user.log"), filepath.Join(dir, "agent.log"), filepath.Join(dir, "click.log")
	for _, p := range []string{userLog, agentLog, clickLog} {
		_ = os.Remove(p)
	}
	logger := filepath.Join(dir, "log.sh")
	_ = os.WriteFile(logger, []byte("#!/bin/sh\n[ \"$2\" = mouse ] && printf '\\033[?1000h\\033[?1006h'\nstty raw -echo\nexec cat > \"$1\"\n"), 0o755)
	orig, _ := hypr.ActiveWindow()
	open := func(app, log, mode, rules string) hypr.Client {
		t.Helper()
		_, _ = hypr.Eval(fmt.Sprintf(`hl.dispatch(hl.dsp.exec_cmd(%s, {%s}))`,
			hypr.LuaString(fmt.Sprintf("foot --app-id %s %s %s %s", app, logger, log, mode)), rules))
		for i := 0; i < 100; i++ {
			time.Sleep(50 * time.Millisecond)
			if c, err := findWindow(app); err == nil {
				return c
			}
		}
		t.Fatalf("%s never appeared", app)
		return hypr.Client{}
	}
	user := open("pc-user-test", userLog, "keys", `float = true, pin = true, size = "520 220", move = "460 800", no_initial_focus = true, monitor = "HDMI-A-1"`)
	agent := open("pc-agent-test2", agentLog, "keys", `workspace = "name:pctest silent", no_initial_focus = true`)
	clicks := open("pc-click-test2", clickLog, "mouse", `float = true, pin = true, size = "360 220", move = "60 800", no_initial_focus = true, monitor = "HDMI-A-1"`)
	defer func() {
		for _, c := range []hypr.Client{user, agent, clicks} {
			_ = hypr.CloseWindow(c.Address)
		}
		if orig.Address != "" {
			_ = hypr.Focus(orig.Address)
		}
	}()
	time.Sleep(700 * time.Millisecond) // libinput adopts the new devices
	real.waitIdle(t, "start")
	// the user's pointer rests on their own window, as it would with
	// focus-follows-mouse, and stays inside it for the whole test
	homeX, homeY := 480.0, 830.0
	_, _ = hypr.Eval(fmt.Sprintf(`hl.dispatch(hl.dsp.cursor.move({x = %v, y = %v}))`, homeX, homeY))
	if err := hypr.Focus(user.Address); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	real.mark()

	// focus must stay on the user's window the whole time
	var focusMoved atomic.Value
	stopWatch := make(chan struct{})
	var watchWG sync.WaitGroup
	watchWG.Add(1)
	var pointerFixed atomic.Bool // set while the test itself is not moving the pointer
	var outsideMotion atomic.Int64
	go func() {
		defer watchWG.Done()
		lx, ly, _ := hypr.CursorPos()
		for {
			select {
			case <-stopWatch:
				return
			case <-time.After(10 * time.Millisecond):
			}
			x, y, _ := hypr.CursorPos()
			movedNow := x != lx || y != ly
			lx, ly = x, y
			if movedNow && pointerFixed.Load() {
				outsideMotion.Add(1)
			}
			if a, err := hypr.ActiveWindow(); err == nil && a.Address != user.Address && focusMoved.Load() == nil {
				why := "with the pointer still: the agent moved it"
				if movedNow || outsideMotion.Load() > 0 {
					why = "after the pointer moved (focus follows the mouse)"
				}
				focusMoved.Store(describeWindow(a) + ", " + why)
			}
		}
	}()
	safe := func() bool { // never let the simulated user type anywhere but its own window
		a, err := hypr.ActiveWindow()
		return err == nil && a.Address == user.Address && focusMoved.Load() == nil
	}
	userTypes := func(text string, hold, gap time.Duration) {
		for _, r := range text {
			if !safe() {
				t.Errorf("focus left the user's test window (%v); stopped typing", focusMoved.Load())
				return
			}
			code, shift := usKey(r)
			if shift {
				kb.key(42, 1)
			}
			kb.key(code, 1)
			time.Sleep(hold)
			kb.key(code, 0)
			if shift {
				kb.key(42, 0)
			}
			time.Sleep(gap)
		}
	}
	agentCall := func(cmd string, args map[string]any) (string, time.Duration) {
		t0 := time.Now()
		r, err := rpc.Call(rpc.Request{Cmd: cmd, Args: args}, 30*time.Second)
		if err != nil {
			return "rpc: " + err.Error(), time.Since(t0)
		}
		if !r.OK {
			return "error: " + r.Error, time.Since(t0)
		}
		return r.Text, time.Since(t0)
	}

	// 1. both type at once
	pointerFixed.Store(true)
	userText := strings.Repeat("the quick brown fox jumps over the lazy dog 0123456789 ", 3)
	var agentWant strings.Builder
	var wg sync.WaitGroup
	wg.Add(1)
	t0 := time.Now()
	go func() {
		defer wg.Done()
		userTypes(userText, 45*time.Millisecond, 25*time.Millisecond)
	}()
	var lat []time.Duration
	for i := 0; i < 24; i++ {
		line := fmt.Sprintf("Agent line %02d: Hello, World! {\"ok\": true} ~/path?x=1\n", i)
		agentWant.WriteString(strings.ReplaceAll(line, "\n", "\r"))
		out, took := agentCall("type", map[string]any{"text": line, "window": agent.Address})
		if strings.HasPrefix(out, "error") || strings.HasPrefix(out, "rpc") {
			t.Errorf("agent type %d: %s", i, out)
		}
		lat = append(lat, took)
		time.Sleep(150 * time.Millisecond)
	}
	wg.Wait()
	t.Logf("typing together for %v: agent type calls took %v (min) to %v (max)", time.Since(t0).Round(time.Millisecond), minDur(lat), maxDur(lat))
	time.Sleep(300 * time.Millisecond)
	gotUser, _ := os.ReadFile(userLog)
	gotAgent, _ := os.ReadFile(agentLog)
	if string(gotUser) != userText {
		t.Errorf("user's window got %d bytes, want %d:\n got %q\nwant %q", len(gotUser), len(userText), gotUser, userText)
	} else {
		t.Logf("user's window: all %d chars, in order, nothing extra", len(gotUser))
	}
	if string(gotAgent) != agentWant.String() {
		t.Errorf("agent's window:\n got %q\nwant %q", gotAgent, agentWant.String())
	} else {
		t.Logf("agent's window: all %d chars of 24 lines, in order, nothing extra", len(gotAgent))
	}

	real.report(t, "typing together")
	if only == "type" {
		return
	}

	// 2. the user holds Shift while the agent types; the agent's text must stay lowercase
	real.waitIdle(t, "held Shift")
	if !safe() {
		t.Fatal("focus is not on the user's test window")
	}
	kb.key(42, 1)
	time.Sleep(50 * time.Millisecond)
	_ = os.Truncate(agentLog, 0)
	before, _ := os.ReadFile(userLog)
	out, took := agentCall("type", map[string]any{"text": "lowercase while shift is held", "window": agent.Address})
	if safe() {
		kb.key(45, 1) // the user, still holding Shift, types x
		time.Sleep(40 * time.Millisecond)
		kb.key(45, 0)
	}
	kb.key(42, 0)
	time.Sleep(200 * time.Millisecond)
	gotAgent, _ = os.ReadFile(agentLog)
	gotUser, _ = os.ReadFile(userLog)
	t.Logf("held Shift: agent call took %v (%s)", took.Round(time.Millisecond), firstLineOf(out))
	if !strings.HasSuffix(string(gotAgent), "lowercase while shift is held") {
		t.Errorf("agent text under a held Shift: %q", gotAgent)
	}
	if tail := strings.TrimPrefix(string(gotUser), string(before)); tail != "X" {
		t.Errorf("user's shifted x after the agent typed: %q, want \"X\"", tail)
	}

	// 3. the user moves the mouse while the agent clicks; first a control run
	real.report(t, "held Shift")
	pointerFixed.Store(false)
	if n := outsideMotion.Load(); n > 0 {
		t.Logf("NOTE: the pointer moved %d times while only the keyboard was in use: that was the human's mouse (or smooly), not the test", n)
	}
	path := func(n int) { // a zigzag inside the user's window
		for i := 0; i < n; i++ {
			dx, dy := int32(2), int32(0)
			if (i/60)%2 == 1 {
				dx, dy = 0, 1
			}
			mouse.rel(dx, dy)
			time.Sleep(4 * time.Millisecond)
		}
	}
	home := func() (float64, float64) {
		_, _ = hypr.Eval(fmt.Sprintf(`hl.dispatch(hl.dsp.cursor.move({x = %v, y = %v}))`, homeX, homeY))
		time.Sleep(100 * time.Millisecond)
		x, y, _ := hypr.CursorPos()
		return x, y
	}
	real.waitIdle(t, "pointer")
	sx, sy := home()
	path(240)
	time.Sleep(100 * time.Millisecond)
	cx, cy, _ := hypr.CursorPos()
	t.Logf("control: the user's mouse moved %.0f,%.0f → %.0f,%.0f", sx, sy, cx, cy)
	_ = os.Truncate(clickLog, 0)
	sx, sy = home()
	var maxStep float64
	var samples int
	stop := make(chan struct{})
	var sampWG sync.WaitGroup
	sampWG.Add(1)
	go func() { // the user's pointer as the compositor reports it, as often as it answers
		defer sampWG.Done()
		px, py, _ := hypr.CursorPos()
		for {
			select {
			case <-stop:
				return
			default:
			}
			x, y, err := hypr.CursorPos()
			if err == nil {
				maxStep = math.Max(maxStep, math.Hypot(x-px, y-py))
				px, py = x, y
				samples++
			}
		}
	}()
	wg.Add(1)
	go func() { defer wg.Done(); path(240) }()
	var clickLat []time.Duration
	for i := 0; i < 8; i++ {
		out, took := agentCall("click", map[string]any{"x": 120.0 + float64(i)*25, "y": 900.0, "abs": true})
		if strings.HasPrefix(out, "error") {
			t.Errorf("agent click %d: %s", i, out)
		}
		clickLat = append(clickLat, took)
	}
	wg.Wait()
	close(stop)
	sampWG.Wait()
	time.Sleep(100 * time.Millisecond)
	ex, ey, _ := hypr.CursorPos()
	t.Logf("with 8 agent clicks: the user's mouse moved %.0f,%.0f → %.0f,%.0f (control ended %.0f,%.0f); largest jump between %d pointer samples %.1fpx; clicks took %v to %v",
		sx, sy, ex, ey, cx, cy, samples, maxStep, minDur(clickLat), maxDur(clickLat))
	if math.Hypot(ex-cx, ey-cy) > 6 {
		t.Errorf("the user's pointer ended %.0fpx away from the control run", math.Hypot(ex-cx, ey-cy))
	}
	if maxStep > 30 {
		t.Errorf("the user's pointer jumped %.0fpx between samples", maxStep)
	}
	gotClicks, _ := os.ReadFile(clickLog)
	press := strings.Count(string(gotClicks), "M")
	release := strings.Count(string(gotClicks), "m")
	t.Logf("click window got %d presses and %d releases", press, release)
	if press != 8 || release != 8 {
		t.Errorf("click window log: %q", gotClicks)
	}

	real.report(t, "pointer")

	// 4. a hover waits for a still mouse, and lets go when the user moves
	real.waitIdle(t, "hover")
	wg.Add(1)
	go func() { defer wg.Done(); path(120) }()
	out, took = agentCall("hover", map[string]any{"x": 900.0, "y": 600.0, "abs": true, "dwell": 1500, "look": false})
	wg.Wait()
	t.Logf("hover while the user moved: %v: %s", took.Round(time.Millisecond), firstLineOf(out))
	hx, hy := home()
	go func() {
		time.Sleep(500 * time.Millisecond) // the user grabs the mouse mid-hover
		mouse.rel(10, 10)
	}()
	out, took = agentCall("hover", map[string]any{"x": 900.0, "y": 600.0, "abs": true, "dwell": 1500, "look": false})
	time.Sleep(100 * time.Millisecond)
	fx, fy, _ := hypr.CursorPos()
	t.Logf("user moved mid-hover: %v: %s; their pointer %.0f,%.0f → %.0f,%.0f", took.Round(time.Millisecond), firstLineOf(out), hx, hy, fx, fy)
	if math.Hypot(fx-hx, fy-hy) > 40 {
		t.Errorf("after the interrupted hover the user's pointer is %.0fpx from where they had it", math.Hypot(fx-hx, fy-hy))
	}

	real.report(t, "hover")
	close(stopWatch)
	watchWG.Wait()
	if m := focusMoved.Load(); m != nil {
		t.Errorf("focus moved off the user's window to %v", m)
	} else {
		t.Logf("focus stayed on the user's window from start to end")
	}
	if out, err := exec.Command("pc", "cursor").Output(); err == nil {
		t.Logf("%s", strings.TrimSpace(string(out)))
	}
}

// watchHyprEvents records Hyprland's event stream (focus, monitor,
// workspace changes) with timestamps.
func watchHyprEvents() func() string {
	var mu sync.Mutex
	var b strings.Builder
	t0 := time.Now()
	conn, err := net.Dial("unix", filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "hypr", hypr.Signature(), ".socket2.sock"))
	if err != nil {
		return func() string { return "(no event socket: " + err.Error() + ")" }
	}
	go func() {
		sc := bufio.NewScanner(conn)
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "activewindowv2") || strings.HasPrefix(line, "activewindow>>") || strings.HasPrefix(line, "focusedmon") ||
				strings.HasPrefix(line, "workspace") || strings.HasPrefix(line, "submap") || strings.HasPrefix(line, "activelayout") {
				mu.Lock()
				fmt.Fprintf(&b, "  %7.3fs %s\n", time.Since(t0).Seconds(), clip(line, 120))
				mu.Unlock()
			}
		}
	}()
	return func() string {
		conn.Close()
		mu.Lock()
		defer mu.Unlock()
		return b.String()
	}
}

// realHands watches the real keyboards and mice (everything but the test's
// own uinput devices), so a step the human touched is reported as such.
type realHands struct {
	last  atomic.Int64
	since atomic.Int64
	done  chan struct{}
	files []*os.File
}

func watchRealHands(t *testing.T) *realHands {
	r := &realHands{done: make(chan struct{})}
	paths, _ := filepath.Glob("/dev/input/event*")
	for _, p := range paths {
		f, err := os.OpenFile(p, os.O_RDONLY|unix.O_NONBLOCK, 0)
		if err != nil {
			continue
		}
		name := make([]byte, 128)
		unix.Syscall(unix.SYS_IOCTL, f.Fd(), uintptr(2<<30|128<<16|'E'<<8|0x06), uintptr(unsafe.Pointer(&name[0])))
		if strings.HasPrefix(string(name), "pc-test-user") {
			f.Close()
			continue
		}
		r.files = append(r.files, f)
	}
	go func() {
		buf := make([]byte, 24*64)
		for {
			select {
			case <-r.done:
				return
			case <-time.After(5 * time.Millisecond):
			}
			for _, f := range r.files {
				for {
					n, err := f.Read(buf)
					if n <= 0 || err != nil {
						break
					}
					for i := 0; i+24 <= n; i += 24 {
						if typ := binary.LittleEndian.Uint16(buf[i+16:]); typ >= 1 && typ <= 3 {
							r.last.Store(time.Now().UnixNano())
						}
					}
				}
			}
		}
	}()
	return r
}

func (r *realHands) stop() {
	close(r.done)
	for _, f := range r.files {
		f.Close()
	}
}

// waitIdle waits (up to 20 s) until the human has left keyboard and mouse
// alone for 1.5 s, then starts counting. Pointer stillness is read from the
// compositor: a mouse grabbed by a remapper (smoolyd here) sends no evdev
// events to anyone else.
func (r *realHands) waitIdle(t *testing.T, step string) {
	deadline := time.Now().Add(20 * time.Second)
	lx, ly, _ := hypr.CursorPos()
	still := time.Now()
	for time.Since(time.Unix(0, r.last.Load())) < 1500*time.Millisecond || time.Since(still) < 1500*time.Millisecond {
		if time.Now().After(deadline) {
			t.Logf("%s: the real keyboard or mouse stayed busy; running anyway", step)
			break
		}
		time.Sleep(25 * time.Millisecond)
		if x, y, _ := hypr.CursorPos(); x != lx || y != ly {
			lx, ly, still = x, y, time.Now()
		}
	}
	r.mark()
}

func (r *realHands) mark() { r.since.Store(time.Now().UnixNano()) }

func (r *realHands) report(t *testing.T, step string) {
	if r.last.Load() > r.since.Load() {
		t.Logf("NOTE %s: the real keyboard or mouse was used during this step, so its result includes the human's input", step)
	}
}

func minDur(ds []time.Duration) time.Duration {
	m := time.Hour
	for _, d := range ds {
		m = min(m, d)
	}
	return m.Round(time.Millisecond)
}

func maxDur(ds []time.Duration) time.Duration {
	var m time.Duration
	for _, d := range ds {
		m = max(m, d)
	}
	return m.Round(time.Millisecond)
}

// ---------------------------------------------------------------- uinput

type uinput struct{ f *os.File }

func uiIoc(dir, nr, size uintptr) uintptr { return dir<<30 | size<<16 | 'U'<<8 | nr }

func newUinput(name string, mouse bool) (*uinput, error) {
	f, err := os.OpenFile("/dev/uinput", os.O_WRONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	set := func(nr, v uintptr) error {
		_, _, e := unix.Syscall(unix.SYS_IOCTL, f.Fd(), uiIoc(1, nr, 4), v)
		if e != 0 {
			return e
		}
		return nil
	}
	_ = set(100, 1) // EV_KEY
	if mouse {
		_ = set(100, 2) // EV_REL
		for _, c := range []uintptr{0, 1, 8} {
			_ = set(102, c)
		}
		for _, c := range []uintptr{0x110, 0x111, 0x112} {
			_ = set(101, c)
		}
	} else {
		for c := uintptr(1); c < 120; c++ {
			_ = set(101, c)
		}
	}
	var setup [92]byte
	binary.LittleEndian.PutUint16(setup[0:], 0x03) // BUS_USB
	binary.LittleEndian.PutUint16(setup[2:], 0x1209)
	binary.LittleEndian.PutUint16(setup[4:], 0x5043)
	copy(setup[8:88], name)
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, f.Fd(), uiIoc(1, 3, 92), uintptr(unsafe.Pointer(&setup[0]))); e != 0 {
		f.Close()
		return nil, e
	}
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, f.Fd(), uiIoc(0, 1, 0), 0); e != 0 {
		f.Close()
		return nil, e
	}
	return &uinput{f}, nil
}

func (u *uinput) emit(typ, code uint16, val int32) {
	var ev [24]byte
	binary.LittleEndian.PutUint16(ev[16:], typ)
	binary.LittleEndian.PutUint16(ev[18:], code)
	binary.LittleEndian.PutUint32(ev[20:], uint32(val))
	_, _ = u.f.Write(ev[:])
}

func (u *uinput) key(code uint16, down int32) { u.emit(1, code, down); u.emit(0, 0, 0) }

func (u *uinput) rel(dx, dy int32) {
	if dx != 0 {
		u.emit(2, 0, dx)
	}
	if dy != 0 {
		u.emit(2, 1, dy)
	}
	u.emit(0, 0, 0)
}

func (u *uinput) Close() {
	_, _, _ = unix.Syscall(unix.SYS_IOCTL, u.f.Fd(), uiIoc(0, 2, 0), 0)
	u.f.Close()
}

// usKey is the evdev code (and Shift) for a character on a US layout.
func usKey(r rune) (uint16, bool) {
	const row1, row2, row3 = "qwertyuiop", "asdfghjkl", "zxcvbnm"
	switch {
	case r == ' ':
		return 57, false
	case r >= '1' && r <= '9':
		return uint16(2 + r - '1'), false
	case r == '0':
		return 11, false
	case r >= 'A' && r <= 'Z':
		c, _ := usKey(r + 32)
		return c, true
	}
	if i := strings.IndexRune(row1, r); i >= 0 {
		return uint16(16 + i), false
	}
	if i := strings.IndexRune(row2, r); i >= 0 {
		return uint16(30 + i), false
	}
	if i := strings.IndexRune(row3, r); i >= 0 {
		return uint16(44 + i), false
	}
	return 57, false
}
