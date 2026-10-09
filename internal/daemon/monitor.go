package daemon

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"pc/internal/hypr"
	"pc/internal/rpc"
	"pc/internal/sys"
)

// Monitor keeps a warm sampler (so CPU% is instant) and turns the difference
// between samples into events: processes starting and exiting, windows
// opening and closing. The journal covers services, crashes and the kernel.
type Monitor struct {
	d       *Daemon
	mu      sync.Mutex
	s       *sys.Sampler
	last    *sys.Snapshot
	since   time.Time
	procs   map[int]tracked
	wins    map[string]hypr.Client
	events  []*sys.Event
	jcache  map[time.Duration]jcached
	fcache  []sys.Unit
	fcached time.Time
	stuck   map[int]int
}

type tracked struct {
	start  time.Time
	name   string
	parent string
	helper bool // a worker of its parent's program, never news on its own
	seen   time.Time
	ev     *sys.Event
}

// isHelper: a process running the same executable as its parent (a
// browser's content processes, a server's workers) or one of systemd's own
// short-lived workers. Their churn says nothing about the machine.
func isHelper(pid, ppid int) bool {
	exe, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/exe")
	if err != nil {
		return false
	}
	if strings.HasPrefix(exe, "/usr/lib/systemd/") {
		return true
	}
	pexe, err := os.Readlink("/proc/" + strconv.Itoa(ppid) + "/exe")
	return err == nil && pexe == exe
}

type jcached struct {
	at  time.Time
	evs []sys.Event
}

func newMonitor(d *Daemon) *Monitor {
	return &Monitor{d: d, s: sys.NewSampler(), since: time.Now(), procs: map[int]tracked{},
		wins: map[string]hypr.Client{}, jcache: map[time.Duration]jcached{}, stuck: map[int]int{}}
}

func (m *Monitor) loop() {
	m.s.Prime()
	time.Sleep(300 * time.Millisecond)
	first := true
	for {
		snap := m.s.Sample()
		m.mu.Lock()
		m.last = snap
		m.diffProcs(snap, first)
		m.mu.Unlock()
		m.diffWindows(first)
		first = false
		interval := 2 * time.Second
		if m.d.idleFor() > 10*time.Minute {
			interval = 5 * time.Second
		}
		time.Sleep(interval)
	}
}

func (m *Monitor) push(ev *sys.Event) {
	m.events = append(m.events, ev)
	if len(m.events) > 5000 {
		m.events = m.events[len(m.events)-4000:]
	}
}

func (m *Monitor) diffProcs(snap *sys.Snapshot, baseline bool) {
	now := time.Now()
	byPid := map[int]sys.Proc{}
	for _, p := range snap.Procs {
		byPid[p.Pid] = p
	}
	alive := map[int]bool{}
	for _, p := range snap.Procs {
		if sys.IsKernelThread(p) {
			continue
		}
		alive[p.Pid] = true
		t, ok := m.procs[p.Pid]
		if ok && t.start.Equal(p.Start) {
			t.seen = now
			// a start becomes notable once it has survived a while and is not
			// just another helper of the same program
			if t.ev != nil && !t.ev.Notable && !t.helper && now.Sub(t.start) > 30*time.Second && t.name != t.parent {
				t.ev.Notable = true
			}
			m.procs[p.Pid] = t
			continue
		}
		parent := byPid[p.PPid].Name
		t = tracked{start: p.Start, name: p.Name, parent: parent, seen: now}
		if !baseline {
			t.helper = isHelper(p.Pid, p.PPid)
		}
		if !baseline {
			ev := &sys.Event{Time: p.Start, Kind: "started", Subject: p.Name, Pid: p.Pid, Source: "process",
				Detail: clip(sys.Cmdline(p.Pid), 140)}
			if parent != "" {
				ev.Detail = strings.TrimSpace(ev.Detail + "  (from " + parent + ")")
			}
			t.ev = ev
			m.push(ev)
		}
		m.procs[p.Pid] = t
	}
	for pid, t := range m.procs {
		if alive[pid] {
			continue
		}
		life := now.Sub(t.start)
		m.push(&sys.Event{Time: now, Kind: "exited", Subject: t.name, Pid: pid, Source: "process",
			Detail: "ran " + human(life), Notable: life > time.Minute && t.name != t.parent && !t.helper})
		delete(m.procs, pid)
	}
	// uninterruptible sleep is only a problem if it persists
	stuck := map[int]int{}
	for _, p := range snap.Stuck {
		stuck[p.Pid] = m.stuck[p.Pid] + 1
	}
	m.stuck = stuck
}

