package daemon

import (
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	"pc/internal/hypr"
	"pc/internal/img"
)

// TestGhostLive drives a second agent cursor on its own Wayland connection
// (no input is sent anywhere) and saves contact sheets of what the screen
// shows while it glides, clicks, types and sits on the seam between two
// monitors. PC_LIVE=1, PC_OUT=dir; PC_AT="x,y" moves the stage.
func TestGhostLive(t *testing.T) {
	if os.Getenv("PC_LIVE") == "" {
		t.Skip("PC_LIVE=1")
	}
	out := os.Getenv("PC_OUT")
	if out == "" {
		out = t.TempDir()
	}
	d := &Daemon{}
	d.ghost = newGhost(d)
	if _, err := d.session(); err != nil {
		t.Fatal(err)
	}
	x, y := 700.0, 300.0
	fmt.Sscanf(os.Getenv("PC_AT"), "%f,%f", &x, &y)
	film := func(name string, r hypr.Rect, dur time.Duration, start func()) {
		var tiles []img.Tile
		t0 := time.Now()
		start()
		for time.Since(t0) < dur {
			at := time.Since(t0)
			m, err := d.grab(r, int(r.W), false)
			if err != nil {
				t.Fatal(err)
			}
			tiles = append(tiles, img.Tile{Label: fmt.Sprintf("%3.0fms", float64(at.Milliseconds())), Img: m})
			time.Sleep(8 * time.Millisecond)
		}
		sheet := img.Sheet(tiles, 5, int(r.W))
		f, _ := os.Create(filepath.Join(out, name+".png"))
		_ = png.Encode(f, sheet)
		f.Close()
		t.Logf("%s: %d frames", name, len(tiles))
	}
	stage := hypr.Rect{X: x - 120, Y: y - 90, W: 420, H: 230}
	d.ghost.startGlide(x, y)
	time.Sleep(700 * time.Millisecond)
	film("glide", stage, 520*time.Millisecond, func() { d.ghost.startGlide(x+170, y+60) })
	film("retarget", stage, 600*time.Millisecond, func() {
		d.ghost.startGlide(x, y-40)
		go func() { time.Sleep(90 * time.Millisecond); d.ghost.startGlide(x+60, y+90) }()
	})
	film("click", stage, 520*time.Millisecond, func() { d.ghost.click() })
	film("typing", stage, 1500*time.Millisecond, func() { d.ghost.busy("is typing", 900*time.Millisecond) })
	seam := hypr.Rect{X: 1920 - 150, Y: y - 90, W: 300, H: 200}
	d.ghost.glideTo(1914, y)
	time.Sleep(500 * time.Millisecond)
	film("seam", seam, 300*time.Millisecond, func() {})
	film("hide", stage, 400*time.Millisecond, func() { d.ghost.hide() })
	time.Sleep(200 * time.Millisecond)
}
