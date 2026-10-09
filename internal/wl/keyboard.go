package wl

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/sys/unix"
)

// Keyboard is a zwp_virtual_keyboard_v1 with a keymap we generate ourselves:
// every character or key we need gets its own keycode, so typing never has to
// guess which modifiers produce a glyph on the user's layout. The keymap only
// grows when an unseen character shows up, so steady typing re-uploads nothing.
type Keyboard struct {
	c    *Conn
	obj  *Object
	t0   time.Time
	mu   sync.Mutex
	syms []string // index i is evdev code i+1
	idx  map[string]int
	sent int // len(syms) at the last upload
}

// Modifier keys sit at fixed codes so their masks are stable.
var modKeys = []struct {
	name, sym string
	mask      uint32
}{
	{"shift", "Shift_L", 1},
	{"ctrl", "Control_L", 4},
	{"alt", "Alt_L", 8},
	{"super", "Super_L", 64},
	{"altgr", "ISO_Level3_Shift", 128},
}

var modAliases = map[string]string{
	"shift": "shift", "ctrl": "ctrl", "control": "ctrl", "alt": "alt", "opt": "alt",
	"option": "alt", "super": "super", "win": "super", "cmd": "super", "logo": "super",
	"meta": "super", "mod4": "super", "altgr": "altgr",
}

var asciiNames = map[rune]string{
	' ': "space", '!': "exclam", '"': "quotedbl", '#': "numbersign", '$': "dollar",
	'%': "percent", '&': "ampersand", '\'': "apostrophe", '(': "parenleft", ')': "parenright",
	'*': "asterisk", '+': "plus", ',': "comma", '-': "minus", '.': "period", '/': "slash",
	':': "colon", ';': "semicolon", '<': "less", '=': "equal", '>': "greater", '?': "question",
	'@': "at", '[': "bracketleft", '\\': "backslash", ']': "bracketright", '^': "asciicircum",
	'_': "underscore", '`': "grave", '{': "braceleft", '|': "bar", '}': "braceright", '~': "asciitilde",
}

var namedKeys = []string{
	"Return", "Tab", "BackSpace", "Escape", "Delete", "Insert", "Home", "End", "Prior", "Next",
	"Left", "Right", "Up", "Down", "Menu", "Print", "Pause", "Scroll_Lock", "Caps_Lock", "Num_Lock",
	"KP_Enter", "F1", "F2", "F3", "F4", "F5", "F6", "F7", "F8", "F9", "F10", "F11", "F12",
	"XF86AudioPlay", "XF86AudioPause", "XF86AudioNext", "XF86AudioPrev", "XF86AudioStop",
	"XF86AudioMute", "XF86AudioRaiseVolume", "XF86AudioLowerVolume",
	"XF86MonBrightnessUp", "XF86MonBrightnessDown",
}

var keyAliases = map[string]string{
	"enter": "Return", "return": "Return", "ret": "Return", "esc": "Escape", "escape": "Escape",
	"tab": "Tab", "space": "space", "spc": "space", "backspace": "BackSpace", "bs": "BackSpace",
	"del": "Delete", "delete": "Delete", "ins": "Insert", "insert": "Insert", "home": "Home",
	"end": "End", "pgup": "Prior", "pageup": "Prior", "prior": "Prior", "pgdn": "Next",
	"pagedown": "Next", "next": "Next", "up": "Up", "down": "Down", "left": "Left",
	"right": "Right", "menu": "Menu", "print": "Print", "prtsc": "Print", "pause": "Pause",
	"capslock": "Caps_Lock", "numlock": "Num_Lock", "plus": "plus", "minus": "minus",
	"equal": "equal", "comma": "comma", "period": "period", "dot": "period", "slash": "slash",
	"backslash": "backslash", "semicolon": "semicolon", "quote": "apostrophe",
	"apostrophe": "apostrophe", "grave": "grave", "backtick": "grave",
	"super": "Super_L", "win": "Super_L", "meta": "Super_L", "cmd": "Super_L", "logo": "Super_L",
	"ctrl": "Control_L", "control": "Control_L", "shift": "Shift_L", "alt": "Alt_L",
	"play": "XF86AudioPlay", "playpause": "XF86AudioPlay", "mute": "XF86AudioMute",
	"volumeup": "XF86AudioRaiseVolume", "volumedown": "XF86AudioLowerVolume",
}

