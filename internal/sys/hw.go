package sys

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Inventory is the static-ish picture of the machine.
type Inventory struct {
	Host     Host      `json:"host"`
	Boot     time.Time `json:"booted"`
	CPUModel string    `json:"cpu_model"`
	Cores    int       `json:"cores"`
	Threads  int       `json:"threads"`
	MemTotal uint64    `json:"mem_total"`
	GPUs     []string  `json:"gpus"`
	Drives   []Drive   `json:"drives"`
	Disks    []Disk    `json:"filesystems"`
	Ifaces   []Iface   `json:"interfaces"`
	Gateway  string    `json:"gateway,omitempty"`
	DNS      []string  `json:"dns,omitempty"`
	Battery  *Battery  `json:"battery,omitempty"`
	Session  string    `json:"session"`
	Shell    string    `json:"shell"`
	Init     string    `json:"init"`
	Packages string    `json:"packages,omitempty"`
}

type Drive struct {
	Name  string `json:"name"`
	Model string `json:"model"`
	Size  uint64 `json:"size"`
	SSD   bool   `json:"ssd"`
}

type Iface struct {
	Name  string   `json:"name"`
	Up    bool     `json:"up"`
	Addrs []string `json:"addrs"`
	MAC   string   `json:"mac,omitempty"`
	Kind  string   `json:"kind"`
}

func trim(p string) string { return strings.TrimSpace(string(readFile(p))) }

// Inspect gathers the inventory. Only cheap reads; a couple of optional tools.
func Inspect() Inventory {
	inv := Inventory{Host: HostInfo(), Boot: bootTime}
	cores := map[string]bool{}
	for _, line := range strings.Split(string(readFile("/proc/cpuinfo")), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch k {
		case "model name":
			inv.CPUModel = v
			inv.Threads++
		case "core id":
			cores[v] = true
		}
	}
	inv.Cores = len(cores)
	inv.MemTotal = memory().Total

	cards, _ := filepath.Glob("/sys/class/drm/card[0-9]")
	for _, c := range cards {
		vendor := trim(c + "/device/vendor")
		name := map[string]string{"0x1002": "AMD", "0x10de": "NVIDIA", "0x8086": "Intel"}[vendor]
		if name == "" {
			name = vendor
		}
		driver, _ := os.Readlink(c + "/device/driver")
		inv.GPUs = append(inv.GPUs, fmt.Sprintf("%s %s (driver %s)", filepath.Base(c), name, filepath.Base(driver)))
	}
	if b, err := run(2*time.Second, "lspci", "-mm"); err == nil {
		inv.GPUs = inv.GPUs[:0]
		for _, line := range strings.Split(string(b), "\n") {
			if strings.Contains(line, "VGA") || strings.Contains(line, "3D controller") || strings.Contains(line, "Display controller") {
				f := strings.Split(line, `"`)
				if len(f) >= 6 {
					inv.GPUs = append(inv.GPUs, f[3]+" "+f[5])
				}
			}
		}
	}

	blocks, _ := filepath.Glob("/sys/block/*")
	for _, b := range blocks {
		name := filepath.Base(b)
		if strings.HasPrefix(name, "loop") || strings.HasPrefix(name, "ram") || strings.HasPrefix(name, "zram") || strings.HasPrefix(name, "dm-") {
			continue
		}
		sectors, _ := strconv.ParseUint(trim(b+"/size"), 10, 64)
		if sectors == 0 {
			continue
		}
		inv.Drives = append(inv.Drives, Drive{Name: name, Model: trim(b + "/device/model"),
			Size: sectors * 512, SSD: trim(b+"/queue/rotational") == "0"})
	}
	inv.Disks = Disks()

	ifs, _ := net.Interfaces()
	for _, i := range ifs {
		if i.Name == "lo" {
			continue
		}
		it := Iface{Name: i.Name, Up: i.Flags&net.FlagUp != 0, MAC: i.HardwareAddr.String(), Kind: "ethernet"}
		if _, err := os.Stat("/sys/class/net/" + i.Name + "/wireless"); err == nil {
			it.Kind = "wifi"
		} else if virtualIface(i.Name) {
			it.Kind = "virtual"
		}
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLinkLocalUnicast() {
				it.Addrs = append(it.Addrs, a.String())
			}
		}
		if it.Kind == "virtual" && len(it.Addrs) == 0 {
			continue
		}
		inv.Ifaces = append(inv.Ifaces, it)
	}
	for _, line := range strings.Split(string(readFile("/proc/net/route")), "\n")[1:] {
		f := strings.Fields(line)
		if len(f) > 2 && f[1] == "00000000" {
			n, _ := strconv.ParseUint(f[2], 16, 32)
			inv.Gateway = fmt.Sprintf("%d.%d.%d.%d via %s", n&0xff, n>>8&0xff, n>>16&0xff, n>>24, f[0])
			break
		}
	}
	for _, line := range strings.Split(string(readFile("/etc/resolv.conf")), "\n") {
		if v, ok := strings.CutPrefix(line, "nameserver "); ok {
			inv.DNS = append(inv.DNS, strings.TrimSpace(v))
		}
	}
	inv.Battery = Power()
	inv.Session = strings.TrimSpace(os.Getenv("XDG_CURRENT_DESKTOP") + " " + os.Getenv("XDG_SESSION_TYPE"))
	inv.Shell = os.Getenv("SHELL")
	if comm := trim("/proc/1/comm"); comm != "" {
		inv.Init = comm
	}
	if ents, err := os.ReadDir("/var/lib/pacman/local"); err == nil {
		inv.Packages = strconv.Itoa(len(ents)-1) + " (pacman)"
	} else if ents, err := os.ReadDir("/var/lib/dpkg/info"); err == nil {
		n := 0
		for _, e := range ents {
			if strings.HasSuffix(e.Name(), ".list") {
				n++
			}
		}
		inv.Packages = strconv.Itoa(n) + " (dpkg)"
	}
	return inv
}
