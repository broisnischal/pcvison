// Package xkb reads the keymap the compositor sends its clients, enough to
// answer one question: which key, with which modifiers, produces this
// character or this named key on the user's layout. pc sends keys through the
// compositor (Hyprland's send_shortcut) rather than with a keymap of its own,
// so they have to be expressed in the layout the target app already has.
package xkb

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// Stroke is one key press: an XKB keycode and the modifiers (as Hyprland
// spells them: SHIFT, CTRL, ALT, SUPER, MOD5...) that select the level.
type Stroke struct {
	Code int
	Mods string
}

// Keymap maps keysyms to the strokes that produce them.
type Keymap struct {
	bySym map[uint32]Stroke
	codes map[string]int // key name to keycode, aliases resolved
}

var (
	reKeycode = regexp.MustCompile(`(?m)^\s*<([^>]+)>\s*=\s*(\d+)\s*;`)
	reAlias   = regexp.MustCompile(`(?m)^\s*alias\s*<([^>]+)>\s*=\s*<([^>]+)>\s*;`)
	reType    = regexp.MustCompile(`(?s)type\s+"([^"]+)"\s*\{(.*?)\};`)
	reMap     = regexp.MustCompile(`map\[([^\]]+)\]\s*=\s*(?:Level)?(\d+)\s*;`)
	reKey     = regexp.MustCompile(`(?s)key\s+<([^>]+)>\s*\{(.*?)\};`)
	reKeyType = regexp.MustCompile(`type(?:\[(?:Group)?1\])?\s*=\s*"([^"]+)"`)
	reSyms    = regexp.MustCompile(`(?:symbols\[(?:Group)?1\]\s*=\s*)?\[([^\]]*)\]`)
)

// section returns the body of the first `xkb_<name> "..." { ... };` block.
func section(text, name string) string {
	i := strings.Index(text, "xkb_"+name)
	if i < 0 {
		return ""
	}
	open := strings.Index(text[i:], "{")
	if open < 0 {
		return ""
	}
	depth := 0
	for j := i + open; j < len(text); j++ {
		switch text[j] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return text[i+open+1 : j]
			}
		}
	}
	return text[i+open+1:]
}

var modNames = map[string]string{
	"shift": "SHIFT", "lock": "CAPS", "control": "CTRL", "ctrl": "CTRL", "mod1": "ALT", "alt": "ALT", "meta": "ALT",
	"mod2": "MOD2", "numlock": "MOD2", "mod3": "MOD3", "levelfive": "MOD3", "mod4": "SUPER", "super": "SUPER",
	"mod5": "MOD5", "levelthree": "MOD5",
}

// levelMods turns a type's map entries into the modifiers for each level,
// preferring the simplest combination (Shift over Lock for capitals).
func levelMods(body string) map[int]string {
	out := map[int]string{1: ""}
	cost := map[int]int{1: 0}
	for _, m := range reMap.FindAllStringSubmatch(body, -1) {
		level, _ := strconv.Atoi(m[2])
		var mods []string
		c := 0
		ok := true
		for _, part := range strings.Split(m[1], "+") {
			name, known := modNames[strings.ToLower(strings.TrimSpace(part))]
			if !known {
				ok = false
				break
			}
			if name == "CAPS" {
				c += 10 // a Caps Lock press is a toggle, not a hold: last resort
			}
			mods = append(mods, name)
			c++
		}
		if !ok {
			continue
		}
		if old, seen := cost[level]; !seen || c < old {
			out[level], cost[level] = strings.Join(mods, " "), c
		}
	}
	return out
}

var implicit = map[int]string{1: "", 2: "SHIFT", 3: "MOD5", 4: "SHIFT MOD5"}

// Parse reads an XKB keymap as compositors send it (xkbcommon's text form).
func Parse(text string) (*Keymap, error) {
	km := &Keymap{bySym: map[uint32]Stroke{}, codes: map[string]int{}}
	kc := section(text, "keycodes")
	for _, m := range reKeycode.FindAllStringSubmatch(kc, -1) {
		n, _ := strconv.Atoi(m[2])
		km.codes[m[1]] = n
	}
	for _, m := range reAlias.FindAllStringSubmatch(kc, -1) {
		if n, ok := km.codes[m[2]]; ok {
			km.codes[m[1]] = n
		}
	}
	if len(km.codes) == 0 {
		return nil, fmt.Errorf("keymap has no keycodes")
	}
	types := map[string]map[int]string{}
	for _, m := range reType.FindAllStringSubmatch(section(text, "types"), -1) {
		types[m[1]] = levelMods(m[2])
	}
	type cand struct {
		s     Stroke
		score int
	}
	best := map[uint32]cand{}
	for _, m := range reKey.FindAllStringSubmatch(section(text, "symbols"), -1) {
		name, body := m[1], m[2]
		code, ok := km.codes[name]
		if !ok {
			continue
		}
		sm := reSyms.FindStringSubmatch(body)
		if sm == nil {
			continue
		}
		var syms []uint32
		for _, tok := range strings.Split(sm[1], ",") {
			syms = append(syms, keysym(strings.TrimSpace(tok)))
		}
		levels := implicit
		if tm := reKeyType.FindStringSubmatch(body); tm != nil {
			if t, ok := types[tm[1]]; ok {
				levels = t
			}
		}
		for i, sym := range syms {
			if sym == 0 {
				continue
			}
			mods, ok := levels[i+1]
			if !ok {
				continue
			}
			score := len(strings.Fields(mods))*100 + code
			if strings.Contains(mods, "CAPS") {
				score += 2000
			}
			if strings.HasPrefix(name, "KP") || strings.HasPrefix(name, "I") && len(name) > 3 {
				score += 1000 // keypad and the I-prefixed "internet" keys only as a last resort
			}
			if c, seen := best[sym]; !seen || score < c.score {
				best[sym] = cand{Stroke{Code: code, Mods: mods}, score}
			}
		}
	}
	for sym, c := range best {
		km.bySym[sym] = c.s
	}
	if len(km.bySym) == 0 {
		return nil, fmt.Errorf("keymap has no symbols")
	}
	return km, nil
}

