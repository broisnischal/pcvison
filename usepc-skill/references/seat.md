# Why the seat is built this way

Notes for anyone (human or agent) changing the pointer or seat code. These are the
approaches that were tried on a real Hyprland 0.56 machine, and what happened.

## The constraint everything follows from

**One seat, one cursor.** A wlroots compositor renders exactly one cursor per
`wl_seat`, and Hyprland only ever creates `seat0`. Every one of these moves *the
user's* cursor:

- `/dev/uinput` virtual mouse — libinput binds it to the existing seat
- `zwlr_virtual_pointer_v1` against the user's compositor — same seat
- `hyprctl dispatch movecursor` — warps the one cursor there is
- `xdotool mousemove`, `CGWarpMouseCursorPosition`, `SetCursorPos` — likewise

So "give the agent a second cursor without disturbing mine" cannot be solved on the
user's seat at all. It needs a second seat, and only the OS can create one.

Rejected on those grounds:
- *Multi-seat via `libseat`/`loginctl attach`* — Hyprland does not implement more
  than one seat.
- *X11 MPX* (`xinput create-master`) — genuinely gives a second visible cursor, but
  only on an X server. Kept as the X11 path; useless under Wayland.
- *Warp, click, warp back* — works, disturbs the user for ~200 ms. Kept, but only
  behind `--real`.

## What won: a nested compositor

`Hyprland` launched with `WAYLAND_DISPLAY` pointing at the user's session starts on
the **aquamarine wayland backend**: it renders into one window and creates a whole
new instance — new `wl_seat`, new cursor, new keyboard, new clipboard, new IPC
socket. Verified: moving the agent pointer left `hyprctl cursorpos` on the host
unchanged at `2431,495`.

Input goes in over protocols, not devices:

| need | mechanism | notes |
|---|---|---|
| pointer | `zwlr_virtual_pointer_v1` on the seat's socket | `scripts/vpointer.py`, pure stdlib |
| keyboard | `zwp_virtual_keyboard_v1` via `wtype` | works with **no host focus** |
| clipboard | `wl-copy` with `WAYLAND_DISPLAY` set to the seat | separate from the user's |
| capture | `grim -c` on the seat's socket | `-s` scales, `-t jpeg` encodes; no Pillow |
| windows | `hyprctl -i <signature>` | never the bare `hyprctl` |

That the keyboard needs no host focus is the load-bearing detail. It is what lets
the seat window stay `nofocus`.

## Two bugs that cost the user real pain — do not reintroduce

**1. The seat stole SUPER.** A focused nested compositor receives every key the
user presses, so their SUPER shortcuts silently stopped working. `hyprctl setprop
… nofocus 1` *after* the window mapped was too late — it had already taken the
keyboard.

The fix, in `_start_hyprland`, is to register the rules **before** launching:

```
hyprctl keyword windowrule "noinitialfocus, class:^(aquamarine)$"
hyprctl keyword windowrule "nofocus, class:^(aquamarine)$"
```

Also do **not** "restore" the user's focus with `hyprctl dispatch focuswindow`
afterwards: on Hyprland that warps their cursor, which is the exact thing this tool
promises not to do. With `noinitialfocus` there is nothing to restore.

`usepc seat handoff` / `reclaim` flip `nofocus` per-window when the human actually
wants to type in the seat.

**2. Clicks landed in the wrong place.** The config said `monitor = WL-1, …` but the
nested backend named its output `WAYLAND-1`, so the rule never applied and the
output inherited the host's `scale = 2`. A 1280×720 window reported itself as
970×550 logical, and every coordinate was off by that ratio.

Fix: an empty output name matches whatever the backend calls it, and scale is
pinned to 1 so logical and pixel sizes agree:

```
monitor = , {width}x{height}@60, 0x0, 1
```

`geometry()` still divides by the reported scale, and reads it **live** every time,
because the nested output resizes with its host window.

## Other things worth knowing

- `misc { vfr = false }` in the seat config. With variable refresh the idle nested
  compositor stops drawing, and captures go stale.
- `cursor { no_hardware_cursors = true }`, or the agent's cursor is missing from its
  own screenshots.
- `ecosystem { no_update_news = true }`, or Hyprland opens a release-notes window
  inside the seat on version changes.
- `xwayland { enabled = false }` by default — it roughly doubles startup. Enable
  with `--xwayland` when an X11 app has to run in the seat.
- Seat startup measured ~1.1 s. It is reused across commands, so only the first
  action pays; the rest are tens of milliseconds.
- Hiding the seat window (special workspace) stops the host sending frame
  callbacks, so the nested compositor stops rendering and captures freeze. Keep it
  visible; dim it instead.

## The translucent cursor

`scripts/overlay.py` is a `wlr-layer-shell` surface on the overlay layer with an
**empty input region** — visible, but it swallows no clicks. One surface per
monitor; it draws only on the monitor containing the point. Needs
`gtk4-layer-shell` + PyGObject + pycairo; everything else works without it.

It is a *marker*, not a pointer: it shows where the agent is acting during a
`--real` takeover. The agent's actual pointer lives in the seat.