func (m *Monitor) diffWindows(baseline bool) {
	cs, err := hypr.Clients()
	if err != nil {
		return
	}
	now := time.Now()
	cur := map[string]hypr.Client{}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range cs {
		if !c.Mapped {
			continue
		}
		cur[c.Address] = c
		if _, ok := m.wins[c.Address]; !ok && !baseline {
			m.push(&sys.Event{Time: now, Kind: "window+", Subject: c.Class, Pid: c.Pid, Source: "desktop",
				Detail: clip(c.Title, 80) + " · ws " + c.Workspace.Name, Notable: true})
		}
	}
	for addr, c := range m.wins {
		if _, ok := cur[addr]; !ok {
			m.push(&sys.Event{Time: now, Kind: "window-", Subject: c.Class, Pid: c.Pid, Source: "desktop",
				Detail: clip(c.Title, 80), Notable: true})
		}
	}
	m.wins = cur
}

// snapshot returns a fresh-enough reading.
func (m *Monitor) snapshot() *sys.Snapshot {
	m.mu.Lock()
	last := m.last
	m.mu.Unlock()
	if last != nil && time.Since(last.At) < 6*time.Second {
		return last
	}
	s := sys.NewSampler()
	s.Prime()
	time.Sleep(150 * time.Millisecond)
	return s.Sample()
}

func (m *Monitor) journal(window time.Duration) []sys.Event {
	m.mu.Lock()
	c, ok := m.jcache[window]
	m.mu.Unlock()
	if ok && time.Since(c.at) < 15*time.Second {
		return c.evs
	}
	evs := sys.JournalEvents(window)
	m.mu.Lock()
	m.jcache[window] = jcached{time.Now(), evs}
	m.mu.Unlock()
	return evs
}

func (m *Monitor) failedUnits() []sys.Unit {
	m.mu.Lock()
	if time.Since(m.fcached) < 20*time.Second {
		u := m.fcache
		m.mu.Unlock()
		return u
	}
	m.mu.Unlock()
	u := sys.Units("failed")
	m.mu.Lock()
	m.fcache, m.fcached = u, time.Now()
	m.mu.Unlock()
	return u
}

// ---------------------------------------------------------------- state

func init() {
	register("state", cmdState)
	register("sys", cmdSys)
	register("ps", cmdPs)
	register("proc", cmdProc)
	register("events", cmdEvents)
	register("units", cmdUnits)
	register("logs", cmdLogs)
	register("ports", cmdPorts)
	register("windows", cmdWindows)
}

