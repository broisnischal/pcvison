package daemon

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"hash/maphash"
	"image"
	"image/jpeg"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"pc/internal/hypr"
	"pc/internal/img"
	"pc/internal/rpc"
)

// Watch keeps the last few minutes of the user's real screen in a ring on
// disk, plus one line of metadata per tick (which window had focus). The
// cheap read is the text timeline; pixels are only spent when asked for.
// Identical frames are stored once, so an idle screen costs no disk. It is
// strictly opt-in: nothing starts it except `watch start`, and a daemon
// restart only resumes a watch the user had running.
type Watch struct {
	d    *Daemon
	mu   sync.Mutex
	cfg  watchCfg
	stop chan struct{}
	beat watchBeat
	seed maphash.Seed
}

type watchCfg struct {
	Running bool      `json:"running"`
	FPS     float64   `json:"fps"`
	Seconds float64   `json:"seconds"`
	Detail  string    `json:"detail"`
	Monitor string    `json:"monitor,omitempty"`
	MaxMB   float64   `json:"max_mb"`
	Started time.Time `json:"started"`
}

type watchBeat struct {
	At       time.Time
	Healthy  bool
	Fails    int
	Reason   string
	Captured int
	Restarts int
}

type tick struct {
	T     float64 `json:"t"`
	F     string  `json:"f"`
	New   bool    `json:"new"`
	App   string  `json:"app,omitempty"`
	Title string  `json:"title,omitempty"`
	WS    string  `json:"ws,omitempty"`
}

func watchDir() string  { return filepath.Join(CacheDir(), "watch") }
func frameDir() string  { return filepath.Join(watchDir(), "frames") }
func indexPath() string { return filepath.Join(watchDir(), "index.jsonl") }
func cfgPath() string   { return filepath.Join(watchDir(), "config.json") }

func newWatch(d *Daemon) *Watch {
	w := &Watch{d: d, seed: maphash.MakeSeed()}
	if b, err := os.ReadFile(cfgPath()); err == nil {
		_ = json.Unmarshal(b, &w.cfg)
	}
	return w
}

func (w *Watch) save() {
	_ = os.MkdirAll(watchDir(), 0o700)
	b, _ := json.Marshal(w.cfg)
	_ = os.WriteFile(cfgPath(), b, 0o600)
}

// resume restarts a watch that was running when the daemon last stopped.
func (w *Watch) resume() {
	w.mu.Lock()
	run := w.cfg.Running
	w.mu.Unlock()
	if run {
		log.Printf("resuming screen watch")
		w.launch()
	}
}

func (w *Watch) launch() {
	w.mu.Lock()
	if w.stop != nil {
		w.mu.Unlock()
		return
	}
	w.stop = make(chan struct{})
	stop := w.stop
	w.mu.Unlock()
	go func() {
		for {
			crashed := func() (crashed bool) {
				defer func() {
					if p := recover(); p != nil {
						log.Printf("screen watch crashed: %v; restarting", p)
						crashed = true
					}
				}()
				w.record(stop)
				return false
			}()
			if !crashed {
				return
			}
			w.mu.Lock()
			w.beat.Restarts++
			w.mu.Unlock()
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Second):
			}
		}
	}()
}

// halt stops recording but keeps the config's running flag (daemon shutdown).
func (w *Watch) halt() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stop != nil {
		close(w.stop)
		w.stop = nil
	}
}

func (w *Watch) region() (hypr.Rect, int, error) {
	ms, err := hypr.Monitors()
	if err != nil {
		return hypr.Rect{}, 0, err
	}
	w.mu.Lock()
	mon, detail := w.cfg.Monitor, w.cfg.Detail
	w.mu.Unlock()
	budget := img.Detail[detail]
	if budget == 0 {
		budget = 1280
	}
	if mon != "" {
		for _, m := range ms {
			if m.Name == mon {
				return m.Box(), budget, nil
			}
		}
		return hypr.Rect{}, 0, fmt.Errorf("monitor %s is not connected", mon)
	}
	n := 0
	for _, m := range ms {
		if !m.Disabled {
			n++
		}
	}
	return hypr.Layout(ms), min(budget*max(n, 1), 3200), nil
}

