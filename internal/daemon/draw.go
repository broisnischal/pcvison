package daemon

import (
	"fmt"
	"image"
	"math"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gomedium"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"

	"pc/internal/xcur"
)

// Drawing on the cursor surface: premultiplied ARGB8888, bytes B G R A.

type rgb [3]float64

// Claude's terracotta: warm enough to read on dark and light themes alike,
// and unlike any stock cursor.
var defaultAccent = rgb{0xd9, 0x77, 0x57}

func (c rgb) hex() string { return fmt.Sprintf("#%02x%02x%02x", int(c[0]), int(c[1]), int(c[2])) }

var namedColors = map[string]rgb{
	"claude": defaultAccent, "orange": {0xf0, 0x8a, 0x3c}, "blue": {0x7a, 0xa2, 0xf7},
	"green": {0x5f, 0xd0, 0x8a}, "purple": {0xb4, 0x8e, 0xf7}, "pink": {0xf7, 0x7a, 0xb8},
	"red": {0xf2, 0x5f, 0x5c}, "yellow": {0xf2, 0xc9, 0x4c}, "teal": {0x4c, 0xc9, 0xc0},
}

func parseColor(s string) (rgb, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if c, ok := namedColors[s]; ok {
		return c, nil
	}
	h := strings.TrimPrefix(s, "#")
	if len(h) == 3 {
		h = string([]byte{h[0], h[0], h[1], h[1], h[2], h[2]})
	}
	if len(h) != 6 {
		return rgb{}, fmt.Errorf("colour is #rrggbb or one of claude, orange, blue, green, purple, pink, red, yellow, teal")
	}
	n, err := strconv.ParseUint(h, 16, 32)
	if err != nil {
		return rgb{}, fmt.Errorf("bad colour %q", s)
	}
	return rgb{float64(n >> 16 & 0xff), float64(n >> 8 & 0xff), float64(n & 0xff)}, nil
}

// canvas is one buffer being painted, and the box of pixels it has touched.
type canvas struct {
	pix       []byte
	stride    int
	w, h      int
	s         float64 // buffer scale
	dirty     image.Rectangle
	scratch   []float32
	scratchAt image.Rectangle
}

func (c *canvas) clip(x0, y0, x1, y1 float64) image.Rectangle {
	r := image.Rect(int(math.Floor(x0)), int(math.Floor(y0)), int(math.Ceil(x1))+1, int(math.Ceil(y1))+1)
	return r.Intersect(image.Rect(0, 0, c.w, c.h))
}

func (c *canvas) touch(r image.Rectangle) { c.dirty = c.dirty.Union(r) }

// over composites one straight-alpha colour at coverage cov onto a pixel.
func over(pix []byte, i int, r, g, b, cov float64) {
	if cov <= 0 {
		return
	}
	if cov > 1 {
		cov = 1
	}
	inv := 1 - cov
	pix[i] = uint8(b*cov + float64(pix[i])*inv)
	pix[i+1] = uint8(g*cov + float64(pix[i+1])*inv)
	pix[i+2] = uint8(r*cov + float64(pix[i+2])*inv)
	pix[i+3] = uint8(255*cov + float64(pix[i+3])*inv)
}

// ring draws an antialiased circle outline.
func (c *canvas) ring(cx, cy, r, width float64, col rgb, alpha float64) {
	if alpha <= 0.004 {
		return
	}
	box := c.clip(cx-r-width-1, cy-r-width-1, cx+r+width+1, cy+r+width+1)
	c.touch(box)
	for y := box.Min.Y; y < box.Max.Y; y++ {
		for x := box.Min.X; x < box.Max.X; x++ {
			d := math.Hypot(float64(x)+0.5-cx, float64(y)+0.5-cy)
			if cov := math.Min(1, width/2-math.Abs(d-r)+0.5); cov > 0 {
				over(c.pix, y*c.stride+x*4, col[0], col[1], col[2], cov*alpha)
			}
		}
	}
}

