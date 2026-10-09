// Package sys reads what the machine is doing straight from /proc and /sys:
// no ps, no top, no subprocesses on the hot path. A Sampler keeps the previous
// counters so CPU and network figures are instantaneous rates, not lifetime
// averages.
package sys

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type CPU struct {
	Percent float64    `json:"percent"`
	PerCore []float64  `json:"per_core,omitempty"`
	Cores   int        `json:"cores"`
	FreqMHz float64    `json:"freq_mhz,omitempty"`
	TempC   float64    `json:"temp_c,omitempty"`
	Load    [3]float64 `json:"load"`
	IOWait  float64    `json:"iowait"`
}

type Mem struct {
	Total     uint64  `json:"total"`
	Used      uint64  `json:"used"`
	Available uint64  `json:"available"`
	Cached    uint64  `json:"cached"`
	SwapTotal uint64  `json:"swap_total"`
	SwapUsed  uint64  `json:"swap_used"`
	Percent   float64 `json:"percent"`
	// Pressure is the PSI "some avg10" for memory: % of time tasks stalled on memory.
	Pressure float64 `json:"pressure"`
}

type Net struct {
	RxRate  float64 `json:"rx_rate"`
	TxRate  float64 `json:"tx_rate"`
	RxTotal uint64  `json:"rx_total"`
	TxTotal uint64  `json:"tx_total"`
}

type Disk struct {
	Mount   string  `json:"mount"`
	Device  string  `json:"device"`
	FS      string  `json:"fs"`
	Total   uint64  `json:"total"`
	Used    uint64  `json:"used"`
	Percent float64 `json:"percent"`
}

type Battery struct {
	Percent int     `json:"percent"`
	State   string  `json:"state"`
	Watts   float64 `json:"watts,omitempty"`
}

type Proc struct {
	Pid     int       `json:"pid"`
	PPid    int       `json:"ppid"`
	Name    string    `json:"name"`
	State   string    `json:"state"`
	User    string    `json:"user"`
	CPU     float64   `json:"cpu"`
	RSS     uint64    `json:"rss"`
	Threads int       `json:"threads"`
	Start   time.Time `json:"start"`
	Cmd     string    `json:"cmd,omitempty"`
	cpuTime float64
	uid     uint32
	startTk uint64
}

// Snapshot is one full reading.
type Snapshot struct {
	At        time.Time `json:"at"`
	Host      Host      `json:"host"`
	Uptime    float64   `json:"uptime"`
	CPU       CPU       `json:"cpu"`
	Mem       Mem       `json:"memory"`
	Net       Net       `json:"network"`
	Disks     []Disk    `json:"disks"`
	Battery   *Battery  `json:"battery,omitempty"`
	GPU       *GPU      `json:"gpu,omitempty"`
	Procs     []Proc    `json:"-"`
	Top       []Proc    `json:"top"`
	ProcCount int       `json:"process_count"`
	Zombies   []Proc    `json:"zombies,omitempty"`
	Stuck     []Proc    `json:"uninterruptible,omitempty"`
}

type Host struct {
	Hostname string `json:"hostname"`
	OS       string `json:"os"`
	Kernel   string `json:"kernel"`
	Arch     string `json:"arch"`
}

type GPU struct {
	Name    string  `json:"name,omitempty"`
	Percent float64 `json:"percent"`
	MemUsed uint64  `json:"mem_used,omitempty"`
	MemTot  uint64  `json:"mem_total,omitempty"`
	TempC   float64 `json:"temp_c,omitempty"`
}

var (
	clkTck   = 100.0
	pageSize = uint64(os.Getpagesize())
	bootTime = readBootTime()
)

func readBootTime() time.Time {
	b, _ := os.ReadFile("/proc/stat")
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "btime ") {
			n, _ := strconv.ParseInt(strings.TrimSpace(line[6:]), 10, 64)
			return time.Unix(n, 0)
		}
	}
	return time.Time{}
}

// BootTime is when the machine booted.
func BootTime() time.Time { return bootTime }

// Sampler differences counters between reads.
type Sampler struct {
	prevCPU   []uint64
	prevCores [][]uint64
	prevNetT  time.Time
	prevRx    uint64
	prevTx    uint64
	prevProcT time.Time
	prevProcs map[int]procKey
}

type procKey struct {
	start uint64
	cpu   float64
	uid   uint32
}

func NewSampler() *Sampler { return &Sampler{} }

func readFile(p string) []byte {
	b, _ := os.ReadFile(p)
	return b
}

func fields(line string) []uint64 {
	var out []uint64
	for _, f := range strings.Fields(line)[1:] {
		n, _ := strconv.ParseUint(f, 10, 64)
		out = append(out, n)
	}
	return out
}

func busy(prev, now []uint64) (float64, float64) {
	if len(prev) < 5 || len(now) < 5 {
		return 0, 0
	}
	var tp, tn uint64
	for i := range now {
		tn += now[i]
		if i < len(prev) {
			tp += prev[i]
		}
	}
	d := float64(tn) - float64(tp)
	if d <= 0 {
		return 0, 0
	}
	idle := float64(now[3]+now[4]) - float64(prev[3]+prev[4])
	iow := float64(now[4]) - float64(prev[4])
	return clamp(100 * (1 - idle/d)), clamp(100 * iow / d)
}