func (w *Watch) record(stop chan struct{}) {
	_ = os.MkdirAll(frameDir(), 0o700)
	var prevHash uint64
	var prevName string
	lastPrune := time.Time{}
	next := time.Now()
	for {
		w.mu.Lock()
		interval := time.Duration(float64(time.Second) / max(0.05, w.cfg.FPS))
		w.mu.Unlock()
		select {
		case <-stop:
			return
		case <-time.After(time.Until(next)):
		}
		now := time.Now()
		next = next.Add(interval)
		if next.Before(now) { // fell behind or the machine slept: no catch-up burst
			next = now.Add(interval)
		}
		r, budget, err := w.region()
		var m *image.RGBA
		if err == nil {
			m, err = w.d.grab(r, budget, false)
		}
		if err != nil {
			w.mu.Lock()
			w.beat.Fails++
			w.beat.Healthy = false
			w.beat.Reason = clip(err.Error(), 160)
			w.beat.At = now
			fails := w.beat.Fails
			w.mu.Unlock()
			// screens come back (lock, DPMS, unplug, compositor restart): back off, keep the history
			backoff := min(30*time.Second, interval*time.Duration(1<<min(fails, 5)))
			next = time.Now().Add(backoff)
			continue
		}
		h := maphash.Bytes(w.seed, m.Pix)
		t := tick{T: float64(now.UnixMilli()) / 1000}
		if aw, err := hypr.ActiveWindow(); err == nil && aw.Address != "" {
			t.App, t.Title, t.WS = aw.Class, clip(aw.Title, 110), aw.Workspace.Name
		}
		if h == prevHash && prevName != "" {
			t.F = prevName
		} else {
			var buf bytes.Buffer
			q := 62
			if w.cfg.Detail == "high" {
				q = 76
			}
			if err := jpeg.Encode(&buf, m, &jpeg.Options{Quality: q}); err == nil {
				name := fmt.Sprintf("f-%d.jpg", now.UnixMilli())
				if os.WriteFile(filepath.Join(frameDir(), name), buf.Bytes(), 0o600) == nil {
					t.F, t.New = name, true
					prevHash, prevName = h, name
					w.mu.Lock()
					w.beat.Captured++
					w.mu.Unlock()
				}
			}
		}
		if f, err := os.OpenFile(indexPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
			b, _ := json.Marshal(t)
			_, _ = f.Write(append(b, '\n'))
			f.Close()
		}
		w.mu.Lock()
		w.beat.At, w.beat.Healthy, w.beat.Fails, w.beat.Reason = now, true, 0, ""
		w.mu.Unlock()
		if now.Sub(lastPrune) > 10*time.Second {
			w.prune()
			lastPrune = now
		}
	}
}

type frameFile struct {
	name string
	t    float64
	size int64
}

func frames() []frameFile {
	ents, _ := os.ReadDir(frameDir())
	var out []frameFile
	for _, e := range ents {
		n := e.Name()
		if !strings.HasPrefix(n, "f-") {
			continue
		}
		ms, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(n, "f-"), ".jpg"), 10, 64)
		if err != nil {
			continue
		}
		info, _ := e.Info()
		var size int64
		if info != nil {
			size = info.Size()
		}
		out = append(out, frameFile{n, float64(ms) / 1000, size})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].t < out[j].t })
	return out
}

