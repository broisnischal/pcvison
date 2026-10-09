package xkb

import (
	"os"
	"testing"
)

func TestUSKeymap(t *testing.T) {
	b, err := os.ReadFile("testdata/us.xkb")
	if err != nil {
		t.Skip(err)
	}
	k, err := Parse(string(b))
	if err != nil {
		t.Fatal(err)
	}
	for r, want := range map[rune]Stroke{
		'a': {38, ""}, 'A': {38, "SHIFT"}, '1': {10, ""}, '!': {10, "SHIFT"}, ' ': {65, ""},
		'\n': {36, ""}, '/': {61, ""}, '?': {61, "SHIFT"}, '~': {49, "SHIFT"}, '"': {48, "SHIFT"},
	} {
		got, ok := k.Rune(r)
		if !ok || got != want {
			t.Errorf("%q: got %+v (found %v), want %+v", r, got, ok, want)
		}
	}
	for name, code := range map[string]int{"Return": 36, "Escape": 9, "F5": 71, "Left": 113, "Delete": 119, "BackSpace": 22, "Tab": 23} {
		if got, ok := k.Sym(name); !ok || got.Code != code || got.Mods != "" {
			t.Errorf("%s: got %+v (found %v), want code %d", name, got, ok, code)
		}
	}
	if m := k.Missing("héllo ✓"); len(m) != 2 {
		t.Errorf("missing: %q", string(m))
	}
}