func (m *Monitor) problems(snap *sys.Snapshot) []string {
	var out []string
	var sysF, userF []string
	for _, u := range m.failedUnits() {
		if u.User {
			userF = append(userF, u.Name)
		} else {
			sysF = append(sysF, u.Name)
		}
	}
	if len(sysF) > 0 {
		out = append(out, fmt.Sprintf("%d failed service(s): %s", len(sysF), strings.Join(sysF, ", ")))
	}
	if len(userF) > 0 {
		out = append(out, fmt.Sprintf("%d failed user service(s): %s", len(userF), strings.Join(userF, ", ")))
	}
	restarts := map[string]int{}
	var crashes, ooms []string
	for _, e := range m.journal(time.Hour) {
		switch e.Kind {
		case "restarting":
			restarts[e.Subject]++
		case "crashed":
			crashes = append(crashes, fmt.Sprintf("%s (%s, %s)", e.Subject, strings.Fields(e.Detail + " ?")[0], ago(e.Time)))
		case "oom-killed":
			ooms = append(ooms, clip(e.Detail, 80)+" ("+ago(e.Time)+")")
		}
	}
	var flappers []string
	for u, n := range restarts {
		if n >= 3 {
			flappers = append(flappers, fmt.Sprintf("%s restarted %d× in the last hour", u, n))
		}
	}
	sort.Strings(flappers)
	out = append(out, flappers...)
	for _, c := range crashes {
		out = append(out, "crash: "+c)
	}
	for _, o := range ooms {
		out = append(out, "out of memory: "+o)
	}
	for _, d := range snap.Disks {
		if d.Percent >= 90 {
			out = append(out, fmt.Sprintf("disk %s is %s full (%s free)", d.Mount, pct(d.Percent), bytesH(d.Total-d.Used)))
		}
	}
	if snap.Mem.Percent >= 90 || snap.Mem.Pressure >= 10 {
		out = append(out, fmt.Sprintf("memory is tight: %.0f%% used, pressure %.1f%%", snap.Mem.Percent, snap.Mem.Pressure))
	}
	if snap.Mem.SwapTotal > 0 && float64(snap.Mem.SwapUsed) > 0.5*float64(snap.Mem.SwapTotal) {
		out = append(out, fmt.Sprintf("swap %.0f%% used", 100*float64(snap.Mem.SwapUsed)/float64(snap.Mem.SwapTotal)))
	}
	if snap.CPU.TempC >= 90 {
		out = append(out, fmt.Sprintf("CPU is hot: %.0f°C", snap.CPU.TempC))
	}
	if snap.CPU.Cores > 0 && snap.CPU.Load[0] > 2*float64(snap.CPU.Cores) {
		out = append(out, fmt.Sprintf("load %.1f on %d cores", snap.CPU.Load[0], snap.CPU.Cores))
	}
	if b := snap.Battery; b != nil && b.Percent <= 15 && b.State == "discharging" {
		out = append(out, fmt.Sprintf("battery low: %d%%", b.Percent))
	}
	if n := len(snap.Zombies); n > 0 {
		parents := map[int]bool{}
		for _, z := range snap.Zombies {
			parents[z.PPid] = true
		}
		out = append(out, fmt.Sprintf("%d zombie process(es) (unreaped by %d parent(s))", n, len(parents)))
	}
	m.mu.Lock()
	for _, p := range snap.Stuck {
		if m.stuck[p.Pid] >= 3 {
			out = append(out, fmt.Sprintf("%s (%d) stuck in uninterruptible IO wait", p.Name, p.Pid))
		}
	}
	m.mu.Unlock()
	return out
}

