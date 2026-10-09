package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"pc/internal/hypr"
	"pc/internal/rpc"
	"pc/internal/spec"
	"pc/internal/wl"
)

// ---------------------------------------------------------------- guard rails

func lockPath() string { return filepath.Join(CacheDir(), "input-disabled") }

func inputAllowed() error {
	if b, err := os.ReadFile(lockPath()); err == nil {
		return fmt.Errorf("mouse/keyboard control is switched OFF by the user (%s). Only they can turn it back on, with: pc input on",
			strings.TrimSpace(string(b)))
	}
	return nil
}

func audit(format string, args ...any) {
	p := filepath.Join(CacheDir(), "actions.log")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s  %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, args...))
	if st, err := f.Stat(); err == nil && st.Size() > 512<<10 {
		b, _ := os.ReadFile(p)
		if len(b) > 256<<10 {
			_ = os.WriteFile(p, b[len(b)-256<<10:], 0o600)
		}
	}
}

// While the real pointer is borrowed, input:follow_mouse is set to 0 so that
// putting the pointer back does not hand keyboard focus back to whatever is
// under the user's pointer. The original value is written down first, so a
// daemon that dies mid-gesture is undone on the next start.
func restorePath() string { return filepath.Join(rpc.Dir(), "restore.json") }

func recoverInputState() {
	b, err := os.ReadFile(restorePath())
	if err != nil {
		return
	}
	var st struct {
		FollowMouse int `json:"follow_mouse"`
	}
	if json.Unmarshal(b, &st) == nil && hypr.Available() {
		_ = hypr.SetFollowMouse(st.FollowMouse)
	}
	_ = os.Remove(restorePath())
}

// ---------------------------------------------------------------- coordinates

// point resolves x,y (pixels of a screenshot, or layout with abs) to layout.
func (d *Daemon) point(a Args, xk, yk string) (float64, float64, string, error) {
	x, okx := a.Num(xk)
	y, oky := a.Num(yk)
	if !okx || !oky {
		gx, gy, ok := d.ghost.position()
		if !okx && !oky && ok {
			return gx, gy, "where the agent cursor is", nil
		}
		return 0, 0, "", fmt.Errorf("need both %s and %s", xk, yk)
	}
	if a.Bool("abs") {
		return x, y, fmt.Sprintf("layout %.0f,%.0f", x, y), nil
	}
	sh, err := d.shots.get(a.Str("shot"))
	if err != nil {
		return 0, 0, "", err
	}
	if x < -1 || y < -1 || x > float64(sh.W)+1 || y > float64(sh.H)+1 {
		return 0, 0, "", fmt.Errorf("%.0f,%.0f is outside the %dx%d image of %s (%s). Use coordinates from that image, or --abs",
			x, y, sh.W, sh.H, sh.ID, sh.Label)
	}
	lx, ly := sh.ToLayout(x, y)
	return lx, ly, fmt.Sprintf("%.0f,%.0f in %s → layout %.0f,%.0f", x, y, sh.ID, lx, ly), nil
}

func describeWindow(c hypr.Client) string {
	if c.Address == "" {
		return "the desktop"
	}
	t := clip(c.Title, 50)
	if t == "" {
		return c.Class
	}
	return c.Class + " \"" + t + "\""
}

func (d *Daemon) withLook(a Args, x, y float64, resp *rpc.Response) *rpc.Response {
	if !a.Bool("look") {
		return resp
	}
	sh, err := d.afterShot(a, x, y)
	if err != nil {
		resp.Text += "\n(after-shot failed: " + err.Error() + ")"
		return resp
	}
	note := shotNote(sh)
	resp.Text += "\n" + note
	resp.Images = append(resp.Images, shotImage(sh, note))
	return resp
}

func (d *Daemon) rememberTarget(addr string) {
	d.lastTarget.Lock()
	d.lastTarget.address, d.lastTarget.at = addr, time.Now()
	d.lastTarget.Unlock()
}

// ---------------------------------------------------------------- handlers

