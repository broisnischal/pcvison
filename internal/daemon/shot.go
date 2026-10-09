package daemon

import (
	"errors"
	"fmt"
	"image"
	"image/color"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"pc/internal/hypr"
	"pc/internal/img"
	"pc/internal/rpc"
	"pc/internal/wl"
)

// Shot remembers how one screenshot maps back to the desktop, so the agent can
// click on what it saw by passing the pixel coordinates it read off the image.
type Shot struct {
	ID     string    `json:"id"`
	At     time.Time `json:"at"`
	Region hypr.Rect `json:"region"` // layout coordinates the image covers
	W, H   int       `json:"-"`
	Path   string    `json:"path"`
	Label  string    `json:"label"`
	// Context says where both pointers are and which windows are open.
	Context string `json:"context,omitempty"`
}

// ToLayout maps image pixels to layout coordinates.
func (s *Shot) ToLayout(x, y float64) (float64, float64) {
	return s.Region.X + x*s.Region.W/float64(s.W), s.Region.Y + y*s.Region.H/float64(s.H)
}

// FromLayout maps a layout point into image pixels.
func (s *Shot) FromLayout(x, y float64) (float64, float64) {
	return (x - s.Region.X) * float64(s.W) / s.Region.W, (y - s.Region.Y) * float64(s.H) / s.Region.H
}

type Shots struct {
	mu   sync.Mutex
	n    int
	list []*Shot
	dir  string
}

func newShots() *Shots {
	dir := filepath.Join(CacheDir(), "shots")
	_ = os.MkdirAll(dir, 0o700)
	return &Shots{dir: dir}
}

func (s *Shots) add(sh *Shot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	sh.ID = "s" + strconv.Itoa(s.n)
	s.list = append(s.list, sh)
	if len(s.list) > 40 {
		old := s.list[0]
		s.list = s.list[1:]
		_ = os.Remove(old.Path)
	}
}

func (s *Shots) get(id string) (*Shot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.list) == 0 {
		return nil, errors.New("no screenshot to measure against yet: take one first (pc shot), or pass --abs for layout coordinates")
	}
	if id == "" {
		return s.list[len(s.list)-1], nil
	}
	for _, sh := range s.list {
		if sh.ID == id {
			return sh, nil
		}
	}
	return nil, fmt.Errorf("no screenshot %q (have %s..%s)", id, s.list[0].ID, s.list[len(s.list)-1].ID)
}

// target is what to capture.
type target struct {
	monitor string
	window  string
	region  string
	all     bool
}

func targetFrom(a Args) target {
	return target{monitor: a.Str("monitor"), window: a.Str("window"), region: a.Str("region"), all: a.Bool("all")}
}

// resolve turns a target into a layout rectangle and a label.
func (t target) resolve() (hypr.Rect, string, error) {
	ms, err := hypr.Monitors()
	if err != nil {
		return hypr.Rect{}, "", err
	}
	switch {
	case t.region != "":
		r, err := parseRegion(t.region)
		return r, "region " + r.String(), err
	case t.window != "":
		c, err := findWindow(t.window)
		if err != nil {
			return hypr.Rect{}, "", err
		}
		if c.Workspace.ID != activeWorkspaceOf(ms, c.Monitor) && c.Workspace.ID >= 0 {
			return hypr.Rect{}, "", fmt.Errorf("%s is on workspace %s, which is not on screen; focus it first (pc focus %q)", c.Class, c.Workspace.Name, c.Class)
		}
		return c.Box(), fmt.Sprintf("window %s %q", c.Class, clip(c.Title, 60)), nil
	case t.monitor != "":
		for _, m := range ms {
			if strings.EqualFold(m.Name, t.monitor) {
				return m.Box(), "monitor " + m.Name, nil
			}
		}
		var names []string
		for _, m := range ms {
			names = append(names, m.Name)
		}
		return hypr.Rect{}, "", fmt.Errorf("no monitor %q (have %s)", t.monitor, strings.Join(names, ", "))
	case t.all:
		return hypr.Layout(ms), "all monitors", nil
	}
	for _, m := range ms {
		if m.Focused {
			return m.Box(), "monitor " + m.Name + " (focused)", nil
		}
	}
	return hypr.Layout(ms), "all monitors", nil
}