func cmdState(d *Daemon, a Args) (*rpc.Response, error) {
	snap := d.mon.snapshot()
	var b strings.Builder
	h := snap.Host
	fmt.Fprintf(&b, "%s · %s · kernel %s · up %s\n", h.Hostname, h.OS, h.Kernel, human(time.Duration(snap.Uptime*float64(time.Second))))
	c := snap.CPU
	fmt.Fprintf(&b, "cpu   %s of %d cores · load %.2f %.2f %.2f", pct(c.Percent), c.Cores, c.Load[0], c.Load[1], c.Load[2])
	if c.TempC > 0 {
		fmt.Fprintf(&b, " · %.0f°C", c.TempC)
	}
	if c.FreqMHz > 0 {
		fmt.Fprintf(&b, " · %.1fGHz", c.FreqMHz/1000)
	}
	if c.IOWait >= 5 {
		fmt.Fprintf(&b, " · iowait %s", pct(c.IOWait))
	}
	mm := snap.Mem
	fmt.Fprintf(&b, "\nmem   %s %s/%s", pct(mm.Percent), bytesH(mm.Used), bytesH(mm.Total))
	if mm.SwapTotal > 0 {
		fmt.Fprintf(&b, " · swap %s/%s", bytesH(mm.SwapUsed), bytesH(mm.SwapTotal))
	}
	if mm.Pressure > 0.5 {
		fmt.Fprintf(&b, " · pressure %.1f%%", mm.Pressure)
	}
	var disks []string
	for _, dk := range snap.Disks {
		disks = append(disks, fmt.Sprintf("%s %s of %s", dk.Mount, pct(dk.Percent), bytesH(dk.Total)))
	}
	if len(disks) > 4 {
		disks = disks[:4]
	}
	fmt.Fprintf(&b, "\ndisk  %s", strings.Join(disks, " · "))
	fmt.Fprintf(&b, "\nnet   ↓%s/s ↑%s/s", bytesH(uint64(snap.Net.RxRate)), bytesH(uint64(snap.Net.TxRate)))
	if bt := snap.Battery; bt != nil {
		fmt.Fprintf(&b, "\npower %d%% %s", bt.Percent, bt.State)
		if bt.Watts > 0 {
			fmt.Fprintf(&b, " (%.1fW)", bt.Watts)
		}
	}
	if g := snap.GPU; g != nil {
		fmt.Fprintf(&b, "\ngpu   %s", pct(g.Percent))
		if g.MemTot > 0 {
			fmt.Fprintf(&b, " · vram %s/%s", bytesH(g.MemUsed), bytesH(g.MemTot))
		}
	}
	var busy []string
	for i, p := range snap.Top {
		if i >= 5 {
			break
		}
		busy = append(busy, fmt.Sprintf("%s %.0f%% (%d)", p.Name, p.CPU, p.Pid))
	}
	if len(busy) > 0 {
		fmt.Fprintf(&b, "\nbusy  %s", strings.Join(busy, ", "))
	}
	fmt.Fprintf(&b, "\nprocs %d", snap.ProcCount)
	if hypr.Available() {
		if aw, err := hypr.ActiveWindow(); err == nil && aw.Address != "" {
			fmt.Fprintf(&b, "\nfocus %s · workspace %s", describeWindow(aw), aw.Workspace.Name)
		}
	}
	input := "on"
	if inputAllowed() != nil {
		input = "OFF"
	}
	fmt.Fprintf(&b, "\nagent cursor %s · input %s · screen watch %s · daemon up %s",
		d.ghost.status(), input, d.watch.brief(), human(time.Since(d.started)))
	probs := d.mon.problems(snap)
	if len(probs) == 0 {
		b.WriteString("\nproblems: none spotted")
	} else {
		b.WriteString("\nPROBLEMS")
		for _, p := range probs {
			b.WriteString("\n  ! " + p)
		}
	}
	return &rpc.Response{Text: b.String(), Data: map[string]any{"snapshot": snap, "problems": probs}}, nil
}

func cmdSys(d *Daemon, a Args) (*rpc.Response, error) {
	inv := sys.Inspect()
	var b strings.Builder
	fmt.Fprintf(&b, "%s · %s · kernel %s %s · booted %s (%s)\n", inv.Host.Hostname, inv.Host.OS, inv.Host.Kernel,
		inv.Host.Arch, inv.Boot.Format("Jan 2 15:04"), ago(inv.Boot))
	fmt.Fprintf(&b, "cpu      %s · %d cores / %d threads\n", inv.CPUModel, inv.Cores, inv.Threads)
	fmt.Fprintf(&b, "memory   %s\n", bytesH(inv.MemTotal))
	for _, g := range inv.GPUs {
		fmt.Fprintf(&b, "gpu      %s\n", g)
	}
	for _, dr := range inv.Drives {
		kind := "HDD"
		if dr.SSD {
			kind = "SSD"
		}
		fmt.Fprintf(&b, "drive    %s %s %s %s\n", dr.Name, bytesH(dr.Size), kind, dr.Model)
	}
	for _, dk := range inv.Disks {
		fmt.Fprintf(&b, "fs       %-14s %s %s %s used of %s\n", dk.Mount, dk.Device, dk.FS, pct(dk.Percent), bytesH(dk.Total))
	}
	var virtual []string
	for _, i := range inv.Ifaces {
		if i.Kind == "virtual" {
			virtual = append(virtual, i.Name)
			continue
		}
		state := "down"
		if i.Up {
			state = "up"
		}
		fmt.Fprintf(&b, "net      %s (%s, %s) %s\n", i.Name, i.Kind, state, strings.Join(i.Addrs, " "))
	}
	if len(virtual) > 0 {
		shown := virtual
		if len(shown) > 4 {
			shown = append(shown[:4:4], fmt.Sprintf("+%d more", len(virtual)-4))
		}
		fmt.Fprintf(&b, "net      %d virtual (docker/vpn bridges): %s\n", len(virtual), strings.Join(shown, ", "))
	}
	if inv.Gateway != "" {
		fmt.Fprintf(&b, "gateway  %s · dns %s\n", inv.Gateway, strings.Join(inv.DNS, " "))
	}
	if inv.Battery != nil {
		fmt.Fprintf(&b, "battery  %d%% %s\n", inv.Battery.Percent, inv.Battery.State)
	}
	if ms, err := hypr.Monitors(); err == nil {
		for _, mo := range ms {
			fmt.Fprintf(&b, "display  %s %dx%d@%.0f scale %g at %d,%d %s\n", mo.Name, mo.Width, mo.Height, mo.RefreshRate, mo.Scale, mo.X, mo.Y, clip(mo.Description, 40))
		}
		fmt.Fprintf(&b, "desktop  Hyprland %s (%s config)\n", hypr.Version(), map[bool]string{true: "lua", false: "hyprlang"}[hypr.Lua()])
	}
	fmt.Fprintf(&b, "session  %s · shell %s · init %s", inv.Session, inv.Shell, inv.Init)
	if inv.Packages != "" {
		fmt.Fprintf(&b, " · %s packages", inv.Packages)
	}
	return &rpc.Response{Text: b.String(), Data: inv}, nil
}

