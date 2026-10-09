package daemon

import (
	"encoding/json"
	"fmt"
	"image"
	"log"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"pc/internal/hypr"
	"pc/internal/wl"
	"pc/internal/xcur"
)

const (
	ghostNamespace = "pc-cursor"
	// The surface is far larger than the cursor: the name tag, the trail and
	// the click ripple are drawn around the tip, which sits at (hotX, hotY).
	// Only the pixels a frame paints are cleared and damaged, so the size
	// costs nothing per frame.
	surfW, surfH = 480, 272
	hotX, hotY   = 240, 136

	fadeIn   = 140 * time.Millisecond
	fadeOut  = 260 * time.Millisecond
	rippleOn = 460 * time.Millisecond
	pressOn  = 240 * time.Millisecond
	labelIn  = 160 * time.Millisecond
	labelOut = 280 * time.Millisecond
	trailFor = 120 * time.Millisecond

	// a glide reaches its target at approachShare of its length, swings
	// past it, and settles back by the end
	approachShare = 0.8
	overStart     = 0.6

	tagLag = 20.0 // how far (px) the name tag may trail the tip
)

// Ghost is the agent's own cursor: a click-through overlay that draws the
// user's own cursor image, tinted and glowing in the accent colour, with a
// name tag that trails it. One render loop owns the surfaces and advances
// every animation (glide, physics, fade, ripple, tag) exactly once per
// display frame, driven by the compositor's frame callbacks.
type Ghost struct {
	d    *Daemon
	mu   sync.Mutex
	wake chan struct{}

	have     bool
	x, y     float64 // where it rests, or is heading
	cx, cy   float64 // where the last frame put the tip
	gl       *glide
	want     bool // should be on screen
	alpha    float64
	fade     *ramp
	disabled bool
	clickAt  time.Time
	clickX   float64 // ripple centre, layout
	clickY   float64
	activity string // "typing", "pressing ctrl+l"; "" = the note, or idle
	actAt    time.Time
	until    time.Time // the activity label gives way after this
	note     string    // what the agent said it is doing (pc cursor say)
	noteTill time.Time
	lastAct  time.Time
	speed    float64
	idle     time.Duration
	lostSurf bool
	style    cursorStyle

	// physics, advanced once per frame
	lastT        time.Time
	lx, ly       float64 // tip at the previous frame
	vx, vy       float64 // px/s
	tilt, tiltV  float64 // radians, clockwise
	tagX, tagY   float64 // the tag's lag behind the tip, px
	tagVX, tagVY float64
	trail        []trailPt
	trailT       []time.Time

	// owned by the render loop
	surfs   map[string]*ghostSurf
	sprites map[spriteKey]*sprite
	images  map[int]*xcur.Image
	flipX   bool
	flipY   bool
	mons    []hypr.Monitor
	monsAt  time.Time
}

type cursorStyle struct {
	Name   string  `json:"name"`
	Color  string  `json:"color"`
	Tag    bool    `json:"tag"`
	Speed  string  `json:"speed"`
	Hidden bool    `json:"hidden,omitempty"`
	X      float64 `json:"x,omitempty"` // where it last rested, so it comes back there
	Y      float64 `json:"y,omitempty"`
	color  rgb
}

type ghostSurf struct {
	ls    *wl.LayerSurface
	scale int
}

type spriteKey struct {
	scale int
	color rgb
}

type ramp struct {
	from, to float64
	start    time.Time
	dur      time.Duration
}

// frame is everything that decides what one rendered frame looks like.
type frame struct {
	x, y       float64 // tip, layout
	alpha      float64
	tilt       float64
	squash     float64 // scale about the tip (1 = none)
	glow       float64 // glow strength
	ripple     float64 // 0 = none, else progress in (0, 1)
	rx, ry     float64 // ripple centre, relative to the tip
	tag        string
	tagAlpha   float64
	tagX, tagY float64 // lag offset
	busy       float64 // 0..1, the tag's dot breathes
	dots       bool
	pulse      float64
	trail      [][3]float64 // relative to the tip: x, y, age
	animating  bool
}

var speeds = map[string]float64{"instant": 0, "fast": 0.6, "normal": 1, "smooth": 1.5}

func cursorConfigPath() string { return filepath.Join(CacheDir(), "cursor.json") }

