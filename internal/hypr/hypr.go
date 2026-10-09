// Package hypr talks to Hyprland over its IPC socket directly, so a query is
// one socket round trip instead of spawning hyprctl.
//
// Hyprland 0.5x can run a Lua config, and then the classic string dispatchers
// (`dispatch movecursor 10 20`, `keyword ...`) are rejected with a Lua syntax
// error. Everything that changes state therefore goes through the helpers
// here (Focus, SwitchWorkspace, CloseWindow, Exec, SetFollowMouse), which speak
// whichever dialect the running compositor understands.
package hypr

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Signature picks the instance: $HYPRLAND_INSTANCE_SIGNATURE if it is alive,
// else the newest instance directory (MCP servers start without the env).
func Signature() string {
	rt := runtimeDir()
	if sig := os.Getenv("HYPRLAND_INSTANCE_SIGNATURE"); sig != "" {
		if _, err := os.Stat(filepath.Join(rt, "hypr", sig, ".socket.sock")); err == nil {
			return sig
		}
	}
	entries, _ := os.ReadDir(filepath.Join(rt, "hypr"))
	type inst struct {
		name string
		t    time.Time
	}
	var all []inst
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(rt, "hypr", e.Name(), ".socket.sock")); err != nil {
			continue
		}
		if nested(filepath.Join(rt, "hypr", e.Name())) {
			continue // a Hyprland running inside another compositor is never the user's desktop
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		all = append(all, inst{e.Name(), info.ModTime()})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].t.After(all[j].t) })
	if len(all) > 0 {
		return all[0].name
	}
	return ""
}

// nested reports whether an instance was started as a client of another
// compositor: its process environment carries WAYLAND_DISPLAY.
func nested(instanceDir string) bool {
	b, err := os.ReadFile(filepath.Join(instanceDir, "hyprland.lock"))
	if err != nil {
		return false
	}
	pid := strings.Fields(string(b))
	if len(pid) == 0 {
		return false
	}
	env, err := os.ReadFile("/proc/" + pid[0] + "/environ")
	if err != nil {
		return false
	}
	for _, kv := range strings.Split(string(env), "\x00") {
		if strings.HasPrefix(kv, "WAYLAND_DISPLAY=") || strings.HasPrefix(kv, "DISPLAY=") {
			return true
		}
	}
	return false
}

func runtimeDir() string {
	if rt := os.Getenv("XDG_RUNTIME_DIR"); rt != "" {
		return rt
	}
	return fmt.Sprintf("/run/user/%d", os.Getuid())
}

// Available reports whether a Hyprland instance is reachable.
func Available() bool { return Signature() != "" }