func init() {
	register("click", cmdClick)
	register("move", cmdMove)
	register("hover", cmdHover)
	register("scroll", cmdScroll)
	register("drag", cmdDrag)
	register("type", cmdType)
	register("key", cmdKey)
	register("paste", cmdPaste)
	register("focus", cmdFocus)
	register("workspace", cmdWorkspace)
	register("open", cmdOpen)
	register("close", cmdClose)
	register("clip", cmdClip)
	register("cursor", cmdCursor)
	register("input", cmdInput)
}

func cmdClick(d *Daemon, a Args) (*rpc.Response, error) {
	x, y, how, err := d.point(a, "x", "y")
	if err != nil {
		return nil, err
	}
	under, _ := windowAt(x, y)
	if exp := a.Str("expect"); exp != "" {
		got := strings.ToLower(under.Class + " " + under.Title + " " + under.InitialClass)
		if under.Address == "" || !strings.Contains(got, strings.ToLower(exp)) {
			return nil, fmt.Errorf("not clicking: %.0f,%.0f is over %s, not %q. Take a fresh screenshot; the layout moved", x, y, describeWindow(under), exp)
		}
	}
	button := a.Str("button")
	if button == "" {
		button = "left"
	}
	count := a.Int("count", 1)
	if a.Bool("double") {
		count = 2
	}
	count = max(1, min(count, 5))
	d.ghost.act("clicking")
	waited, err := d.borrow(x, y, a.Bool("stay"), turnClick, d.glideStart(x, y), func(p *wl.Pointer, at []wl.Call, _ hypr.Pin, _ hypr.Rect) error {
		calls := append([]wl.Call(nil), at...)
		for i := 0; i < count; i++ {
			dn, err := p.ButtonCall(button, true)
			if err != nil {
				return err
			}
			up, _ := p.ButtonCall(button, false)
			calls = append(calls, dn...)
			calls = append(calls, up...)
		}
		return p.Conn().SendAll(calls...)
	})
	if err != nil {
		return nil, err
	}
	d.ghost.click()
	d.ghost.done(700 * time.Millisecond)
	if under.Address != "" {
		d.rememberTarget(under.Address)
	}
	what := button + " click"
	if count == 2 {
		what = "double-click"
	} else if count > 2 {
		what = fmt.Sprintf("%s ×%d", what, count)
	}
	msg := fmt.Sprintf("%s at %s, on %s (not focused or raised: the user's focus and pointer are where they were; the agent's keys now go to it)%s",
		what, how, describeWindow(under), waitedNote(waited))
	audit("click %s", msg)
	return d.withLook(a, x, y, text(msg)), nil
}

func waitedNote(w time.Duration) string {
	if w < 100*time.Millisecond {
		return ""
	}
	return fmt.Sprintf("; waited %dms for the user's hands", w.Milliseconds())
}

func cmdMove(d *Daemon, a Args) (*rpc.Response, error) {
	x, y, how, err := d.point(a, "x", "y")
	if err != nil {
		return nil, err
	}
	d.ghost.act("pointing")
	d.ghost.glideTo(x, y)
	d.ghost.done(1200 * time.Millisecond)
	under, _ := windowAt(x, y)
	msg := fmt.Sprintf("agent cursor at %s, over %s (nothing clicked; apps did not see this)", how, describeWindow(under))
	if gx, gy, _ := d.ghost.position(); math.Hypot(gx-x, gy-y) > 1 {
		msg = fmt.Sprintf("%.0f,%.0f is off every monitor, so the agent cursor stopped at the nearest visible spot, %.0f,%.0f (nothing clicked)", x, y, gx, gy)
	}
	return text(msg), nil
}

// glideStart sets the agent's cursor gliding and returns a wait for its
// arrival, so the caller can do its own checks while it moves.
func (d *Daemon) glideStart(x, y float64) func() {
	done := make(chan struct{})
	go func() {
		d.ghost.glideTo(x, y)
		close(done)
	}()
	return func() { <-done }
}

