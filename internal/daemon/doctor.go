package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"pc/internal/hypr"
	"pc/internal/rpc"
	"pc/internal/sys"
	"pc/internal/xcur"
)

func init() { register("doctor", cmdDoctor) }

func cmdDoctor(d *Daemon, a Args) (*rpc.Response, error) {
	var b strings.Builder
	ok := func(good bool, what, detail string) {
		mark := "✓"
		if !good {
			mark = "✗"
		}
		fmt.Fprintf(&b, " %s %-18s %s\n", mark, what, detail)
	}
	fmt.Fprintf(&b, "pc daemon pid %d, up %s\n", os.Getpid(), human(time.Since(d.started)))
	sig := hypr.Signature()
	ok(sig != "", "hyprland ipc", orStr(sig, "no instance found (pc needs Hyprland for windows, focus and cursor position)"))
	if sig != "" {
		dialect := "hyprlang config (legacy dispatchers)"
		if hypr.Lua() {
			dialect = "lua config (dispatch via hl.dsp, no hyprctl keyword)"
		}
		ok(true, "hyprland", hypr.Version()+" · "+dialect)
		fm := hypr.FollowMouse()
		ok(true, "follow_mouse", fmt.Sprintf("%d (pinned to 3 for the few ms a click borrows the pointer, so nothing is focused or raised)", fm))
	}
	disp := waylandDisplay()
	ok(disp != "", "wayland socket", orStr(disp, "none found"))
	s, err := d.session()
	if err != nil {
		ok(false, "wayland session", err.Error())
	} else {
		for _, part := range []struct {
			name string
			have bool
			why  string
		}{
			{"virtual-pointer", s.ptr != nil, "clicks, scrolls, drags (no /dev/uinput needed)"},
			{"keymap", s.km != nil, "the user's layout, for keys sent to the agent's window through the compositor (send_shortcut)"},
			{"screencopy", s.sc != nil, "screenshots without grim"},
			{"layer-shell", s.shell != nil, "the agent's on-screen cursor"},
			{"clipboard", s.clip != nil, "paste and clipboard read/write (data-control)"},
		} {
			detail := part.why
			if !part.have {
				detail = s.errs[part.name] + "; " + part.why
			}
			ok(part.have, part.name, detail)
		}
		names := []string{}
		for _, o := range s.outs.All() {
			names = append(names, fmt.Sprintf("%s %dx%d", o.Name, o.ModeW, o.ModeH))
		}
		ok(len(names) > 0, "outputs", strings.Join(names, ", "))
	}
	if img, err := xcur.Load(xcur.Theme(), xcur.Size()); err == nil {
		ok(true, "cursor theme", fmt.Sprintf("%s/%s at %dpx (the agent cursor clones it)", img.Theme, img.Name, img.Nominal))
	} else {
		ok(false, "cursor theme", err.Error())
	}
	h := sys.UserHands()
	ok(h.Keyboards > 0 && h.Pointers > 0, "hands monitor", fmt.Sprintf("%d keyboard(s), %d pointing device(s): the agent waits while the user holds a button or modifier, and lets go if they move mid-hover/drag (needs the input group)", h.Keyboards, h.Pointers))
	if inputAllowed() != nil {
		ok(false, "input", "switched OFF by the user (pc input on)")
	} else {
		ok(true, "input", "on")
	}
	_, jerr := exec.LookPath("journalctl")
	ok(jerr == nil, "journal", "service failures, crashes, OOM kills, logs")
	if out, err := exec.Command("journalctl", "-n", "1", "-q", "--no-pager").Output(); err != nil || len(out) == 0 {
		ok(false, "journal access", "cannot read the system journal (add the user to systemd-journal or wheel)")
	}
	_, ferr := exec.LookPath("ffmpeg")
	ok(ferr == nil, "ffmpeg", "only for pc watch clip (mp4); everything else is built in")
	fmt.Fprintf(&b, "cache %s · socket %s", CacheDir(), rpc.SocketPath())
	return text(b.String()), nil
}
