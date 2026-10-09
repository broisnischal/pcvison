# pc

`pc` lets an agent see and drive my Linux desktop, and tells it what the machine is
doing. It is one static Go binary (about 4 MB, no runtime, no cgo) that is three things:

- a CLI: `pc shot`, `pc click 640 360`, `pc state`, `pc events`, ...
- a resident daemon that holds everything expensive warm (`pc daemon`, started on demand)
- an MCP server: `pc mcp`, so Claude gets the same commands as tools with images inline

It replaces my earlier Python `usepc` skill and `pcvision` MCP server.

## Install

```bash
make install        # or: bash install.sh
```

That builds `plugin/bin/pc`, links it to `~/.local/bin/pc`, adds this checkout as the
`pc` plugin marketplace, installs the `pc` plugin (skill + MCP server + `pc` on the Bash
PATH) and unregisters the old `pcvision` / `usepc`. In a running Claude Code session,
`/reload-plugins` picks it up. `make test` runs vet and the tests; `bash install.sh
--uninstall` takes it all out again.

The marketplace is a local directory, so Claude Code loads the plugin in place: a
`make build` followed by `pc daemon restart` and `/reload-plugins` is the whole update loop.

## What it does

### The machine

```
pc state      # load, memory, disks, net, power, temps, busiest processes, focus, PROBLEMS
pc events     # last 15m: failed/flapping services, crashes, OOM kills, processes and windows
pc ps         # processes with live CPU% (--sort mem|new|name, -n 40, a filter)
pc proc zen   # one process: cmdline, cwd, unit, parents, children, ports, windows, its log
pc units      # failed systemd units, system and user (running | all | <name>)
pc logs nginx # journal for a unit; --level err, --since 1h, --grep timeout
pc ports 5432 # what listens where
pc sys        # hardware and OS inventory
pc top        # all of the above, live, in a terminal
```

`PROBLEMS` is the point of `pc state`: on this machine it immediately listed four failed
units, a service restarting 33 times an hour, and a root disk at 99%.

### The desktop

```
pc windows                 # monitors, windows, focus, both cursors
pc shot                    # focused monitor; -w firefox, -m HDMI-A-2, -r "X,Y WxH", --all
pc click 412 230           # x,y = pixels of the last screenshot, nothing to convert
pc type -w firefox "hello" # focuses without moving my pointer, verifies, then types
pc key ctrl+l              # any combo: ctrl+shift+t, super+2, alt+F4, "ctrl+a Delete"
pc paste "long text"       # via the clipboard, and my clipboard is put back after
pc scroll 600 400 --down 5 · pc drag 10 10 300 300 · pc hover 50 50
pc open alacritty          # launches and waits for the window
pc do 'click 412 230; type hi --enter; wait idle'
pc wait window firefox     # or: gone <q>, port 8000, change, idle
pc watch start             # opt-in recorder of my screen; timeline / view / stop --purge
pc input off               # kill switch; only I turn it back on
```

## The agent's cursor

The agent gets a pointer of its own on my screen: a clone of my cursor (same theme,
same size), drawn on an overlay that clicks pass through. It fades in, glides to
whatever the agent is about to touch along a smooth, hand-like curve locked to the
display refresh, ripples when it clicks, and fades out when the agent goes quiet. While
the agent types, a badge next to it says "AI is typing" (or "AI is pasting", or the
shortcut it pressed). `pc cursor speed instant|fast|normal|smooth` sets the glide.

Hyprland has exactly one seat, and a seat has exactly one cursor, so a click has to
use my real pointer. pc sends warp, press, release and warp back as one write on the
Wayland socket. Hyprland handles the whole burst before it draws the next frame, so my
pointer never visibly leaves where I put it. Keyboard focus stays on the window the
agent clicked, because `follow_mouse` is pinned to 0 for those few milliseconds.

If I am holding Ctrl, Super or Alt, pc waits and then refuses instead of acting:
Hyprland merges every keyboard's modifiers, so the agent's typing would turn into
shortcuts.

## Speed

Measured on this machine, warm daemon, two 1920x1080 monitors:

| command | time |
|---|---|
| `pc state`, `pc ps`, `pc windows` | 3-5 ms |
| `pc shot` (1280px JPEG) | 26 ms |
| `pc type` 37-53 characters, `pc key` | 3-4 ms |
| `pc click` (plus the glide: ~200 ms for 440 px, 280 ms at most) | 7-8 ms |

The old Python path took hundreds of milliseconds per screenshot (grim PNG, Pillow
decode, re-encode) and spawned `hyprctl` several times per action.

## Why input stopped working before

Omarchy moved Hyprland to the Lua config. With `hyprland.lua`, `hyprctl dispatch
movecursor ...` and `focuswindow ...` fail with a Lua syntax error and `hyprctl keyword`
is refused, so pcvision could no longer move the pointer, focus a window or switch
workspace. It also needed a per-boot ACL on `/dev/uinput` for mouse buttons. pc talks
to the IPC socket in whichever dialect is running and drives input through Wayland
protocols that need no permissions.

`plugin/skills/pc/references/design.md` has the rest of the reasoning, with numbers.

## Layout

```
cmd/pc            CLI entry, `pc daemon run`, `pc mcp`, `pc top`
internal/wl       Wayland client: wire protocol, screencopy, virtual pointer/keyboard,
                  layer-shell surface, data-control clipboard
internal/hypr     Hyprland IPC, Lua and legacy dialects
internal/daemon   the resident half: actions, shots, the agent cursor, monitor, watch
internal/sys      /proc, /sys, journal, systemd, ports, held-key guard
internal/spec     the one command table behind the CLI, MCP tools and `pc do`
internal/mcp      stdio MCP server
plugin/           the Claude Code plugin: skill, .mcp.json, bin/pc (built)
```

Linux with Hyprland only. The process, service and log commands work on any systemd
Linux; the desktop half needs Hyprland 0.5x.
