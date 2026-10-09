package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"sync"
	"time"

	"pc/internal/hypr"
	"pc/internal/rpc"
	"pc/internal/sys"
	"pc/internal/wl"
	"pc/internal/xkb"
)

// Two people, one seat. Hyprland has a single pointer and a single keyboard
// focus, and the human keeps both:
//
//   - The agent's keys never go through the seat's focus. They are sent from
//     inside the compositor to the agent's window (send_shortcut), each in one
//     step of its main loop, so the user's keys cannot interleave with them and
//     the user's focused window, workspace and borders never change.
//   - Pointer gestures borrow the real pointer for one burst with
//     input:follow_mouse set to 3, where clicking or moving over a window
//     neither focuses nor raises it, and put it back where the user left it.
//   - Before any of that, the agent waits for a gap: never while the user holds
//     a mouse button or a modifier (they would join the gesture), and for
//     gestures that hold the pointer (hover, drag) until the mouse is still.
//     The user never waits for the agent; the agent waits for the user.

// turn says what an action needs from the user's hands before it runs.
type turn struct {
	what        string
	buttonsUp   bool          // hard: no mouse button held
	modsUp      bool          // hard: no modifier held (the compositor adds it to the agent's pointer events)
	keysUp      bool          // soft: no key held (a held key's autorepeat stops when keyboard focus blips)
	quietMotion time.Duration // soft: the mouse has been still this long
	strict      bool          // the soft conditions are required too
	softMax     time.Duration // stop waiting for the soft conditions after this
	hardMax     time.Duration // give up after this
}

var (
	turnClick  = turn{what: "click", buttonsUp: true, modsUp: true, quietMotion: 40 * time.Millisecond, softMax: 300 * time.Millisecond, hardMax: 6 * time.Second}
	turnScroll = turn{what: "scroll", buttonsUp: true, modsUp: true, quietMotion: 60 * time.Millisecond, softMax: 300 * time.Millisecond, hardMax: 6 * time.Second}
	turnHold   = turn{what: "hold the pointer", buttonsUp: true, modsUp: true, keysUp: true, quietMotion: 300 * time.Millisecond, strict: true, softMax: 10 * time.Second, hardMax: 10 * time.Second}
	turnKeys   = turn{what: "type", keysUp: true, softMax: 1200 * time.Millisecond, hardMax: 1200 * time.Millisecond}
)

// waitTurn holds an action until the user's hands leave room for it, showing
// "waiting for you" on the agent's cursor meanwhile. It returns how long it
// waited.
func (d *Daemon) waitTurn(t turn) (time.Duration, error) {
	start := time.Now()
	shown := false
	before := d.ghost.lastLabel()
	defer func() {
		if shown {
			d.ghost.act(before)
		}
	}()
	// Motion is read from the compositor too: a mouse grabbed by a remapper
	// (smoolyd, keyd, ...) and replayed through a virtual pointer sends no
	// evdev events anyone else can read, but the pointer still moves.
	lx, ly, _ := hypr.CursorPos()
	moved := start.Add(-t.quietMotion).Add(20 * time.Millisecond)
	if t.strict {
		moved = start // a hold needs the whole quiet stretch seen, not assumed
	}
	for {
		h := sys.UserHands()
		if x, y, err := hypr.CursorPos(); err == nil && (x != lx || y != ly) {
			lx, ly, moved = x, y, time.Now()
		}
		if h.LastMotion.After(moved) {
			moved = h.LastMotion
		}
		var hardWhy []string
		if t.buttonsUp && h.ButtonsDown > 0 {
			hardWhy = append(hardWhy, "a mouse button")
		}
		if t.modsUp && h.ModsDown > 0 {
			hardWhy = append(hardWhy, strings.Join(dedupe(sys.HeldModifiers()), ", "))
		}
		soft := (!t.keysUp || h.KeysDown == 0) && time.Since(moved) >= t.quietMotion
		el := time.Since(start)
		if len(hardWhy) == 0 && (soft || (!t.strict && el >= t.softMax)) {
			return el, nil
		}
		if el >= t.hardMax {
			if len(hardWhy) > 0 {
				return el, fmt.Errorf("did not %s: the user has held %s for %s, and the compositor would make it part of the agent's gesture. Try again in a moment", t.what, strings.Join(hardWhy, " and "), human(el))
			}
			return el, fmt.Errorf("did not %s: the user kept using the mouse for %s and this needs their pointer for a while. Try again when they pause, or use the keyboard instead", t.what, human(el))
		}
		if !shown && el > 60*time.Millisecond {
			shown = true
			d.ghost.act("waiting for you")
		}
		time.Sleep(8 * time.Millisecond)
	}
}

