// Package spec is the one table of commands. The CLI parser, the MCP tool
// schemas and `pc do` batch scripts are all generated from it, so the three
// can never drift apart.
package spec

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

type Arg struct {
	Name     string
	Type     string // string | number | int | bool | strings
	Help     string
	Pos      bool // positional (in order); "strings" positional soaks up the rest
	Required bool
	Short    string
}

type Cmd struct {
	Name  string
	Help  string
	Args  []Arg
	Tool  string // MCP tool name; "" keeps it CLI-only
	TDesc string // longer MCP description
	Input bool   // drives the mouse or keyboard (kill switch applies)
}

var coordArgs = []Arg{
	{Name: "abs", Type: "bool", Help: "x,y are global layout coordinates, not pixels of the last screenshot"},
	{Name: "shot", Type: "string", Help: "id of the screenshot the x,y were read off (default: the most recent)"},
}

var lookArgs = []Arg{
	{Name: "look", Type: "bool", Help: "capture the result once the screen settles (the 'after' shot)"},
	{Name: "detail", Type: "string", Help: "after-shot size: low (800px), normal (1280px), high (1568px)"},
}

func with(base []Arg, extra ...[]Arg) []Arg {
	out := append([]Arg(nil), base...)
	for _, e := range extra {
		out = append(out, e...)
	}
	return out
}