func newGhost(d *Daemon) *Ghost {
	g := &Ghost{d: d, wake: make(chan struct{}, 1), images: map[int]*xcur.Image{}, sprites: map[spriteKey]*sprite{},
		surfs: map[string]*ghostSurf{}, idle: 30 * time.Second, speed: 1,
		style: cursorStyle{Name: "Claude", Tag: true, Speed: "normal", color: defaultAccent}}
	if b, err := os.ReadFile(cursorConfigPath()); err == nil {
		var cfg map[string]any
		if json.Unmarshal(b, &cfg) == nil {
			if v, ok := cfg["speed"].(string); ok {
				if s, ok := speeds[v]; ok {
					g.speed, g.style.Speed = s, v
				}
			}
			if v, ok := cfg["name"].(string); ok && v != "" {
				g.style.Name = v
			}
			if v, ok := cfg["color"].(string); ok {
				if c, err := parseColor(v); err == nil {
					g.style.color, g.style.Color = c, v
				}
			}
			if v, ok := cfg["tag"].(bool); ok {
				g.style.Tag = v
			}
			if v, ok := cfg["hidden"].(bool); ok {
				g.style.Hidden, g.disabled = v, v
			}
			g.style.X, _ = cfg["x"].(float64)
			g.style.Y, _ = cfg["y"].(float64)
		}
	}
	go g.loop()
	return g
}

// appear puts the cursor on screen when the daemon starts: it is always
// there, idle or not, where it last rested (or beside the user's pointer).
func (g *Ghost) appear() {
	g.mu.Lock()
	hidden, x, y := g.disabled, g.style.X, g.style.Y
	g.mu.Unlock()
	if hidden {
		return
	}
	ms, err := hypr.Monitors()
	if err != nil {
		return
	}
	if _, ok := hypr.MonitorAt(ms, x, y); !ok || (x == 0 && y == 0) {
		px, py, err := hypr.CursorPos()
		if err != nil {
			return
		}
		x, y = px+90, py+70
		if _, ok := hypr.MonitorAt(ms, x, y); !ok {
			x, y = px-90, py-70
		}
	}
	g.mu.Lock()
	g.have, g.x, g.y, g.cx, g.cy = true, x, y, x, y
	g.lastAct = time.Time{}
	g.mu.Unlock()
	g.startGlide(x, y)
}

func (g *Ghost) saveStyle() error {
	g.style.Color = g.style.color.hex()
	b, _ := json.Marshal(g.style)
	return os.WriteFile(cursorConfigPath(), b, 0o600)
}

func (g *Ghost) poke() {
	select {
	case g.wake <- struct{}{}:
	default:
	}
}

// ---------------------------------------------------------------- the glide

// glide is one reach toward a target. The tip follows a quintic from where
// it is, at the speed it already has (a new target mid-flight bends the path
// instead of stopping dead), along a slight arc like a wrist movement, then
// swings a few pixels past the target and settles back.
type glide struct {
	x0, y0   float64
	vx0, vy0 float64 // px/s when it started
	x1, y1   float64
	px, py   float64 // unit perpendicular: the side the arc bows to
	dx, dy   float64 // unit direction
	arc      float64 // px
	over     float64 // px
	start    time.Time
	approach time.Duration // until the tip reaches the target
	dur      time.Duration // until it has settled
	arrived  chan struct{}
}

func newGlide(x0, y0, vx, vy, x1, y1 float64, approach time.Duration, now time.Time) *glide {
	gl := &glide{x0: x0, y0: y0, vx0: vx, vy0: vy, x1: x1, y1: y1, start: now, approach: approach,
		dur: time.Duration(float64(approach) / approachShare), arrived: make(chan struct{})}
	d := math.Hypot(x1-x0, y1-y0)
	if d > 0 {
		gl.dx, gl.dy = (x1-x0)/d, (y1-y0)/d
		gl.px, gl.py = -gl.dy, gl.dx
		if gl.py > 0 || (math.Abs(gl.py) < 0.2 && gl.px > 0) {
			gl.px, gl.py = -gl.px, -gl.py // bow upward, or left on vertical moves
		}
	}
	if d > 60 {
		gl.arc = math.Min(0.06*d, 32)
	}
	if d > 90 {
		gl.over = math.Min(0.016*d, 9)
	}
	return gl
}