// disc draws a filled antialiased circle.
func (c *canvas) disc(cx, cy, r float64, col rgb, alpha float64) {
	if alpha <= 0.004 || r <= 0 {
		return
	}
	box := c.clip(cx-r-1, cy-r-1, cx+r+1, cy+r+1)
	c.touch(box)
	for y := box.Min.Y; y < box.Max.Y; y++ {
		for x := box.Min.X; x < box.Max.X; x++ {
			d := math.Hypot(float64(x)+0.5-cx, float64(y)+0.5-cy)
			if cov := math.Min(1, r-d+0.5); cov > 0 {
				over(c.pix, y*c.stride+x*4, col[0], col[1], col[2], cov*alpha)
			}
		}
	}
}

// pill fills an antialiased rounded rectangle, with an optional border.
func (c *canvas) pill(x0, y0, x1, y1, rad float64, fill rgb, fa float64, border rgb, ba, bw float64) {
	box := c.clip(x0, y0, x1, y1)
	c.touch(box)
	for y := box.Min.Y; y < box.Max.Y; y++ {
		for x := box.Min.X; x < box.Max.X; x++ {
			px, py := float64(x)+0.5, float64(y)+0.5
			// signed distance to the rounded box
			qx := math.Abs(px-(x0+x1)/2) - ((x1-x0)/2 - rad)
			qy := math.Abs(py-(y0+y1)/2) - ((y1-y0)/2 - rad)
			out := math.Hypot(math.Max(qx, 0), math.Max(qy, 0)) + math.Min(math.Max(qx, qy), 0) - rad
			cov := math.Min(1, math.Max(0, 0.5-out))
			if cov <= 0 {
				continue
			}
			i := y*c.stride + x*4
			over(c.pix, i, fill[0], fill[1], fill[2], cov*fa)
			if bw > 0 {
				if edge := math.Min(1, math.Max(0, bw-math.Abs(out+bw/2)+0.5)); edge > 0 {
					over(c.pix, i, border[0], border[1], border[2], edge*cov*ba)
				}
			}
		}
	}
}

// ---------------------------------------------------------------- the arrow

// sprite is the user's cursor image restyled as the agent's: its light parts
// (Adwaita's outline, a white theme's fill) take the accent colour, and a
// soft glow of the same colour sits under it. Built once per scale and colour.
type sprite struct {
	w, h   int     // buffer pixels
	hx, hy float64 // hotspot inside
	arrow  []byte  // premultiplied BGRA
	glow   []float32
	cx, cy float64 // the arrow's centre of mass relative to the hotspot, logical px
	reach  float64 // furthest painted pixel from the hotspot, buffer px
}

func buildSprite(img *xcur.Image, scale int, accent rgb) *sprite {
	s := float64(scale)
	pad := int(math.Ceil(9 * s))
	sp := &sprite{w: img.W + 2*pad, h: img.H + 2*pad, hx: float64(img.HotX + pad), hy: float64(img.HotY + pad)}
	sp.arrow = make([]byte, sp.w*sp.h*4)
	mask := make([]float32, sp.w*sp.h)
	var mx, my, mass float64
	for y := 0; y < img.H; y++ {
		for x := 0; x < img.W; x++ {
			si := (y*img.W + x) * 4
			a := float64(img.Pix[si+3])
			if a == 0 {
				continue
			}
			b, g, r := float64(img.Pix[si])*255/a, float64(img.Pix[si+1])*255/a, float64(img.Pix[si+2])*255/a
			l := (0.2126*r + 0.7152*g + 0.0722*b) / 255
			t := smoothstep(math.Min(1, math.Max(0, (l-0.35)/0.5)))
			r, g, b = r+(accent[0]*(0.88+0.12*l)-r)*t, g+(accent[1]*(0.88+0.12*l)-g)*t, b+(accent[2]*(0.88+0.12*l)-b)*t
			di := ((y+pad)*sp.w + x + pad) * 4
			sp.arrow[di] = uint8(b * a / 255)
			sp.arrow[di+1] = uint8(g * a / 255)
			sp.arrow[di+2] = uint8(r * a / 255)
			sp.arrow[di+3] = uint8(a)
			mask[(y+pad)*sp.w+x+pad] = float32(a / 255)
			mx += float64(x+pad) * a
			my += float64(y+pad) * a
			mass += a
		}
	}
	if mass > 0 {
		sp.cx, sp.cy = (mx/mass-sp.hx)/s, (my/mass-sp.hy)/s
	}
	// glow: grow the silhouette a little, then blur it
	grown := dilate(mask, sp.w, sp.h, int(math.Round(1.5*s)))
	r := int(math.Max(1, math.Round(2.2*s)))
	for i := 0; i < 3; i++ {
		grown = boxBlur(grown, sp.w, sp.h, r)
	}
	sp.glow = grown
	for y := 0; y < sp.h; y++ {
		for x := 0; x < sp.w; x++ {
			if sp.glow[y*sp.w+x] > 0.01 || sp.arrow[(y*sp.w+x)*4+3] > 0 {
				sp.reach = math.Max(sp.reach, math.Hypot(float64(x)+0.5-sp.hx, float64(y)+0.5-sp.hy))
			}
		}
	}
	return sp
}