var symName = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

func NewKeyboard(c *Conn) (*Keyboard, error) {
	mgr, err := c.BindIface("zwp_virtual_keyboard_manager_v1", 1, nil)
	if err != nil {
		return nil, err
	}
	seat, err := c.BindIface("wl_seat", 1, nil)
	if err != nil {
		return nil, err
	}
	obj := c.NewObject("zwp_virtual_keyboard_v1", nil)
	if err := mgr.Req(0, NewMsg().U(seat.ID).U(obj.ID)); err != nil {
		return nil, err
	}
	k := &Keyboard{c: c, obj: obj, t0: time.Now(), idx: map[string]int{}}
	for _, m := range modKeys {
		k.add(m.sym)
	}
	for r := rune(0x20); r <= 0x7e; r++ {
		k.add(runeSym(r))
	}
	for _, n := range namedKeys {
		k.add(n)
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.upload(); err != nil {
		return nil, err
	}
	return k, nil
}

func (k *Keyboard) now() uint32 { return uint32(time.Since(k.t0).Milliseconds()) }

func (k *Keyboard) add(sym string) int {
	if i, ok := k.idx[sym]; ok {
		return i
	}
	k.syms = append(k.syms, sym)
	k.idx[sym] = len(k.syms) - 1
	return len(k.syms) - 1
}

// runeSym names the keysym for a character, the way xkbcommon spells it.
func runeSym(r rune) string {
	switch {
	case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		return string(r)
	case r == '\n' || r == '\r':
		return "Return"
	case r == '\t':
		return "Tab"
	}
	if n, ok := asciiNames[r]; ok {
		return n
	}
	return fmt.Sprintf("U%04X", r)
}

func (k *Keyboard) keymap() string {
	var b strings.Builder
	b.WriteString("xkb_keymap {\nxkb_keycodes \"pc\" {\nminimum = 8;\n")
	fmt.Fprintf(&b, "maximum = %d;\n", len(k.syms)+8)
	for i := range k.syms {
		fmt.Fprintf(&b, "<K%d> = %d;\n", i+1, i+9)
	}
	b.WriteString("};\nxkb_types \"pc\" { include \"complete\" };\n")
	b.WriteString("xkb_compatibility \"pc\" { include \"complete\" };\n")
	b.WriteString("xkb_symbols \"pc\" {\n")
	for i, s := range k.syms {
		if len(s) == 1 && s[0] >= 'a' && s[0] <= 'z' {
			// two levels, so shift+letter in a shortcut reads the way a real keyboard does
			fmt.Fprintf(&b, "key <K%d> { [ %s, %s ] };\n", i+1, s, strings.ToUpper(s))
		} else {
			fmt.Fprintf(&b, "key <K%d> { [ %s ] };\n", i+1, s)
		}
	}
	b.WriteString("modifier_map Shift { <K1> };\nmodifier_map Control { <K2> };\n")
	b.WriteString("modifier_map Mod1 { <K3> };\nmodifier_map Mod4 { <K4> };\nmodifier_map Mod5 { <K5> };\n")
	b.WriteString("};\n};\n")
	return b.String()
}

func (k *Keyboard) upload() error {
	text := k.keymap() + "\x00"
	fd, err := unix.MemfdCreate("pc-keymap", unix.MFD_CLOEXEC)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if _, err := unix.Write(fd, []byte(text)); err != nil {
		return err
	}
	if err := k.obj.Req(0, NewMsg().U(1).FD(fd).U(uint32(len(text)))); err != nil {
		return err
	}
	k.sent = len(k.syms)
	return nil
}

// ensure makes sure every sym has a keycode in the uploaded keymap. Past ~250
// codes X11 clients cannot see them, so the map is rebuilt from the base set.
func (k *Keyboard) ensure(syms []string) error {
	for _, s := range syms {
		k.add(s)
	}
	if len(k.syms) > 250 {
		base := len(modKeys) + 95 + len(namedKeys)
		keep := map[string]bool{}
		for _, s := range syms {
			keep[s] = true
		}
		syms := k.syms[:base]
		k.syms, k.idx = nil, map[string]int{}
		for _, s := range syms {
			k.add(s)
		}
		for s := range keep {
			k.add(s)
		}
		k.sent = 0
	}
	if len(k.syms) != k.sent {
		return k.upload()
	}
	return nil
}

func (k *Keyboard) key(code int, down bool) error {
	state := uint32(0)
	if down {
		state = 1
	}
	return k.obj.Req(1, NewMsg().U(k.now()).U(uint32(code+1)).U(state))
}

func (k *Keyboard) mods(mask uint32) error {
	return k.obj.Req(2, NewMsg().U(mask).U(0).U(0).U(0))
}

// Type enters text exactly as given. delay is the pause between characters.
func (k *Keyboard) Type(text string, delay time.Duration) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	var syms []string
	for _, r := range text {
		if r == '\r' {
			continue
		}
		if !unicode.IsPrint(r) && r != '\n' && r != '\t' {
			continue
		}
		syms = append(syms, runeSym(r))
	}
	if err := k.ensure(syms); err != nil {
		return err
	}
	if err := k.mods(0); err != nil {
		return err
	}
	for i, s := range syms {
		code := k.idx[s]
		if err := k.key(code, true); err != nil {
			return err
		}
		if err := k.key(code, false); err != nil {
			return err
		}
		if delay > 0 && i+1 < len(syms) {
			time.Sleep(delay)
		}
	}
	return nil
}