func (w *Watch) prune() {
	w.mu.Lock()
	window, maxBytes := w.cfg.Seconds, int64(w.cfg.MaxMB*1024*1024)
	w.mu.Unlock()
	cutoff := float64(time.Now().UnixMilli())/1000 - window
	fs := frames()
	var keep []frameFile
	var total int64
	for _, f := range fs {
		if f.t < cutoff {
			_ = os.Remove(filepath.Join(frameDir(), f.name))
			continue
		}
		keep = append(keep, f)
		total += f.size
	}
	for len(keep) > 0 && total > maxBytes {
		_ = os.Remove(filepath.Join(frameDir(), keep[0].name))
		total -= keep[0].size
		keep = keep[1:]
	}
	floor := cutoff
	if len(keep) > 0 {
		floor = max(cutoff, keep[0].t-1)
	}
	ticks := readTicks(0)
	var buf bytes.Buffer
	for _, t := range ticks {
		if t.T >= floor {
			b, _ := json.Marshal(t)
			buf.Write(append(b, '\n'))
		}
	}
	_ = os.WriteFile(indexPath()+".tmp", buf.Bytes(), 0o600)
	_ = os.Rename(indexPath()+".tmp", indexPath())
}

func readTicks(seconds float64) []tick {
	f, err := os.Open(indexPath())
	if err != nil {
		return nil
	}
	defer f.Close()
	cutoff := 0.0
	if seconds > 0 {
		cutoff = float64(time.Now().UnixMilli())/1000 - seconds
	}
	var out []tick
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var t tick
		if json.Unmarshal(sc.Bytes(), &t) == nil && t.T >= cutoff {
			out = append(out, t)
		}
	}
	return out
}

func (w *Watch) brief() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stop == nil {
		return "off"
	}
	if !w.beat.Healthy && w.beat.Fails > 0 {
		return "waiting (" + clip(w.beat.Reason, 50) + ")"
	}
	return fmt.Sprintf("recording %.4gfps", w.cfg.FPS)
}

func init() { register("watch", cmdWatch) }

func cmdWatch(d *Daemon, a Args) (*rpc.Response, error) {
	w := d.watch
	switch a.Str("action") {
	case "start":
		w.mu.Lock()
		running := w.stop != nil
		w.cfg = watchCfg{Running: true, FPS: 1, Seconds: 300, Detail: "normal", MaxMB: 256, Started: time.Now()}
		if v, ok := a.Num("fps"); ok {
			w.cfg.FPS = max(0.05, min(v, 4))
		}
		if v, ok := a.Num("seconds"); ok {
			w.cfg.Seconds = max(10, v)
		}
		if v := a.Str("detail"); v != "" {
			w.cfg.Detail = v
		}
		w.cfg.Monitor = a.Str("monitor")
		w.save()
		w.mu.Unlock()
		if !running {
			w.launch()
		}
		time.Sleep(time.Duration(float64(time.Second)/w.cfg.FPS) + 200*time.Millisecond)
		return text(fmt.Sprintf("watching %s at %.4g fps, keeping the last %s on this machine only (~/.cache/pc/watch). %s",
			orStr(w.cfg.Monitor, "every monitor"), w.cfg.FPS, human(time.Duration(w.cfg.Seconds)*time.Second), statusLine(w))), nil
	case "stop":
		w.mu.Lock()
		w.cfg.Running = false
		w.save()
		w.mu.Unlock()
		w.halt()
		msg := "screen watch stopped"
		if a.Bool("purge") {
			_ = os.RemoveAll(frameDir())
			_ = os.Remove(indexPath())
			for _, pat := range []string{"sheet-*.jpg", "clip-*.mp4", "clip.txt"} {
				old, _ := filepath.Glob(filepath.Join(watchDir(), pat))
				for _, p := range old {
					_ = os.Remove(p)
				}
			}
			msg += "; recorded frames, contact sheets and clips deleted"
		} else {
			msg += fmt.Sprintf("; %d frames left in the ring (watch stop --purge deletes them)", len(frames()))
		}
		return text(msg), nil
	case "", "status":
		return text(statusLine(w)), nil
	case "timeline":
		return watchTimeline(a)
	case "view":
		return watchView(a)
	case "latest":
		fs := frames()
		if len(fs) == 0 {
			return nil, errors.New("no frames recorded: pc watch start")
		}
		f := fs[len(fs)-1]
		p := filepath.Join(frameDir(), f.name)
		age := time.Since(time.UnixMilli(int64(f.t * 1000)))
		note := fmt.Sprintf("newest recorded frame, %s old (for clicking, take a fresh pc shot)", human(age))
		return &rpc.Response{Text: note + "\n" + p, Images: []rpc.Image{{Path: p, Mime: "image/jpeg", Note: note}}}, nil
	case "clip":
		return watchClip(a)
	}
	return nil, fmt.Errorf("watch: unknown action %q (start, stop, status, timeline, view, latest, clip)", a.Str("action"))
}

func orStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func statusLine(w *Watch) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	fs := frames()
	var bytesUsed int64
	for _, f := range fs {
		bytesUsed += f.size
	}
	if w.stop == nil {
		s := "screen watch: off"
		if len(fs) > 0 {
			s += fmt.Sprintf(" (%d old frames on disk)", len(fs))
		}
		return s
	}
	mark := "●"
	state := "recording"
	if !w.beat.Healthy && w.beat.Fails > 0 {
		mark, state = "◐", "waiting for the screen: "+w.beat.Reason
	}
	span := "0s"
	if len(fs) > 1 {
		span = human(time.Duration((fs[len(fs)-1].t - fs[0].t) * float64(time.Second)))
	}
	return fmt.Sprintf("screen watch %s %s · %.4g fps · %s · %d distinct frames over %s · %s · last tick %s",
		mark, state, w.cfg.FPS, w.cfg.Detail, len(fs), span, bytesH(uint64(bytesUsed)), ago(w.beat.At))
}

func watchTimeline(a Args) (*rpc.Response, error) {
	secs := 300.0
	if v, ok := a.Num("seconds"); ok {
		secs = v
	}
	ticks := readTicks(secs)
	if len(ticks) == 0 {
		return nil, errors.New("nothing recorded in that window: pc watch start (or pc watch status)")
	}
	type seg struct {
		app, title, ws string
		start, end     float64
		changed        int
	}
	var segs []*seg
	for _, t := range ticks {
		if n := len(segs); n > 0 && segs[n-1].app == t.App && segs[n-1].title == t.Title {
			segs[n-1].end = t.T
			if t.New {
				segs[n-1].changed++
			}
			continue
		}
		s := &seg{app: t.App, title: t.Title, ws: t.WS, start: t.T, end: t.T}
		if t.New {
			s.changed = 1
		}
		segs = append(segs, s)
	}
	minSecs, _ := a.Num("min")
	var b strings.Builder
	var covered, idle float64
	for _, s := range segs {
		dur := s.end - s.start
		covered += dur
		isIdle := s.changed <= 1 && dur >= 5
		if isIdle {
			idle += dur
		}
		if dur < minSecs {
			continue
		}
		tail := ""
		if isIdle {
			tail = "  idle"
		}
		fmt.Fprintf(&b, " %s %7s  %-14s %s%s\n", time.UnixMilli(int64(s.start*1000)).Format("15:04:05"),
			human(time.Duration(dur*float64(time.Second))), clip(orStr(s.app, "?"), 14), clip(s.title, 60), tail)
	}
	fmt.Fprintf(&b, "%s covered · %s of it with the screen unchanged · %d stretches",
		human(time.Duration(covered*float64(time.Second))), human(time.Duration(idle*float64(time.Second))), len(segs))
	return text(b.String()), nil
}

