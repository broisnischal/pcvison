package wl

import (
	"os"
	"testing"
	"time"
)

// TestKeymapLive saves the seat keymap to PC_OUT (PC_LIVE=1).
func TestKeymapLive(t *testing.T) {
	if os.Getenv("PC_LIVE") == "" {
		t.Skip("PC_LIVE=1")
	}
	c, err := Connect("")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Roundtrip(time.Second); err != nil {
		t.Fatal(err)
	}
	k, err := WatchKeymap(c)
	if err != nil {
		t.Fatal(err)
	}
	text, gen, err := k.Text(2 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("keymap gen %d, %d bytes", gen, len(text))
	if p := os.Getenv("PC_OUT"); p != "" {
		_ = os.WriteFile(p, []byte(text), 0o600)
	}
}
