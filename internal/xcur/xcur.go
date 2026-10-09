// Package xcur loads the user's own cursor image, so the agent's cursor is a
// pixel-for-pixel clone of the one they see, not a lookalike.
package xcur

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Image is one cursor frame: premultiplied ARGB, as little-endian uint32s
// (bytes B, G, R, A), which is exactly wl_shm ARGB8888.
type Image struct {
	W, H        int
	HotX, HotY  int
	Nominal     int
	Pix         []byte
	Theme, Name string
}

// Size is the cursor size the session asks for.
func Size() int {
	for _, k := range []string{"XCURSOR_SIZE", "HYPRCURSOR_SIZE"} {
		if n, err := strconv.Atoi(os.Getenv(k)); err == nil && n > 0 {
			return n
		}
	}
	return 24
}

// Theme is the session's cursor theme.
func Theme() string {
	for _, k := range []string{"XCURSOR_THEME", "HYPRCURSOR_THEME"} {
		if t := os.Getenv(k); t != "" {
			return t
		}
	}
	return "default"
}

func searchPath() []string {
	if p := os.Getenv("XCURSOR_PATH"); p != "" {
		return strings.Split(p, ":")
	}
	home, _ := os.UserHomeDir()
	data := os.Getenv("XDG_DATA_HOME")
	if data == "" {
		data = filepath.Join(home, ".local/share")
	}
	dirs := []string{filepath.Join(data, "icons"), filepath.Join(home, ".icons")}
	xdg := os.Getenv("XDG_DATA_DIRS")
	if xdg == "" {
		xdg = "/usr/local/share:/usr/share"
	}
	for _, d := range strings.Split(xdg, ":") {
		dirs = append(dirs, filepath.Join(d, "icons"))
	}
	return append(dirs, "/usr/share/pixmaps")
}

// Load finds the arrow cursor of the given theme (following Inherits=) at the
// nominal size closest to size.
func Load(theme string, size int) (*Image, error) {
	seen := map[string]bool{}
	queue := []string{theme}
	if theme != "default" {
		queue = append(queue, "default")
	}
	queue = append(queue, "Adwaita", "hicolor")
	dirs := searchPath()
	for len(queue) > 0 {
		t := queue[0]
		queue = queue[1:]
		if seen[t] {
			continue
		}
		seen[t] = true
		for _, d := range dirs {
			for _, name := range []string{"left_ptr", "default", "arrow"} {
				p := filepath.Join(d, t, "cursors", name)
				if img, err := readFile(p, size); err == nil {
					img.Theme, img.Name = t, name
					return img, nil
				}
			}
			queue = append(queue, inherits(filepath.Join(d, t, "index.theme"))...)
		}
	}
	return nil, fmt.Errorf("no arrow cursor found for theme %q", theme)
}

func inherits(index string) []string {
	f, err := os.Open(index)
	if err != nil {
		return nil
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "Inherits") {
			_, v, _ := strings.Cut(line, "=")
			var out []string
			for _, t := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ';' || r == ' ' }) {
				out = append(out, strings.TrimSpace(t))
			}
			return out
		}
	}
	return nil
}

const imageType = 0xfffd0002

func readFile(path string, size int) (*Image, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(b) < 16 || string(b[:4]) != "Xcur" {
		return nil, errors.New("not an Xcursor file")
	}
	le := binary.LittleEndian
	ntoc := int(le.Uint32(b[12:]))
	best, bestPos := -1, 0
	for i := 0; i < ntoc; i++ {
		off := 16 + i*12
		if off+12 > len(b) {
			break
		}
		if le.Uint32(b[off:]) != imageType {
			continue
		}
		nominal := int(le.Uint32(b[off+4:]))
		pos := int(le.Uint32(b[off+8:]))
		if best < 0 || abs(nominal-size) < abs(best-size) {
			best, bestPos = nominal, pos
		}
	}
	if best < 0 || bestPos+36 > len(b) {
		return nil, errors.New("no image in cursor file")
	}
	h := b[bestPos:]
	w, ht := int(le.Uint32(h[16:])), int(le.Uint32(h[20:]))
	n := w * ht * 4
	if w <= 0 || ht <= 0 || w > 1024 || ht > 1024 || 36+n > len(h) {
		return nil, errors.New("corrupt cursor image")
	}
	return &Image{W: w, H: ht, HotX: int(le.Uint32(h[24:])), HotY: int(le.Uint32(h[28:])),
		Nominal: best, Pix: append([]byte(nil), h[36:36+n]...)}, nil
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
