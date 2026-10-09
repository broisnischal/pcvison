package daemon

import (
	"testing"

	"pc/internal/hypr"
	"pc/internal/img"
)

func TestShotMapping(t *testing.T) {
	// a 1280x720 image of the right-hand 1920x1080 monitor
	s := &Shot{Region: hypr.Rect{X: 1920, Y: 0, W: 1920, H: 1080}, W: 1280, H: 720}
	x, y := s.ToLayout(640, 360)
	if x != 2880 || y != 540 {
		t.Fatalf("centre maps to %v,%v", x, y)
	}
	bx, by := s.FromLayout(x, y)
	if bx != 640 || by != 360 {
		t.Fatalf("round trip gave %v,%v", bx, by)
	}
}

func TestFit(t *testing.T) {
	for _, c := range [][5]int{{1920, 1080, 1280, 1280, 720}, {800, 600, 1280, 800, 600}, {1080, 1920, 1280, 720, 1280}, {3840, 1080, 0, 3840, 1080}} {
		w, h := img.Fit(c[0], c[1], c[2])
		if w != c[3] || h != c[4] {
			t.Fatalf("Fit(%d,%d,%d) = %d,%d", c[0], c[1], c[2], w, h)
		}
	}
}

func TestParseRegion(t *testing.T) {
	r, err := parseRegion("10,20 300x400")
	if err != nil || r != (hypr.Rect{X: 10, Y: 20, W: 300, H: 400}) {
		t.Fatalf("got %v %v", r, err)
	}
	if _, err := parseRegion("10,20 0x5"); err == nil {
		t.Fatal("empty region should fail")
	}
}

func TestDur(t *testing.T) {
	a := Args{"a": "15m", "b": "90", "c": "1d", "d": "nonsense"}
	if a.Dur("a", 0).Minutes() != 15 || a.Dur("b", 0).Seconds() != 90 || a.Dur("c", 0).Hours() != 24 || a.Dur("d", 7) != 7 {
		t.Fatal("duration parsing is off")
	}
}