func cmdPs(d *Daemon, a Args) (*rpc.Response, error) {
	snap := d.mon.snapshot()
	q := a.Str("query")
	var list []sys.Proc
	if q != "" {
		list = sys.Find(q, snap.Procs)
	} else {
		for _, p := range snap.Procs {
			if !sys.IsKernelThread(p) {
				list = append(list, p)
			}
		}
	}
	if u := a.Str("user"); u != "" {
		var f []sys.Proc
		for _, p := range list {
			if p.User == u {
				f = append(f, p)
			}
		}
		list = f
	}
	switch a.Str("sort") {
	case "mem", "rss":
		sort.Slice(list, func(i, j int) bool { return list[i].RSS > list[j].RSS })
	case "new", "start", "recent":
		sort.Slice(list, func(i, j int) bool { return list[i].Start.After(list[j].Start) })
	case "name":
		sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	default:
		sort.SliceStable(list, func(i, j int) bool {
			if list[i].CPU != list[j].CPU {
				return list[i].CPU > list[j].CPU
			}
			return list[i].RSS > list[j].RSS
		})
	}
	total := len(list)
	limit := a.Int("limit", 25)
	if limit > 0 && len(list) > limit {
		list = list[:limit]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%7s %-10s %6s %7s %2s %9s  %s\n", "PID", "USER", "CPU", "MEM", "ST", "STARTED", "COMMAND")
	for i := range list {
		p := &list[i]
		if p.Cmd == "" {
			p.Cmd = sys.Cmdline(p.Pid)
		}
		cmd := p.Cmd
		if cmd == "" {
			cmd = "[" + p.Name + "]"
		}
		fmt.Fprintf(&b, "%7d %-10s %5.1f%% %7s %2s %9s  %s\n", p.Pid, clip(p.User, 10), p.CPU, bytesH(p.RSS), p.State,
			shortAgo(p.Start), clip(cmd, 110))
	}
	fmt.Fprintf(&b, "%d of %d processes", len(list), total)
	if q != "" {
		fmt.Fprintf(&b, " matching %q", q)
	}
	return &rpc.Response{Text: b.String(), Data: list}, nil
}

func shortAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func cmdProc(d *Daemon, a Args) (*rpc.Response, error) {
	snap := d.mon.snapshot()
	q := a.Str("target")
	hits := sys.Find(q, snap.Procs)
	if len(hits) == 0 {
		return nil, fmt.Errorf("no process matches %q", q)
	}
	// A process named q beats one whose name contains q, which beats one that
	// only mentions q in its command line (sddm-helper ... Hyprland).
	rank := func(p sys.Proc) int {
		switch {
		case strings.EqualFold(p.Name, q):
			return 2
		case strings.Contains(strings.ToLower(p.Name), strings.ToLower(q)):
			return 1
		}
		return 0
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if ri, rj := rank(hits[i]), rank(hits[j]); ri != rj {
			return ri > rj
		}
		return hits[i].CPU+float64(hits[i].RSS)/1e9 > hits[j].CPU+float64(hits[j].RSS)/1e9
	})
	pick := hits[0]
	if _, err := strconv.Atoi(q); err != nil {
		// within the best rank, prefer the root of a process family
		// (firefox over its content processes)
		names := map[int]bool{}
		for _, h := range hits {
			names[h.Pid] = true
		}
		for _, h := range hits {
			if rank(h) == rank(hits[0]) && !names[h.PPid] {
				pick = h
				break
			}
		}
	}
	det, err := sys.Describe(pick.Pid, snap.Procs)
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s (pid %d) · %s · user %s · started %s (%s)\n", det.Name, det.Pid, stateName(det.State), det.User,
		det.Start.Format("15:04:05"), ago(det.Start))
	fmt.Fprintf(&b, "cmd     %s\n", clip(det.Cmd, 400))
	fmt.Fprintf(&b, "exe     %s\ncwd     %s\n", det.Exe, det.Cwd)
	fmt.Fprintf(&b, "usage   cpu %.1f%% · mem %s · %d threads · %d fds · io read %s write %s · oom score %d\n",
		det.CPU, bytesH(det.RSS), det.Threads, det.FDs, bytesH(det.IORead), bytesH(det.IOWrite), det.OOMScore)
	if det.Unit != "" {
		fmt.Fprintf(&b, "unit    %s\n", det.Unit)
	}
	if len(det.Parents) > 0 {
		fmt.Fprintf(&b, "parents %s\n", strings.Join(det.Parents, " ← "))
	}
	if len(det.Children) > 0 {
		var kids []string
		for i, k := range det.Children {
			if i >= 8 {
				kids = append(kids, fmt.Sprintf("+%d more", len(det.Children)-8))
				break
			}
			kids = append(kids, fmt.Sprintf("%s %d (%.0f%%)", k.Name, k.Pid, k.CPU))
		}
		fmt.Fprintf(&b, "children %s\n", strings.Join(kids, ", "))
	}
	for _, p := range det.Ports {
		fmt.Fprintf(&b, "listens %s %s:%d\n", p.Proto, p.Addr, p.Port)
	}
	if cs, err := hypr.Clients(); err == nil {
		for _, c := range cs {
			if c.Pid == det.Pid {
				fmt.Fprintf(&b, "window  %s · ws %s · %s\n", describeWindow(c), c.Workspace.Name, c.Box())
			}
		}
	}
	if len(hits) > 1 {
		fmt.Fprintf(&b, "(%d processes matched %q; showing pid %d)\n", len(hits), q, det.Pid)
	}
	if det.Unit != "" {
		lines := sys.Logs(det.Unit, 24*time.Hour, 6, "", 12)
		if len(lines) > 0 {
			b.WriteString("recent log:\n")
			for _, l := range lines {
				fmt.Fprintf(&b, "  %s %s\n", l.Time.Format("15:04:05"), clip(l.Message, 160))
			}
		}
	}
	return &rpc.Response{Text: strings.TrimRight(b.String(), "\n"), Data: det}, nil
}