var Cmds = []Cmd{
	// ---------------------------------------------------------------- the machine
	{Name: "state", Help: "one-screen overview: load, memory, disks, network, power, focus, and what is wrong",
		Tool: "system_state", TDesc: "Overview of the machine right now: CPU/memory/disk/network/battery/temps, the busiest processes, the focused window, and a PROBLEMS list (failed services, crashes, OOM kills, flapping restarts, zombies, full disks). Start here for any 'how is my PC doing / what is wrong' question. A few hundred tokens."},
	{Name: "sys", Help: "hardware and OS inventory: CPU, RAM, GPUs, drives, network, session",
		Tool: "system_info", TDesc: "Static inventory: OS, kernel, CPU model, RAM, GPUs, drives, filesystems, network interfaces with IPs, gateway, DNS, battery, session, package count."},
	{Name: "ps", Help: "processes with live CPU%; optional name/cmdline filter",
		Args: []Arg{
			{Name: "query", Type: "string", Pos: true, Help: "name or command-line substring, or a pid"},
			{Name: "sort", Type: "string", Help: "cpu (default) | mem | new | name"},
			{Name: "limit", Type: "int", Short: "n", Help: "rows (default 25)"},
			{Name: "user", Type: "string", Help: "only this user's processes"},
		},
		Tool: "processes", TDesc: "List processes with instantaneous CPU%, memory, state, user, start time. Filter by name/cmdline substring; sort by cpu, mem, new (most recently started) or name."},
	{Name: "proc", Help: "everything about one process: cmdline, cwd, unit, parents, children, ports, windows, logs",
		Args: []Arg{{Name: "target", Type: "string", Pos: true, Required: true, Help: "pid, or a name/cmdline substring"}},
		Tool: "process_info", TDesc: "Deep look at one process (pid or name): command line, exe, cwd, user, state, CPU, memory, threads, fds, IO, its systemd unit, parent chain, children, listening ports, its windows, and its recent log lines."},
	{Name: "events", Help: "what happened recently: crashes, failures, restarts, OOM kills, processes started/exited",
		Args: []Arg{
			{Name: "since", Type: "string", Help: "how far back: 30s, 15m (default), 2h, 1d"},
			{Name: "all", Type: "bool", Help: "include routine starts/stops and short-lived processes"},
			{Name: "grep", Type: "string", Help: "only events mentioning this"},
		},
		Tool: "system_events", TDesc: "Timeline of what changed on the machine: services that failed, crashed or keep restarting, coredumps, OOM kills, suspend/resume, error-level log lines, and (while the pc daemon runs) processes that started or exited. Repeats are collapsed. Default window 15m."},
	{Name: "units", Help: "systemd services: failed (default), running, all, or a name filter",
		Args: []Arg{{Name: "filter", Type: "string", Pos: true, Help: "failed | running | all | name substring"}},
		Tool: "services", TDesc: "systemd units for both the system and the user manager. Default lists failed ones; pass running, all, or a name substring."},
	{Name: "logs", Help: "journal lines, newest last",
		Args: []Arg{
			{Name: "unit", Type: "string", Pos: true, Help: "unit name (foo or foo.service); empty = everything"},
			{Name: "since", Type: "string", Help: "how far back (default 15m)"},
			{Name: "level", Type: "string", Help: "err | warn (default) | info | debug"},
			{Name: "grep", Type: "string", Help: "case-insensitive pattern"},
			{Name: "lines", Type: "int", Short: "n", Help: "max lines (default 60)"},
		},
		Tool: "logs", TDesc: "Read the systemd journal: for one unit or everything, filtered by level (err/warn/info) and an optional pattern."},
	{Name: "ports", Help: "listening sockets and the process that owns each",
		Args: []Arg{{Name: "query", Type: "string", Pos: true, Help: "port number or process name"}},
		Tool: "ports", TDesc: "Listening TCP and bound UDP sockets with the owning process."},

	// ---------------------------------------------------------------- the desktop
	{Name: "windows", Help: "open windows (focused first) and monitors",
		Args: []Arg{{Name: "query", Type: "string", Pos: true, Help: "class/title filter"}},
		Tool: "windows", TDesc: "Monitors (layout, scale, active workspace), every open window (class, title, workspace, geometry, pid, focus) and where both cursors are. Use it to find what to target before a screenshot or a click."},
	{Name: "shot", Help: "screenshot; prints the image path and the coordinate mapping",
		Args: []Arg{
			{Name: "monitor", Type: "string", Short: "m", Help: "one monitor by name (e.g. HDMI-A-1)"},
			{Name: "window", Type: "string", Short: "w", Help: "a window: class/title substring, address, or 'active'"},
			{Name: "region", Type: "string", Short: "r", Help: "\"X,Y WxH\" in layout coordinates"},
			{Name: "all", Type: "bool", Help: "every monitor in one wide image (default: the focused monitor)"},
			{Name: "detail", Type: "string", Short: "d", Help: "low 800px | normal 1280px (default) | high 1568px | full native PNG"},
			{Name: "cursor", Type: "bool", Help: "include the user's real cursor"},
			{Name: "grid", Type: "bool", Help: "overlay a labelled coordinate grid (helps pick exact points)"},
		},
		Tool: "screenshot", TDesc: "Capture the screen (default: the focused monitor) and return it. Every shot gets an id and becomes the coordinate space for the next click/move/drag/scroll: pass x,y exactly as you read them off THIS image. The agent's own cursor appears in shots wherever it last pointed."},

	// ---------------------------------------------------------------- acting
	{Name: "click", Help: "click with the agent's cursor (x,y are pixels of the last screenshot)", Input: true,
		Args: with([]Arg{
			{Name: "x", Type: "number", Pos: true}, {Name: "y", Type: "number", Pos: true},
			{Name: "button", Type: "string", Short: "b", Help: "left (default) | right | middle | back | forward"},
			{Name: "double", Type: "bool", Help: "double-click"},
			{Name: "count", Type: "int", Help: "number of clicks (3 = select a line)"},
			{Name: "expect", Type: "string", Help: "abort unless the point is over a window matching this class/title"},
			{Name: "stay", Type: "bool", Help: "leave the real pointer at the target (hover menus); default puts it back"},
		}, coordArgs, lookArgs),
		Tool: "click", TDesc: "Click at x,y with the agent's cursor. x,y are pixel coordinates in the most recent screenshot (or pass shot=<id>, or abs=true for layout coordinates). The agent's cursor glides there; the user's real pointer is borrowed for one burst and put back, and the clicked window is neither focused nor raised, so the user keeps typing where they were. The clicked window becomes the agent's: later type_text/press_keys go to it. Waits while the user holds a mouse button or modifier. Pass expect=<window> to refuse if the point is not over that window."},
	{Name: "move", Help: "move the agent's cursor (shows the user where you are pointing; nothing is clicked)",
		Args: with([]Arg{{Name: "x", Type: "number", Pos: true}, {Name: "y", Type: "number", Pos: true}}, coordArgs),
		Tool: "point", TDesc: "Move the agent's visible cursor to x,y without clicking, to show the user what you are about to act on. Apps do not see this; use hover for hover effects."},
	{Name: "hover", Help: "hold the real pointer over x,y for a moment (tooltips, hover menus), then give it back", Input: true,
		Args: with([]Arg{
			{Name: "x", Type: "number", Pos: true}, {Name: "y", Type: "number", Pos: true},
			{Name: "dwell", Type: "int", Help: "milliseconds to hover (default 700)"},
		}, coordArgs, lookArgs),
		Tool: "hover", TDesc: "Hover the pointer over x,y for dwell ms so tooltips and hover menus appear, capture (look=true, default) while hovering, then return the user's pointer. It holds the user's one pointer, so it waits until their mouse is still and lets go the moment they move it."},
	{Name: "scroll", Help: "scroll the wheel at x,y (or under the agent's cursor)", Input: true,
		Args: with([]Arg{
			{Name: "x", Type: "number", Pos: true}, {Name: "y", Type: "number", Pos: true},
			{Name: "down", Type: "int", Help: "notches down (default 3)"},
			{Name: "up", Type: "int", Help: "notches up"},
			{Name: "left", Type: "int", Help: "notches left"},
			{Name: "right", Type: "int", Help: "notches right"},
		}, coordArgs, lookArgs),
		Tool: "scroll", TDesc: "Scroll with the wheel over x,y (default: where the agent's cursor is). Use down/up/left/right = notches."},
	{Name: "drag", Help: "press at x1,y1, move, release at x2,y2", Input: true,
		Args: with([]Arg{
			{Name: "x1", Type: "number", Pos: true, Required: true}, {Name: "y1", Type: "number", Pos: true, Required: true},
			{Name: "x2", Type: "number", Pos: true, Required: true}, {Name: "y2", Type: "number", Pos: true, Required: true},
			{Name: "button", Type: "string", Short: "b", Help: "left (default) | right | middle"},
			{Name: "ms", Type: "int", Help: "how long the move takes (default 350)"},
		}, coordArgs, lookArgs),
		Tool: "drag", TDesc: "Drag from x1,y1 to x2,y2 (sliders, selections, moving things). Coordinates as for click. Holds the user's pointer for the drag, so it waits until their mouse is still and stops if they move it."},
	{Name: "type", Help: "type text into the agent's window (--window, or the one it last clicked); the user's focus is untouched", Input: true,
		Args: with([]Arg{
			{Name: "text", Type: "strings", Pos: true, Required: true},
			{Name: "window", Type: "string", Short: "w", Help: "the window to type into (class/title/address); default: the one the agent last clicked or typed into"},
			{Name: "enter", Type: "bool", Help: "press Return afterwards"},
			{Name: "delay", Type: "int", Help: "ms between characters (default 0)"},
		}, lookArgs),
		Tool: "type_text", TDesc: "Type text into a window without taking the user's keyboard focus: keys go from inside the compositor to that window only, so the user can keep typing in theirs at the same time. Target: window=<class/title>, else the window the agent last clicked or typed into. Works on windows on hidden workspaces too. Characters the user's layout lacks go in through the clipboard. enter=true presses Return after."},
	{Name: "key", Help: "press key combos: ctrl+shift+t, Return, alt+F4, super+1", Input: true,
		Args: with([]Arg{
			{Name: "keys", Type: "strings", Pos: true, Required: true},
			{Name: "window", Type: "string", Short: "w", Help: "the window to send them to; default: the agent's last window"},
		}, lookArgs),
		Tool: "press_keys", TDesc: "Press key combos in order, space-separated (\"ctrl+l\", \"ctrl+shift+t\", \"Return\", \"ctrl+a Delete\"), sent to one window (window=, else the agent's last window) without touching the user's focus. They go to the app only: compositor keybinds such as super+2 do not fire; use switch_workspace / focus_window / open_app for those."},
	{Name: "paste", Help: "paste text via the clipboard, then restore the user's clipboard", Input: true,
		Args: with([]Arg{
			{Name: "text", Type: "strings", Pos: true, Required: true},
			{Name: "window", Type: "string", Short: "w", Help: "the window to paste into; default: the agent's last window"},
			{Name: "keep", Type: "bool", Help: "leave the text on the clipboard instead of restoring"},
		}, lookArgs),
		Tool: "paste_text", TDesc: "Paste text through the clipboard into one window (ctrl+v, or ctrl+shift+v in terminals) without taking the user's focus. The user's clipboard is restored a moment later (not if they copied something meanwhile). Best for long text and code."},
	{Name: "focus", Help: "make a window the agent's (its keys go there); --show also brings it up for the user", Input: true,
		Args: []Arg{
			{Name: "window", Type: "string", Pos: true, Required: true},
			{Name: "show", Type: "bool", Help: "really focus it, switching the user's workspace if needed (changes what they see)"},
		},
		Tool: "focus_window", TDesc: "Make a window (class/title substring or address) the agent's: type_text and press_keys go to it from now on. The user's focus is not touched. show=true really focuses it for the user, switching workspace if needed, without warping their pointer: only when they asked to see it."},
	{Name: "workspace", Help: "switch to a workspace", Input: true,
		Args: []Arg{{Name: "name", Type: "string", Pos: true, Required: true}},
		Tool: "switch_workspace", TDesc: "Switch the focused monitor to a workspace (number or name)."},
	{Name: "open", Help: "launch an app and wait for its window (it does not take the user's focus)", Input: true,
		Args: []Arg{
			{Name: "command", Type: "strings", Pos: true, Required: true},
			{Name: "wait", Type: "number", Help: "seconds to wait for a window (default 10, 0 = don't)"},
			{Name: "background", Type: "bool", Help: "open it on the agent's own hidden workspace instead of the user's screen"},
		},
		Tool: "open_app", TDesc: "Launch an application (\"firefox https://example.com\", \"foot\") and wait for its window. The window does not take the user's keyboard focus and becomes the agent's window. background=true opens it on the agent's own hidden workspace: keys still reach it, but it cannot be seen or clicked until focus_window show=true."},
	{Name: "close", Help: "close a window politely (like its close button)", Input: true,
		Args: []Arg{{Name: "window", Type: "string", Pos: true, Required: true}},
		Tool: "close_window", TDesc: "Ask a window to close (class/title substring, address, or 'active'). Apps may prompt to save; check with a screenshot."},
	{Name: "clip", Help: "read the clipboard, or `pc clip set <text>`",
		Args: []Arg{{Name: "args", Type: "strings", Pos: true}},
		Tool: "clipboard", TDesc: "Read the clipboard text. To set it, pass args=[\"set\", \"<text>\"]."},
	{Name: "do", Help: "run several actions in one go: pc do 'click 400 300; type hello; key Return'", Input: true,
		Args: []Arg{
			{Name: "script", Type: "strings", Pos: true, Required: true},
			{Name: "look", Type: "bool", Help: "capture once at the end"},
			{Name: "detail", Type: "string"},
		},
		Tool: "act", TDesc: "Run a sequence of actions in one call, separated by ';' or newlines, each written like the CLI: \"click 412 230; type hello world --enter; key ctrl+s; wait 0.5\". Coordinates refer to the screenshot that was current when act started. Stops at the first failure. look=true returns one after-shot."},
	{Name: "wait", Help: "wait for something instead of sleeping: window <q> | gone <q> | port <n> | change | idle | <seconds>",
		Args: []Arg{
			{Name: "what", Type: "strings", Pos: true, Required: true},
			{Name: "timeout", Type: "number", Help: "seconds (default 10)"},
		},
		Tool: "wait_for", TDesc: "Block until a condition holds: [\"window\", q] a window matching q exists; [\"gone\", q] no process/window matches q; [\"port\", n] something listens on port n; [\"change\"] the screen changes; [\"idle\"] the screen stops changing; [\"2.5\"] just sleep. Returns as soon as it is true."},

	// ---------------------------------------------------------------- the agent's own state
	{Name: "cursor", Help: "the agent's on-screen cursor: show | hide | status | say TEXT | demo | speed S | name N | color C | tag on|off",
		Args: []Arg{
			{Name: "action", Type: "string", Pos: true, Help: "show | hide | status | say | demo | speed | name | color | tag"},
			{Name: "value", Type: "strings", Pos: true, Help: "say: a short status for the tag (\"reading the logs\") · speed: instant|fast|normal|smooth · name: the tag's text · color: #rrggbb or claude|blue|green|purple|... · tag: on|off"},
		},
		Tool: "agent_cursor", TDesc: "The agent's on-screen cursor: a tinted, glowing clone of the user's pointer that stays on screen, with a tag saying what the agent is doing (\"Claude · typing\", \"Claude · waiting for you\", \"Claude · idle\"). action=say value=\"reading the logs\" sets the status shown between actions (keep it to a few words). demo plays its effects without input; speed/name/color/tag change it; hide/show persist."},
	{Name: "input", Help: "kill switch for mouse/keyboard control: off | on | status",
		Args: []Arg{{Name: "action", Type: "string", Pos: true, Help: "off | on | status"}}},
	{Name: "watch", Help: "keep watching the user's screen: start | stop | status | timeline | view | latest | clip",
		Args: []Arg{
			{Name: "action", Type: "string", Pos: true, Help: "start | stop | status | timeline | view | latest | clip"},
			{Name: "fps", Type: "number", Help: "captures per second (default 1, max 4)"},
			{Name: "seconds", Type: "number", Help: "history to keep (start) or to read back (default 300)"},
			{Name: "detail", Type: "string", Help: "low | normal (default) | high"},
			{Name: "monitor", Type: "string", Help: "record one monitor only"},
			{Name: "frames", Type: "int", Help: "tiles in view (default 6)"},
			{Name: "purge", Type: "bool", Help: "delete the frames on stop"},
			{Name: "min", Type: "number", Help: "hide timeline stretches shorter than this many seconds"},
		},
		Tool: "screen_watch", TDesc: "Background recorder of the user's real screen (opt-in, stays on this machine). action=start begins it; timeline (cheap text: which window had focus when, idle stretches) is the first thing to read; view returns one contact sheet of the last `seconds`; latest returns the freshest frame; status; stop (purge=true deletes frames)."},
	{Name: "doctor", Help: "what works on this machine, what is missing, and why",
		Tool: "doctor", TDesc: "Check what pc can do here: compositor, protocols, input, capture, journal access, daemon health."},
}

