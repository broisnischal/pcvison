package daemon

import (
	"image"
	"testing"

	"pc/internal/xcur"
)

// BenchmarkPaint is one busy frame: trail, ripple, tag with dots, and a
// tilted, squashed arrow.
func BenchmarkPaint(b *testing.B) {
	img, err := xcur.Load(xcur.Theme(), xcur.Size())
	if err != nil {
		b.Skip(err)
	}
	g := &Ghost{}
	sp := buildSprite(img, 1, defaultAccent)
	pix := make([]byte, surfW*surfH*4)
	f := frame{alpha: 1, tilt: 0.15, squash: 0.9, glow: 0.8, ripple: 0.3, rx: 3, ry: 2, tag: "Claude is typing", tagAlpha: 1, busy: 1, dots: true, pulse: 0.3,
		trail: [][3]float64{{-120, -40, 1}, {-90, -30, 0.75}, {-60, -18, 0.5}, {-30, -8, 0.25}, {0, 0, 0}}}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c := &canvas{pix: pix, stride: surfW * 4, w: surfW, h: surfH, s: 1}
		g.paint(c, f, sp, defaultAccent, 16, 21)
		if c.dirty == (image.Rectangle{}) {
			b.Fatal("painted nothing")
		}
	}
}