func activeWorkspaceOf(ms []hypr.Monitor, id int) int {
	for _, m := range ms {
		if m.ID == id {
			return m.ActiveWorkspace.ID
		}
	}
	return -999
}

func parseRegion(s string) (hypr.Rect, error) {
	var r hypr.Rect
	s = strings.NewReplacer(",", " ", "x", " ", "X", " ").Replace(s)
	f := strings.Fields(s)
	if len(f) != 4 {
		return r, fmt.Errorf("region must look like \"X,Y WxH\"")
	}
	vals := make([]float64, 4)
	for i, v := range f {
		n, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return r, fmt.Errorf("bad region %q", s)
		}
		vals[i] = n
	}
	if vals[2] <= 0 || vals[3] <= 0 {
		return r, fmt.Errorf("region has no area")
	}
	return hypr.Rect{X: vals[0], Y: vals[1], W: vals[2], H: vals[3]}, nil
}

func budgetOf(detail string) int {
	if b, ok := img.Detail[detail]; ok {
		return b
	}
	return img.Detail["normal"]
}

// grab captures a layout rectangle across however many monitors it spans and
// scales it into one image no larger than the detail budget.
func (d *Daemon) grab(r hypr.Rect, budget int, cursor bool) (*image.RGBA, error) {
	s, err := d.session()
	if err != nil {
		return nil, err
	}
	if s.sc == nil {
		return nil, s.need("screencopy", false)
	}
	ms, err := hypr.Monitors()
	if err != nil {
		return nil, err
	}
	type part struct {
		raw  *wl.Raw
		area hypr.Rect
	}
	var parts []part
	maxScale := 1.0
	for _, m := range ms {
		if m.Disabled {
			continue
		}
		box := m.Box()
		in, ok := box.Intersect(r)
		if !ok {
			continue
		}
		out, err := s.outs.ByName(m.Name)
		if err != nil {
			return nil, err
		}
		var region *[4]int
		if in != box {
			region = &[4]int{int(in.X - box.X), int(in.Y - box.Y), int(math.Ceil(in.W)), int(math.Ceil(in.H))}
		}
		raw, err := s.sc.Capture(out, region, cursor)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", m.Name, err)
		}
		parts = append(parts, part{raw, in})
		maxScale = math.Max(maxScale, float64(raw.W)/in.W)
	}
	if len(parts) == 0 {
		return nil, fmt.Errorf("%s is not on any monitor", r)
	}
	nw, nh := int(math.Round(r.W*maxScale)), int(math.Round(r.H*maxScale))
	w, h := img.Fit(nw, nh, budget)
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	kx, ky := float64(w)/r.W, float64(h)/r.H
	for _, p := range parts {
		at := image.Rect(
			int(math.Round((p.area.X-r.X)*kx)), int(math.Round((p.area.Y-r.Y)*ky)),
			int(math.Round((p.area.X-r.X+p.area.W)*kx)), int(math.Round((p.area.Y-r.Y+p.area.H)*ky)))
		img.ScaleInto(dst, at.Intersect(dst.Rect), p.raw, image.Rect(0, 0, p.raw.W, p.raw.H))
	}
	return dst, nil
}

// snap captures, saves and registers a shot.
func (d *Daemon) snap(t target, detail string, cursor, grid bool) (*Shot, *image.RGBA, error) {
	if _, ok := img.Detail[detail]; !ok {
		detail = "normal"
	}
	r, label, err := t.resolve()
	if err != nil {
		return nil, nil, err
	}
	m, err := d.grab(r, budgetOf(detail), cursor)
	if err != nil {
		return nil, nil, err
	}
	return d.store(m, r, label, detail, grid)
}