func dilate(m []float32, w, h, r int) []float32 {
	out := make([]float32, len(m))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var best float32
			for dy := -r; dy <= r; dy++ {
				yy := y + dy
				if yy < 0 || yy >= h {
					continue
				}
				for dx := -r; dx <= r; dx++ {
					xx := x + dx
					if xx < 0 || xx >= w || dx*dx+dy*dy > r*r+r {
						continue
					}
					if v := m[yy*w+xx]; v > best {
						best = v
					}
				}
			}
			out[y*w+x] = best
		}
	}
	return out
}

func boxBlur(m []float32, w, h, r int) []float32 {
	tmp := make([]float32, len(m))
	out := make([]float32, len(m))
	n := float32(2*r + 1)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var sum float32
			for k := -r; k <= r; k++ {
				if xx := x + k; xx >= 0 && xx < w {
					sum += m[y*w+xx]
				}
			}
			tmp[y*w+x] = sum / n
		}
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var sum float32
			for k := -r; k <= r; k++ {
				if yy := y + k; yy >= 0 && yy < h {
					sum += tmp[yy*w+x]
				}
			}
			out[y*w+x] = sum / n
		}
	}
	return out
}

// arrow draws the sprite with its hotspot at (tx, ty), turned by angle
// (radians, clockwise) and scaled by k about the hotspot.
func (c *canvas) arrow(sp *sprite, tx, ty, angle, k, alpha, glowA float64, accent rgb) {
	if alpha <= 0.004 {
		return
	}
	reach := sp.reach*k + 2
	box := c.clip(tx-reach, ty-reach, tx+reach, ty+reach)
	c.touch(box)
	sin, cos := math.Sincos(-angle)
	straight := math.Abs(angle) < 0.002 && math.Abs(k-1) < 0.002
	ox, oy := int(math.Round(tx-sp.hx)), int(math.Round(ty-sp.hy))
	for y := box.Min.Y; y < box.Max.Y; y++ {
		for x := box.Min.X; x < box.Max.X; x++ {
			var g float64
			var b, gr, r, a float64
			if straight {
				sx, sy := x-ox, y-oy
				if sx < 0 || sy < 0 || sx >= sp.w || sy >= sp.h {
					continue
				}
				i := sy*sp.w + sx
				g = float64(sp.glow[i])
				b, gr, r, a = float64(sp.arrow[i*4]), float64(sp.arrow[i*4+1]), float64(sp.arrow[i*4+2]), float64(sp.arrow[i*4+3])
			} else {
				dx, dy := (float64(x)+0.5-tx)/k, (float64(y)+0.5-ty)/k
				sx := dx*cos - dy*sin + sp.hx - 0.5
				sy := dx*sin + dy*cos + sp.hy - 0.5
				g, b, gr, r, a = sp.sample(sx, sy)
			}
			i := y*c.stride + x*4
			if g > 0.003 {
				over(c.pix, i, accent[0], accent[1], accent[2], g*glowA*alpha)
			}
			if a > 0.5 {
				// premultiplied source over destination
				f := alpha
				inv := 1 - a*f/255
				c.pix[i] = uint8(math.Min(255, b*f+float64(c.pix[i])*inv))
				c.pix[i+1] = uint8(math.Min(255, gr*f+float64(c.pix[i+1])*inv))
				c.pix[i+2] = uint8(math.Min(255, r*f+float64(c.pix[i+2])*inv))
				c.pix[i+3] = uint8(math.Min(255, a*f+float64(c.pix[i+3])*inv))
			}
		}
	}
}

