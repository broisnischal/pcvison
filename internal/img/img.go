// Package img turns raw screencopy frames into small images a model can read:
// format conversion, an area-averaging downscale (fast enough for 4K in a few
// milliseconds across cores), composition of several monitors into one shot,
// and contact sheets with labels.
package img

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"runtime"
	"sync"

	"pc/internal/wl"

	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
)

// Detail levels: the long-edge pixel budget sent to the model.
var Detail = map[string]int{"low": 800, "normal": 1280, "high": 1568, "full": 0}

// pixelReader returns a function reading pixel (x, y) of a raw frame as RGB.
func pixelReader(r *wl.Raw) func(x, y int) (uint32, uint32, uint32) {
	pix, stride := r.Pix, r.Stride
	row := func(y int) int {
		if r.YInvert {
			y = r.H - 1 - y
		}
		return y * stride
	}
	switch r.Format {
	case wl.FmtARGB8888, wl.FmtXRGB8888: // bytes B G R X
		return func(x, y int) (uint32, uint32, uint32) {
			i := row(y) + x*4
			return uint32(pix[i+2]), uint32(pix[i+1]), uint32(pix[i])
		}
	case wl.FmtABGR8888, wl.FmtXBGR8888: // bytes R G B X
		return func(x, y int) (uint32, uint32, uint32) {
			i := row(y) + x*4
			return uint32(pix[i]), uint32(pix[i+1]), uint32(pix[i+2])
		}
	case wl.FmtXRGB2101010, wl.FmtARGB2101010:
		return func(x, y int) (uint32, uint32, uint32) {
			i := row(y) + x*4
			v := uint32(pix[i]) | uint32(pix[i+1])<<8 | uint32(pix[i+2])<<16 | uint32(pix[i+3])<<24
			return (v >> 22) & 0xff, (v >> 12) & 0xff, (v >> 2) & 0xff
		}
	default: // XBGR2101010 / ABGR2101010
		return func(x, y int) (uint32, uint32, uint32) {
			i := row(y) + x*4
			v := uint32(pix[i]) | uint32(pix[i+1])<<8 | uint32(pix[i+2])<<16 | uint32(pix[i+3])<<24
			return (v >> 2) & 0xff, (v >> 12) & 0xff, (v >> 22) & 0xff
		}
	}
}

// ScaleInto area-averages a raw frame (or the sub-rectangle src of it, in
// pixels) into dst's rectangle at. Rows are split across cores.
func ScaleInto(dst *image.RGBA, at image.Rectangle, r *wl.Raw, src image.Rectangle) {
	read := pixelReader(r)
	sw, sh := src.Dx(), src.Dy()
	dw, dh := at.Dx(), at.Dy()
	if sw <= 0 || sh <= 0 || dw <= 0 || dh <= 0 {
		return
	}
	xs := make([]int, dw+1)
	for i := range xs {
		xs[i] = src.Min.X + i*sw/dw
	}
	ys := make([]int, dh+1)
	for i := range ys {
		ys[i] = src.Min.Y + i*sh/dh
	}
	workers := runtime.GOMAXPROCS(0)
	if dh < 64 {
		workers = 1
	}
	var wg sync.WaitGroup
	chunk := (dh + workers - 1) / workers
	for w := 0; w < workers; w++ {
		y0, y1 := w*chunk, min((w+1)*chunk, dh)
		if y0 >= y1 {
			break
		}
		wg.Add(1)
		go func(y0, y1 int) {
			defer wg.Done()
			for j := y0; j < y1; j++ {
				sy0, sy1 := ys[j], max(ys[j+1], ys[j]+1)
				drow := (at.Min.Y+j-dst.Rect.Min.Y)*dst.Stride + (at.Min.X-dst.Rect.Min.X)*4
				for i := 0; i < dw; i++ {
					sx0, sx1 := xs[i], max(xs[i+1], xs[i]+1)
					var rs, gs, bs, n uint32
					for y := sy0; y < sy1; y++ {
						for x := sx0; x < sx1; x++ {
							r, g, b := read(x, y)
							rs += r
							gs += g
							bs += b
							n++
						}
					}
					o := drow + i*4
					dst.Pix[o] = uint8(rs / n)
					dst.Pix[o+1] = uint8(gs / n)
					dst.Pix[o+2] = uint8(bs / n)
					dst.Pix[o+3] = 255
				}
			}
		}(y0, y1)
	}
	wg.Wait()
}