var errHandsBack = errors.New("the user moved the mouse or typed in the middle of it, so the agent let go and gave the pointer back. Try again when they pause")

func cmdHover(d *Daemon, a Args) (*rpc.Response, error) {
	x, y, how, err := d.point(a, "x", "y")
	if err != nil {
		return nil, err
	}
	dwell := time.Duration(a.Int("dwell", 700)) * time.Millisecond
	look := a.BoolDef("look", true)
	var sh *Shot
	d.ghost.act("hovering")
	defer d.ghost.done(600 * time.Millisecond)
	interrupted := false
	_, err = d.borrow(x, y, false, turnHold, d.glideStart(x, y), func(p *wl.Pointer, at []wl.Call, pin hypr.Pin, L hypr.Rect) error {
		t0 := time.Now()
		if err := p.Send(at); err != nil {
			return err
		}
		if err := p.Conn().Roundtrip(time.Second); err != nil { // the move has landed before the pointer is checked
			return err
		}
		for time.Since(t0) < dwell {
			time.Sleep(15 * time.Millisecond)
			if handsSince(t0, x, y) {
				interrupted = true
				giveBack(p, pin, x, y, L)
				return errHandsBack
			}
		}
		if look {
			ms, _ := hypr.Monitors()
			if m, ok := hypr.MonitorAt(ms, x, y); ok {
				detail := a.Str("detail")
				if detail == "" {
					detail = "normal"
				}
				if img, err := d.grab(m.Box(), budgetOf(detail), false); err == nil {
					sh, _, _ = d.store(img, m.Box(), "while hovering · monitor "+m.Name, detail, false)
				}
			}
		}
		if handsSince(t0, x, y) {
			interrupted = true
			giveBack(p, pin, x, y, L)
			return errHandsBack
		}
		return nil
	})
	if err != nil {
		if interrupted {
			audit("hover %s interrupted by the user", how)
		}
		return nil, err
	}
	under, _ := windowAt(x, y)
	resp := text(fmt.Sprintf("hovered %v at %s over %s, pointer returned", dwell, how, describeWindow(under)))
	if sh != nil {
		resp.Text += "\n" + shotNote(sh)
		resp.Images = []rpc.Image{shotImage(sh, shotNote(sh))}
	}
	audit("hover %s", how)
	return resp, nil
}

func cmdScroll(d *Daemon, a Args) (*rpc.Response, error) {
	x, y, how, err := d.point(a, "x", "y")
	if err != nil {
		if _, has := a.Num("x"); has {
			return nil, err
		}
		// no point and no agent cursor yet: scroll under the user's pointer
		px, py, perr := hypr.CursorPos()
		if perr != nil {
			return nil, err
		}
		x, y, how = px, py, "under the pointer"
	}
	notches, horizontal, dir := 3, false, "down"
	switch {
	case a.Has("up"):
		notches, dir = -a.Int("up", 3), "up"
	case a.Has("left"):
		notches, horizontal, dir = -a.Int("left", 3), true, "left"
	case a.Has("right"):
		notches, horizontal, dir = a.Int("right", 3), true, "right"
	case a.Has("down"):
		notches = a.Int("down", 3)
	}
	d.ghost.act("scrolling")
	defer d.ghost.done(600 * time.Millisecond)
	waited, err := d.borrow(x, y, false, turnScroll, d.glideStart(x, y), func(p *wl.Pointer, at []wl.Call, _ hypr.Pin, _ hypr.Rect) error {
		return p.Send(at, p.ScrollCall(notches, horizontal))
	})
	if err != nil {
		return nil, err
	}
	under, _ := windowAt(x, y)
	msg := fmt.Sprintf("scrolled %s %d at %s, over %s%s", dir, abs(notches), how, describeWindow(under), waitedNote(waited))
	audit("%s", msg)
	return d.withLook(a, x, y, text(msg)), nil
}