// Request sends a raw IPC request and returns the reply.
func Request(req string) ([]byte, error) {
	sig := Signature()
	if sig == "" {
		return nil, errors.New("no running Hyprland instance found")
	}
	conn, err := net.DialTimeout("unix", filepath.Join(runtimeDir(), "hypr", sig, ".socket.sock"), 2*time.Second)
	if err != nil {
		return nil, fmt.Errorf("hyprland ipc: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte(req)); err != nil {
		return nil, err
	}
	return io.ReadAll(conn)
}

// JSON runs a query with the json flag and decodes it.
func JSON(cmd string, v any) error {
	b, err := Request("j/" + cmd)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("hyprland %s: %v (%q)", cmd, err, truncate(string(b), 120))
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

type Workspace struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

type Monitor struct {
	ID               int       `json:"id"`
	Name             string    `json:"name"`
	Description      string    `json:"description"`
	Make             string    `json:"make"`
	Model            string    `json:"model"`
	Width            int       `json:"width"`
	Height           int       `json:"height"`
	RefreshRate      float64   `json:"refreshRate"`
	X                int       `json:"x"`
	Y                int       `json:"y"`
	Scale            float64   `json:"scale"`
	Transform        int       `json:"transform"`
	Focused          bool      `json:"focused"`
	Disabled         bool      `json:"disabled"`
	DPMS             bool      `json:"dpmsStatus"`
	ActiveWorkspace  Workspace `json:"activeWorkspace"`
	SpecialWorkspace Workspace `json:"specialWorkspace"`
}

// LogicalSize is the monitor's size in layout coordinates.
func (m Monitor) LogicalSize() (float64, float64) {
	s := m.Scale
	if s <= 0 {
		s = 1
	}
	w, h := float64(m.Width)/s, float64(m.Height)/s
	if m.Transform%2 == 1 {
		w, h = h, w
	}
	return w, h
}

// Box returns x, y, w, h in layout coordinates.
func (m Monitor) Box() Rect {
	w, h := m.LogicalSize()
	return Rect{float64(m.X), float64(m.Y), w, h}
}

type Client struct {
	Address        string    `json:"address"`
	Mapped         bool      `json:"mapped"`
	Hidden         bool      `json:"hidden"`
	At             [2]int    `json:"at"`
	Size           [2]int    `json:"size"`
	Workspace      Workspace `json:"workspace"`
	Floating       bool      `json:"floating"`
	Monitor        int       `json:"monitor"`
	Class          string    `json:"class"`
	Title          string    `json:"title"`
	InitialClass   string    `json:"initialClass"`
	InitialTitle   string    `json:"initialTitle"`
	Pid            int       `json:"pid"`
	Xwayland       bool      `json:"xwayland"`
	Pinned         bool      `json:"pinned"`
	Fullscreen     int       `json:"fullscreen"`
	FocusHistoryID int       `json:"focusHistoryID"`
}

func (c Client) Box() Rect {
	return Rect{float64(c.At[0]), float64(c.At[1]), float64(c.Size[0]), float64(c.Size[1])}
}

// Rect is a box in layout coordinates.
type Rect struct{ X, Y, W, H float64 }

func (r Rect) Contains(x, y float64) bool {
	return x >= r.X && y >= r.Y && x < r.X+r.W && y < r.Y+r.H
}

func (r Rect) Intersect(o Rect) (Rect, bool) {
	x0, y0 := max(r.X, o.X), max(r.Y, o.Y)
	x1, y1 := min(r.X+r.W, o.X+o.W), min(r.Y+r.H, o.Y+o.H)
	if x1 <= x0 || y1 <= y0 {
		return Rect{}, false
	}
	return Rect{x0, y0, x1 - x0, y1 - y0}, true
}

func (r Rect) String() string {
	return fmt.Sprintf("%.0f,%.0f %.0fx%.0f", r.X, r.Y, r.W, r.H)
}

func Monitors() ([]Monitor, error) {
	var ms []Monitor
	err := JSON("monitors", &ms)
	return ms, err
}

func Clients() ([]Client, error) {
	var cs []Client
	err := JSON("clients", &cs)
	return cs, err
}

func ActiveWindow() (Client, error) {
	var c Client
	err := JSON("activewindow", &c)
	return c, err
}

// CursorPos is where the one real pointer is, in layout coordinates.
func CursorPos() (float64, float64, error) {
	var p struct{ X, Y float64 }
	if err := JSON("cursorpos", &p); err != nil {
		return 0, 0, err
	}
	return p.X, p.Y, nil
}

// Layout is the bounding box of all enabled monitors, which is the space the
// virtual pointer's absolute motion maps onto.
func Layout(ms []Monitor) Rect {
	first := true
	var x0, y0, x1, y1 float64
	for _, m := range ms {
		if m.Disabled {
			continue
		}
		b := m.Box()
		if first {
			x0, y0, x1, y1 = b.X, b.Y, b.X+b.W, b.Y+b.H
			first = false
			continue
		}
		x0, y0 = min(x0, b.X), min(y0, b.Y)
		x1, y1 = max(x1, b.X+b.W), max(y1, b.Y+b.H)
	}
	return Rect{x0, y0, x1 - x0, y1 - y0}
}

// MonitorAt finds the monitor containing a layout point.
func MonitorAt(ms []Monitor, x, y float64) (Monitor, bool) {
	for _, m := range ms {
		if !m.Disabled && m.Box().Contains(x, y) {
			return m, true
		}
	}
	return Monitor{}, false
}

var (
	luaOnce sync.Once
	luaMode bool
)

// Lua reports whether the compositor runs the Lua config manager.
func Lua() bool {
	luaOnce.Do(func() {
		b, err := Request("eval return 1")
		luaMode = err == nil && !strings.Contains(string(b), "only supported with the lua")
	})
	return luaMode
}

// ResetDialect forgets the cached dialect (after a compositor restart).
func ResetDialect() { luaOnce = sync.Once{} }

// Eval runs Lua inside the compositor.
func Eval(code string) (string, error) {
	b, err := Request("eval " + code)
	out := strings.TrimSpace(string(b))
	if err != nil {
		return out, err
	}
	if strings.HasPrefix(out, "error") {
		return out, errors.New(out)
	}
	return out, nil
}

func luaOK(out string) error {
	if strings.Contains(out, "warning:") || strings.HasPrefix(out, "error") {
		msg := out
		if i := strings.LastIndex(out, ": "); i >= 0 {
			msg = out[i+2:]
		}
		return errors.New(strings.TrimSpace(msg))
	}
	return nil
}

func legacyOK(out []byte) error {
	s := strings.TrimSpace(string(out))
	if s == "ok" || strings.HasPrefix(s, "ok") {
		return nil
	}
	if s == "" {
		return nil
	}
	return errors.New(s)
}

// withOption runs Lua with one config option temporarily overridden, and puts
// the old value back even when the body fails, so a bad dispatch can never
// leave the user's config changed.
func withOption(key, value, body string) error {
	section, field, _ := strings.Cut(key, ".")
	code := fmt.Sprintf(`local old = hl.get_config(%[1]s)
hl.config({%[2]s = {%[3]s = %[4]s}})
local ok, err = pcall(function() %[5]s end)
if old ~= nil then hl.config({%[2]s = {%[3]s = old}}) end
if not ok then error(err) end`, LuaString(key), section, field, value, body)
	out, err := Eval(code)
	if err != nil {
		return err
	}
	return luaOK(out)
}

// LuaString quotes a Go string as a Lua literal.
func LuaString(s string) string { return strconv.Quote(s) }

// Focus focuses a window (by address) without moving the user's cursor:
// Hyprland warps the pointer to a newly focused window unless cursor:no_warps
// is set, so it is set for exactly the length of the dispatch.
func Focus(address string) error {
	if Lua() {
		return withOption("cursor.no_warps", "true",
			fmt.Sprintf(`hl.dispatch(hl.dsp.focus({window = %s}))`, LuaString("address:"+address)))
	}
	orig := legacyOption("cursor:no_warps")
	b, err := Request(fmt.Sprintf("[[BATCH]]keyword cursor:no_warps 1;dispatch focuswindow address:%s;keyword cursor:no_warps %s", address, orig))
	if err != nil {
		return err
	}
	if strings.Contains(string(b), "No such window") {
		return errors.New("no such window")
	}
	return nil
}

func legacyOption(name string) string {
	var v struct {
		Int   *int64   `json:"int"`
		Float *float64 `json:"float"`
		Str   *string  `json:"str"`
	}
	if err := JSON("getoption "+name, &v); err != nil {
		return "0"
	}
	switch {
	case v.Int != nil:
		return strconv.FormatInt(*v.Int, 10)
	case v.Float != nil:
		return strconv.FormatFloat(*v.Float, 'f', -1, 64)
	case v.Str != nil:
		return *v.Str
	}
	return "0"
}

// Workspace switches to a workspace by name or number.
// A workspace that lives on another monitor would drag the user's pointer to
// that monitor's centre, so cursor:no_warps is held for the switch.
func SwitchWorkspace(ws string) error {
	if Lua() {
		return withOption("cursor.no_warps", "true", fmt.Sprintf(`hl.dispatch(hl.dsp.focus({workspace = %s}))`, LuaString(ws)))
	}
	orig := legacyOption("cursor:no_warps")
	b, err := Request(fmt.Sprintf("[[BATCH]]keyword cursor:no_warps 1;dispatch workspace %s;keyword cursor:no_warps %s", ws, orig))
	if err != nil {
		return err
	}
	if s := string(b); strings.Contains(s, "rror") || strings.Contains(s, "nvalid") {
		return errors.New(strings.TrimSpace(s))
	}
	return nil
}

// CloseWindow asks a window to close, like clicking its close button.
func CloseWindow(address string) error {
	if Lua() {
		out, err := Eval(fmt.Sprintf(`hl.dispatch(hl.dsp.window.close({window = %s}))`, LuaString("address:"+address)))
		if err != nil {
			return err
		}
		return luaOK(out)
	}
	b, err := Request("dispatch closewindow address:" + address)
	if err != nil {
		return err
	}
	return legacyOK(b)
}

// FollowMouse reads input:follow_mouse.
func FollowMouse() int {
	n, _ := strconv.Atoi(legacyOption("input:follow_mouse"))
	return n
}

// SetFollowMouse changes input:follow_mouse at runtime.
func SetFollowMouse(v int) error {
	if Lua() {
		out, err := Eval(fmt.Sprintf(`hl.config({input = {follow_mouse = %d}})`, v))
		if err != nil {
			return err
		}
		return luaOK(out)
	}
	b, err := Request(fmt.Sprintf("keyword input:follow_mouse %d", v))
	if err != nil {
		return err
	}
	return legacyOK(b)
}

// NoAnimLayer stops Hyprland animating a layer namespace (the agent cursor
// glides on its own; a compositor slide-in on top of that looks broken).
func NoAnimLayer(namespace string) {
	if Lua() {
		_, _ = Eval(fmt.Sprintf(`hl.layer_rule({name = %s, match = {namespace = %s}, no_anim = true})`,
			LuaString("pc-"+namespace), LuaString("^"+namespace+"$")))
		return
	}
	_, _ = Request("keyword layerrule noanim, ^" + namespace + "$")
	_, _ = Request("keyword layerrule no_anim on, match:namespace ^" + namespace + "$")
}

// Version returns the compositor's version line.
func Version() string {
	var v struct {
		Tag string `json:"tag"`
	}
	if err := JSON("version", &v); err != nil {
		return ""
	}
	return v.Tag
}

// Exec launches a command the way a keybind would, inheriting the
// compositor's environment.
func Exec(cmd string) error {
	if Lua() {
		out, err := Eval(fmt.Sprintf(`hl.dispatch(hl.dsp.exec_cmd(%s))`, LuaString(cmd)))
		if err != nil {
			return err
		}
		return luaOK(out)
	}
	b, err := Request("dispatch exec " + cmd)
	if err != nil {
		return err
	}
	return legacyOK(b)
}

// Repl runs Lua and returns what it printed.
func Repl(code string) (string, error) {
	b, err := Request("repl " + code)
	out := strings.TrimSpace(string(b))
	if err != nil {
		return out, err
	}
	if strings.HasPrefix(out, "error") {
		return out, errors.New(out)
	}
	return out, nil
}

// Key is one tap: an XKB keycode and the modifiers that pick its level, as
// Hyprland spells them (SHIFT, CTRL, ALT, SUPER, MOD5).
type Key struct {
	Mods string
	Code int
}

// SendKeys taps keys into one window from inside the compositor
// (send_shortcut with a window): for each key, the seat's keyboard focus moves
// to that window, the key goes with exactly the modifiers given, and focus
// goes straight back, all in one step of the compositor's main loop. Keys the
// user types at the same moment cannot interleave, a modifier they hold does
// not leak into the agent's keys, and their focused window, workspace and
// borders never change. Compositor keybinds do not fire.
//
// check, if set, runs before every burst of 8 keys; an error from it stops
// the rest, and SendKeys reports how many keys went out.
func SendKeys(address string, keys []Key, check func() error) (int, error) {
	const chunk = 8 // short bursts: any client drains them, and a check runs between them
	for i := 0; i < len(keys); i += chunk {
		part := keys[i:min(i+chunk, len(keys))]
		if i > 0 {
			time.Sleep(3 * time.Millisecond)
		}
		if check != nil {
			if err := check(); err != nil {
				return i, err
			}
		}
		if Lua() {
			var b strings.Builder
			fmt.Fprintf(&b, "local w = %s\nfor _, k in ipairs({", LuaString("address:"+address))
			for _, k := range part {
				fmt.Fprintf(&b, "{%s,%d},", LuaString(k.Mods), k.Code)
			}
			b.WriteString("}) do hl.dispatch(hl.dsp.send_shortcut({mods = k[1], key = \"code:\" .. k[2], window = w})) end")
			out, err := Eval(b.String())
			if err != nil {
				return i, err
			}
			if err := luaOK(out); err != nil {
				return i, err
			}
			continue
		}
		var b strings.Builder
		b.WriteString("[[BATCH]]")
		for _, k := range part {
			fmt.Fprintf(&b, "dispatch sendshortcut %s, code:%d, address:%s;", k.Mods, k.Code, address)
		}
		out, err := Request(b.String())
		if err != nil {
			return i, err
		}
		if err := legacyOK(out); err != nil {
			return i, err
		}
	}
	return len(keys), nil
}

// Pin is the pointer state saved while the agent borrows the pointer.
type Pin struct {
	FollowMouse int
	X, Y        float64
}

// PinPointer sets input:follow_mouse to 3 (pointer focus fully separate from
// keyboard focus: moving or clicking over a window neither focuses nor raises
// it) and reads where the user's pointer is, in one request, so the time
// between reading the position and the agent's burst is as short as it gets.
func PinPointer() (Pin, error) {
	if Lua() {
		out, err := Repl(`local fm = hl.get_config("input.follow_mouse")
hl.config({input = {follow_mouse = 3}})
local p = hl.get_cursor_pos()
print(fm, p.x, p.y)`)
		if err != nil {
			return Pin{}, err
		}
		var p Pin
		if _, err := fmt.Sscan(out, &p.FollowMouse, &p.X, &p.Y); err != nil {
			return Pin{}, fmt.Errorf("pinning the pointer: %q", out)
		}
		return p, nil
	}
	fm := FollowMouse()
	if err := SetFollowMouse(3); err != nil {
		return Pin{}, err
	}
	x, y, err := CursorPos()
	return Pin{FollowMouse: fm, X: x, Y: y}, err
}

// ExecQuiet launches a command whose window must not take the user's
// keyboard focus; with a workspace ("name:claude") it opens there silently.
func ExecQuiet(cmd, workspace string) error {
	if Lua() {
		rule := "no_initial_focus = true"
		if workspace != "" {
			rule += ", workspace = " + LuaString(workspace+" silent")
		}
		out, err := Eval(fmt.Sprintf(`hl.dispatch(hl.dsp.exec_cmd(%s, {%s}))`, LuaString(cmd), rule))
		if err != nil {
			return err
		}
		return luaOK(out)
	}
	rules := "noinitialfocus"
	if workspace != "" {
		rules += ";workspace " + workspace + " silent"
	}
	b, err := Request("dispatch exec [" + rules + "] " + cmd)
	if err != nil {
		return err
	}
	return legacyOK(b)
}