// ParseCombo splits "ctrl+shift+t" into modifier names and one key sym.
func ParseCombo(combo string) (mods []string, sym string, err error) {
	parts := strings.Split(strings.TrimSpace(combo), "+")
	if combo == "+" || strings.HasSuffix(combo, "++") {
		parts = append(parts[:len(parts)-2], "plus")
	}
	for i, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		low := strings.ToLower(p)
		if m, ok := modAliases[low]; ok && i < len(parts)-1 {
			mods = append(mods, m)
			continue
		}
		if sym != "" {
			return nil, "", fmt.Errorf("%q names two keys (%s and %s)", combo, sym, p)
		}
		sym = keySym(p)
	}
	if sym == "" {
		return nil, "", fmt.Errorf("%q has no key to press", combo)
	}
	if !symName.MatchString(sym) {
		return nil, "", fmt.Errorf("%q is not a key name (try Return, Escape, F5, a, slash, XF86AudioPlay)", sym)
	}
	return mods, sym, nil
}

func keySym(token string) string {
	low := strings.ToLower(token)
	if a, ok := keyAliases[low]; ok {
		return a
	}
	if r := []rune(token); len(r) == 1 {
		return runeSym(r[0])
	}
	if len(low) >= 2 && low[0] == 'f' && strings.Trim(low[1:], "0123456789") == "" {
		return "F" + low[1:]
	}
	return token
}

// Press sends one combo: modifiers down, key tap, modifiers up.
func (k *Keyboard) Press(combo string) error {
	mods, sym, err := ParseCombo(combo)
	if err != nil {
		return err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.ensure([]string{sym}); err != nil {
		return err
	}
	var mask uint32
	var held []int
	for _, m := range mods {
		for _, mk := range modKeys {
			if mk.name == m {
				code := k.idx[mk.sym]
				if err := k.key(code, true); err != nil {
					return err
				}
				held = append(held, code)
				mask |= mk.mask
				if err := k.mods(mask); err != nil {
					return err
				}
			}
		}
	}
	code := k.idx[sym]
	if err := k.key(code, true); err != nil {
		return err
	}
	if err := k.key(code, false); err != nil {
		return err
	}
	for i := len(held) - 1; i >= 0; i-- {
		if err := k.key(held[i], false); err != nil {
			return err
		}
	}
	return k.mods(0)
}