// sample reads the sprite bilinearly at a fractional pixel position.
func (sp *sprite) sample(x, y float64) (g, b, gr, r, a float64) {
	x0, y0 := int(math.Floor(x)), int(math.Floor(y))
	fx, fy := x-float64(x0), y-float64(y0)
	for j := 0; j < 2; j++ {
		for i := 0; i < 2; i++ {
			xx, yy := x0+i, y0+j
			if xx < 0 || yy < 0 || xx >= sp.w || yy >= sp.h {
				continue
			}
			w := (1 - math.Abs(float64(i)-fx)) * (1 - math.Abs(float64(j)-fy))
			if w <= 0 {
				continue
			}
			p := yy*sp.w + xx
			g += float64(sp.glow[p]) * w
			b += float64(sp.arrow[p*4]) * w
			gr += float64(sp.arrow[p*4+1]) * w
			r += float64(sp.arrow[p*4+2]) * w
			a += float64(sp.arrow[p*4+3]) * w
		}
	}
	return
}

// ---------------------------------------------------------------- trail

type trailPt struct {
	x, y float64 // layout
	age  float64 // 0 = now, 1 = about to vanish
}

// trail draws a tapering comet tail through pts (buffer coordinates, oldest
// first, ending at the cursor). Coverage is the max over segments, so joints
// do not double up.
func (c *canvas) trail(pts [][3]float64, width float64, col rgb, alpha float64) {
	if len(pts) < 2 || alpha <= 0.004 {
		return
	}
	x0, y0, x1, y1 := math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)
	for _, p := range pts {
		x0, y0 = math.Min(x0, p[0]), math.Min(y0, p[1])
		x1, y1 = math.Max(x1, p[0]), math.Max(y1, p[1])
	}
	box := c.clip(x0-width-1, y0-width-1, x1+width+1, y1+width+1)
	if box.Empty() {
		return
	}
	n := box.Dx() * box.Dy()
	if cap(c.scratch) < n {
		c.scratch = make([]float32, n)
	}
	m := c.scratch[:n]
	clear(m)
	for i := 1; i < len(pts); i++ {
		a, b := pts[i-1], pts[i]
		sb := c.clip(math.Min(a[0], b[0])-width-1, math.Min(a[1], b[1])-width-1, math.Max(a[0], b[0])+width+1, math.Max(a[1], b[1])+width+1)
		vx, vy := b[0]-a[0], b[1]-a[1]
		l2 := vx*vx + vy*vy
		for y := sb.Min.Y; y < sb.Max.Y; y++ {
			for x := sb.Min.X; x < sb.Max.X; x++ {
				px, py := float64(x)+0.5-a[0], float64(y)+0.5-a[1]
				u := 0.0
				if l2 > 0 {
					u = math.Max(0, math.Min(1, (px*vx+py*vy)/l2))
				}
				d := math.Hypot(px-u*vx, py-u*vy)
				fresh := 1 - (a[2] + (b[2]-a[2])*u) // 1 at the cursor, 0 at the tail
				wd := width * (0.15 + 0.85*fresh)
				cov := math.Min(1, wd/2-d+0.5) * fresh * fresh
				if cov > 0 {
					p := &m[(y-box.Min.Y)*box.Dx()+x-box.Min.X]
					if float32(cov) > *p {
						*p = float32(cov)
					}
				}
			}
		}
	}
	c.touch(box)
	for y := box.Min.Y; y < box.Max.Y; y++ {
		for x := box.Min.X; x < box.Max.X; x++ {
			if v := m[(y-box.Min.Y)*box.Dx()+x-box.Min.X]; v > 0 {
				over(c.pix, y*c.stride+x*4, col[0], col[1], col[2], float64(v)*alpha)
			}
		}
	}
}

// ---------------------------------------------------------------- the tag

