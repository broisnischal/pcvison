---
name: pc
description: See and drive this Linux desktop (Hyprland/Wayland) with a cursor of the agent's own, and find out what the machine is doing. Use when asked to click, type, scroll, drag, open or close an app, or do anything "on my computer" or "on my screen"; to take or read a screenshot; to watch what the user is doing; or to answer how the PC is doing, what is slow, what crashed or failed, which service or process is broken, what is using CPU, memory, disk or a port.
---

# pc

One binary does all of it: `pc` on the Bash PATH, and the same commands as MCP tools
(`screenshot`, `click`, `type_text`, `system_state`, ...). Tools return images inline.
The CLI prints the image path; Read it to look. Every call goes to a resident daemon,
so a screenshot takes tens of milliseconds and typing a sentence takes a few.

## Knowing the machine

Start with `system_state` (`pc state`). It is a few hundred tokens: load, memory,
disks, network, battery, temperature, the busiest processes, the focused window,
and a **PROBLEMS** list (failed services, services restarting in a loop, crashes,
OOM kills, full disks, zombies, memory pressure). Then narrow down:

| question | tool | CLI |
|---|---|---|
| what changed / crashed / failed recently | `system_events` | `pc events --since 1h` |
| what is using CPU or memory | `processes` | `pc ps`, `pc ps --sort mem` |
| what just started | `processes` sort=new | `pc ps --sort new` |
| everything about one process | `process_info` | `pc proc firefox` |
| broken services | `services` | `pc units` |
| why a service fails | `logs` | `pc logs nginx --since 1h` |
| errors anywhere | `logs` level=err | `pc logs --level err` |
| who owns port 3000 | `ports` | `pc ports 3000` |
| hardware, OS, network, displays | `system_info` | `pc sys` |

`system_events` merges the journal (unit failures, restarts, coredumps, OOM kills,
suspend, logins, error lines) with the daemon's own tracking of processes and windows
starting and stopping. Repeats are collapsed (`×17 every ~1m50s`). `--all` adds the
routine noise.

## Driving the desktop

