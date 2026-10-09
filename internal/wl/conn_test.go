package wl

import (
	"os"
	"testing"
	"time"
)

func TestGlobals(t *testing.T) {
	if os.Getenv("WAYLAND_DISPLAY") == "" {
		t.Skip("no wayland session")
	}
	c, err := Connect("")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, g := range c.Globals() {
		t.Logf("%-45s v%d", g.Iface, g.Version)
	}
	if err := c.Roundtrip(time.Second); err != nil {
		t.Fatal(err)
	}
}
