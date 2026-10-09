package wl

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseCombo(t *testing.T) {
	cases := []struct {
		in   string
		mods []string
		sym  string
	}{
		{"ctrl+shift+t", []string{"ctrl", "shift"}, "t"},
		{"Return", nil, "Return"},
		{"enter", nil, "Return"},
		{"alt+F4", []string{"alt"}, "F4"},
		{"super+1", []string{"super"}, "1"},
		{"ctrl+plus", []string{"ctrl"}, "plus"},
		{"ctrl++", []string{"ctrl"}, "plus"},
		{"ctrl+/", []string{"ctrl"}, "slash"},
		{"super", nil, "Super_L"},
		{"pgdn", nil, "Next"},
		{"XF86AudioPlay", nil, "XF86AudioPlay"},
	}
	for _, c := range cases {
		mods, sym, err := ParseCombo(c.in)
		if err != nil {
			t.Fatalf("%s: %v", c.in, err)
		}
		if !reflect.DeepEqual(mods, c.mods) || sym != c.sym {
			t.Fatalf("%s: got %v %s, want %v %s", c.in, mods, sym, c.mods, c.sym)
		}
	}
	for _, bad := range []string{"", "ctrl+a+b", "ctrl+ü+x", "ctrl+<script>"} {
		if _, _, err := ParseCombo(bad); err == nil {
			t.Fatalf("%q should not parse", bad)
		}
	}
}

func TestRuneSym(t *testing.T) {
	for r, want := range map[rune]string{'a': "a", 'Z': "Z", '7': "7", ' ': "space", '\n': "Return", '"': "quotedbl", 'é': "U00E9", '😀': "U1F600"} {
		if got := runeSym(r); got != want {
			t.Fatalf("%q: got %s want %s", r, got, want)
		}
	}
}

func TestKeymapShape(t *testing.T) {
	k := &Keyboard{idx: map[string]int{}}
	for _, m := range modKeys {
		k.add(m.sym)
	}
	k.add("a")
	k.add("U00E9")
	km := k.keymap()
	for _, want := range []string{"<K1> = 9;", "key <K1> { [ Shift_L ] };", "key <K6> { [ a, A ] };", "key <K7> { [ U00E9 ] };", "modifier_map Mod4 { <K4> };"} {
		if !strings.Contains(km, want) {
			t.Fatalf("keymap missing %q:\n%s", want, km)
		}
	}
}