func stateName(s string) string {
	return map[string]string{"R": "running", "S": "sleeping", "D": "waiting on IO", "Z": "zombie",
		"T": "stopped", "t": "traced", "I": "idle", "X": "dead"}[s] + " (" + s + ")"
}

type group struct {
	kind, subject string
	n             int
	first, last   time.Time
	detail        string
	pid           int
	notable       bool
}

func cmdEvents(d *Daemon, a Args) (*rpc.Response, error) {
	window := a.Dur("since", 15*time.Minute)
	all := a.Bool("all")
	grep := strings.ToLower(a.Str("grep"))
	from := time.Now().Add(-window)
	evs := d.mon.journal(window)
	d.mon.mu.Lock()
	for _, e := range d.mon.events {
		if e.Time.After(from) {
			evs = append(evs, *e)
		}
	}
	tracking := d.mon.since
	d.mon.mu.Unlock()
	sys.SortEvents(evs)
	restarting := map[string]bool{}
	for _, e := range evs {
		if e.Kind == "restarting" {
			restarting[e.Subject] = true
		}
	}
	groups := map[string]*group{}
	var order []*group
	hidden := 0
	for _, e := range evs {
		if !all && !e.Notable {
			hidden++
			continue
		}
		if e.Kind == "started" && restarting[e.Subject] {
			continue
		}
		if grep != "" && !strings.Contains(strings.ToLower(e.Kind+" "+e.Subject+" "+e.Detail), grep) {
			continue
		}
		key := e.Kind + "|" + e.Subject
		if e.Source == "process" {
			key += "|" + e.Detail
		}
		g := groups[key]
		if g == nil {
			g = &group{kind: e.Kind, subject: e.Subject, first: e.Time, detail: e.Detail, pid: e.Pid, notable: e.Notable}
			groups[key] = g
			order = append(order, g)
		}
		if g.n > 0 && e.Time.Sub(g.last) < 2*time.Second && e.Source == "journal" {
			continue // one failure is logged twice ("failed with result", "failed to start")
		}
		g.n++
		g.last = e.Time
		g.detail = e.Detail
	}
	sort.SliceStable(order, func(i, j int) bool { return order[i].last.Before(order[j].last) })
	var b strings.Builder
	fmt.Fprintf(&b, "last %s", human(window))
	if from.Before(tracking) {
		fmt.Fprintf(&b, " (process and window tracking since the daemon started %s)", ago(tracking))
	}
	b.WriteString(":\n")
	if len(order) == 0 {
		b.WriteString("  nothing notable happened\n")
	}
	for _, g := range order {
		subj := g.subject
		if g.pid > 1 && g.n == 1 {
			subj = fmt.Sprintf("%s (%d)", subj, g.pid)
		}
		line := fmt.Sprintf("  %s  %-11s %s", g.last.Format("15:04:05"), g.kind, subj)
		if g.n > 1 {
			every := g.last.Sub(g.first) / time.Duration(g.n-1)
			line += fmt.Sprintf(" ×%d (every ~%s since %s)", g.n, human(every), g.first.Format("15:04"))
		}
		if g.detail != "" && g.kind != "restarting" {
			line += "  " + clip(g.detail, 110)
		}
		b.WriteString(line + "\n")
	}
	if hidden > 0 && !all {
		fmt.Fprintf(&b, "(%d routine events hidden; --all shows them)", hidden)
	}
	return &rpc.Response{Text: strings.TrimRight(b.String(), "\n"), Data: evs}, nil
}