// quintic runs from p0 at velocity v0 (per unit s) to p1, arriving at rest
// with no acceleration.
func quintic(p0, p1, v0, s float64) float64 {
	d := p1 - p0
	return p0 + v0*s + (10*d-6*v0)*s*s*s + (-15*d+8*v0)*s*s*s*s + (6*d-3*v0)*s*s*s*s*s
}

// at is where the tip is el into the glide.
func (gl *glide) at(el time.Duration) (x, y float64, arrived, done bool) {
	if el >= gl.dur {
		return gl.x1, gl.y1, true, true
	}
	t := el.Seconds() / gl.dur.Seconds()
	s := math.Min(1, el.Seconds()/gl.approach.Seconds())
	T := gl.approach.Seconds()
	x = quintic(gl.x0, gl.x1, gl.vx0*T, s)
	y = quintic(gl.y0, gl.y1, gl.vy0*T, s)
	bow := gl.arc * math.Sin(math.Pi*minJerk(s))
	x += gl.px * bow
	y += gl.py * bow
	if w := (t - overStart) / (1 - overStart); w > 0 && w < 1 {
		sw := math.Sin(math.Pi * w)
		x += gl.dx * gl.over * sw * sw
		y += gl.dy * gl.over * sw * sw
	}
	return x, y, s >= 1, false
}

// ---------------------------------------------------------------- what callers use

// duration follows Fitts's law: long moves take longer, but not linearly.
func (g *Ghost) duration(dist float64) time.Duration {
	if dist < 2 || g.speed == 0 {
		return 0
	}
	ms := math.Min(50+40*math.Log2(1+dist/50), 280) * g.speed
	return time.Duration(ms * float64(time.Millisecond))
}

// show makes the cursor visible, fading in. Caller holds g.mu.
func (g *Ghost) show(now time.Time) {
	g.lastAct = now
	if g.want {
		return
	}
	g.want = true
	g.fade = &ramp{from: g.alpha, to: 1, start: now, dur: fadeIn}
}

// glideTo moves the cursor to (x, y) and returns when the tip has reached it
// on screen, so a click lands the moment the cursor gets there.
func (g *Ghost) glideTo(x, y float64) {
	arrived, dur := g.startGlide(x, y)
	if arrived != nil {
		select {
		case <-arrived:
		case <-time.After(dur + 400*time.Millisecond):
		}
	}
}

// startGlide sets the glide going and returns its arrival signal (nil when
// there is nothing to animate).
func (g *Ghost) startGlide(x, y float64) (chan struct{}, time.Duration) {
	x, y = onMonitors(x, y)
	g.mu.Lock()
	now := time.Now()
	if g.disabled {
		g.x, g.y, g.have = x, y, true
		g.mu.Unlock()
		return nil, 0
	}
	from := [2]float64{g.cx, g.cy}
	vx, vy := g.vx, g.vy
	if !g.have || !g.want {
		if !g.have {
			from = [2]float64{x, y}
			if px, py, err := hypr.CursorPos(); err == nil {
				from = [2]float64{px, py} // first appearance: it leaves from the user's pointer
			}
		} else {
			from = [2]float64{g.x, g.y}
		}
		g.cx, g.cy, g.lx, g.ly = from[0], from[1], from[0], from[1]
		vx, vy = 0, 0
		g.lastT = time.Time{}
	}
	g.show(now)
	if g.gl != nil {
		if g.gl.arrived != nil {
			close(g.gl.arrived)
		}
		g.gl = nil
	}
	dur := g.duration(math.Hypot(x-from[0], y-from[1]))
	g.x, g.y, g.have = x, y, true
	var arrived chan struct{}
	if dur > 0 {
		g.gl = newGlide(from[0], from[1], vx, vy, x, y, dur, now)
		arrived = g.gl.arrived
	} else {
		g.cx, g.cy = x, y
	}
	g.mu.Unlock()
	g.poke()
	return arrived, dur
}

// jump moves with no glide (it follows a drag in lockstep); the physics
// still sees the motion, so the arrow leans and trails while dragging.
func (g *Ghost) jump(x, y float64) {
	x, y = onMonitors(x, y)
	g.mu.Lock()
	if g.gl != nil {
		if g.gl.arrived != nil {
			close(g.gl.arrived)
		}
		g.gl = nil
	}
	g.x, g.y, g.cx, g.cy, g.have = x, y, x, y, true
	g.show(time.Now())
	g.mu.Unlock()
	g.poke()
}