func (d *Daemon) store(m *image.RGBA, r hypr.Rect, label, detail string, grid bool) (*Shot, *image.RGBA, error) {
	sh := &Shot{At: time.Now(), Region: r, W: m.Bounds().Dx(), H: m.Bounds().Dy(), Label: label}
	// the user's pointer is not in captures: draw it, unless the agent is
	// holding it right now (a hover), when it is not where the user left it
	px, py, perr := hypr.CursorPos()
	havePtr := perr == nil && !d.borrowing.Load()
	if havePtr {
		markUser(m, r, px, py)
	}
	sh.Context = d.shotContext(r, sh.W, sh.H, px, py, havePtr)
	if grid {
		drawGrid(m)
	}
	data, format, err := img.Encode(m, detail)
	if err != nil {
		return nil, nil, err
	}
	d.shots.add(sh)
	sh.Path = filepath.Join(d.shots.dir, fmt.Sprintf("%s-%s.%s", sh.ID, sh.At.Format("150405"), map[string]string{"jpeg": "jpg", "png": "png"}[format]))
	if err := os.WriteFile(sh.Path, data, 0o600); err != nil {
		return nil, nil, err
	}
	return sh, m, nil
}

func drawGrid(m *image.RGBA) {
	w, h := m.Bounds().Dx(), m.Bounds().Dy()
	step := 100
	if w > 1400 {
		step = 200
	}
	line := color.RGBA{255, 80, 80, 255}
	for x := step; x < w; x += step {
		for y := 0; y < h; y += 2 {
			m.SetRGBA(x, y, line)
		}
		img.Label(m, x+2, 12, strconv.Itoa(x), color.RGBA{255, 220, 220, 255})
	}
	for y := step; y < h; y += step {
		for x := 0; x < w; x += 2 {
			m.SetRGBA(x, y, line)
		}
		img.Label(m, 2, y-2, strconv.Itoa(y), color.RGBA{255, 220, 220, 255})
	}
}

func shotImage(sh *Shot, note string) rpc.Image {
	mime := "image/jpeg"
	if strings.HasSuffix(sh.Path, ".png") {
		mime = "image/png"
	}
	return rpc.Image{Path: sh.Path, Mime: mime, W: sh.W, H: sh.H, ShotID: sh.ID, Note: note}
}

func shotNote(sh *Shot) string {
	return fmt.Sprintf("%s · %s · %dx%d image of %s. Coordinates read off this image work as-is for click/move/drag (it is now the current shot)%s",
		sh.ID, sh.Label, sh.W, sh.H, sh.Region, sh.Context)
}

// signature is a tiny grey thumbnail, for "has the screen settled?".
func signature(m *image.RGBA) []uint8 {
	const sw, sh = 48, 27
	b := m.Bounds()
	out := make([]uint8, sw*sh)
	for j := 0; j < sh; j++ {
		for i := 0; i < sw; i++ {
			x := b.Min.X + i*b.Dx()/sw
			y := b.Min.Y + j*b.Dy()/sh
			o := m.PixOffset(x, y)
			out[j*sw+i] = uint8((uint32(m.Pix[o])*3 + uint32(m.Pix[o+1])*6 + uint32(m.Pix[o+2])) / 10)
		}
	}
	return out
}

func sigDiff(a, b []uint8) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 1
	}
	var sum int
	for i := range a {
		d := int(a[i]) - int(b[i])
		if d < 0 {
			d = -d
		}
		sum += d
	}
	return float64(sum) / float64(len(a)) / 255
}

// settled captures after an action, once the UI has stopped changing (or after
// maxWait), so the after-shot shows the result and not a half-drawn frame.
func (d *Daemon) settled(r hypr.Rect, label, detail string, maxWait time.Duration) (*Shot, error) {
	time.Sleep(50 * time.Millisecond)
	deadline := time.Now().Add(maxWait)
	var prev []uint8
	var m *image.RGBA
	var err error
	for {
		m, err = d.grab(r, budgetOf(detail), false)
		if err != nil {
			return nil, err
		}
		sig := signature(m)
		if prev != nil && sigDiff(prev, sig) < 0.002 || time.Now().After(deadline) {
			break
		}
		prev = sig
		time.Sleep(45 * time.Millisecond)
	}
	sh, _, err := d.store(m, r, label, detail, false)
	return sh, err
}

// afterShot is the "look" that follows an action: the monitor where it happened.
func (d *Daemon) afterShot(a Args, x, y float64) (*Shot, error) {
	detail := a.Str("detail")
	if detail == "" {
		detail = "normal"
	}
	ms, err := hypr.Monitors()
	if err != nil {
		return nil, err
	}
	if m, ok := hypr.MonitorAt(ms, x, y); ok {
		return d.settled(m.Box(), "after · monitor "+m.Name, detail, 900*time.Millisecond)
	}
	return d.settled(hypr.Layout(ms), "after · all monitors", detail, 900*time.Millisecond)
}

