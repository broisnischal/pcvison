package sys

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

var (
	usersOnce sync.Once
	users     map[uint32]string
)

// UserName maps a uid to a login name via /etc/passwd (cached).
func UserName(uid uint32) string {
	usersOnce.Do(func() {
		users = map[uint32]string{}
		sc := bufio.NewScanner(bytes.NewReader(readFile("/etc/passwd")))
		for sc.Scan() {
			f := strings.Split(sc.Text(), ":")
			if len(f) > 3 {
				if n, err := strconv.ParseUint(f[2], 10, 32); err == nil {
					users[uint32(n)] = f[0]
				}
			}
		}
	})
	if u, ok := users[uid]; ok {
		return u
	}
	return strconv.FormatUint(uint64(uid), 10)
}

// parseStat reads /proc/<pid>/stat. Fields after the ")" are 0-indexed from
// the state field: 1 ppid, 11 utime, 12 stime, 17 threads, 19 starttime, 21 rss.
func parseStat(pid int) (Proc, bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return Proc{}, false
	}
	open := bytes.IndexByte(b, '(')
	close := bytes.LastIndexByte(b, ')')
	if open < 0 || close < open {
		return Proc{}, false
	}
	f := strings.Fields(string(b[close+1:]))
	if len(f) < 22 {
		return Proc{}, false
	}
	num := func(i int) uint64 { n, _ := strconv.ParseUint(f[i], 10, 64); return n }
	p := Proc{Pid: pid, Name: string(b[open+1 : close]), State: f[0]}
	p.PPid = int(num(1))
	p.cpuTime = float64(num(11)+num(12)) / clkTck
	p.Threads = int(num(17))
	p.startTk = num(19)
	p.Start = bootTime.Add(time.Duration(float64(p.startTk) / clkTck * float64(time.Second)))
	p.RSS = num(21) * pageSize
	return p, true
}

func pids() []int {
	d, err := os.Open("/proc")
	if err != nil {
		return nil
	}
	defer d.Close()
	names, _ := d.Readdirnames(-1)
	out := make([]int, 0, len(names))
	for _, n := range names {
		if n[0] < '0' || n[0] > '9' {
			continue
		}
		if pid, err := strconv.Atoi(n); err == nil {
			out = append(out, pid)
		}
	}
	return out
}

func (s *Sampler) processes() []Proc {
	now := time.Now()
	cur := map[int]procKey{}
	var out []Proc
	dt := now.Sub(s.prevProcT).Seconds()
	for _, pid := range pids() {
		p, ok := parseStat(pid)
		if !ok {
			continue
		}
		prev, known := s.prevProcs[pid]
		known = known && prev.start == p.startTk
		if known {
			p.uid = prev.uid // a process keeps its owner; skip the stat
		} else {
			var st syscall.Stat_t
			if syscall.Stat("/proc/"+strconv.Itoa(pid), &st) == nil {
				p.uid = st.Uid
			}
		}
		p.User = UserName(p.uid)
		cur[pid] = procKey{p.startTk, p.cpuTime, p.uid}
		if known && dt > 0.01 {
			p.CPU = 100 * (p.cpuTime - prev.cpu) / dt
		}
		out = append(out, p)
	}
	s.prevProcs, s.prevProcT = cur, now
	return out
}

// Cmdline returns the full command line ("[name]" for kernel threads).
func Cmdline(pid int) string {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil || len(b) == 0 {
		return ""
	}
	return strings.TrimSpace(strings.ReplaceAll(string(bytes.TrimRight(b, "\x00")), "\x00", " "))
}

// IsKernelThread: kthreadd and its children.
func IsKernelThread(p Proc) bool { return p.Pid == 2 || p.PPid == 2 }