// click plays the press: the arrow squashes, a ring and a pulse spread from
// the point that was clicked. It never holds the caller up.
func (g *Ghost) click() {
	g.mu.Lock()
	g.clickAt = time.Now()
	g.clickX, g.clickY = g.x, g.y
	g.lastAct = g.clickAt
	g.mu.Unlock()
	g.poke()
}

// act shows what the agent is doing in the tag ("typing", "clicking",
// "waiting for you") until done is called.
func (g *Ghost) act(activity string) {
	g.mu.Lock()
	now := time.Now()
	if g.disabled {
		g.mu.Unlock()
		return
	}
	if g.have {
		g.show(now)
	}
	if g.activity == "" || now.After(g.until.Add(labelOut)) {
		g.actAt = now
	}
	g.activity = activity
	g.until = now.Add(10 * time.Minute)
	g.lastAct = now
	g.mu.Unlock()
	g.poke()
}

// done keeps the current activity up for hold more (so even a 4 ms burst of
// keys is seen), after which the tag goes back to the note or to idle.
func (g *Ghost) done(hold time.Duration) {
	g.mu.Lock()
	now := time.Now()
	g.until = now.Add(hold)
	g.lastAct = now
	if g.have && !g.disabled && (math.Abs(g.style.X-g.x) > 1 || math.Abs(g.style.Y-g.y) > 1) {
		g.style.X, g.style.Y = g.x, g.y
		_ = g.saveStyle()
	}
	g.mu.Unlock()
	g.poke()
}

// busy is act then done(min).
func (g *Ghost) busy(activity string, min time.Duration) {
	g.act(activity)
	g.done(min)
}

// lastLabel is the activity on show (for putting it back after a wait).
func (g *Ghost) lastLabel() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.activity == "" || g.activity == "waiting for you" {
		return "working"
	}
	return g.activity
}

// say sets a note the tag shows whenever the agent is between actions
// ("reading the logs"); "" clears it. It lasts ten minutes.
func (g *Ghost) say(note string) {
	g.mu.Lock()
	g.note = clip(strings.TrimSpace(note), 40)
	g.noteTill = time.Now().Add(10 * time.Minute)
	g.lastAct = time.Now()
	g.mu.Unlock()
	g.poke()
}

// hide fades the cursor out; the surfaces go once it is invisible.
func (g *Ghost) hide() {
	g.mu.Lock()
	if g.want {
		g.want = false
		g.fade = &ramp{from: g.alpha, to: 0, start: time.Now(), dur: fadeOut}
	}
	g.mu.Unlock()
	g.poke()
}

// hideNow drops the surfaces immediately (daemon shutdown).
func (g *Ghost) hideNow() {
	g.mu.Lock()
	g.want, g.alpha, g.fade = false, 0, nil
	g.mu.Unlock()
	g.poke()
	time.Sleep(60 * time.Millisecond)
}

// lost is called when the Wayland session died: the surfaces went with it.
func (g *Ghost) lost() {
	g.mu.Lock()
	g.lostSurf = true
	g.want, g.alpha = false, 0
	g.mu.Unlock()
}

func (g *Ghost) setSpeed(name string) error {
	s, ok := speeds[name]
	if !ok {
		return fmt.Errorf("speed is one of instant, fast, normal, smooth")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.speed, g.style.Speed = s, name
	return g.saveStyle()
}

func (g *Ghost) setStyle(fn func(st *cursorStyle) error) error {
	g.mu.Lock()
	if err := fn(&g.style); err != nil {
		g.mu.Unlock()
		return err
	}
	err := g.saveStyle()
	g.lastT = time.Time{}
	g.mu.Unlock()
	g.poke()
	return err
}

func (g *Ghost) speedName() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	for n, v := range speeds {
		if v == g.speed {
			return n
		}
	}
	return fmt.Sprintf("%.1f×", g.speed)
}

func (g *Ghost) styleLine() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	tag := "tag " + strconvQuote(g.style.Name)
	if !g.style.Tag {
		tag = "no tag at rest (" + strconvQuote(g.style.Name) + " shows while busy)"
	}
	return fmt.Sprintf("%s · colour %s · speed %s", tag, g.style.color.hex(), g.style.Speed)
}

func (g *Ghost) styleName() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.style.Name
}

func strconvQuote(s string) string { return "\"" + s + "\"" }