func cmdUnits(d *Daemon, a Args) (*rpc.Response, error) {
	f := a.Str("filter")
	state := "failed"
	name := ""
	switch f {
	case "", "failed":
	case "running", "active", "all", "inactive", "exited":
		state = f
	default:
		state, name = "all", strings.ToLower(f)
	}
	us := sys.Units(state)
	var b strings.Builder
	n := 0
	for _, u := range us {
		if name != "" && !strings.Contains(strings.ToLower(u.Name+" "+u.Desc), name) {
			continue
		}
		scope := "system"
		if u.User {
			scope = "user"
		}
		fmt.Fprintf(&b, "%-6s %-8s %-10s %-44s %s\n", scope, u.Active, u.Sub, clip(u.Name, 44), clip(u.Desc, 60))
		n++
	}
	if n == 0 {
		if state == "failed" && name == "" {
			return text("no failed units (system or user)"), nil
		}
		return text("no units match"), nil
	}
	fmt.Fprintf(&b, "%d unit(s)", n)
	if state == "failed" {
		b.WriteString(" failed · pc logs <unit> shows why")
	}
	return &rpc.Response{Text: b.String(), Data: us}, nil
}

func cmdLogs(d *Daemon, a Args) (*rpc.Response, error) {
	prio := map[string]int{"emerg": 0, "alert": 1, "crit": 2, "err": 3, "error": 3, "warn": 4, "warning": 4,
		"notice": 5, "info": 6, "debug": 7}
	level := 4
	if l := a.Str("level"); l != "" {
		if p, ok := prio[strings.ToLower(l)]; ok {
			level = p
		}
	}
	unit := a.Str("unit")
	if unit != "" && level == 4 && !a.Has("level") {
		level = 6 // for one unit, show everything it said
	}
	lines := sys.Logs(unit, a.Dur("since", 15*time.Minute), level, a.Str("grep"), a.Int("lines", 60))
	if len(lines) == 0 {
		return text("no journal lines match"), nil
	}
	var b strings.Builder
	tag := []string{"EMERG", "ALERT", "CRIT", "ERR", "WARN", "note", "info", "dbg"}
	for _, l := range lines {
		t := "?"
		if l.Priority >= 0 && l.Priority < len(tag) {
			t = tag[l.Priority]
		}
		src := l.Source
		if l.Pid > 0 {
			src += "[" + strconv.Itoa(l.Pid) + "]"
		}
		fmt.Fprintf(&b, "%s %-5s %s: %s\n", l.Time.Format("15:04:05"), t, src, clip(l.Message, 220))
	}
	return &rpc.Response{Text: strings.TrimRight(b.String(), "\n"), Data: lines}, nil
}