// Detail is everything worth knowing about one process.
type Detail struct {
	Proc
	Exe      string   `json:"exe"`
	Cwd      string   `json:"cwd"`
	Unit     string   `json:"unit,omitempty"`
	Cgroup   string   `json:"cgroup,omitempty"`
	FDs      int      `json:"fds"`
	Parents  []string `json:"parents"`
	Children []Proc   `json:"children"`
	Ports    []Port   `json:"ports,omitempty"`
	Windows  []string `json:"windows,omitempty"`
	OOMScore int      `json:"oom_score"`
	IORead   uint64   `json:"io_read"`
	IOWrite  uint64   `json:"io_write"`
}

// Describe builds a Detail; cpu comes from a live Sampler if one is passed.
func Describe(pid int, all []Proc) (*Detail, error) {
	p, ok := parseStat(pid)
	if !ok {
		return nil, fmt.Errorf("no process %d", pid)
	}
	for _, q := range all {
		if q.Pid == pid {
			p.CPU = q.CPU
			p.User = q.User
		}
	}
	if p.User == "" {
		var st syscall.Stat_t
		if syscall.Stat("/proc/"+strconv.Itoa(pid), &st) == nil {
			p.User = UserName(st.Uid)
		}
	}
	p.Cmd = Cmdline(pid)
	d := &Detail{Proc: p}
	base := "/proc/" + strconv.Itoa(pid)
	d.Exe, _ = os.Readlink(base + "/exe")
	d.Cwd, _ = os.Readlink(base + "/cwd")
	if ents, err := os.ReadDir(base + "/fd"); err == nil {
		d.FDs = len(ents)
	}
	d.OOMScore, _ = strconv.Atoi(strings.TrimSpace(string(readFile(base + "/oom_score"))))
	for _, line := range strings.Split(string(readFile(base+"/io")), "\n") {
		if v, ok := strings.CutPrefix(line, "read_bytes: "); ok {
			d.IORead, _ = strconv.ParseUint(v, 10, 64)
		}
		if v, ok := strings.CutPrefix(line, "write_bytes: "); ok {
			d.IOWrite, _ = strconv.ParseUint(v, 10, 64)
		}
	}
	cg := strings.TrimSpace(string(readFile(base + "/cgroup")))
	if i := strings.LastIndex(cg, "::"); i >= 0 {
		cg = cg[i+2:]
	}
	d.Cgroup = cg
	for _, part := range strings.Split(cg, "/") {
		if strings.HasSuffix(part, ".service") || strings.HasSuffix(part, ".scope") {
			d.Unit = part
		}
	}
	// ancestry, up to init
	for cur, hops := p.PPid, 0; cur > 1 && hops < 24; hops++ {
		q, ok := parseStat(cur)
		if !ok {
			break
		}
		d.Parents = append(d.Parents, fmt.Sprintf("%d %s", q.Pid, q.Name))
		cur = q.PPid
	}
	for _, q := range all {
		if q.PPid == pid {
			d.Children = append(d.Children, q)
		}
	}
	sort.Slice(d.Children, func(i, j int) bool { return d.Children[i].CPU > d.Children[j].CPU })
	for _, port := range Ports() {
		if port.Pid == pid {
			d.Ports = append(d.Ports, port)
		}
	}
	return d, nil
}

// Find resolves a pid or a name/cmdline substring to processes.
func Find(query string, all []Proc) []Proc {
	if pid, err := strconv.Atoi(query); err == nil {
		for _, p := range all {
			if p.Pid == pid {
				return []Proc{p}
			}
		}
		return nil
	}
	q := strings.ToLower(query)
	var hits []Proc
	for _, p := range all {
		if IsKernelThread(p) {
			continue
		}
		if strings.Contains(strings.ToLower(p.Name), q) {
			hits = append(hits, p)
			continue
		}
		if c := Cmdline(p.Pid); strings.Contains(strings.ToLower(c), q) {
			p.Cmd = c
			hits = append(hits, p)
		}
	}
	return hits
}