func (g *Ghost) status() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case g.disabled:
		return "off (pc cursor show to turn it back on)"
	case g.want:
		on := ""
		if g.activity != "" {
			on = " (" + g.activity + ")"
		}
		if m, ok := hypr.MonitorAt(g.mons, g.x, g.y); ok {
			on += " on " + m.Name
		}
		return fmt.Sprintf("visible at %.0f,%.0f%s", g.x, g.y, on)
	case g.have:
		return fmt.Sprintf("resting (last at %.0f,%.0f; reappears on the next action)", g.x, g.y)
	}
	return "not shown yet (appears on the first action)"
}

// position is where the agent last pointed.
func (g *Ghost) position() (float64, float64, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.x, g.y, g.have
}

// ---------------------------------------------------------------- one frame

func minJerk(t float64) float64 { return t * t * t * (10 + t*(-15+6*t)) }

func smoothstep(t float64) float64 { return t * t * (3 - 2*t) }

func easeOut(t float64) float64 { return 1 - math.Pow(1-t, 3) }

// spring advances a damped spring pulling x toward 0.
func spring(x, v *float64, omega, zeta, dt float64) {
	a := -omega*omega*(*x) - 2*zeta*omega*(*v)
	*v += a * dt
	*x += *v * dt
}

// frameAt advances every animation and the physics to now. Caller holds g.mu.
func (g *Ghost) frameAt(now time.Time) frame {
	f := frame{squash: 1}
	if gl := g.gl; gl != nil {
		x, y, arrived, done := gl.at(now.Sub(gl.start))
		g.cx, g.cy = x, y
		if arrived && gl.arrived != nil {
			close(gl.arrived)
			gl.arrived = nil
		}
		if done {
			g.gl = nil
		}
		f.animating = true
	}

	// physics: velocity from the path actually drawn, a tilt that leans the
	// arrow into the motion and wobbles when it stops, a tag that is left
	// behind and pulled back on a spring
	dt := now.Sub(g.lastT).Seconds()
	if g.lastT.IsZero() || dt > 0.1 {
		g.vx, g.vy, g.trail, g.trailT = 0, 0, nil, nil
		g.lx, g.ly = g.cx, g.cy
		dt = 0
	}
	if dt > 0 {
		jx, jy := g.cx-g.lx, g.cy-g.ly
		if math.Hypot(jx, jy) > 500 { // a teleport, not a motion
			g.vx, g.vy, g.trail, g.trailT = 0, 0, nil, nil
		} else {
			g.vx = 0.4*g.vx + 0.6*jx/dt
			g.vy = 0.4*g.vy + 0.6*jy/dt
			g.tagX = math.Max(-tagLag, math.Min(tagLag, g.tagX-jx))
			g.tagY = math.Max(-tagLag, math.Min(tagLag, g.tagY-jy))
		}
		target := math.Max(-0.22, math.Min(0.22, g.vx*0.0001))
		for left := dt; left > 0; left -= 1.0 / 240 {
			step := math.Min(left, 1.0/240)
			off := g.tilt - target
			spring(&off, &g.tiltV, 24, 0.42, step)
			g.tilt = off + target
			spring(&g.tagX, &g.tagVX, 20, 0.78, step)
			spring(&g.tagY, &g.tagVY, 20, 0.78, step)
		}
		if g.gl == nil && math.Hypot(jx, jy) < 0.01 {
			g.vx, g.vy = g.vx*0.5, g.vy*0.5
		}
	}
	g.lx, g.ly, g.lastT = g.cx, g.cy, now
	if math.Hypot(g.vx, g.vy) > 140 {
		g.trail = append(g.trail, trailPt{x: g.cx, y: g.cy})
		g.trailT = append(g.trailT, now)
	}
	for len(g.trailT) > 0 && now.Sub(g.trailT[0]) > trailFor {
		g.trail, g.trailT = g.trail[1:], g.trailT[1:]
	}
	if len(g.trail) > 0 {
		for i, p := range g.trail {
			age := now.Sub(g.trailT[i]).Seconds() / trailFor.Seconds()
			f.trail = append(f.trail, [3]float64{p.x - g.cx, p.y - g.cy, age})
		}
		f.trail = append(f.trail, [3]float64{0, 0, 0})
		f.animating = true
	}
	if math.Abs(g.tilt) > 0.002 || math.Abs(g.tiltV) > 0.02 || math.Hypot(g.tagX, g.tagY) > 0.3 || math.Hypot(g.tagVX, g.tagVY) > 3 {
		f.animating = true
	}
	f.x, f.y, f.tilt, f.tagX, f.tagY = g.cx, g.cy, g.tilt, g.tagX, g.tagY

	if g.fade != nil {
		t := now.Sub(g.fade.start).Seconds() / g.fade.dur.Seconds()
		if t >= 1 {
			g.alpha = g.fade.to
			g.fade = nil
		} else {
			g.alpha = g.fade.from + (g.fade.to-g.fade.from)*smoothstep(math.Max(t, 0))
			f.animating = true
		}
	}
	f.alpha = g.alpha
	f.glow = 0.5

	if !g.clickAt.IsZero() {
		el := now.Sub(g.clickAt)
		if p := el.Seconds() / rippleOn.Seconds(); p < 1 {
			f.ripple = math.Max(p, 0.001)
			f.rx, f.ry = g.clickX-g.cx, g.clickY-g.cy
			f.glow += 0.5 * (1 - p) * (1 - p)
			f.animating = true
		} else {
			g.clickAt = time.Time{}
		}
		if p := el.Seconds() / pressOn.Seconds(); p < 1 {
			if p < 0.2 {
				f.squash = 1 - 0.14*smoothstep(p/0.2)
			} else {
				q := (p - 0.2) / 0.8
				f.squash = 1 - 0.14*math.Exp(-5*q)*math.Cos(2.5*math.Pi*q)
			}
		}
	}

	label, ongoing := "", false
	if g.activity != "" {
		in := math.Min(1, now.Sub(g.actAt).Seconds()/labelIn.Seconds())
		out := 1.0
		if now.After(g.until) {
			out = 1 - now.Sub(g.until).Seconds()/labelOut.Seconds()
		}
		if out <= 0 {
			g.activity = ""
		} else {
			label = g.activity
			ongoing = now.Before(g.until.Add(-200 * time.Millisecond))
			f.busy = smoothstep(math.Min(in, out))
			f.pulse = math.Mod(now.Sub(g.actAt).Seconds()*1.4, 1)
			f.dots = ongoing && (strings.HasSuffix(label, "ing") || strings.HasSuffix(label, "you"))
			f.glow += 0.25 * f.busy * (0.5 + 0.5*math.Sin(f.pulse*2*math.Pi))
			f.animating = true
		}
	}
	if label == "" {
		if g.note != "" && now.Before(g.noteTill) {
			label = g.note
		} else {
			label = "idle"
		}
	}
	// idle: a little dimmer, never gone
	quiet := now.Sub(g.lastAct)
	if g.activity == "" && g.gl == nil {
		if dim := (quiet - 3*time.Second).Seconds() / 0.6; dim > 0 {
			f.alpha *= 1 - 0.3*smoothstep(math.Min(1, dim))
			if dim < 1 {
				f.animating = true
			}
		} else if quiet > 2*time.Second {
			f.animating = true // keep frames coming until the dim has played
		}
	}
	if g.style.Tag || g.activity != "" {
		f.tag = clip(g.style.Name+" · "+label, 44)
		f.tagAlpha = f.alpha
		if !g.style.Tag {
			f.tagAlpha *= f.busy
		}
	}
	return f
}

