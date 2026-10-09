// pc: see and drive this computer, fast. One static binary that is the CLI,
// the resident daemon (`pc daemon run`) and the MCP server (`pc mcp`).
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"pc/internal/daemon"
	"pc/internal/mcp"
	"pc/internal/rpc"
	"pc/internal/spec"
)

var version = "2.0.0"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(argv []string) int {
	if len(argv) == 0 {
		usage("")
		return 0
	}
	asJSON := false
	var words []string
	for _, w := range argv {
		if w == "--json" {
			asJSON = true
			continue
		}
		words = append(words, w)
	}
	switch words[0] {
	case "-h", "--help", "help":
		topic := ""
		if len(words) > 1 {
			topic = words[1]
		}
		usage(topic)
		return 0
	case "-v", "--version", "version":
		fmt.Println("pc", version)
		return 0
	case "mcp", "serve":
		if err := mcp.Serve(version); err != nil {
			fmt.Fprintln(os.Stderr, "pc mcp:", err)
			return 1
		}
		return 0
	case "daemon":
		return daemonCmd(words[1:])
	case "top":
		return top()
	}
	c, ok := spec.Find(words[0])
	if !ok {
		fmt.Fprintf(os.Stderr, "pc: unknown command %q (pc help)\n", words[0])
		return 2
	}
	args, err := c.Parse(words[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "pc:", err)
		fmt.Fprintln(os.Stderr, "    pc help", c.Name)
		return 2
	}
	timeout := 45 * time.Second
	switch c.Name {
	case "wait", "open", "watch", "do", "events", "logs":
		timeout = 150 * time.Second
	}
	r, err := rpc.Call(rpc.Request{Cmd: c.Name, Args: args}, timeout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pc:", err)
		return 1
	}
	if asJSON {
		out := r.Data
		if out == nil {
			out = r
		}
		b, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(b))
		if !r.OK {
			fmt.Fprintln(os.Stderr, "pc:", r.Error)
			return 1
		}
		return 0
	}
	if r.Text != "" {
		fmt.Println(r.Text)
	}
	for _, im := range r.Images {
		if !strings.Contains(r.Text, im.Path) {
			fmt.Println("image:", im.Path)
		}
	}
	if !r.OK {
		fmt.Fprintln(os.Stderr, "pc:", r.Error)
		return 1
	}
	return 0
}

func daemonCmd(args []string) int {
	action := "status"
	if len(args) > 0 {
		action = args[0]
	}
	switch action {
	case "run":
		err := daemon.Run() // only returns when the listener dies
		fmt.Fprintln(os.Stderr, "pc daemon:", err)
		return 1
	case "start":
		if rpc.Running() {
			fmt.Println("pc daemon already running")
			return 0
		}
		if err := rpc.Start(); err != nil {
			fmt.Fprintln(os.Stderr, "pc:", err)
			return 1
		}
		fmt.Println("pc daemon started")
		return 0
	case "stop", "restart":
		if rpc.Running() {
			_, _ = rpc.Call(rpc.Request{Cmd: "shutdown"}, 3*time.Second)
			for i := 0; i < 100 && rpc.Running(); i++ {
				time.Sleep(20 * time.Millisecond)
			}
			fmt.Println("pc daemon stopped")
		} else if action == "stop" {
			fmt.Println("pc daemon was not running")
		}
		if action == "restart" {
			return daemonCmd([]string{"start"})
		}
		return 0
	case "log":
		b, _ := os.ReadFile(filepath.Join(rpc.Dir(), "daemon.log"))
		lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
		if len(lines) > 40 {
			lines = lines[len(lines)-40:]
		}
		fmt.Println(strings.Join(lines, "\n"))
		return 0
	}
	if !rpc.Running() {
		fmt.Println("pc daemon not running (it starts on the first command)")
		return 0
	}
	r, err := rpc.Call(rpc.Request{Cmd: "ping"}, 3*time.Second)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pc:", err)
		return 1
	}
	fmt.Println(r.Text)
	return 0
}

