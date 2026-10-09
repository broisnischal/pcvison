package bench

import (
	"fmt"
	"os"
	"testing"
	"time"

	"pc/internal/wl"
)

// TestFade measures how visible the agent cursor is, frame by frame, while it
// fades out and back in (PC_LIVE=1; cursor resting at 960,300 on HDMI-A-1).
func TestFade(t *testing.T) {
	if os.Getenv("PC_LIVE") == "" {
		t.Skip("PC_LIVE=1")
	}
	c, _ := wl.Connect("")
	outs := wl.TrackOutputs(c)
	shm, _ := c.BindIface("wl_shm", 1, nil)
	c.Roundtrip(time.Second)
	sc, _ := wl.NewScreencopy(c, shm)
	out, _ := outs.ByName("HDMI-A-1")
	region := [4]int{950, 290, 30, 40}
	call("cursor", map[string]any{"action": "hide"})
	time.Sleep(500 * time.Millisecond)
	base, _ := sc.Capture(out, &region, false)
	call("cursor", map[string]any{"action": "show"})
	call("move", map[string]any{"x": 960.0, "y": 300.0, "abs": true})
	time.Sleep(400 * time.Millisecond)
	full, _ := sc.Capture(out, &region, false)
	diff := func(r *wl.Raw) float64 {
		s := 0
		for i := 0; i+3 < len(r.Pix); i += 4 {
			s += absd(r.Pix[i], base.Pix[i]) + absd(r.Pix[i+1], base.Pix[i+1]) + absd(r.Pix[i+2], base.Pix[i+2])
		}
		return float64(s)
	}
	ref := diff(full)
	for _, step := range []string{"hide", "show"} {
		go func() {
			if step == "hide" {
				call("cursor", map[string]any{"action": "hide"})
			} else {
				call("cursor", map[string]any{"action": "show"})
			}
		}()
		start := time.Now()
		fmt.Printf("-- %s\n", step)
		for time.Since(start) < 400*time.Millisecond {
			r, err := sc.Capture(out, &region, false)
			if err != nil {
				break
			}
			fmt.Printf("%6.1fms  visible %3.0f%%\n", float64(time.Since(start).Microseconds())/1000, 100*diff(r)/ref)
		}
	}
}