// ---------------------------------------------------------------- the render loop

func (g *Ghost) loop() {
	tick := time.NewTicker(500 * time.Millisecond)
	for {
		select {
		case <-g.wake:
		case <-tick.C:
			// between actions nothing animates; wake for the idle dim and an
			// expiring note, which change the look with no action behind them
			g.mu.Lock()
			now := time.Now()
			wake := false
			if g.note != "" && now.After(g.noteTill) {
				g.note, wake = "", true
			}
			if q := now.Sub(g.lastAct); g.want && q > 2*time.Second && q < 5*time.Second {
				wake = true
			}
			g.mu.Unlock()
			if !wake {
				continue
			}
		}
		g.run()
	}
}

// run renders frames until nothing is animating.
func (g *Ghost) run() {
	for {
		g.mu.Lock()
		f := g.frameAt(time.Now())
		style := g.style
		lost := g.lostSurf
		g.lostSurf = false
		gone := !g.want && f.alpha <= 0.001 && g.fade == nil
		g.mu.Unlock()
		if lost {
			g.surfs = map[string]*ghostSurf{} // went down with the connection; nothing to destroy
		}
		if gone {
			g.dropSurfaces()
			return
		}
		next := g.render(f, style)
		if !f.animating {
			return
		}
		if next != nil {
			select {
			case <-next:
			case <-time.After(50 * time.Millisecond):
			}
		} else {
			time.Sleep(12 * time.Millisecond)
		}
	}
}