func dedupe(ss []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range ss {
		k, _, _ := strings.Cut(s, " on ")
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

// borrow runs a pointer gesture on the one real pointer: wait for the user's
// turn to allow it, pin follow_mouse to 3 and note where their pointer is,
// send the gesture, and put the pointer back (unless stay). A gesture sent as
// one burst is dispatched before the compositor renders again, so their
// pointer never visibly leaves its spot, and with follow_mouse at 3 nothing
// gets focused or raised. arrived, if set, blocks until the agent's cursor
// has glided to the target; the wait for the user overlaps the glide.
func (d *Daemon) borrow(x, y float64, stay bool, t turn, arrived func(), gesture func(p *wl.Pointer, at []wl.Call, pin hypr.Pin, L hypr.Rect) error) (time.Duration, error) {
	s, err := d.session()
	if err != nil {
		return 0, err
	}
	if s.ptr == nil {
		return 0, s.need("virtual-pointer", false)
	}
	ms, err := hypr.Monitors()
	if err != nil {
		return 0, err
	}
	L := hypr.Layout(ms)
	if !L.Contains(x, y) {
		return 0, fmt.Errorf("%.0f,%.0f is outside the %s monitor layout", x, y, L)
	}
	waited, err := d.waitTurn(t)
	if err != nil {
		return waited, err
	}
	if arrived != nil {
		arrived()
	}
	// the glide took a moment: the hard conditions must still hold
	if h := sys.UserHands(); (t.buttonsUp && h.ButtonsDown > 0) || (t.modsUp && h.ModsDown > 0) {
		w2, err := d.waitTurn(t)
		waited += w2
		if err != nil {
			return waited, err
		}
	}
	pin, err := hypr.PinPointer()
	if err != nil {
		return waited, err
	}
	if pin.FollowMouse != 3 {
		b, _ := json.Marshal(map[string]int{"follow_mouse": pin.FollowMouse})
		_ = os.WriteFile(restorePath(), b, 0o600)
		defer func() {
			_ = hypr.SetFollowMouse(pin.FollowMouse)
			_ = os.Remove(restorePath())
		}()
	}
	at := s.ptr.MoveCall(x-L.X, y-L.Y, L.W, L.H)
	d.borrowing.Store(true)
	defer d.borrowing.Store(false)
	if err := gesture(s.ptr, at, pin, L); err != nil {
		return waited, err
	}
	if !stay {
		if err := s.ptr.Send(s.ptr.MoveCall(pin.X-L.X, pin.Y-L.Y, L.W, L.H)); err != nil {
			return waited, err
		}
	}
	return waited, s.c.Roundtrip(2 * time.Second)
}

// handsSince reports whether the user touched keyboard or mouse after t0:
// a key or button on their devices, or the pointer no longer where the agent
// put it at (ex, ey) (that catches motion from grabbed or virtual devices).
func handsSince(t0 time.Time, ex, ey float64) bool {
	h := sys.UserHands()
	if h.LastMotion.After(t0) || h.LastButton.After(t0) || h.LastKey.After(t0) {
		return true
	}
	x, y, err := hypr.CursorPos()
	return err == nil && math.Hypot(x-ex, y-ey) > 1.5
}

// giveBack hands the pointer back after the user moved in the middle of a
// held gesture: their motion so far is kept, applied from where their pointer
// was rather than from where the agent had it.
func giveBack(p *wl.Pointer, pin hypr.Pin, x, y float64, L hypr.Rect) {
	cx, cy, err := hypr.CursorPos()
	if err != nil {
		cx, cy = x, y
	}
	nx := min(max(pin.X+cx-x, L.X), L.X+L.W-1)
	ny := min(max(pin.Y+cy-y, L.Y), L.Y+L.H-1)
	_ = p.Send(p.MoveCall(nx-L.X, ny-L.Y, L.W, L.H))
}

// ---------------------------------------------------------------- the keyboard

// keymapCache holds the parsed layout the compositor last sent.
type keymapCache struct {
	sync.Mutex
	gen int
	km  *xkb.Keymap
}

func (d *Daemon) keymap() (*xkb.Keymap, error) {
	s, err := d.session()
	if err != nil {
		return nil, err
	}
	if s.km == nil {
		return nil, s.need("keymap", false)
	}
	text, gen, err := s.km.Text(time.Second)
	if err != nil {
		return nil, err
	}
	d.kmap.Lock()
	defer d.kmap.Unlock()
	if d.kmap.km != nil && d.kmap.gen == gen {
		return d.kmap.km, nil
	}
	km, err := xkb.Parse(text)
	if err != nil {
		return nil, fmt.Errorf("reading the keyboard layout: %w", err)
	}
	d.kmap.km, d.kmap.gen = km, gen
	return km, nil
}

// keyTarget picks the window the agent's keys go to. It never focuses it.
// In order: window=, the window the agent last clicked or typed into, the
// window under the agent's cursor, and last the user's focused window.
func (d *Daemon) keyTarget(a Args) (hypr.Client, string, error) {
	if q := a.Str("window"); q != "" {
		c, err := findWindow(q)
		if err != nil {
			return hypr.Client{}, "", err
		}
		d.rememberTarget(c.Address)
		return c, "", nil
	}
	d.lastTarget.Lock()
	want, at := d.lastTarget.address, d.lastTarget.at
	d.lastTarget.Unlock()
	if want != "" && time.Since(at) < 15*time.Minute {
		if c, err := findWindow(want); err == nil && c.Mapped {
			return c, "", nil
		}
	}
	if x, y, ok := d.ghost.position(); ok {
		if c, ok := windowAt(x, y); ok {
			return c, " (the window under the agent's cursor)", nil
		}
	}
	c, err := hypr.ActiveWindow()
	if err != nil || c.Address == "" {
		return hypr.Client{}, "", errors.New("no window to type into: pass window=<class or title>, or click one first")
	}
	return c, " (the user's focused window: pass window= to aim elsewhere)", nil
}

// announce shows what the agent is doing in its cursor's tag. If the cursor
// is not over the window the keys go to and that window is on screen, it
// glides there; the keys do not wait for it.
func (d *Daemon) announce(target hypr.Client, activity string) {
	if target.Address != "" {
		x, y, ok := d.ghost.position()
		if (!ok || !target.Box().Contains(x, y)) && onScreenNow(target) {
			cx, cy := centre(target)
			d.ghost.startGlide(cx, cy)
		}
	}
	d.ghost.act(clip(activity, 30))
}

// onScreenNow says whether a window can actually be seen: its workspace is
// shown and its box overlaps a monitor (a window can sit on a visible
// workspace at -1920,0, outside every monitor).
func onScreenNow(c hypr.Client) bool {
	cs, err := clientsOnScreen()
	if err != nil {
		return false
	}
	ms, err := hypr.Monitors()
	if err != nil {
		return false
	}
	for _, o := range cs {
		if o.Address != c.Address {
			continue
		}
		for _, m := range ms {
			if in, ok := o.Box().Intersect(m.Box()); ok && in.W*in.H >= 0.25*o.Box().W*o.Box().H {
				return true
			}
		}
	}
	return false
}

// unseen warns when the agent acted on a window the user cannot see.
func unseen(c hypr.Client) string {
	if c.Address == "" || onScreenNow(c) {
		return ""
	}
	b := c.Box()
	return fmt.Sprintf(". NOTE: %s is not visible on any monitor (workspace %s, at %.0f,%.0f): the user cannot see this happen. If they asked to see it, focus_window show=true",
		describeWindow(c), c.Workspace.Name, b.X, b.Y)
}

// sendKeys taps keys into target, guarded: before every short burst it
// checks that the user's focused window is still the one they had when it
// started and that the target still exists. If either changed (the user
// switched windows, or a window asked to be activated and took focus), the
// agent stops rather than risk its keys going somewhere unintended.
func sendKeys(target hypr.Client, keys []hypr.Key) error {
	start, _ := hypr.ActiveWindow()
	check := func() error {
		now, err := hypr.ActiveWindow()
		if err != nil {
			return err
		}
		if now.Address != start.Address {
			return fmt.Errorf("focus moved from %s to %s while the agent was typing", describeWindow(start), describeWindow(now))
		}
		if now.Address == target.Address && start.Address != target.Address {
			return fmt.Errorf("%s took focus while the agent was typing into it", describeWindow(target))
		}
		cs, err := hypr.Clients()
		if err != nil {
			return err
		}
		for _, c := range cs {
			if c.Address == target.Address && c.Mapped {
				return nil
			}
		}
		return fmt.Errorf("%s is gone", describeWindow(target))
	}
	n, err := hypr.SendKeys(target.Address, keys, check)
	if err != nil {
		return fmt.Errorf("stopped after %d of %d keys: %w. Nothing more was sent; check both windows before retrying", n, len(keys), err)
	}
	return nil
}

// strokes turns text into taps on the user's layout; ok is false when some
// character is not on it.
func strokes(km *xkb.Keymap, text string) ([]hypr.Key, bool) {
	var out []hypr.Key
	for _, r := range text {
		if r == '\r' {
			continue
		}
		s, found := km.Rune(r)
		if !found {
			return nil, false
		}
		out = append(out, hypr.Key{Mods: s.Mods, Code: s.Code})
	}
	return out, true
}

var hyprMods = map[string]string{"shift": "SHIFT", "ctrl": "CTRL", "alt": "ALT", "super": "SUPER", "altgr": "MOD5"}

// comboKey turns "ctrl+shift+t" into one tap on the user's layout.
func comboKey(km *xkb.Keymap, combo string) (hypr.Key, error) {
	mods, sym, err := wl.ParseCombo(combo)
	if err != nil {
		return hypr.Key{}, err
	}
	s, ok := km.Sym(sym)
	if !ok {
		return hypr.Key{}, fmt.Errorf("%q: %s is not on the keyboard layout", combo, sym)
	}
	set := map[string]bool{}
	for _, m := range strings.Fields(s.Mods) {
		set[m] = true
	}
	for _, m := range mods {
		set[hyprMods[m]] = true
	}
	var names []string
	for _, m := range []string{"SHIFT", "CTRL", "ALT", "SUPER", "MOD5"} {
		if set[m] {
			names = append(names, m)
		}
	}
	return hypr.Key{Mods: strings.Join(names, " "), Code: s.Code}, nil
}

func cmdType(d *Daemon, a Args) (*rpc.Response, error) {
	txt := a.Str("text")
	if txt == "" {
		return nil, errors.New("nothing to type")
	}
	km, err := d.keymap()
	if err != nil {
		return nil, err
	}
	target, note, err := d.keyTarget(a)
	if err != nil {
		return nil, err
	}
	keys, ok := strokes(km, txt)
	if !ok {
		// a character the layout has no key for: the clipboard can carry anything
		missing := string(km.Missing(txt))
		r, err := d.pasteInto(target, txt, false, a)
		if err != nil {
			return nil, err
		}
		r.Text = fmt.Sprintf("%q is not on the keyboard layout, so the text went in through the clipboard. %s", missing, r.Text)
		return r, nil
	}
	if a.Bool("enter") {
		if k, err := comboKey(km, "Return"); err == nil {
			keys = append(keys, k)
		}
	}
	if _, err := d.waitTurn(turnKeys); err != nil {
		return nil, err
	}
	d.announce(target, "typing")
	delay := time.Duration(a.Int("delay", 0)) * time.Millisecond
	if delay > 0 {
		for i, k := range keys {
			if i > 0 {
				time.Sleep(delay)
			}
			if err := sendKeys(target, []hypr.Key{k}); err != nil {
				return nil, err
			}
		}
	} else if err := sendKeys(target, keys); err != nil {
		return nil, err
	}
	d.ghost.done(900 * time.Millisecond)
	d.rememberTarget(target.Address)
	msg := fmt.Sprintf("typed %d chars into %s%s; the user's focus was not touched", len([]rune(txt)), describeWindow(target), note)
	if a.Bool("enter") {
		msg += ", then Return"
	}
	msg += unseen(target)
	audit("type %d chars into %s", len([]rune(txt)), target.Class)
	cx, cy := centre(target)
	return d.withLook(a, cx, cy, text(msg)), nil
}

func cmdKey(d *Daemon, a Args) (*rpc.Response, error) {
	var combos []string
	for _, k := range a.Strs("keys") {
		combos = append(combos, strings.Fields(k)...)
	}
	if len(combos) == 0 {
		return nil, errors.New("no keys given")
	}
	km, err := d.keymap()
	if err != nil {
		return nil, err
	}
	var keys []hypr.Key
	for _, c := range combos {
		k, err := comboKey(km, c)
		if err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	target, note, err := d.keyTarget(a)
	if err != nil {
		return nil, err
	}
	if _, err := d.waitTurn(turnKeys); err != nil {
		return nil, err
	}
	d.announce(target, "pressing "+strings.Join(combos, " "))
	for i, k := range keys {
		if i > 0 {
			time.Sleep(25 * time.Millisecond) // let the app react to one combo before the next
		}
		if err := sendKeys(target, []hypr.Key{k}); err != nil {
			return nil, err
		}
	}
	d.ghost.done(900 * time.Millisecond)
	d.rememberTarget(target.Address)
	msg := fmt.Sprintf("pressed %s in %s%s (sent to that window only: compositor keybinds do not fire)%s", strings.Join(combos, " "), describeWindow(target), note, unseen(target))
	audit("keys %s in %s", strings.Join(combos, " "), target.Class)
	cx, cy := centre(target)
	return d.withLook(a, cx, cy, text(msg)), nil
}

var terminals = []string{"alacritty", "foot", "kitty", "ghostty", "wezterm", "xterm", "urxvt", "konsole",
	"gnome-terminal", "terminator", "rio", "contour", "st-256color", "tilix", "warp"}

func isTerminal(class string) bool {
	low := strings.ToLower(class)
	for _, t := range terminals {
		if strings.Contains(low, t) {
			return true
		}
	}
	return strings.Contains(low, "term")
}

func cmdPaste(d *Daemon, a Args) (*rpc.Response, error) {
	txt := a.Str("text")
	if txt == "" {
		return nil, errors.New("nothing to paste")
	}
	target, note, err := d.keyTarget(a)
	if err != nil {
		return nil, err
	}
	r, err := d.pasteInto(target, txt, a.Bool("keep"), a)
	if err != nil {
		return nil, err
	}
	r.Text += note
	return r, nil
}

// pasteInto puts text on the clipboard, sends the paste shortcut to the
// target window only, and puts the user's clipboard back once the app has
// read it.
func (d *Daemon) pasteInto(target hypr.Client, txt string, keep bool, a Args) (*rpc.Response, error) {
	s, err := d.session()
	if err != nil {
		return nil, err
	}
	if s.clip == nil {
		return nil, s.need("clipboard", false)
	}
	km, err := d.keymap()
	if err != nil {
		return nil, err
	}
	combo := "ctrl+v"
	if isTerminal(target.Class) {
		combo = "ctrl+shift+v"
	}
	key, err := comboKey(km, combo)
	if err != nil {
		return nil, err
	}
	if _, err := d.waitTurn(turnKeys); err != nil {
		return nil, err
	}
	d.announce(target, "pasting")
	// Back-to-back pastes share one saved copy of the user's clipboard: the
	// second must not mistake the first paste's text for theirs.
	d.clipMu.Lock()
	if d.clipSaved == nil && !keep {
		d.clipSaved = &clipState{snap: s.clip.Save()}
	}
	d.clipGen++
	gen, saved := d.clipGen, d.clipSaved
	d.clipMu.Unlock()
	if err := s.clip.SetText(txt); err != nil {
		return nil, err
	}
	if err := sendKeys(target, []hypr.Key{key}); err != nil {
		return nil, err
	}
	d.ghost.done(900 * time.Millisecond)
	d.rememberTarget(target.Address)
	restored := ""
	if keep {
		d.clipMu.Lock()
		d.clipSaved = nil // the user asked for the text to stay
		d.clipMu.Unlock()
	} else if saved != nil {
		go func() {
			time.Sleep(900 * time.Millisecond) // the app reads the clipboard asynchronously
			d.clipMu.Lock()
			defer d.clipMu.Unlock()
			if d.clipGen != gen || d.clipSaved != saved {
				return // a later paste owns the restore
			}
			if s.clip.Owned() && saved.snap != nil { // not if the user copied something meanwhile
				_ = s.clip.Restore(saved.snap)
			}
			d.clipSaved = nil
		}()
		if saved.snap != nil {
			restored = "; the user's clipboard is put back in a moment"
		} else {
			restored = "; the clipboard held nothing before, so the text stays on it"
		}
	}
	msg := fmt.Sprintf("pasted %d chars into %s with %s%s%s", len([]rune(txt)), describeWindow(target), combo, restored, unseen(target))
	audit("paste %d chars into %s", len([]rune(txt)), target.Class)
	cx, cy := centre(target)
	return d.withLook(a, cx, cy, text(msg)), nil
}

// cmdFocus makes a window the agent's: its keys go there from now on. The
// user's focus stays where it is unless show is set, which brings the window
// up for the user too.
func cmdFocus(d *Daemon, a Args) (*rpc.Response, error) {
	c, err := findWindow(a.Str("window"))
	if err != nil {
		return nil, err
	}
	d.rememberTarget(c.Address)
	if a.Bool("show") {
		if err := d.focusVerified(c); err != nil {
			return nil, err
		}
		audit("focus %s (shown)", c.Class)
		return text(fmt.Sprintf("focused %s on workspace %s for the user too (their pointer was not moved)", describeWindow(c), c.Workspace.Name)), nil
	}
	where := "on screen"
	if onScreenNow(c) {
		cx, cy := centre(c)
		d.ghost.startGlide(cx, cy)
	} else {
		where = "NOT visible on any monitor (workspace " + c.Workspace.Name + fmt.Sprintf(", at %.0f,%.0f): keys still reach it, but the user cannot see it; to show it to them, focus_window show=true", c.Box().X, c.Box().Y)
	}
	active, _ := hypr.ActiveWindow()
	audit("target %s", c.Class)
	return text(fmt.Sprintf("the agent's keys now go to %s, %s. The user's focus stays on %s", describeWindow(c), where, describeWindow(active))), nil
}

func (d *Daemon) focusVerified(c hypr.Client) error {
	if a, err := hypr.ActiveWindow(); err == nil && a.Address == c.Address {
		return nil
	}
	if err := hypr.Focus(c.Address); err != nil {
		return fmt.Errorf("could not focus %s: %w", describeWindow(c), err)
	}
	deadline := time.Now().Add(600 * time.Millisecond)
	for time.Now().Before(deadline) {
		if a, err := hypr.ActiveWindow(); err == nil && a.Address == c.Address {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	a, _ := hypr.ActiveWindow()
	return fmt.Errorf("asked for %s but focus is on %s", describeWindow(c), describeWindow(a))
}