func cmdPorts(d *Daemon, a Args) (*rpc.Response, error) {
	q := strings.ToLower(a.Str("query"))
	var b strings.Builder
	n := 0
	for _, p := range sys.Ports() {
		if q != "" && strconv.Itoa(p.Port) != q && !strings.Contains(strings.ToLower(p.Name), q) {
			continue
		}
		owner := p.Name
		if p.Pid > 0 {
			owner = fmt.Sprintf("%s (pid %d)", p.Name, p.Pid)
		} else if owner == "" {
			owner = "? (owned by " + p.User + ")"
		}
		fmt.Fprintf(&b, "%-4s %-22s %s\n", p.Proto, fmt.Sprintf("%s:%d", p.Addr, p.Port), owner)
		n++
	}
	if n == 0 {
		return text("nothing listening matches"), nil
	}
	return text(strings.TrimRight(b.String(), "\n")), nil
}

func cmdWindows(d *Daemon, a Args) (*rpc.Response, error) {
	if !hypr.Available() {
		return nil, fmt.Errorf("no Hyprland session reachable")
	}
	ms, err := hypr.Monitors()
	if err != nil {
		return nil, err
	}
	cs, err := hypr.Clients()
	if err != nil {
		return nil, err
	}
	q := strings.ToLower(a.Str("query"))
	var b strings.Builder
	b.WriteString("monitors:\n")
	for _, m := range ms {
		f := ""
		if m.Focused {
			f = " · focused"
		}
		fmt.Fprintf(&b, "  %s %dx%d scale %g at %s · workspace %s%s\n", m.Name, m.Width, m.Height, m.Scale, m.Box(), m.ActiveWorkspace.Name, f)
	}
	if x, y, err := hypr.CursorPos(); err == nil {
		fmt.Fprintf(&b, "cursors: yours at %.0f,%.0f · agent's %s\n", x, y, d.ghost.status())
	}
	visible := map[int]bool{}
	for _, m := range ms {
		visible[m.ActiveWorkspace.ID] = true
	}
	sort.SliceStable(cs, func(i, j int) bool { return cs[i].FocusHistoryID < cs[j].FocusHistoryID })
	b.WriteString("windows (most recently focused first; * = focused):\n")
	hiddenN := 0
	for _, c := range cs {
		if !c.Mapped {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(c.Class+" "+c.Title), q) {
			continue
		}
		mark := " "
		if c.FocusHistoryID == 0 {
			mark = "*"
		}
		where := "ws " + c.Workspace.Name
		if !visible[c.Workspace.ID] && !c.Pinned {
			where += " (not on screen)"
			hiddenN++
		}
		extra := ""
		if c.Floating {
			extra += " floating"
		}
		if c.Fullscreen > 0 {
			extra += " fullscreen"
		}
		if c.Xwayland {
			extra += " xwayland"
		}
		fmt.Fprintf(&b, " %s %s · %s · %s%s · pid %d · %s\n", mark, describeWindow(c), where, c.Box(), extra, c.Pid, c.Address)
	}
	return &rpc.Response{Text: strings.TrimRight(b.String(), "\n"), Data: map[string]any{"monitors": ms, "clients": cs}}, nil
}