func (g *Ghost) dropSurfaces() {
	for name, gs := range g.surfs {
		gs.ls.Destroy()
		delete(g.surfs, name)
	}
}

func (g *Ghost) monitors() []hypr.Monitor {
	if time.Since(g.monsAt) < 2*time.Second && g.mons != nil {
		return g.mons
	}
	if ms, err := hypr.Monitors(); err == nil {
		var on []hypr.Monitor
		for _, m := range ms {
			if !m.Disabled {
				on = append(on, m)
			}
		}
		g.mu.Lock()
		g.mons, g.monsAt = on, time.Now()
		g.mu.Unlock()
	}
	return g.mons
}

func (g *Ghost) sprite(scale int, color rgb) *sprite {
	k := spriteKey{scale, color}
	if sp := g.sprites[k]; sp != nil {
		return sp
	}
	img := g.images[scale]
	if img == nil {
		var err error
		img, err = xcur.Load(xcur.Theme(), xcur.Size()*scale)
		if err != nil {
			log.Printf("cursor theme: %v", err)
			return nil
		}
		g.images[scale] = img
	}
	if len(g.sprites) > 8 {
		g.sprites = map[spriteKey]*sprite{}
	}
	sp := buildSprite(img, scale, color)
	g.sprites[k] = sp
	return sp
}

// onMonitors keeps a point on some monitor: the agent's cursor is only any
// use where it can be seen. A point off every monitor moves to the nearest
// spot on the nearest one.
func onMonitors(x, y float64) (float64, float64) {
	ms, err := hypr.Monitors()
	if err != nil || len(ms) == 0 {
		return x, y
	}
	if _, ok := hypr.MonitorAt(ms, x, y); ok {
		return x, y
	}
	bx, by, best := x, y, math.Inf(1)
	for _, m := range ms {
		if m.Disabled {
			continue
		}
		b := m.Box()
		cx := math.Max(b.X+8, math.Min(b.X+b.W-24, x))
		cy := math.Max(b.Y+8, math.Min(b.Y+b.H-24, y))
		if d := math.Hypot(cx-x, cy-y); d < best {
			bx, by, best = cx, cy, d
		}
	}
	return bx, by
}

func onScreen(ms []hypr.Monitor, x, y float64) bool {
	_, ok := hypr.MonitorAt(ms, x, y)
	return ok
}

// layout decides where the tag goes (beside the tip, flipped away from
// screen edges with a little hysteresis) and returns the box, relative to
// the tip in logical px, that this frame paints.
func (g *Ghost) layout(f frame, ms []hypr.Monitor) (tagX, tagY float64, box hypr.Rect) {
	reach := 34.0
	x0, y0, x1, y1 := -reach, -reach, reach, reach
	grow := func(ax, ay, bx, by float64) {
		x0, y0, x1, y1 = math.Min(x0, ax), math.Min(y0, ay), math.Max(x1, bx), math.Max(y1, by)
	}
	if f.tag != "" && f.tagAlpha > 0.01 {
		w, h := tagSize(f.tag, f.dots)
		right := onScreen(ms, f.x+18+w+6, f.y) && onScreen(ms, f.x+18+w+6, f.y+40)
		if g.flipX && !onScreen(ms, f.x+18+w+40, f.y) {
			right = false
		}
		g.flipX = !right
		below := onScreen(ms, f.x, f.y+22+h+6)
		if g.flipY && !onScreen(ms, f.x, f.y+22+h+30) {
			below = false
		}
		g.flipY = !below
		tagX, tagY = 16, 21
		if g.flipX {
			tagX = -10 - w
		}
		if g.flipY {
			tagY = -10 - h
		}
		tagX += f.tagX
		tagY += f.tagY
		grow(tagX-2, tagY-2, tagX+w+2, tagY+h+2)
	}
	for _, p := range f.trail {
		grow(p[0]-6, p[1]-6, p[0]+18, p[1]+22)
	}
	if f.ripple > 0 {
		grow(f.rx-34, f.ry-34, f.rx+34, f.ry+34)
	}
	x0, y0 = math.Max(x0, -hotX), math.Max(y0, -hotY)
	x1, y1 = math.Min(x1, surfW-hotX), math.Min(y1, surfH-hotY)
	return tagX, tagY, hypr.Rect{X: x0, Y: y0, W: x1 - x0, H: y1 - y0}
}

