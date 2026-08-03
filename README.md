# pcvision

Lets Claude see this machine's screen — screenshots, per-monitor captures, and
short "video" clips. Works as an **MCP server** for Claude Code / Claude Desktop
and as a standalone **CLI**.

Backend: `grim` (capture) · `slurp` (interactive picker) · `hyprctl` (monitor &
window geometry) · `ffmpeg` (mp4). Wayland/wlroots — tested on Hyprland.

## Install

Already done on this machine:

- deps in `./.venv` (`mcp` + `pillow`)
- CLI on PATH: `~/.local/bin/pcvision`
- registered at user scope:

```bash
claude mcp add pcvision -s user -- /home/nees/usepc/.venv/bin/python /home/nees/usepc/pcvision.py serve
```

Restart Claude Code once to pick it up (`claude mcp list` should say ✔ Connected).

For Claude Desktop, add to `~/.config/Claude/claude_desktop_config.json`:

```json
{
  "mcpServers": {
    "pcvision": {
      "command": "/home/nees/usepc/.venv/bin/python",
      "args": ["/home/nees/usepc/pcvision.py", "serve"]
    }
  }
}
```

## Tools Claude gets

| tool | what it does |
| --- | --- |
| `list_monitors` | names, resolution, layout position, focus, active workspace |
| `list_windows` | class/title/workspace/geometry of every open window |
| `screenshot` | one capture — whole desktop, a `monitor`, a `window` (or `"active"`), or a `region` |
| `screenshot_each_monitor` | one labelled image per screen |
| `watch_screen` | N frames spread over T seconds, returned in order — see change/progress |
| `record_clip` | short clip as a single timestamped contact sheet + an `.mp4` path |
| `screenshot_user_selection` | you drag a box (or click a window) with slurp; blocks until you pick |
| `live_start` / `live_stop` / `live_status` | background recorder keeping the last N seconds of screen on disk |
| `live_view` | contact sheet of the last N seconds from that buffer — instant, and skips unchanged frames |
| `live_latest` | freshest buffered frame; cheapest "what's on screen right now" |

`detail`: `low` 800px · `normal` 1280px · `high` 1568px (small text) · `full`
native PNG. Images are downscaled + JPEG-encoded so a look costs ~1–2k tokens
instead of ~30k. Caps: 12 frames, 120 s, 4 fps.

Videos and saved shots land in `~/.cache/pcvision/`.

## "Live" viewing

MCP is request/response — a server cannot push frames at the model. So continuous
watching is two halves:

1. **Nothing is missed** — `live_start` detaches a recorder that writes one small
   JPEG per second into a ring buffer (`~/.cache/pcvision/live/`), pruning past
   `window_seconds`. It runs whether or not Claude is looking. ~1 % of a core,
   ~5 MB of disk for 2 minutes at 1 fps.
2. **Claude looks on a timer** — `live_view` returns the buffered history as one
   contact sheet, so a single cheap call covers everything that happened since
   the last one. `changes_only` drops near-identical frames, so the tiles are
   actual activity, not 30 copies of an idle desktop.

For hands-off narration, pair it with the loop command:

```
/loop 30s check my screen with live_view and tell me if anything needs attention
```

Stop it with `pcvision live stop` (or the `live_stop` tool). Check it is not
still running with `pcvision live status`.

```bash
pcvision live start --fps 1 --window-seconds 180   # or -m HDMI-A-1 / -w cursor
pcvision live view -s 60 -n 6 -o /tmp/sheet.jpg
pcvision live status
pcvision live stop
```

## CLI

```bash
pcvision monitors                       # JSON
pcvision windows                        # JSON
pcvision shot                           # whole desktop -> ~/.cache/pcvision/
pcvision shot -m HDMI-A-1 -o /tmp/a.png
pcvision shot -w active --cursor
pcvision shot --pick                    # drag a region first
pcvision region                         # just print "X,Y WxH"
pcvision watch -s 10 -n 6 --sheet /tmp/sheet.jpg
pcvision record -s 8 -f 2 -o /tmp/clip.mp4 --sheet /tmp/sheet.jpg
pcvision serve                          # MCP stdio (default with no args)
```

