package daemon

import (
	"fmt"
	"image"
	"image/color"
	"math"
	"sort"
	"strings"
	"sync"

	"pc/internal/hypr"
	"pc/internal/img"
	"pc/internal/xcur"
)

// Every screenshot says where the user's real pointer is and which windows
// are open, on screen or not. Screen capture leaves the hardware cursor out,
// so the user's pointer is drawn into the image (their own cursor, ringed and
// labelled "you"); the agent's cursor is already on screen as an overlay.

var (
	userCurOnce sync.Once
	userCur     *xcur.Image
)

// markUser draws the user's pointer into a shot of layout region r.
func markUser(m *image.RGBA, r hypr.Rect, px, py float64) {
	if !r.Contains(px, py) {
		return
	}
	userCurOnce.Do(func() { userCur, _ = xcur.Load(xcur.Theme(), xcur.Size()) })
	kx, ky := float64(m.Bounds().Dx())/r.W, float64(m.Bounds().Dy())/r.H
	x, y := (px-r.X)*kx, (py-r.Y)*ky
	// a ring that stands out on dark and light, then the cursor at screen size
	for _, ring := range []struct {
		rad, w float64
		c      color.RGBA
	}{{13, 3.2, color.RGBA{0, 0, 0, 150}}, {13, 1.6, color.RGBA{80, 200, 255, 255}}} {
		strokeCircle(m, x, y, ring.rad, ring.w, ring.c)
	}
	if userCur != nil {
		s := math.Max(0.5, math.Min(kx, 2)) // keep it legible in downscaled shots
		for j := 0; j < int(float64(userCur.H)*s); j++ {
			for i := 0; i < int(float64(userCur.W)*s); i++ {
				si := (int(float64(j)/s)*userCur.W + int(float64(i)/s)) * 4
				a := uint32(userCur.Pix[si+3])
				if a == 0 {
					continue
				}
				dx, dy := int(x)+i-int(float64(userCur.HotX)*s), int(y)+j-int(float64(userCur.HotY)*s)
				if !(image.Point{dx, dy}.In(m.Bounds())) {
					continue
				}
				o := m.PixOffset(dx, dy)
				inv := 255 - a
				m.Pix[o] = uint8(uint32(userCur.Pix[si+2]) + uint32(m.Pix[o])*inv/255)
				m.Pix[o+1] = uint8(uint32(userCur.Pix[si+1]) + uint32(m.Pix[o+1])*inv/255)
				m.Pix[o+2] = uint8(uint32(userCur.Pix[si]) + uint32(m.Pix[o+2])*inv/255)
				m.Pix[o+3] = 255
			}
		}
	}
	img.Label(m, int(x)+16, int(y)+30, "you", color.RGBA{120, 215, 255, 255})
}

func strokeCircle(m *image.RGBA, cx, cy, r, w float64, c color.RGBA) {
	b := m.Bounds()
	for y := int(cy - r - w - 1); y <= int(cy+r+w+1); y++ {
		for x := int(cx - r - w - 1); x <= int(cx+r+w+1); x++ {
			if !(image.Point{x, y}.In(b)) {
				continue
			}
			d := math.Hypot(float64(x)+0.5-cx, float64(y)+0.5-cy)
			cov := math.Min(1, w/2-math.Abs(d-r)+0.5)
			if cov <= 0 {
				continue
			}
			o := m.PixOffset(x, y)
			a := cov * float64(c.A) / 255
			m.Pix[o] = uint8(float64(c.R)*a + float64(m.Pix[o])*(1-a))
			m.Pix[o+1] = uint8(float64(c.G)*a + float64(m.Pix[o+1])*(1-a))
			m.Pix[o+2] = uint8(float64(c.B)*a + float64(m.Pix[o+2])*(1-a))
		}
	}
}

// shotContext describes, for a shot of layout region r scaled to w×h, where
// the user's pointer and the agent's cursor are and which windows are open:
// the ones in the shot with boxes in image pixels, and every other one by
// workspace, so nothing is hidden from the agent.
func (d *Daemon) shotContext(r hypr.Rect, w, h int, px, py float64, havePtr bool) string {
	kx, ky := float64(w)/r.W, float64(h)/r.H
	in := func(x, y float64) string { return fmt.Sprintf("%.0f,%.0f", (x-r.X)*kx, (y-r.Y)*ky) }
	var b strings.Builder
	if havePtr {
		under, _ := windowAt(px, py)
		if r.Contains(px, py) {
			fmt.Fprintf(&b, "\nuser's pointer: %s in this image (drawn, labelled \"you\"), over %s", in(px, py), describeWindow(under))
		} else {
			fmt.Fprintf(&b, "\nuser's pointer: not in this image (layout %.0f,%.0f, over %s)", px, py, describeWindow(under))
		}
	}
	if gx, gy, ok := d.ghost.position(); ok && r.Contains(gx, gy) {
		fmt.Fprintf(&b, "\nagent's cursor: %s in this image", in(gx, gy))
	}
	cs, err := hypr.Clients()
	if err != nil {
		return b.String()
	}
	vis, _ := clientsOnScreen()
	onScreen := map[string]bool{}
	var shown []string
	for _, c := range vis {
		onScreen[c.Address] = true
		box, ok := c.Box().Intersect(r)
		if !ok {
			continue
		}
		tag := ""
		if c.Fullscreen > 0 {
			tag = " fullscreen"
		} else if c.Floating {
			tag = " floating"
		}
		shown = append(shown, fmt.Sprintf("%s %s,%s %.0fx%.0f%s", clip(describeWindow(c), 46), fmtf((box.X-r.X)*kx), fmtf((box.Y-r.Y)*ky), box.W*kx, box.H*ky, tag))
	}
	if len(shown) > 0 {
		if len(shown) > 12 {
			shown = append(shown[:12], fmt.Sprintf("+%d more", len(shown)-12))
		}
		b.WriteString("\nwindows in this image (top-most first; x,y w×h in image pixels): " + strings.Join(shown, " · "))
	}
	other := map[string][]string{}
	var wss []string
	for _, c := range cs {
		if !c.Mapped || onScreen[c.Address] {
			continue
		}
		ws := c.Workspace.Name
		if _, ok := other[ws]; !ok {
			wss = append(wss, ws)
		}
		other[ws] = append(other[ws], clip(describeWindow(c), 34))
	}
	sort.Strings(wss)
	if len(wss) > 0 {
		var parts []string
		for _, ws := range wss {
			parts = append(parts, "ws "+ws+": "+strings.Join(other[ws], ", "))
		}
		b.WriteString("\nopen but not on screen: " + strings.Join(parts, " · "))
	}
	return b.String()
}

func fmtf(v float64) string { return fmt.Sprintf("%.0f", math.Max(0, v)) }