// Fit returns the size that fits w×h into a long-edge budget (0 = native),
// never upscaling.
func Fit(w, h, budget int) (int, int) {
	if budget <= 0 || (w <= budget && h <= budget) {
		return w, h
	}
	if w >= h {
		return budget, max(1, h*budget/w)
	}
	return max(1, w*budget/h), budget
}

// Encode writes JPEG (or PNG for detail "full").
func Encode(m image.Image, detail string) ([]byte, string, error) {
	var buf bytes.Buffer
	if detail == "full" {
		enc := png.Encoder{CompressionLevel: png.BestSpeed}
		err := enc.Encode(&buf, m)
		return buf.Bytes(), "png", err
	}
	q := 72
	if detail == "high" {
		q = 80
	}
	err := jpeg.Encode(&buf, m, &jpeg.Options{Quality: q})
	return buf.Bytes(), "jpeg", err
}

// Tile is one labelled frame on a contact sheet.
type Tile struct {
	Label string
	Img   image.Image
}

// Sheet tiles frames into one labelled image, so several moments cost one look.
func Sheet(tiles []Tile, cols, cellW int) *image.RGBA {
	if len(tiles) == 0 {
		return image.NewRGBA(image.Rect(0, 0, 1, 1))
	}
	cols = max(1, min(cols, len(tiles)))
	rows := (len(tiles) + cols - 1) / cols
	cellH := 0
	for _, t := range tiles {
		b := t.Img.Bounds()
		h := b.Dy() * cellW / max(1, b.Dx())
		cellH = max(cellH, h)
	}
	const bar, pad = 22, 6
	W := cols*(cellW+pad) + pad
	H := rows*(cellH+bar+pad) + pad
	sheet := image.NewRGBA(image.Rect(0, 0, W, H))
	draw.Draw(sheet, sheet.Bounds(), &image.Uniform{color.RGBA{18, 18, 20, 255}}, image.Point{}, draw.Src)
	for i, t := range tiles {
		x := pad + (i%cols)*(cellW+pad)
		y := pad + (i/cols)*(cellH+bar+pad)
		b := t.Img.Bounds()
		h := b.Dy() * cellW / max(1, b.Dx())
		scaleRGBA(sheet, image.Rect(x, y+bar, x+cellW, y+bar+h), t.Img)
		Label(sheet, x+3, y+15, t.Label, color.RGBA{235, 235, 235, 255})
	}
	return sheet
}

// scaleRGBA area-averages any image into a destination rectangle.
func scaleRGBA(dst *image.RGBA, at image.Rectangle, src image.Image) {
	sb := src.Bounds()
	sw, sh, dw, dh := sb.Dx(), sb.Dy(), at.Dx(), at.Dy()
	if sw <= 0 || sh <= 0 || dw <= 0 || dh <= 0 {
		return
	}
	s, ok := src.(*image.RGBA)
	if !ok {
		s = image.NewRGBA(sb)
		draw.Draw(s, sb, src, sb.Min, draw.Src)
	}
	for j := 0; j < dh; j++ {
		sy0 := sb.Min.Y + j*sh/dh
		sy1 := max(sb.Min.Y+(j+1)*sh/dh, sy0+1)
		for i := 0; i < dw; i++ {
			sx0 := sb.Min.X + i*sw/dw
			sx1 := max(sb.Min.X+(i+1)*sw/dw, sx0+1)
			var r, g, b, n uint32
			for y := sy0; y < sy1; y++ {
				o := s.PixOffset(sx0, y)
				for x := sx0; x < sx1; x++ {
					r += uint32(s.Pix[o])
					g += uint32(s.Pix[o+1])
					b += uint32(s.Pix[o+2])
					o += 4
					n++
				}
			}
			o := dst.PixOffset(at.Min.X+i, at.Min.Y+j)
			dst.Pix[o], dst.Pix[o+1], dst.Pix[o+2], dst.Pix[o+3] = uint8(r/n), uint8(g/n), uint8(b/n), 255
		}
	}
}

// Label draws text with a dark backing so it reads on any frame.
func Label(dst *image.RGBA, x, y int, text string, c color.RGBA) {
	face := basicfont.Face7x13
	d := &font.Drawer{Dst: dst, Src: image.NewUniform(c), Face: face, Dot: fixed.P(x, y)}
	w := d.MeasureString(text).Ceil()
	draw.Draw(dst, image.Rect(x-3, y-12, x+w+3, y+4), &image.Uniform{color.RGBA{0, 0, 0, 170}}, image.Point{}, draw.Over)
	d.DrawString(text)
}