func watchView(a Args) (*rpc.Response, error) {
	secs := 120.0
	if v, ok := a.Num("seconds"); ok {
		secs = v
	}
	n := a.Int("frames", 6)
	ticks := readTicks(secs)
	var picks []tick
	seen := map[string]bool{}
	for _, t := range ticks {
		if t.New && !seen[t.F] {
			if _, err := os.Stat(filepath.Join(frameDir(), t.F)); err == nil {
				seen[t.F] = true
				picks = append(picks, t)
			}
		}
	}
	if len(picks) == 0 {
		return nil, errors.New("no frames in that window (the screen may not have changed, or the ring aged out)")
	}
	if len(picks) > n && n > 1 {
		step := float64(len(picks)-1) / float64(n-1)
		var spread []tick
		for i := 0; i < n; i++ {
			spread = append(spread, picks[int(float64(i)*step+0.5)])
		}
		picks = spread
	}
	var tiles []img.Tile
	now := time.Now()
	for _, p := range picks {
		f, err := os.Open(filepath.Join(frameDir(), p.F))
		if err != nil {
			continue
		}
		m, err := jpeg.Decode(f)
		f.Close()
		if err != nil {
			continue
		}
		at := time.UnixMilli(int64(p.T * 1000))
		label := fmt.Sprintf("%s  -%s  %s", at.Format("15:04:05"), human(now.Sub(at)), clip(p.App, 18))
		tiles = append(tiles, img.Tile{Label: label, Img: m})
	}
	cols := 3
	if len(tiles) <= 2 {
		cols = len(tiles)
	}
	sheet := img.Sheet(tiles, cols, 520)
	data, _, err := img.Encode(sheet, "normal")
	if err != nil {
		return nil, err
	}
	if old, _ := filepath.Glob(filepath.Join(watchDir(), "sheet-*.jpg")); len(old) > 4 {
		sort.Strings(old)
		for _, p := range old[:len(old)-4] {
			_ = os.Remove(p)
		}
	}
	p := filepath.Join(watchDir(), "sheet-"+now.Format("150405")+".jpg")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		return nil, err
	}
	note := fmt.Sprintf("last %s: %d moments where the screen changed, oldest first", human(time.Duration(secs)*time.Second), len(tiles))
	return &rpc.Response{Text: note + "\n" + p, Images: []rpc.Image{{Path: p, Mime: "image/jpeg", Note: note}}}, nil
}

func watchClip(a Args) (*rpc.Response, error) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return nil, errors.New("no ffmpeg; pc watch view gives a contact sheet instead")
	}
	secs := 300.0
	if v, ok := a.Num("seconds"); ok {
		secs = v
	}
	ticks := readTicks(secs)
	var list strings.Builder
	var last tick
	n := 0
	for i, t := range ticks {
		if !t.New {
			continue
		}
		if n > 0 {
			fmt.Fprintf(&list, "duration %.3f\n", max(0.04, (t.T-last.T)/10))
		}
		fmt.Fprintf(&list, "file '%s'\n", filepath.Join(frameDir(), t.F))
		last = t
		n++
		_ = i
	}
	if n < 2 {
		return nil, errors.New("not enough distinct frames for a clip")
	}
	fmt.Fprintf(&list, "duration 1\nfile '%s'\n", filepath.Join(frameDir(), last.F))
	lp := filepath.Join(watchDir(), "clip.txt")
	_ = os.WriteFile(lp, []byte(list.String()), 0o600)
	out := filepath.Join(watchDir(), "clip-"+time.Now().Format("20060102-150405")+".mp4")
	cmd := exec.Command("ffmpeg", "-y", "-loglevel", "error", "-f", "concat", "-safe", "0", "-i", lp,
		"-vf", "scale=trunc(iw/2)*2:trunc(ih/2)*2", "-r", "12", "-pix_fmt", "yuv420p", "-movflags", "+faststart", out)
	if b, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("ffmpeg: %s", clip(string(b), 200))
	}
	return text(fmt.Sprintf("%s of screen at 10× speed: %s", human(time.Duration(secs)*time.Second), out)), nil
}
