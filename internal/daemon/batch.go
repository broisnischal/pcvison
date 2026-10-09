package daemon

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"pc/internal/hypr"
	"pc/internal/rpc"
	"pc/internal/spec"
	"pc/internal/sys"
)

func init() {
	register("do", cmdDo)
	register("wait", cmdWait)
}

// cmdDo runs a script of actions in one request. Every coordinate in it refers
// to the screenshot that was current when the script started, even if a step
// in the middle takes another one.
func cmdDo(d *Daemon, a Args) (*rpc.Response, error) {
	cmds, err := spec.Split(a.Str("script"))
	if err != nil {
		return nil, err
	}
	if len(cmds) == 0 {
		return nil, errors.New("empty script")
	}
	pinned := ""
	if sh, err := d.shots.get(""); err == nil {
		pinned = sh.ID
	}
	var out []string
	var images []rpc.Image
	lastX, lastY := 0.0, 0.0
	for i, words := range cmds {
		name := words[0]
		if _, err := strconv.ParseFloat(name, 64); err == nil {
			words = append([]string{"wait"}, words...) // a bare number is a pause
			name = "wait"
		}
		c, ok := spec.Find(name)
		if !ok || name == "do" {
			return partial(out, images, fmt.Errorf("step %d: unknown command %q", i+1, name))
		}
		args, err := c.Parse(words[1:])
		if err != nil {
			return partial(out, images, fmt.Errorf("step %d: %w", i+1, err))
		}
		if pinned != "" && !hasKey(args, "abs") && !hasKey(args, "shot") {
			args["shot"] = pinned
		}
		if c.Input {
			if err := inputAllowed(); err != nil {
				return partial(out, images, err)
			}
		}
		h := handlers[c.Name]
		if h == nil {
			return partial(out, images, fmt.Errorf("step %d: %s cannot run inside do", i+1, name))
		}
		r, err := h(d, Args(args))
		if err != nil {
			return partial(out, images, fmt.Errorf("step %d (%s): %w", i+1, strings.Join(words, " "), err))
		}
		if r != nil {
			out = append(out, fmt.Sprintf("%d. %s", i+1, firstLineOf(r.Text)))
			images = append(images, r.Images...)
		}
		if x, y, ok := d.ghost.position(); ok {
			lastX, lastY = x, y
		}
		if i+1 < len(cmds) {
			time.Sleep(40 * time.Millisecond) // let the UI take each step in
		}
	}
	resp := &rpc.Response{Text: strings.Join(out, "\n"), Images: images}
	if a.Bool("look") {
		if lastX == 0 && lastY == 0 {
			lastX, lastY, _ = hypr.CursorPos()
		}
		resp = d.withLook(a, lastX, lastY, resp)
	}
	return resp, nil
}

func hasKey(m map[string]any, k string) bool { _, ok := m[k]; return ok }

func firstLineOf(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

func partial(done []string, images []rpc.Image, err error) (*rpc.Response, error) {
	r := &rpc.Response{Images: images}
	if len(done) > 0 {
		r.Text = strings.Join(done, "\n") + "\nstopped:"
	}
	return r, err
}

// cmdWait polls for a condition instead of the agent sleeping a guessed time.
func cmdWait(d *Daemon, a Args) (*rpc.Response, error) {
	what := a.Strs("what")
	if len(what) == 1 {
		what = strings.Fields(what[0])
	}
	if len(what) == 0 {
		return nil, errors.New("wait for what? window <q> | gone <q> | port <n> | change | idle | <seconds>")
	}
	timeout := 10.0
	if v, ok := a.Num("timeout"); ok {
		timeout = v
	}
	deadline := time.Now().Add(time.Duration(timeout * float64(time.Second)))
	start := time.Now()
	if secs, err := strconv.ParseFloat(what[0], 64); err == nil {
		time.Sleep(time.Duration(min(secs, 120) * float64(time.Second)))
		return text(fmt.Sprintf("waited %gs", secs)), nil
	}
	arg := strings.Join(what[1:], " ")
	took := func() string { return human(time.Since(start)) }
	switch what[0] {
	case "window":
		for {
			if c, err := findWindow(arg); err == nil {
				return text(fmt.Sprintf("window %s is there (after %s): ws %s at %s", describeWindow(c), took(), c.Workspace.Name, c.Box())), nil
			}
			if time.Now().After(deadline) {
				return nil, fmt.Errorf("no window matching %q after %gs", arg, timeout)
			}
			time.Sleep(80 * time.Millisecond)
		}
	case "gone":
		for {
			_, werr := findWindow(arg)
			procs := sys.FindName(arg, sys.Processes())
			if werr != nil && len(procs) == 0 {
				return text(fmt.Sprintf("%q is gone (after %s)", arg, took())), nil
			}
			if time.Now().After(deadline) {
				return nil, fmt.Errorf("%q still there after %gs (%d processes)", arg, timeout, len(procs))
			}
			time.Sleep(200 * time.Millisecond)
		}
	case "port":
		port, err := strconv.Atoi(arg)
		if err != nil {
			return nil, fmt.Errorf("port %q is not a number", arg)
		}
		for {
			for _, host := range []string{"127.0.0.1", "::1"} {
				if c, err := net.DialTimeout("tcp", net.JoinHostPort(host, arg), 200*time.Millisecond); err == nil {
					c.Close()
					return text(fmt.Sprintf("port %d is accepting connections (after %s)", port, took())), nil
				}
			}
			if time.Now().After(deadline) {
				return nil, fmt.Errorf("nothing listening on port %d after %gs", port, timeout)
			}
			time.Sleep(150 * time.Millisecond)
		}
	case "change", "idle", "settle", "still":
		r, _, err := targetFrom(Args{}).resolve()
		if err != nil {
			return nil, err
		}
		base, err := d.grab(r, 480, false)
		if err != nil {
			return nil, err
		}
		prev := signature(base)
		stillSince := time.Now()
		for {
			time.Sleep(100 * time.Millisecond)
			m, err := d.grab(r, 480, false)
			if err != nil {
				return nil, err
			}
			sig := signature(m)
			diff := sigDiff(prev, sig)
			if what[0] == "change" {
				if diff > 0.004 {
					return text(fmt.Sprintf("the screen changed (after %s)", took())), nil
				}
			} else {
				if diff > 0.002 {
					stillSince = time.Now()
					prev = sig
				} else if time.Since(stillSince) > 700*time.Millisecond {
					return text(fmt.Sprintf("the screen is still (after %s)", took())), nil
				}
			}
			if time.Now().After(deadline) {
				return nil, fmt.Errorf("screen did not %s within %gs", map[bool]string{true: "change", false: "settle"}[what[0] == "change"], timeout)
			}
		}
	}
	return nil, fmt.Errorf("wait: unknown condition %q (window, gone, port, change, idle, or seconds)", what[0])
}
