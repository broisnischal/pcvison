package bench

import (
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"pc/internal/rpc"
	"pc/internal/wl"
)

func call(cmd string, args map[string]any) {
	rpc.Call(rpc.Request{Cmd: cmd, Args: args}, 5*time.Second)
}

// TestGlideFrames records where the agent cursor really is on screen, frame by
// frame, while it glides along y=520 on HDMI-A-1 (PC_LIVE=1). The cursor is
// found by diffing against a baseline captured with it hidden.
func TestGlideFrames(t *testing.T) {
	if os.Getenv("PC_LIVE") == "" {
		t.Skip("PC_LIVE=1")
	}
	c, err := wl.Connect("")
	if err != nil {
		t.Fatal(err)
	}
	outs := wl.TrackOutputs(c)
	shm, _ := c.BindIface("wl_shm", 1, nil)
	c.Roundtrip(time.Second)
	sc, _ := wl.NewScreencopy(c, shm)
	out, err := outs.ByName("HDMI-A-1")
	if err != nil {
		t.Fatal(err)
	}
	from, to := 300.0, 1600.0
	if v, err := strconv.ParseFloat(os.Getenv("PC_TO"), 64); err == nil {
		to = v
	}
	region := [4]int{200, 490, 1520, 70}
	call("cursor", map[string]any{"action": "hide"})
	time.Sleep(300 * time.Millisecond)
	base, err := sc.Capture(out, &region, false)
	if err != nil {
		t.Fatal(err)
	}
	call("cursor", map[string]any{"action": "show"})
	call("move", map[string]any{"x": from, "y": 520.0, "abs": true})
	time.Sleep(400 * time.Millisecond)
	type sample struct {
		t time.Duration
		x int
	}
	var samples []sample
	done := make(chan time.Duration)
	start := time.Now()
	go func() {
		t0 := time.Now()
		call("move", map[string]any{"x": to, "y": 520.0, "abs": true})
		done <- time.Since(t0)
	}()
	var moveTook time.Duration
	for moveTook == 0 || time.Since(start) < moveTook+120*time.Millisecond {
		select {
		case moveTook = <-done:
		default:
		}
		raw, err := sc.Capture(out, &region, false)
		if err != nil {
			t.Fatal(err)
		}
		at := time.Since(start)
		best := -1
		for x := 0; x < raw.W && best < 0; x++ {
			for y := 0; y < raw.H; y++ {
				i := y*raw.Stride + x*4
				d := absd(raw.Pix[i], base.Pix[i]) + absd(raw.Pix[i+1], base.Pix[i+1]) + absd(raw.Pix[i+2], base.Pix[i+2])
				if d > 90 {
					best = x
					break
				}
			}
		}
		samples = append(samples, sample{at, best + region[0]})
	}
	fmt.Printf("move call took %v; %d frames captured\n", moveTook, len(samples))
	prev := -1
	for _, s := range samples {
		d := 0
		if prev >= 0 {
			d = s.x - prev
		}
		fmt.Printf("%6.1fms  x=%5d  step=%4d\n", float64(s.t.Microseconds())/1000, s.x, d)
		prev = s.x
	}
}

func absd(a, b byte) int {
	if a > b {
		return int(a - b)
	}
	return int(b - a)
}