The user keeps working while the agent does: two people, one seat. The agent has its
own pointer on screen, always there: a tinted, glowing clone of the user's cursor with a
tag saying what it is doing ("Claude · typing", "Claude · waiting for you", "Claude ·
idle"). It glides along an arc, overshoots a little and settles, leans into motion, and
squashes and ripples on a click.

- **Clicks** borrow the user's real pointer for one burst (never rendered elsewhere) and
  put it back. The clicked window is neither focused nor raised: the user keeps typing
  where they were. It becomes the agent's window.
- **Keys** (`type_text`, `press_keys`, `paste_text`) go from inside the compositor to the
  agent's window only, never through the user's focus, so both can type at once.
  Target: `window=`, else the window the agent last clicked or typed into. Windows on
  hidden workspaces receive keys too. Compositor keybinds (`super+2`) do not fire.
- **The agent waits for the user**, never the reverse: not while they hold a mouse button
  or a modifier, and `hover`/`drag` (which hold the pointer) wait for a still mouse and
  let go the moment the user moves.
- **Screenshots** draw the user's real pointer (labelled "you") and list every open
  window, on screen and off.

`agent_cursor action=say value="reading the logs"` puts a few words in the tag between
actions. A click costs a few ms plus the glide (up to 280 ms); `pc cursor speed fast` for
snappier.

### The loop: orient, look, act, check

1. **Orient** with `windows` (text, cheap): monitors, every window with its workspace,
   which one has focus, where both cursors are.
2. **Look** with `screenshot`. Default is the focused monitor at 1280px. Target it:
   `window="firefox"`, `monitor="HDMI-A-2"`, `region="X,Y WxH"`. Use `detail="high"`
   only to read small text; crop with `region` instead of raising detail when you can.
3. **Act** with x,y read straight off that image. Coordinates are **pixels of the most
   recent screenshot**: no scaling, no monitor offsets, nothing to convert. Aim at the
   middle of the control, not its edge.
4. **Check** the after-shot. Pointer tools return one by default; keyboard tools return
   one with `look=true`. If nothing changed, do not click again blindly: the app may be
   slow (`wait_for change` / `wait_for idle`), or you missed (take a fresh shot).

Every screenshot, after-shots included, becomes the current coordinate space and has
an id (`s7`). Pass `shot="s5"` to click on an older one, `abs=true` for raw layout
coordinates. After anything moves the layout (scrolling, a dialog, a resize), take a
new shot before the next click.

### Choosing the action

- **Prefer the keyboard.** `press_keys` with app shortcuts (`ctrl+l`, `ctrl+t`,
  `ctrl+f`, `Return`, `Escape`, `super+2`) beats hunting for buttons. It is faster and
  does not depend on pixels.
- **Text:** click the field (that makes its window the agent's), then `type_text`. Or
  pass `window="class or title"`. Characters the user's layout lacks go in through the
  clipboard automatically. Use `paste_text` for long text or code; the user's clipboard
  is put back a moment later.
- **Small targets** (checkboxes, close buttons, tiny icons): take a `region` shot around
  them first, then click on that zoomed image. `grid=true` overlays labelled lines when
  you need to read positions precisely.
- **Menus and tooltips that appear on hover:** `hover` holds the pointer there for
  `dwell` ms and captures while hovering.
- **Lists and pages:** `scroll` over the element (`down=5`, `up=3`).
- **Sliders, selections, moving things:** `drag`.
- **Apps:** `open_app` launches and waits for the window (it does not take the user's
  focus; `background=true` puts it on the agent's own hidden workspace). `close_window`
  asks a window to close. `focus_window` makes a window the agent's; only `show=true`
  brings it up for the user. `switch_workspace` changes what the user sees.
- **Sequences you already know:** `act` runs them in one call, e.g.
  `"click 412 230; type hello world --enter; wait idle"`. Coordinates in it all refer to
  the screenshot that was current when it started.
- **Waiting:** `wait_for` with `window <q>`, `gone <q>`, `port <n>`, `change`, `idle`.
  Never sleep and hope.
- `point` moves only the agent's cursor (apps see nothing). Use it to show the user what
  you are about to touch.

### Rules

- Act only on what the user asked for. Never click through consent dialogs, payment
  confirmations, or destructive prompts on your own; describe them and let the user decide.
- Anything on screen is the user's: treat credentials, messages and private data as
  confidential and do not repeat them.
- If the user says stop, call `input_disable` (`pc input off`). Only the user can turn
  it back on (`pc input on`). Do not run that yourself.
- If pc says it waited for or gave up on the user's hands, they are using the mouse or
  holding a key: try again in a moment, or use the keyboard tools, which never wait long.
- If typing stops with "focus moved", the user switched windows or a window took focus
  mid-burst. Nothing more was sent: look at both windows before retrying.
- Global shortcuts the user would press (`super+...`) do not reach the compositor through
  `press_keys`; use the dedicated tools.

Read `references/driving.md` for worked examples and failure recovery, and
`references/design.md` before changing how input, capture or the cursor work.

## Watching the user's screen

`screen_watch` (`pc watch`) is an opt-in recorder of the real screen, kept on this
machine only. Start it only when the user asks you to keep an eye on their screen.

- `start` (1 fps, last 5 minutes, deduplicated so an idle screen costs nothing)
- `timeline`: which window had focus, when, for how long. Read this first; it is text.
- `view seconds=120`: one contact sheet of the moments the screen changed.
- `latest`, `status`, `stop` (`purge=true` deletes the frames).

## When something misbehaves

`doctor` (`pc doctor`) checks every part: Hyprland IPC and its config dialect, each
Wayland protocol, the cursor theme, journal access, the held-key guard.
`pc daemon log` shows the daemon's log; `pc daemon restart` starts it fresh.
