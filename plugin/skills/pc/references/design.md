# Why pc is built this way

Notes for whoever changes input, capture or the cursor. Everything here was measured
or read in the source of Hyprland 0.56.2 on the machine pc was built on.

## One binary, one resident daemon

The Python version paid for an interpreter start, several `hyprctl` and `grim`
processes, a PNG encode and a Pillow decode on every call, and sleeps around each
gesture. pc is a static Go binary. The CLI and the MCP server are thin clients; a
daemon (`pc daemon run`, started on demand) owns the Wayland connection, the virtual
pointer and keyboard, the agent's cursor, a warm process sampler and the screen watch.
A request is one line of JSON over a unix socket.

Measured on this machine (two 1920x1080 monitors):

| step | time |
|---|---|
| connect, read the registry, round trip | 3 ms |
| capture one monitor (screencopy into shm) | 5-12 ms |
| area-average downscale to 1280px | 3 ms |
| JPEG encode | 12 ms |
| `pc shot` end to end, 1280px / 800px | 26 ms / 16 ms |
| `pc type` 37-53 characters | 3-4 ms |
| `pc key` | 3 ms |
| `pc click` where the cursor already is | 7-8 ms |
| `pc click` / `pc move` 440 px away (glide included) | ~200 ms |
| `pc state`, `pc ps`, `pc windows` (warm daemon) | 3-5 ms |
| `pc state` on a cold daemon (reads the journal) | ~70 ms |

The held-modifier check first cost 60 ms per action because it reopened the
keyboard devices every time (opening a USB input device wakes it). The devices
now stay open and the check takes microseconds.

## Talking to the compositor directly

- **Wayland wire protocol in Go** (`internal/wl`), no libwayland, no cgo. Object ids
  are reused after `delete_id`: libwayland servers only accept a new id that is free
  or exactly one past the end, and never reusing grows the compositor's table.
- **Hyprland IPC over its socket** (`internal/hypr`), never `hyprctl`.
- **No `/dev/uinput`.** The old pcvision needed a per-boot ACL on it, which is one
  reason input stopped working. `zwlr_virtual_pointer_v1` and
  `zwp_virtual_keyboard_v1` need no permissions.

## Hyprland's Lua config breaks the old dispatchers

With `hyprland.lua`, `hyprctl dispatch X args` becomes `hl.dispatch(X args)` and fails
with a Lua syntax error, and `hyprctl keyword` is refused outright. That silently broke
pcvision's `movecursor`, `focuswindow` and `workspace`. pc detects the dialect
(`eval return 1`) and speaks `hl.dsp.*` or the legacy strings accordingly. Use `eval`,
not `repl`: `repl` returns printed values but hides warnings such as "window not found".

## Two people, one seat

Hyprland has one seat: one pointer and one keyboard focus. The human keeps both.

**Keys** never go through the seat's focus. `type`, `key` and `paste` send each key from
inside the compositor with `hl.dsp.send_shortcut{mods, key = "code:N", window = ...}`
(Hyprland 0.56 `Actions::pass`): the seat's keyboard focus moves to the target surface,
the key goes with exactly the modifiers given, mods are reset, and focus goes back to the
surface Hyprland considers focused, all inside one step of the main loop. Measured: 8
keys into a window on a hidden workspace in 6.6 ms with the user's focus, workspaces and
borders unchanged. Keys the user types at the same moment cannot interleave, and a
modifier they hold does not leak in (the mask is explicit). Costs: the user's window sees
a keyboard leave/enter per key (apps with focus reporting notice), the data device
re-offers the clipboard on each flip, and compositor keybinds do not fire. Keys go in
bursts of 8; before each burst pc checks that the user's focused window is unchanged and
the target still exists, and stops otherwise.

Keycodes come from the keymap the compositor sends every `wl_keyboard`
(`wl.WatchKeymap`, parsed by `internal/xkb`): `!` on a US layout is `SHIFT code:10`.
Characters the layout lacks are pasted.

**Clicks** use the virtual pointer, not `pass`: `pass` stamps every event with the time of
the user's last keybind, so two agent clicks on one spot would read as a double-click. The
burst: `PinPointer` (one Lua `repl` that sets `input:follow_mouse = 3` and returns the old
value and the pointer position), one write with move, press, release, move back, then
follow_mouse restored. With follow_mouse 3, Hyprland neither refocuses nor raises on press
(`processMouseDownNormal`). The old value goes to `restore.json` first and is put back on
the next start if the daemon dies mid-gesture.

**Waiting for the user** (`waitTurn`): never while they hold a mouse button or a modifier
(the compositor would merge it into the agent's pointer events); keys wait up to 1.2 s for
held keys (a held key's autorepeat stops when focus blips). Hover and drag need 300 ms of
stillness and stop the moment the pointer leaves where pc put it, handing it back with
the user's motion kept. Motion is read from the compositor's pointer position as well as
evdev: on this machine smoolyd holds an exclusive grab on both mice and replays them
through a virtual pointer, so no other reader sees their events.

**Activation:** `misc:focus_on_activate` is on here. A window that asks for activation is
focused and the pointer warped to it, across workspaces. In testing, a freshly opened
window on a hidden workspace was activated about 200 ms into typing; the focus check
between bursts exists for that.

**Focus for the user** (`focus --show`, the only path that changes their focus) uses
`focuswindow` with `cursor:no_warps` set for exactly the length of the dispatch, inside
one Lua `eval` with `pcall`, so a failing dispatch still restores it.

## The agent's cursor

An overlay layer-shell surface with an empty input region (clicks pass through), 480x272
logical, the tip at its centre, so the tag, trail and ripple fit around it. Only the
pixels a frame paints are cleared and damaged. Near a monitor edge it is drawn on every
monitor it overlaps (one surface each), so it is never cut at the seam. The image is the
user's XCursor at their size and scale, its light parts tinted in the accent colour
(Claude terracotta by default) over a blurred glow of the same colour.

One render loop advances everything once per frame callback. A glide is a quintic from
the current position and velocity (a new target mid-flight bends the path), along a
slight arc, over a Fitts-style duration (`50 + 40·log2(1 + d/50)` ms capped at 280),
reaching the target at 80% of its time, then swinging up to 9 px past and settling. A
spring leans the arrow into horizontal motion and wobbles it when it stops; the tag
trails on a second spring; a 120 ms comet trail follows fast moves; a click squashes the
arrow and spreads a ring and a pulse from the clicked point. A busy frame paints in about
0.4 ms; a resting cursor costs nothing.

It is always on screen (`pc cursor hide` hides it until `show`), back where it rested
after a restart. Its tag shows the current activity, the agent's note (`say`), or "idle",
dimming a little after 3 s idle.

## Capture

`zwlr_screencopy_manager_v1` into shm buffers cached per size, area-averaged straight
from the compositor's pixel format (8-bit and 10-bit, either channel order) into the
target size across all cores. A region spanning monitors is captured per monitor and
composed. After-shots wait until two consecutive 48x27 signatures match, so they show
the result of an action rather than a half-drawn frame.

A nested Hyprland that is hidden or covered by a fullscreen window stops rendering, and
captures of it never finish. That only affects sandboxes used for testing.

## Process and service tracking

`/proc` is read directly: 684 processes sample in about 15 ms. The daemon diffs samples
every 2 s (5 s when idle) into started/exited events, and Hyprland clients into window
opened/closed events. Service failures, restarts, coredumps and OOM kills come from the
journal by `MESSAGE_ID`. A single unit failure is logged twice ("Failed with result",
"Failed to start"); events within 2 s of each other count once.