func init() {
	register("shot", func(d *Daemon, a Args) (*rpc.Response, error) {
		detail := a.Str("detail")
		if detail == "" {
			detail = "normal"
		}
		t := targetFrom(a)
		if t.window == "active" {
			if c, err := hypr.ActiveWindow(); err == nil && c.Address != "" {
				t.window = c.Address
			}
		}
		sh, _, err := d.snap(t, detail, a.Bool("cursor"), a.Bool("grid"))
		if err != nil {
			return nil, err
		}
		return &rpc.Response{Text: shotNote(sh), Images: []rpc.Image{shotImage(sh, shotNote(sh))}, Data: sh}, nil
	})
}

// clientsOnScreen lists mapped windows on visible workspaces, top-most first.
func clientsOnScreen() ([]hypr.Client, error) {
	ms, err := hypr.Monitors()
	if err != nil {
		return nil, err
	}
	visible := map[int]bool{}
	for _, m := range ms {
		visible[m.ActiveWorkspace.ID] = true
		if m.SpecialWorkspace.ID != 0 {
			visible[m.SpecialWorkspace.ID] = true
		}
	}
	cs, err := hypr.Clients()
	if err != nil {
		return nil, err
	}
	var out []hypr.Client
	for _, c := range cs {
		if c.Mapped && !c.Hidden && (visible[c.Workspace.ID] || c.Pinned) {
			out = append(out, c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if (a.Workspace.ID < 0) != (b.Workspace.ID < 0) { // special workspaces sit on top
			return a.Workspace.ID < 0
		}
		if a.Pinned != b.Pinned { // pinned floating windows are drawn over everything, fullscreen included
			return a.Pinned
		}
		if a.Fullscreen != b.Fullscreen {
			return a.Fullscreen > b.Fullscreen
		}
		if a.Floating != b.Floating {
			return a.Floating
		}
		return a.FocusHistoryID < b.FocusHistoryID
	})
	return out, nil
}

// windowAt guesses the top-most window under a layout point.
func windowAt(x, y float64) (hypr.Client, bool) {
	cs, err := clientsOnScreen()
	if err != nil {
		return hypr.Client{}, false
	}
	for _, c := range cs {
		if c.Box().Contains(x, y) {
			return c, true
		}
	}
	return hypr.Client{}, false
}

// findWindow resolves "active", an address, or a class/title substring.
func findWindow(q string) (hypr.Client, error) {
	cs, err := hypr.Clients()
	if err != nil {
		return hypr.Client{}, err
	}
	low := strings.ToLower(strings.TrimSpace(q))
	if low == "active" || low == "focused" || low == "current" {
		c, err := hypr.ActiveWindow()
		if err != nil || c.Address == "" {
			return hypr.Client{}, errors.New("no window has focus")
		}
		return c, nil
	}
	for _, c := range cs {
		if c.Address == q || c.Address == "0x"+strings.TrimPrefix(low, "0x") {
			return c, nil
		}
	}
	var hits []hypr.Client
	for _, c := range cs {
		if !c.Mapped {
			continue
		}
		if strings.EqualFold(c.Class, q) || strings.Contains(strings.ToLower(c.Class), low) ||
			strings.Contains(strings.ToLower(c.Title), low) || strings.Contains(strings.ToLower(c.InitialClass), low) {
			hits = append(hits, c)
		}
	}
	if len(hits) == 0 {
		var names []string
		for _, c := range cs {
			if c.Mapped {
				names = append(names, c.Class+": "+clip(c.Title, 30))
			}
		}
		return hypr.Client{}, fmt.Errorf("no window matches %q. Open: %s", q, strings.Join(names, " | "))
	}
	sort.SliceStable(hits, func(i, j int) bool {
		ei := strings.EqualFold(hits[i].Class, q)
		ej := strings.EqualFold(hits[j].Class, q)
		if ei != ej {
			return ei
		}
		return hits[i].FocusHistoryID < hits[j].FocusHistoryID
	})
	return hits[0], nil
}
