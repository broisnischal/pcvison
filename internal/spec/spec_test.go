package spec

import (
	"reflect"
	"testing"
)

func mustParse(t *testing.T, cmd string, words ...string) map[string]any {
	t.Helper()
	c, ok := Find(cmd)
	if !ok {
		t.Fatalf("no command %s", cmd)
	}
	m, err := c.Parse(words)
	if err != nil {
		t.Fatalf("%s %v: %v", cmd, words, err)
	}
	return m
}

func TestParseFlagsAnywhere(t *testing.T) {
	m := mustParse(t, "click", "--button", "right", "400", "300", "--double", "--shot=s3")
	want := map[string]any{"button": "right", "x": 400.0, "y": 300.0, "double": true, "shot": "s3"}
	if !reflect.DeepEqual(m, want) {
		t.Fatalf("got %v want %v", m, want)
	}
}

func TestNegativeNumbersAreNotFlags(t *testing.T) {
	m := mustParse(t, "click", "-5", "12", "--abs")
	if m["x"] != -5.0 || m["y"] != 12.0 || m["abs"] != true {
		t.Fatalf("got %v", m)
	}
}

func TestStringsSoakTheRest(t *testing.T) {
	m := mustParse(t, "type", "-w", "firefox", "hello", "world", "--enter")
	if !reflect.DeepEqual(m["text"], []string{"hello", "world"}) || m["window"] != "firefox" || m["enter"] != true {
		t.Fatalf("got %v", m)
	}
}

func TestBoolFalse(t *testing.T) {
	m := mustParse(t, "click", "1", "2", "--look=false")
	if m["look"] != false {
		t.Fatalf("got %v", m)
	}
}

func TestMissingRequired(t *testing.T) {
	c, _ := Find("drag")
	if _, err := c.Parse([]string{"1", "2", "3"}); err == nil {
		t.Fatal("drag with three numbers should fail")
	}
}

func TestUnknownFlag(t *testing.T) {
	c, _ := Find("shot")
	if _, err := c.Parse([]string{"--nope"}); err == nil {
		t.Fatal("unknown flag should fail")
	}
}

func TestSplit(t *testing.T) {
	got, err := Split("click 1 2; type \"a; b\" --enter\nkey 'ctrl+l' Return;; wait 0.5")
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"click", "1", "2"}, {"type", "a; b", "--enter"}, {"key", "ctrl+l", "Return"}, {"wait", "0.5"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q", got)
	}
	if _, err := Split(`type "open`); err == nil {
		t.Fatal("unterminated quote should fail")
	}
}

func TestEveryToolHasASchema(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range Cmds {
		if c.Tool == "" {
			continue
		}
		if seen[c.Tool] {
			t.Fatalf("duplicate tool name %s", c.Tool)
		}
		seen[c.Tool] = true
		s := c.Schema()
		if s["type"] != "object" {
			t.Fatalf("%s schema: %v", c.Tool, s)
		}
	}
}