// top redraws the machine overview every second until q or ctrl+c.
func top() int {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	fmt.Print("\x1b[?1049h\x1b[?25l")
	defer fmt.Print("\x1b[?25h\x1b[?1049l")
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		var b strings.Builder
		b.WriteString("\x1b[H\x1b[2J")
		if r, err := rpc.Call(rpc.Request{Cmd: "state"}, 10*time.Second); err == nil {
			b.WriteString(colorize(r.Text))
		} else {
			b.WriteString("pc: " + err.Error())
		}
		if r, err := rpc.Call(rpc.Request{Cmd: "ps", Args: map[string]any{"limit": 12}}, 10*time.Second); err == nil {
			b.WriteString("\n\n\x1b[36m" + r.Text + "\x1b[0m")
		}
		if r, err := rpc.Call(rpc.Request{Cmd: "events", Args: map[string]any{"since": "10m"}}, 20*time.Second); err == nil {
			lines := strings.Split(r.Text, "\n")
			if len(lines) > 12 {
				lines = append(lines[:1], lines[len(lines)-11:]...)
			}
			b.WriteString("\n\n" + strings.Join(lines, "\n"))
		}
		b.WriteString("\n\n\x1b[2mpc top · ctrl+c to quit · " + time.Now().Format("15:04:05") + "\x1b[0m")
		fmt.Print(b.String())
		select {
		case <-sig:
			return 0
		case <-tick.C:
		}
	}
}

func colorize(s string) string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		switch {
		case strings.HasPrefix(line, "  ! "), strings.HasPrefix(line, "PROBLEMS"):
			line = "\x1b[31m" + line + "\x1b[0m"
		case strings.HasPrefix(line, "problems: none"):
			line = "\x1b[32m" + line + "\x1b[0m"
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func usage(topic string) {
	if topic != "" {
		c, ok := spec.Find(topic)
		if !ok {
			fmt.Printf("no command %q\n", topic)
			return
		}
		fmt.Printf("pc %s", c.Name)
		for _, a := range c.Args {
			if a.Pos {
				if a.Required {
					fmt.Printf(" <%s>", a.Name)
				} else {
					fmt.Printf(" [%s]", a.Name)
				}
			}
		}
		fmt.Printf("\n  %s\n", c.Help)
		if c.TDesc != "" {
			fmt.Printf("\n  %s\n", c.TDesc)
		}
		var flags []string
		for _, a := range c.Args {
			if a.Pos {
				continue
			}
			f := "--" + a.Name
			if a.Short != "" {
				f += ", -" + a.Short
			}
			if a.Type != "bool" {
				f += " " + strings.ToUpper(a.Type)
			}
			flags = append(flags, fmt.Sprintf("  %-26s %s", f, a.Help))
		}
		if len(flags) > 0 {
			fmt.Println("\n" + strings.Join(flags, "\n"))
		}
		return
	}
	fmt.Println(`pc ` + version + `: see and drive this computer, fast

the machine
  state          overview + PROBLEMS (failed/flapping services, crashes, OOM, full disks)
  events         what changed: failures, restarts, crashes, processes, windows (--since 1h)
  ps [q]         processes with live CPU% (--sort mem|new|name, -n 40)
  proc <q>       one process in depth: cmdline, unit, parents, ports, windows, logs
  units [f]      systemd services: failed (default) | running | all | <name>
  logs [unit]    journal (--level err|warn|info, --since 1h, --grep x)
  ports [q]      listening sockets and their processes
  sys            hardware and OS inventory
  top            live dashboard in this terminal

the desktop
  windows        monitors, windows, both cursors
  shot           screenshot (focused monitor; -m MON, -w WIN, -r "X,Y WxH", --all, -d high, --grid)
  watch ...      keep watching the user's screen: start | timeline | view | latest | stop

acting (x,y = pixels of the last screenshot; --abs for layout coordinates)
  click X Y      --double, -b right, --expect WIN, --look
  move X Y       point the agent's cursor without clicking
  hover X Y      hover for tooltips/menus, capture, give the pointer back
  scroll [X Y]   --down N | --up N | --left N | --right N
  drag X1 Y1 X2 Y2
  type TEXT      -w WIN to target a window, --enter
  key COMBO...   ctrl+l, Return, alt+F4, "ctrl+a Delete"
  paste TEXT     via the clipboard, restoring it after
  focus WIN · workspace N · open CMD · close WIN · clip [set TEXT]
  do 'SCRIPT'    several steps in one go: 'click 40 80; type hi --enter; wait idle'
  wait COND      window Q | gone Q | port N | change | idle | SECONDS

the agent
  cursor show|hide|status   input off|on|status   doctor   daemon start|stop|status|log
  mcp            run as an MCP server (stdio)

pc help <command> for its flags · --json on any command for structured output`)
}