// keysym reads one symbol token: 0x61, a name, or NoSymbol.
func keysym(tok string) uint32 {
	if strings.HasPrefix(tok, "0x") {
		n, err := strconv.ParseUint(tok[2:], 16, 32)
		if err != nil {
			return 0
		}
		return uint32(n)
	}
	if v, ok := Named[tok]; ok {
		return v
	}
	if r := []rune(tok); len(r) == 1 && r[0] < 0x80 {
		return uint32(r[0])
	}
	if len(tok) > 1 && tok[0] == 'U' {
		if n, err := strconv.ParseUint(tok[1:], 16, 32); err == nil {
			return 0x01000000 + uint32(n)
		}
	}
	for r, n := range asciiNames {
		if n == tok {
			return uint32(r)
		}
	}
	return 0
}

// SymOf is the keysym that types a character.
func SymOf(r rune) uint32 {
	switch {
	case r == '\n' || r == '\r':
		return Named["Return"]
	case r == '\t':
		return Named["Tab"]
	case r >= 0x20 && r <= 0x7e, r >= 0xa0 && r <= 0xff:
		return uint32(r)
	case r == '€':
		return 0x20ac
	}
	return 0x01000000 + uint32(r)
}

// Rune finds the stroke for a character.
func (k *Keymap) Rune(r rune) (Stroke, bool) {
	s, ok := k.bySym[SymOf(r)]
	if !ok && unicode.IsUpper(r) {
		// a layout with only lowercase listed: shift the lowercase key
		if l, ok2 := k.bySym[SymOf(unicode.ToLower(r))]; ok2 && l.Mods == "" {
			return Stroke{Code: l.Code, Mods: "SHIFT"}, true
		}
	}
	return s, ok
}

// Sym finds the stroke for a keysym name such as Return, F5, a or slash.
func (k *Keymap) Sym(name string) (Stroke, bool) {
	if v, ok := Named[name]; ok {
		s, found := k.bySym[v]
		return s, found
	}
	if r := []rune(name); len(r) == 1 {
		return k.Rune(r[0])
	}
	if v := keysym(name); v != 0 {
		s, found := k.bySym[v]
		return s, found
	}
	return Stroke{}, false
}

// Missing lists the characters of text the layout cannot type.
func (k *Keymap) Missing(text string) []rune {
	var out []rune
	seen := map[rune]bool{}
	for _, r := range text {
		if r == '\r' || seen[r] {
			continue
		}
		if _, ok := k.Rune(r); !ok {
			out = append(out, r)
			seen[r] = true
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Named holds the keysyms pc presses by name (from xkbcommon's keysym table).
var Named = map[string]uint32{
	"space": 0x20, "Return": 0xff0d, "Tab": 0xff09, "BackSpace": 0xff08, "Escape": 0xff1b, "Delete": 0xffff,
	"Insert": 0xff63, "Home": 0xff50, "End": 0xff57, "Prior": 0xff55, "Next": 0xff56,
	"Left": 0xff51, "Up": 0xff52, "Right": 0xff53, "Down": 0xff54, "Menu": 0xff67, "Print": 0xff61,
	"Pause": 0xff13, "Scroll_Lock": 0xff14, "Caps_Lock": 0xffe5, "Num_Lock": 0xff7f, "KP_Enter": 0xff8d,
	"Shift_L": 0xffe1, "Shift_R": 0xffe2, "Control_L": 0xffe3, "Control_R": 0xffe4, "Alt_L": 0xffe9, "Alt_R": 0xffea,
	"Super_L": 0xffeb, "Super_R": 0xffec, "ISO_Level3_Shift": 0xfe03, "NoSymbol": 0,
	"F1": 0xffbe, "F2": 0xffbf, "F3": 0xffc0, "F4": 0xffc1, "F5": 0xffc2, "F6": 0xffc3, "F7": 0xffc4,
	"F8": 0xffc5, "F9": 0xffc6, "F10": 0xffc7, "F11": 0xffc8, "F12": 0xffc9,
	"XF86AudioPlay": 0x1008ff14, "XF86AudioPause": 0x1008ff31, "XF86AudioStop": 0x1008ff15,
	"XF86AudioPrev": 0x1008ff16, "XF86AudioNext": 0x1008ff17, "XF86AudioMute": 0x1008ff12,
	"XF86AudioLowerVolume": 0x1008ff11, "XF86AudioRaiseVolume": 0x1008ff13,
	"XF86MonBrightnessUp": 0x1008ff02, "XF86MonBrightnessDown": 0x1008ff03,
}

var asciiNames = map[rune]string{
	' ': "space", '!': "exclam", '"': "quotedbl", '#': "numbersign", '$': "dollar",
	'%': "percent", '&': "ampersand", '\'': "apostrophe", '(': "parenleft", ')': "parenright",
	'*': "asterisk", '+': "plus", ',': "comma", '-': "minus", '.': "period", '/': "slash",
	':': "colon", ';': "semicolon", '<': "less", '=': "equal", '>': "greater", '?': "question",
	'@': "at", '[': "bracketleft", '\\': "backslash", ']': "bracketright", '^': "asciicircum",
	'_': "underscore", '`': "grave", '{': "braceleft", '|': "bar", '}': "braceright", '~': "asciitilde",
}