func cmdDrag(d *Daemon, a Args) (*rpc.Response, error) {
	x1, y1, _, err := d.point(a, "x1", "y1")
	if err != nil {
		return nil, err
	}
	x2, y2, _, err := d.point(a, "x2", "y2")
	if err != nil {
		return nil, err
	}
	button := a.Str("button")
	if button == "" {
		button = "left"
	}
	dur := time.Duration(a.Int("ms", 350)) * time.Millisecond
	d.ghost.act("dragging")
	defer d.ghost.done(600 * time.Millisecond)
	waited, err := d.borrow(x1, y1, false, turnHold, d.glideStart(x1, y1), func(p *wl.Pointer, at []wl.Call, pin hypr.Pin, L hypr.Rect) error {
		t0 := time.Now()
		dn, err := p.ButtonCall(button, true)
		if err != nil {
			return err
		}
		if err := p.Send(at, dn); err != nil {
			return err
		}
		released := false
		defer func() {
			if !released {
				up, _ := p.ButtonCall(button, false)
				_ = p.Send(up)
			}
		}()
		time.Sleep(60 * time.Millisecond)
		steps := max(8, int(dur/(12*time.Millisecond)))
		cx, cy := x1, y1
		for i := 1; i <= steps; i++ {
			if handsSince(t0, cx, cy) { // each step has landed by the next check
				// let go where it is and hand the pointer back: the user comes first
				up, _ := p.ButtonCall(button, false)
				released = true
				_ = p.Send(up)
				giveBack(p, pin, cx, cy, L)
				return fmt.Errorf("drag stopped at %.0f,%.0f: %w", cx, cy, errHandsBack)
			}
			t := easeInOut(float64(i) / float64(steps))
			cx, cy = x1+(x2-x1)*t, y1+(y2-y1)*t
			if err := p.Send(p.MoveCall(cx-L.X, cy-L.Y, L.W, L.H)); err != nil {
				return err
			}
			d.ghost.jump(cx, cy)
			time.Sleep(dur / time.Duration(steps))
		}
		time.Sleep(60 * time.Millisecond)
		up, _ := p.ButtonCall(button, false)
		released = true
		return p.Send(up)
	})
	if err != nil {
		return nil, err
	}
	msg := fmt.Sprintf("dragged %.0f,%.0f → %.0f,%.0f (layout) with %s%s", x1, y1, x2, y2, button, waitedNote(waited))
	audit("%s", msg)
	return d.withLook(a, x2, y2, text(msg)), nil
}

func easeInOut(t float64) float64 {
	if t < 0.5 {
		return 2 * t * t
	}
	return 1 - 2*(1-t)*(1-t)
}

func centre(c hypr.Client) (float64, float64) {
	if c.Address == "" {
		x, y, _ := hypr.CursorPos()
		return x, y
	}
	b := c.Box()
	return b.X + b.W/2, b.Y + b.H/2
}

// agentWorkspace is where open_app background=true puts windows: off
// screen, out of the user's way, still reachable by the agent's keys.
const agentWorkspace = "claude"

func cmdWorkspace(d *Daemon, a Args) (*rpc.Response, error) {
	ws := a.Str("name")
	if err := hypr.SwitchWorkspace(ws); err != nil {
		return nil, err
	}
	audit("workspace %s", ws)
	return text("switched to workspace " + ws + " (this changes what the user sees)"), nil
}