func clamp(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

func (s *Sampler) cpu() CPU {
	var c CPU
	var total []uint64
	var cores [][]uint64
	for _, line := range strings.Split(string(readFile("/proc/stat")), "\n") {
		if !strings.HasPrefix(line, "cpu") {
			break
		}
		if strings.HasPrefix(line, "cpu ") {
			total = fields(line)
		} else {
			cores = append(cores, fields(line))
		}
	}
	c.Cores = len(cores)
	if s.prevCPU != nil {
		c.Percent, c.IOWait = busy(s.prevCPU, total)
		if len(s.prevCores) == len(cores) {
			for i := range cores {
				p, _ := busy(s.prevCores[i], cores[i])
				c.PerCore = append(c.PerCore, p)
			}
		}
	}
	s.prevCPU, s.prevCores = total, cores
	if f := strings.Fields(string(readFile("/proc/loadavg"))); len(f) >= 3 {
		for i := 0; i < 3; i++ {
			c.Load[i], _ = strconv.ParseFloat(f[i], 64)
		}
	}
	var sum float64
	var n int
	matches, _ := filepath.Glob("/sys/devices/system/cpu/cpu[0-9]*/cpufreq/scaling_cur_freq")
	for _, m := range matches {
		if v, err := strconv.ParseFloat(strings.TrimSpace(string(readFile(m))), 64); err == nil {
			sum += v
			n++
		}
	}
	if n > 0 {
		c.FreqMHz = sum / float64(n) / 1000
	}
	c.TempC = CPUTemp()
	return c
}

// CPUTemp is the hottest CPU sensor: package temp if there is one.
func CPUTemp() float64 {
	best := 0.0
	hw, _ := filepath.Glob("/sys/class/hwmon/hwmon*")
	for _, h := range hw {
		name := strings.TrimSpace(string(readFile(h + "/name")))
		if name != "coretemp" && name != "k10temp" && name != "zenpower" && name != "cpu_thermal" {
			continue
		}
		inputs, _ := filepath.Glob(h + "/temp*_input")
		for _, in := range inputs {
			v, err := strconv.ParseFloat(strings.TrimSpace(string(readFile(in))), 64)
			if err == nil && v/1000 > best && v/1000 < 130 {
				best = v / 1000
			}
		}
	}
	if best > 0 {
		return best
	}
	zones, _ := filepath.Glob("/sys/class/thermal/thermal_zone*")
	for _, z := range zones {
		kind := strings.TrimSpace(string(readFile(z + "/type")))
		if !strings.HasPrefix(kind, "x86_pkg") && !strings.HasPrefix(kind, "cpu") && kind != "acpitz" {
			continue
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(string(readFile(z+"/temp"))), 64)
		if err == nil && v/1000 > best && v/1000 < 130 {
			best = v / 1000
		}
	}
	return best
}

func memory() Mem {
	info := map[string]uint64{}
	sc := bufio.NewScanner(bytes.NewReader(readFile("/proc/meminfo")))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		f := strings.Fields(v)
		if len(f) > 0 {
			n, _ := strconv.ParseUint(f[0], 10, 64)
			info[k] = n * 1024
		}
	}
	m := Mem{Total: info["MemTotal"], Available: info["MemAvailable"], Cached: info["Cached"],
		SwapTotal: info["SwapTotal"], SwapUsed: info["SwapTotal"] - info["SwapFree"]}
	m.Used = m.Total - m.Available
	if m.Total > 0 {
		m.Percent = 100 * float64(m.Used) / float64(m.Total)
	}
	m.Pressure = psi("/proc/pressure/memory")
	return m
}

func psi(path string) float64 {
	for _, line := range strings.Split(string(readFile(path)), "\n") {
		if strings.HasPrefix(line, "some ") {
			for _, f := range strings.Fields(line) {
				if v, ok := strings.CutPrefix(f, "avg10="); ok {
					n, _ := strconv.ParseFloat(v, 64)
					return n
				}
			}
		}
	}
	return 0
}

