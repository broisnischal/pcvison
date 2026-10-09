package wl

import (
	"crypto/sha1"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestKeymapWatch saves every distinct keymap the compositor sends over
// PC_SECONDS (default 10) into PC_OUT (PC_LIVE=1). Read-only.
func TestKeymapWatch(t *testing.T) {
	if os.Getenv("PC_LIVE") == "" {
		t.Skip("PC_LIVE=1")
	}
	c, err := Connect("")
	if err != nil {
		t.Fatal(err)
	}
	c.Roundtrip(time.Second)
	k, err := WatchKeymap(c)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	lastGen := -1
	end := time.Now().Add(10 * time.Second)
	for time.Now().Before(end) {
		text, gen, err := k.Text(time.Second)
		if err == nil && gen != lastGen {
			lastGen = gen
			h := fmt.Sprintf("%x", sha1.Sum([]byte(text)))[:10]
			t.Logf("%s gen %d keymap %s (%d bytes)", time.Now().Format("15:04:05.000"), gen, h, len(text))
			if !seen[h] {
				seen[h] = true
				_ = os.WriteFile(filepath.Join(os.Getenv("PC_OUT"), "km-"+h+".xkb"), []byte(text), 0o600)
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
}