func cmdOpen(d *Daemon, a Args) (*rpc.Response, error) {
	cmd := strings.TrimSpace(a.Str("command"))
	if cmd == "" {
		return nil, errors.New("open what?")
	}
	before := map[string]bool{}
	if cs, err := hypr.Clients(); err == nil {
		for _, c := range cs {
			before[c.Address] = true
		}
	}
	d.ghost.act("opening " + clip(strings.Fields(cmd)[0], 20))
	defer d.ghost.done(800 * time.Millisecond)
	ws := ""
	if a.Bool("background") {
		ws = "name:" + agentWorkspace
	}
	if err := hypr.ExecQuiet(cmd, ws); err != nil {
		return nil, err
	}
	audit("open %s", cmd)
	wait := 10.0
	if v, ok := a.Num("wait"); ok {
		wait = v
	}
	deadline := time.Now().Add(time.Duration(wait * float64(time.Second)))
	for time.Now().Before(deadline) {
		time.Sleep(60 * time.Millisecond)
		cs, err := hypr.Clients()
		if err != nil {
			continue
		}
		for _, c := range cs {
			if !before[c.Address] && c.Mapped {
				time.Sleep(150 * time.Millisecond) // first frame
				d.rememberTarget(c.Address)
				where := "on screen"
				if !onScreenNow(c) {
					where = "not on screen: the agent's keys reach it, but to see or click it, focus_window show=true"
				}
				return text(fmt.Sprintf("opened %s: window %s on workspace %s at %s, %s (pid %d, address %s). It did not take the user's focus; the agent's keys now go to it",
					cmd, describeWindow(c), c.Workspace.Name, c.Box(), where, c.Pid, c.Address)), nil
			}
		}
	}
	if wait == 0 {
		return text("launched " + cmd), nil
	}
	return text(fmt.Sprintf("launched %s; no new window within %.0fs (it may be a background app, or already running elsewhere)", cmd, wait)), nil
}

func cmdClose(d *Daemon, a Args) (*rpc.Response, error) {
	c, err := findWindow(a.Str("window"))
	if err != nil {
		return nil, err
	}
	if err := hypr.CloseWindow(c.Address); err != nil {
		return nil, err
	}
	audit("close %s", c.Class)
	return text("asked " + describeWindow(c) + " to close (it may prompt to save; check with a screenshot)"), nil
}

func cmdClip(d *Daemon, a Args) (*rpc.Response, error) {
	s, err := d.session()
	if err != nil {
		return nil, err
	}
	if s.clip == nil {
		return nil, s.need("clipboard", false)
	}
	args := a.Strs("args")
	if len(args) > 0 && args[0] == "set" {
		if err := inputAllowed(); err != nil {
			return nil, err
		}
		txt := strings.Join(args[1:], " ")
		if err := s.clip.SetText(txt); err != nil {
			return nil, err
		}
		audit("clipboard set %d chars", len(txt))
		return text(fmt.Sprintf("clipboard set (%d chars)", len([]rune(txt)))), nil
	}
	t, err := s.clip.Text()
	if err != nil {
		return nil, err
	}
	if t == "" {
		return text("(the clipboard holds no text)"), nil
	}
	return &rpc.Response{Text: t, Data: map[string]any{"text": t}}, nil
}

func cmdCursor(d *Daemon, a Args) (*rpc.Response, error) {
	switch a.Str("action") {
	case "hide", "off":
		d.ghost.mu.Lock()
		d.ghost.disabled = true
		d.ghost.style.Hidden = true
		_ = d.ghost.saveStyle()
		d.ghost.mu.Unlock()
		d.ghost.hide()
		return text("agent cursor hidden until pc cursor show; actions still work"), nil
	case "show", "on":
		d.ghost.mu.Lock()
		d.ghost.disabled = false
		d.ghost.style.Hidden = false
		_ = d.ghost.saveStyle()
		have := d.ghost.have
		d.ghost.mu.Unlock()
		if have {
			x, y, _ := d.ghost.position()
			d.ghost.glideTo(x, y)
		} else {
			d.ghost.appear()
		}
		return text("agent cursor shown, " + d.ghost.status()), nil
	case "say":
		d.ghost.say(a.Str("value"))
		if a.Str("value") == "" {
			return text("agent cursor note cleared"), nil
		}
		return text("agent cursor says: " + d.ghost.styleName() + " · " + clip(a.Str("value"), 40)), nil
	case "speed":
		if v := a.Str("value"); v != "" {
			if err := d.ghost.setSpeed(v); err != nil {
				return nil, err
			}
		}
		return text("agent cursor speed: " + d.ghost.speedName() + " (instant, fast, normal, smooth)"), nil
	case "name":
		v := strings.TrimSpace(a.Str("value"))
		if v == "" {
			return nil, errors.New("name what? pc cursor name Claude")
		}
		if err := d.ghost.setStyle(func(st *cursorStyle) error { st.Name = clip(v, 24); return nil }); err != nil {
			return nil, err
		}
		return text("agent cursor: " + d.ghost.styleLine()), nil
	case "color", "colour":
		c, err := parseColor(a.Str("value"))
		if err != nil {
			return nil, err
		}
		if err := d.ghost.setStyle(func(st *cursorStyle) error { st.color = c; return nil }); err != nil {
			return nil, err
		}
		return text("agent cursor: " + d.ghost.styleLine()), nil
	case "tag":
		on := a.Str("value") != "off" && a.Str("value") != "false" && a.Str("value") != "0"
		if err := d.ghost.setStyle(func(st *cursorStyle) error { st.Tag = on; return nil }); err != nil {
			return nil, err
		}
		return text("agent cursor: " + d.ghost.styleLine()), nil
	case "demo":
		return cursorDemo(d)
	}
	return text("agent cursor: " + d.ghost.status() + " · " + d.ghost.styleLine()), nil
}