func virtualIface(name string) bool {
	if name == "lo" {
		return true
	}
	for _, p := range []string{"veth", "docker", "br-", "virbr", "vnet", "tun", "tap", "wg", "tailscale", "zt"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func (s *Sampler) network() Net {
	var n Net
	lines := strings.Split(string(readFile("/proc/net/dev")), "\n")
	for _, line := range lines[min(2, len(lines)):] {
		name, rest, ok := strings.Cut(line, ":")
		if !ok || virtualIface(strings.TrimSpace(name)) {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 9 {
			continue
		}
		rx, _ := strconv.ParseUint(f[0], 10, 64)
		tx, _ := strconv.ParseUint(f[8], 10, 64)
		n.RxTotal += rx
		n.TxTotal += tx
	}
	now := time.Now()
	if !s.prevNetT.IsZero() {
		if dt := now.Sub(s.prevNetT).Seconds(); dt > 0.01 && n.RxTotal >= s.prevRx && n.TxTotal >= s.prevTx {
			n.RxRate = float64(n.RxTotal-s.prevRx) / dt
			n.TxRate = float64(n.TxTotal-s.prevTx) / dt
		}
	}
	s.prevNetT, s.prevRx, s.prevTx = now, n.RxTotal, n.TxTotal
	return n
}

// Disks lists real mounted filesystems, one row per device.
func Disks() []Disk {
	var out []Disk
	seen := map[string]bool{}
	for _, line := range strings.Split(string(readFile("/proc/self/mounts")), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || !strings.HasPrefix(f[0], "/dev/") || strings.HasPrefix(f[0], "/dev/loop") {
			continue
		}
		mount := strings.ReplaceAll(f[1], `\040`, " ")
		if seen[f[0]] || strings.HasPrefix(mount, "/var/lib/docker") || strings.HasPrefix(mount, "/snap") {
			continue
		}
		var st syscall.Statfs_t
		if syscall.Statfs(mount, &st) != nil || st.Blocks == 0 {
			continue
		}
		seen[f[0]] = true
		total := st.Blocks * uint64(st.Bsize)
		free := st.Bavail * uint64(st.Bsize)
		used := total - st.Bfree*uint64(st.Bsize)
		d := Disk{Mount: mount, Device: f[0], FS: f[2], Total: total, Used: used}
		if used+free > 0 {
			d.Percent = 100 * float64(used) / float64(used+free)
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return len(out[i].Mount) < len(out[j].Mount) })
	return out
}

// Power reads the first battery.
func Power() *Battery {
	bats, _ := filepath.Glob("/sys/class/power_supply/BAT*")
	for _, b := range bats {
		pct, err := strconv.Atoi(strings.TrimSpace(string(readFile(b + "/capacity"))))
		if err != nil {
			continue
		}
		bt := &Battery{Percent: pct, State: strings.ToLower(strings.TrimSpace(string(readFile(b + "/status"))))}
		if uw, err := strconv.ParseFloat(strings.TrimSpace(string(readFile(b+"/power_now"))), 64); err == nil {
			bt.Watts = uw / 1e6
		}
		return bt
	}
	return nil
}

// GPUInfo reads amdgpu/intel busy counters from sysfs (no nvidia-smi spawn here).
func GPUInfo() *GPU {
	cards, _ := filepath.Glob("/sys/class/drm/card[0-9]/device")
	for _, c := range cards {
		v, err := strconv.ParseFloat(strings.TrimSpace(string(readFile(c+"/gpu_busy_percent"))), 64)
		if err != nil {
			continue
		}
		g := &GPU{Percent: v}
		g.MemUsed, _ = strconv.ParseUint(strings.TrimSpace(string(readFile(c+"/mem_info_vram_used"))), 10, 64)
		g.MemTot, _ = strconv.ParseUint(strings.TrimSpace(string(readFile(c+"/mem_info_vram_total"))), 10, 64)
		return g
	}
	return nil
}

// Uptime in seconds.
func Uptime() float64 {
	f := strings.Fields(string(readFile("/proc/uptime")))
	if len(f) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(f[0], 64)
	return v
}

// HostInfo names the machine.
func HostInfo() Host {
	h := Host{}
	h.Hostname, _ = os.Hostname()
	var u syscall.Utsname
	if syscall.Uname(&u) == nil {
		h.Kernel = cstr(u.Release[:])
		h.Arch = cstr(u.Machine[:])
	}
	for _, line := range strings.Split(string(readFile("/etc/os-release")), "\n") {
		if v, ok := strings.CutPrefix(line, "PRETTY_NAME="); ok {
			h.OS = strings.Trim(v, `"`)
		}
	}
	return h
}

func cstr(b []int8) string {
	var out []byte
	for _, c := range b {
		if c == 0 {
			break
		}
		out = append(out, byte(c))
	}
	return string(out)
}

// Sample takes one reading. The first call on a fresh Sampler has no rates;
// Prime then a short sleep fixes that for one-shot use.
func (s *Sampler) Sample() *Snapshot {
	snap := &Snapshot{At: time.Now(), Host: HostInfo(), Uptime: Uptime()}
	snap.CPU = s.cpu()
	snap.Mem = memory()
	snap.Net = s.network()
	snap.Disks = Disks()
	snap.Battery = Power()
	snap.GPU = GPUInfo()
	snap.Procs = s.processes()
	snap.ProcCount = len(snap.Procs)
	top := append([]Proc(nil), snap.Procs...)
	sort.Slice(top, func(i, j int) bool { return top[i].CPU > top[j].CPU })
	for _, p := range top {
		if len(snap.Top) >= 8 {
			break
		}
		if p.CPU >= 0.5 {
			snap.Top = append(snap.Top, p)
		}
	}
	for _, p := range snap.Procs {
		switch p.State {
		case "Z":
			snap.Zombies = append(snap.Zombies, p)
		case "D":
			snap.Stuck = append(snap.Stuck, p)
		}
	}
	return snap
}

// Prime reads the counters once so the next Sample has rates.
func (s *Sampler) Prime() {
	s.cpu()
	s.network()
	s.processes()
}