Targets are shared by `shot` / `watch` / `record`: `-m/--monitor`,
`-w/--window`, `-r/--region "X,Y WxH"`, `--cursor`.

## Controlling the machine

`pcvision` can also drive the mouse and keyboard. Pointer positioning goes through
`hyprctl dispatch movecursor` (exact layout coordinates, no acceleration curve to
fight); buttons and the wheel come from a `/dev/uinput` virtual mouse created in
process; keystrokes go through `wtype` (virtual-keyboard protocol).

| tool | does |
| --- | --- |
| `cursor_position` | where the pointer is, in layout coordinates |
| `mouse_move` | warp the pointer |
| `mouse_click` | click; `expect_window` aborts if the pointer is over the wrong window |
| `mouse_scroll` | wheel, vertical or horizontal |
| `mouse_drag` | press, move, release — sliders, selections, drag-and-drop |
| `keyboard_type` | type text; `window` focuses and VERIFIES the target first |
| `keyboard_paste` | clipboard + paste shortcut (ctrl+shift+v in terminals) |
| `keyboard_keys` | key combos: `ctrl+shift+t`, `Return`, `alt+F4`, `"ctrl+a Delete"` |
| `focus_window` / `switch_workspace` | aim before typing |
| `input_disable` | Claude's own stop button; only you can undo it |

### Coordinates are the thing that bites

Screenshots are downscaled, and per-monitor shots are monitor-relative. Clicking
what you saw therefore needs both:

- `monitor=` — the shot was of that monitor, so x,y are relative to its top-left
- `from_width=` — the width of the image you measured on

Every screenshot note prints the exact values to pass back. Get it wrong and the
click lands short and up-left, on whatever happens to be there.

### Guardrails

These exist because the first live test typed into a browser URL bar instead of the
intended window: the target had lost focus between the screenshot and the keystroke.

- `keyboard_type` / `keyboard_paste` / `keyboard_keys` take `window=`, which focuses
  it and **confirms** focus landed there, refusing rather than typing blind.
- `mouse_click` takes `expect_window=`, which checks what is actually under the
  pointer and aborts on a mismatch.
- Every input tool returns an "after" screenshot by default — verify, don't assume.
- Every action is appended to `~/.cache/pcvision/input.log` with a timestamp.
- Kill switch: `pcvision input disable` blocks all input immediately. Claude can
  set it but cannot lift it — only `pcvision input enable` does that.

```bash
pcvision input status
pcvision input disable            # and: pcvision input enable
pcvision input log
pcvision input type -w Cursor "hello"      # focus + verify, then type
pcvision input keys -w zen ctrl+l Escape
pcvision input click 960 540 -m HDMI-A-1 -e Chrome
pcvision input move 500 400 -m HDMI-A-2 --from-width 1280
pcvision input scroll -5
pcvision input drag 100 100 400 400
```

If `/dev/uinput` is not writable (mouse buttons fail, typing still works), grant it
once per boot with `sudo setfacl -m u:$USER:rw /dev/uinput`, or permanently:

```
# /etc/udev/rules.d/99-uinput.rules
KERNEL=="uinput", MODE="0660", GROUP="input", OPTIONS+="static_node=uinput"
```

## Notes

- `record_clip` samples with `grim` (≤4 fps), so it captures *moments*, not
  smooth motion. Install `wf-recorder` if you want a real high-fps screencast;
  the contact sheet is what Claude actually reads either way.
- No X11 path: `grim` needs a wlroots compositor. On GNOME/KDE Wayland you'd
  need an `xdg-desktop-portal` backend instead.
- MCP servers start without a login shell, so `bootstrap_env()` recovers
  `WAYLAND_DISPLAY`, `XDG_RUNTIME_DIR` and `HYPRLAND_INSTANCE_SIGNATURE` from
  `/run/user/$UID`.
- Anything on screen goes to the model: passwords, tokens, DMs. Prefer
  `window=`/`region=` over full-desktop captures when something private is open.
- Input control means an agent can act on your machine. It types into whatever has
  focus and clicks whatever is under the pointer; a stale screenshot is enough to
  send keystrokes somewhere unintended. Use `window=` / `expect_window=`, keep the
  audit log honest, and reach for `pcvision input disable` whenever you want it to
  stop.