var (
	faceMu  sync.Mutex
	faces   = map[float64]font.Face{}
	texts   = map[string]*image.Alpha{}
	ttfOnce sync.Once
	ttf     *opentype.Font
)

func face(px float64) font.Face {
	faceMu.Lock()
	defer faceMu.Unlock()
	if f := faces[px]; f != nil {
		return f
	}
	ttfOnce.Do(func() { ttf, _ = opentype.Parse(gomedium.TTF) })
	if ttf == nil {
		return nil
	}
	f, err := opentype.NewFace(ttf, &opentype.FaceOptions{Size: px, DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		return nil
	}
	faces[px] = f
	return f
}

// textMask renders a label once into a coverage mask and caches it.
func textMask(label string, px float64) *image.Alpha {
	key := label + "|" + strconv.FormatFloat(px, 'f', 1, 64)
	faceMu.Lock()
	if m := texts[key]; m != nil {
		faceMu.Unlock()
		return m
	}
	faceMu.Unlock()
	fc := face(px)
	if fc == nil {
		return nil
	}
	adv := font.MeasureString(fc, label).Ceil()
	met := fc.Metrics()
	hgt := (met.Ascent + met.Descent).Ceil()
	m := image.NewAlpha(image.Rect(0, 0, adv+2, hgt+2))
	d := &font.Drawer{Dst: m, Src: image.Opaque, Face: fc, Dot: fixed.Point26_6{X: fixed.I(1), Y: met.Ascent + fixed.I(1)}}
	d.DrawString(label)
	faceMu.Lock()
	if len(texts) > 64 {
		texts = map[string]*image.Alpha{}
	}
	texts[key] = m
	faceMu.Unlock()
	return m
}

const tagFont, tagH = 12.5, 22.0

// tagSize is the tag's logical size for a label.
func tagSize(label string, dots bool) (w, h float64) {
	tw := 0.0
	if m := textMask(label, tagFont); m != nil {
		tw = float64(m.Bounds().Dx())
	}
	w = 21 + tw + 10
	if dots {
		w += 19
	}
	return w, tagH
}

// tag draws the name pill with its top-left at (x0, y0), buffer pixels: a dot
// that breathes while the agent is busy, the label, and pulsing dots for
// "is typing" and "is pasting".
func (c *canvas) tag(x0, y0 float64, label string, dots bool, alpha, pulse, busy float64, accent rgb) {
	if alpha <= 0.01 {
		return
	}
	s := c.s
	lw, lh := tagSize(label, dots)
	bw, bh := lw*s, lh*s
	c.pill(x0, y0, x0+bw, y0+bh, bh/2, rgb{16, 17, 22}, 0.9*alpha, accent, 0.75*alpha, 1*s)
	breath := 1 - busy*(0.45-0.45*math.Sin(pulse*2*math.Pi))
	c.disc(x0+11*s, y0+bh/2, 3.4*s, accent, alpha*breath)
	mask := textMask(label, tagFont*s)
	if mask != nil {
		mb := mask.Bounds()
		tx, ty := int(x0+21*s), int(y0+(bh-float64(mb.Dy()))/2)
		r := image.Rect(tx, ty, tx+mb.Dx(), ty+mb.Dy()).Intersect(image.Rect(0, 0, c.w, c.h))
		c.touch(r)
		for y := r.Min.Y; y < r.Max.Y; y++ {
			for x := r.Min.X; x < r.Max.X; x++ {
				if v := float64(mask.AlphaAt(x-tx, y-ty).A) / 255; v > 0 {
					over(c.pix, y*c.stride+x*4, 238, 238, 242, v*alpha)
				}
			}
		}
		if dots {
			left := x0 + 21*s + float64(mb.Dx()) + 5*s
			for i := 0; i < 3; i++ {
				phase := math.Mod(pulse*3-float64(i)*0.5+3, 3) / 3
				lift := math.Max(0, math.Sin(phase*2*math.Pi))
				c.disc(left+float64(i)*5.2*s, y0+bh/2+2*s-2.5*s*lift, 1.6*s, rgb{238, 238, 242}, alpha*(0.35+0.65*lift))
			}
		}
	}
}