// Find looks a command up by CLI name or tool name.
func Find(name string) (*Cmd, bool) {
	for i := range Cmds {
		if Cmds[i].Name == name || (Cmds[i].Tool != "" && Cmds[i].Tool == name) {
			return &Cmds[i], true
		}
	}
	return nil, false
}

// Parse turns argv-style words into an argument map, per the command's spec.
// Flags may appear anywhere; "--" ends flag parsing.
func (c *Cmd) Parse(words []string) (map[string]any, error) {
	out := map[string]any{}
	byName := map[string]*Arg{}
	var pos []*Arg
	for i := range c.Args {
		a := &c.Args[i]
		byName[a.Name] = a
		if a.Short != "" {
			byName[a.Short] = a
		}
		if a.Pos {
			pos = append(pos, a)
		}
	}
	var rest []string
	noFlags := false
	for i := 0; i < len(words); i++ {
		w := words[i]
		if !noFlags && w == "--" {
			noFlags = true
			continue
		}
		if !noFlags && strings.HasPrefix(w, "-") && len(w) > 1 && !isNumber(w) {
			name := strings.TrimLeft(w, "-")
			val, hasVal := "", false
			if k, v, ok := strings.Cut(name, "="); ok {
				name, val, hasVal = k, v, true
			}
			a := byName[name]
			if a == nil || a.Pos && a.Type == "strings" {
				return nil, fmt.Errorf("%s: unknown flag --%s", c.Name, name)
			}
			if a.Type == "bool" {
				b := true
				if hasVal {
					b = val == "1" || val == "true" || val == "yes"
				}
				out[a.Name] = b
				continue
			}
			if !hasVal {
				if i+1 >= len(words) {
					return nil, fmt.Errorf("%s: --%s needs a value", c.Name, name)
				}
				i++
				val = words[i]
			}
			v, err := convert(a, val)
			if err != nil {
				return nil, fmt.Errorf("%s: --%s: %v", c.Name, name, err)
			}
			out[a.Name] = v
			continue
		}
		rest = append(rest, w)
	}
	for _, a := range pos {
		if len(rest) == 0 {
			break
		}
		if a.Type == "strings" {
			out[a.Name] = rest
			rest = nil
			break
		}
		v, err := convert(a, rest[0])
		if err != nil {
			return nil, fmt.Errorf("%s: %s: %v", c.Name, a.Name, err)
		}
		out[a.Name] = v
		rest = rest[1:]
	}
	if len(rest) > 0 {
		return nil, fmt.Errorf("%s: unexpected %q", c.Name, strings.Join(rest, " "))
	}
	for _, a := range c.Args {
		if a.Required {
			if _, ok := out[a.Name]; !ok {
				return nil, fmt.Errorf("%s: missing %s", c.Name, a.Name)
			}
		}
	}
	return out, nil
}