// cursorDemo plays every effect next to the user's pointer: a glide, a
// retarget mid-flight, a click, typing, a key press. Nothing is sent to any
// app.
func cursorDemo(d *Daemon) (*rpc.Response, error) {
	px, py, err := hypr.CursorPos()
	if err != nil {
		return nil, err
	}
	ms, err := hypr.Monitors()
	if err != nil {
		return nil, err
	}
	m, ok := hypr.MonitorAt(ms, px, py)
	if !ok {
		return nil, errors.New("the pointer is not on a monitor")
	}
	b := m.Box()
	// a box around the pointer, kept on its monitor
	cx := math.Max(b.X+260, math.Min(b.X+b.W-320, px))
	cy := math.Max(b.Y+180, math.Min(b.Y+b.H-200, py))
	d.ghost.mu.Lock()
	d.ghost.disabled = false
	d.ghost.mu.Unlock()
	d.ghost.glideTo(cx-220, cy-120)
	time.Sleep(350 * time.Millisecond)
	d.ghost.startGlide(cx+240, cy-60)
	time.Sleep(110 * time.Millisecond)
	d.ghost.glideTo(cx+60, cy+130) // retargeted mid-flight: the path bends
	time.Sleep(450 * time.Millisecond)
	d.ghost.click()
	time.Sleep(700 * time.Millisecond)
	d.ghost.glideTo(cx-180, cy+40)
	d.ghost.busy("is typing", 1600*time.Millisecond)
	time.Sleep(2100 * time.Millisecond)
	d.ghost.busy("pressed ctrl+s", 900*time.Millisecond)
	time.Sleep(1300 * time.Millisecond)
	d.ghost.glideTo(cx+30, cy-20)
	d.ghost.click()
	return text("played the agent cursor's glide, retarget, click, typing and key press near your pointer (no input was sent) · " + d.ghost.styleLine()), nil
}

func cmdInput(d *Daemon, a Args) (*rpc.Response, error) {
	switch a.Str("action") {
	case "off", "disable", "stop":
		_ = os.WriteFile(lockPath(), []byte("switched off "+time.Now().Format("2006-01-02 15:04:05")+"\n"), 0o600)
		audit("INPUT DISABLED")
		d.ghost.hide()
		return text("mouse/keyboard control is OFF. Turn it back on with: pc input on"), nil
	case "on", "enable":
		_ = os.Remove(lockPath())
		audit("input enabled")
		return text("mouse/keyboard control is on"), nil
	}
	if err := inputAllowed(); err != nil {
		return text("input: " + err.Error()), nil
	}
	return text("input: on (pc input off stops all mouse/keyboard control at once)"), nil
}

// isInput says whether a command drives the mouse or keyboard.
func isInput(cmd string) bool {
	c, ok := spec.Find(cmd)
	return ok && c.Input
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