// FindName is Find without the command-line match, for "is it gone yet":
// a command line that merely mentions the name (the waiting shell) must not count.
func FindName(query string, all []Proc) []Proc {
	if _, err := strconv.Atoi(query); err == nil {
		return Find(query, all)
	}
	q := strings.ToLower(query)
	var hits []Proc
	for _, p := range all {
		if !IsKernelThread(p) && strings.Contains(strings.ToLower(p.Name), q) {
			hits = append(hits, p)
		}
	}
	return hits
}

// Port is a listening socket and who owns it.
type Port struct {
	Proto string `json:"proto"`
	Addr  string `json:"addr"`
	Port  int    `json:"port"`
	Pid   int    `json:"pid"`
	Name  string `json:"name"`
	User  string `json:"user,omitempty"`
}

// Ports lists listening TCP and bound UDP sockets with their owning process.
// Sockets owned by other users show pid 0 unless we can read their fds.
func Ports() []Port {
	inodes := map[uint64]Port{}
	parse := func(path, proto string, v6 bool) {
		sc := bufio.NewScanner(bytes.NewReader(readFile(path)))
		sc.Scan()
		for sc.Scan() {
			f := strings.Fields(sc.Text())
			if len(f) < 10 {
				continue
			}
			if proto == "tcp" && f[3] != "0A" { // LISTEN
				continue
			}
			if proto == "udp" && f[3] != "07" { // unconnected
				continue
			}
			addr, port := decodeAddr(f[1], v6)
			inode, _ := strconv.ParseUint(f[9], 10, 64)
			uid, _ := strconv.ParseUint(f[7], 10, 32)
			inodes[inode] = Port{Proto: proto, Addr: addr, Port: port, User: UserName(uint32(uid))}
		}
	}
	parse("/proc/net/tcp", "tcp", false)
	parse("/proc/net/tcp6", "tcp", true)
	parse("/proc/net/udp", "udp", false)
	parse("/proc/net/udp6", "udp", true)
	if len(inodes) == 0 {
		return nil
	}
	for _, pid := range pids() {
		dir := "/proc/" + strconv.Itoa(pid) + "/fd"
		ents, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range ents {
			link, err := os.Readlink(filepath.Join(dir, e.Name()))
			if err != nil || !strings.HasPrefix(link, "socket:[") {
				continue
			}
			inode, _ := strconv.ParseUint(link[8:len(link)-1], 10, 64)
			if p, ok := inodes[inode]; ok && p.Pid == 0 {
				p.Pid = pid
				if st, ok := parseStat(pid); ok {
					p.Name = st.Name
				}
				inodes[inode] = p
			}
		}
	}
	seen := map[string]bool{}
	var out []Port
	for _, p := range inodes {
		key := fmt.Sprintf("%s|%s|%d|%d", p.Proto, p.Addr, p.Port, p.Pid)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Proto != out[j].Proto {
			return out[i].Proto < out[j].Proto
		}
		return out[i].Port < out[j].Port
	})
	return out
}

func decodeAddr(s string, v6 bool) (string, int) {
	h, p, ok := strings.Cut(s, ":")
	if !ok {
		return s, 0
	}
	port, _ := strconv.ParseUint(p, 16, 32)
	if !v6 && len(h) == 8 {
		n, _ := strconv.ParseUint(h, 16, 32)
		return fmt.Sprintf("%d.%d.%d.%d", n&0xff, n>>8&0xff, n>>16&0xff, n>>24), int(port)
	}
	switch h {
	case "00000000000000000000000000000000":
		return "::", int(port)
	case "00000000000000000000000001000000":
		return "::1", int(port)
	}
	if strings.HasPrefix(h, "0000000000000000FFFF0000") && len(h) == 32 {
		n, _ := strconv.ParseUint(h[24:], 16, 32)
		return fmt.Sprintf("%d.%d.%d.%d", n&0xff, n>>8&0xff, n>>16&0xff, n>>24), int(port)
	}
	return "[" + strings.ToLower(h) + "]", int(port)
}

// Processes lists every process once (no CPU rates; use a Sampler for those).
func Processes() []Proc {
	var s Sampler
	return s.processes()
}