// render puts one frame on every monitor it touches (a cursor at a monitor
// edge is drawn on both sides of the seam) and returns the compositor's
// "ready for the next frame" signal from the monitor under the tip. Only the
// render loop calls it.
func (g *Ghost) render(f frame, style cursorStyle) <-chan struct{} {
	ms := g.monitors()
	home, ok := hypr.MonitorAt(ms, f.x, f.y)
	if !ok {
		return nil
	}
	tagX, tagY, box := g.layout(f, ms)
	lay := hypr.Rect{X: math.Round(f.x) + box.X, Y: math.Round(f.y) + box.Y, W: box.W, H: box.H}
	want := map[string]hypr.Monitor{}
	for _, m := range ms {
		if _, hit := m.Box().Intersect(lay); hit || m.Name == home.Name {
			want[m.Name] = m
		}
	}
	for name, gs := range g.surfs {
		m, keep := want[name]
		if !keep || gs.ls.Closed() || gs.scale != max(1, int(math.Ceil(m.Scale))) {
			gs.ls.Destroy()
			delete(g.surfs, name)
		}
	}
	var next <-chan struct{}
	for name, m := range want {
		scale := max(1, int(math.Ceil(m.Scale)))
		top := int(math.Round(f.y)) - m.Y - hotY
		left := int(math.Round(f.x)) - m.X - hotX
		gs := g.surfs[name]
		if gs == nil {
			s, err := g.d.session()
			if err != nil || s.shell == nil || s.comp == nil || s.shm == nil {
				return nil
			}
			out, err := s.outs.ByName(name)
			if err != nil {
				continue
			}
			ls, err := wl.NewLayerSurface(s.c, s.comp, s.shm, s.shell, out, ghostNamespace, surfW, surfH, scale, top, left)
			if err != nil {
				log.Printf("agent cursor: %v", err)
				continue
			}
			gs = &ghostSurf{ls: ls, scale: scale}
			g.surfs[name] = gs
		}
		sp := g.sprite(scale, style.color)
		ch, err := gs.ls.Draw(func(pix []byte, stride, w, h int) image.Rectangle {
			c := &canvas{pix: pix, stride: stride, w: w, h: h, s: float64(scale)}
			g.paint(c, f, sp, style.color, tagX, tagY)
			return c.dirty
		}, top, left, true)
		if err != nil {
			continue
		}
		if name == home.Name {
			next = ch
		}
	}
	return next
}

// paint draws one frame: trail, ripple, the tag, then the arrow.
func (g *Ghost) paint(c *canvas, f frame, sp *sprite, accent rgb, tagX, tagY float64) {
	s := c.s
	tx, ty := hotX*s, hotY*s
	if len(f.trail) > 1 && sp != nil {
		pts := make([][3]float64, len(f.trail))
		for i, p := range f.trail {
			pts[i] = [3]float64{tx + (p[0]+sp.cx)*s, ty + (p[1]+sp.cy)*s, p[2]}
		}
		c.trail(pts, 7*s, accent, 0.42*f.alpha)
	}
	if f.ripple > 0 {
		p := f.ripple
		rx, ry := tx+f.rx*s, ty+f.ry*s
		c.disc(rx, ry, (4+14*easeOut(math.Min(1, p*2.2)))*s, accent, 0.32*math.Pow(1-p, 2)*f.alpha)
		c.ring(rx, ry, (5+25*easeOut(p))*s, (2.6-1.4*p)*s, accent, 0.9*math.Pow(1-p, 1.4)*f.alpha)
	}
	if f.tag != "" {
		c.tag(tx+tagX*s, ty+tagY*s, f.tag, f.dots, f.tagAlpha, f.pulse, f.busy, accent)
	}
	if sp != nil { // over the tag: the tip must never be hidden
		c.arrow(sp, tx, ty, f.tilt, f.squash, f.alpha, f.glow, accent)
	}
}