func isNumber(s string) bool {
	_, err := strconv.ParseFloat(s, 64)
	return err == nil
}

func convert(a *Arg, s string) (any, error) {
	switch a.Type {
	case "number":
		return strconv.ParseFloat(strings.TrimSuffix(s, ","), 64)
	case "int":
		n, err := strconv.Atoi(s)
		return n, err
	case "strings":
		return []string{s}, nil
	}
	return s, nil
}

// Split breaks a batch script into commands (on ';' and newlines, outside quotes)
// and each command into shell-like words.
func Split(script string) ([][]string, error) {
	var cmds [][]string
	var words []string
	var cur strings.Builder
	inWord := false
	quote := rune(0)
	flushWord := func() {
		if inWord {
			words = append(words, cur.String())
			cur.Reset()
			inWord = false
		}
	}
	flushCmd := func() {
		flushWord()
		if len(words) > 0 {
			cmds = append(cmds, words)
			words = nil
		}
	}
	rs := []rune(script)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else if r == '\\' && quote == '"' && i+1 < len(rs) {
				i++
				cur.WriteRune(unescape(rs[i]))
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
			inWord = true
		case r == '\\' && i+1 < len(rs):
			i++
			cur.WriteRune(rs[i])
			inWord = true
		case r == ';' || r == '\n':
			flushCmd()
		case unicode.IsSpace(r):
			flushWord()
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated %c quote", quote)
	}
	flushCmd()
	return cmds, nil
}

func unescape(r rune) rune {
	switch r {
	case 'n':
		return '\n'
	case 't':
		return '\t'
	}
	return r
}

// Schema renders a command's arguments as a JSON schema for MCP.
func (c *Cmd) Schema() map[string]any {
	props := map[string]any{}
	var req []string
	for _, a := range c.Args {
		p := map[string]any{}
		switch a.Type {
		case "number":
			p["type"] = "number"
		case "int":
			p["type"] = "integer"
		case "bool":
			p["type"] = "boolean"
		case "strings":
			if a.Name == "text" || a.Name == "script" || a.Name == "command" || a.Name == "keys" {
				p["type"] = "string"
			} else {
				p["type"] = "array"
				p["items"] = map[string]any{"type": "string"}
			}
		default:
			p["type"] = "string"
		}
		if a.Help != "" {
			p["description"] = a.Help
		}
		props[a.Name] = p
		if a.Required {
			req = append(req, a.Name)
		}
	}
	s := map[string]any{"type": "object", "properties": props}
	if len(req) > 0 {
		s["required"] = req
	}
	return s
}
