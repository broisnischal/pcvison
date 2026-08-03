---
name: usepc
description: Watch and operate this computer using a cursor of the agent's own, never the user's. Use when asked to monitor the PC (CPU/memory/disk/network/battery/windows), to click, type, scroll or drag, to drive a GUI app, to open something on the desktop, or to run a task "on my computer" — on Linux, macOS or Windows.
---

# usepc

Two capabilities, one rule.

**The rule: the user's pointer is not yours.** Every pointer and keyboard action
goes to the agent's own seat — a separate cursor, keyboard and clipboard that the
user's session does not share. Touching their real desktop takes an explicit
`--real`, and asking first.

Why this is built the way it is: a Wayland compositor, macOS session, or Windows
desktop has exactly **one cursor per seat**. Adding a pointer device to the user's
seat *is* moving the user's cursor — `uinput`, `virtual-pointer` and
`hyprctl movecursor` all do this. A second cursor therefore requires a second
seat, and the tool gets one from the OS rather than faking it.

## Setup

```bash
usepc doctor            # what works here, what is missing, and the exact fix
```

If `usepc` is not on PATH: `bash <skill-dir>/install.sh`.

## Monitoring

```bash
usepc monitor start     # live dashboard in a shell (tmux + a terminal window)
usepc state             # ~15 lines: cpu, mem, net, disk, battery, windows, seat
usepc state --json      # same reading, machine-readable
```

Prefer `usepc state` over a screenshot when the question is "how is the machine
doing" — it costs a few hundred tokens instead of a few thousand. While the
monitor runs, `state` returns its last tick for free; otherwise it samples live.

## Acting — in the agent's own seat

The seat starts by itself on the first action. No setup step.

```bash
usepc seat open firefox          # launch an app inside the seat
usepc shot                       # capture the seat; prints the path + --from-width
usepc click 640 360              # click, with the agent's own cursor
usepc click 640 360 --from-width 845    # coordinates read off a downscaled shot
usepc type "hello"               # types into the seat's focused window
usepc keys ctrl+shift+t          # key combos
usepc paste "long text"          # via the seat's clipboard, not the user's
usepc scroll --amount 5          # positive scrolls down
usepc drag 100 200 --to 400 500
usepc windows                    # what is open in the seat
usepc focus firefox              # focus a window inside the seat
usepc seat status | stop
```

**Coordinates.** Screenshots are downscaled. `usepc shot` prints the width it
sent — pass it back as `--from-width N` or every click lands short and up-left.

**Look before and after.** `--shot` on any action captures the result: 
`usepc click 640 360 --shot`. Check it worked instead of assuming.

## The seat window

On Linux the seat is a translucent window on the user's desktop — they watch the
agent work in it. It is deliberately `nofocus`: it must never swallow the user's
keystrokes or SUPER shortcuts. The agent does not need that focus, because it types
straight into the seat over the virtual-keyboard protocol.

```bash
usepc seat start --width 1600 --height 900 --opacity 0.85
usepc seat opacity 0.6           # dim it further
usepc seat handoff               # let the human type in it (takes their keybinds)
usepc seat reclaim               # give their keyboard and SUPER binds back
```

## The translucent cursor

A see-through pointer drawn over the user's screen, click-through so it swallows
nothing. Use it to show where you are about to act during a `--real` takeover.

```bash
usepc cursor start
usepc cursor move 1200 700
usepc cursor hide
```

## Touching the user's real desktop

Ask first. Then:

```bash
usepc click 1200 700 --real      # borrows their cursor, puts it back afterwards
usepc shot --real                # their actual screen
usepc windows --real
```

`--real` moves the user's pointer for a few hundred milliseconds and returns it to
where they left it. There is no way around that: one seat, one cursor. If the task
does not truly require their desktop, use the seat.

Never click through consent dialogs, payment confirmations, or destructive prompts
on your own initiative. Describe what is on screen and let the user decide.

## Per-OS specifics

| | agent seat | how |
|---|---|---|
| Linux / Wayland | nested Hyprland | own compositor instance, own seat. Tested. |
| Linux / X11 | Xephyr | nested X server, driven with xdotool. |
| Windows | desktop object | `CreateDesktop` — own input queue and cursor. |
| macOS | bound app | `CGEventPostToPid`; no second cursor exists in a session. |

macOS is the honest exception: there is no second cursor to hand out. Events are
posted to one app's queue without moving the user's pointer, and for real isolation
the answer is a second login session or a VM. See `references/platforms.md` for
per-OS app launching, window management, shells, package managers and keyboard
conventions.

## When something misbehaves

- `usepc doctor` first. It names the missing package.
- The user's SUPER key stops working → a seat window took focus.
  `usepc seat reclaim`, or `usepc seat stop`.
- Clicks land in the wrong place → the `--from-width` was wrong, or the seat was
  resized between the shot and the click. Re-shoot.
- Captures look frozen → the seat window is hidden, so the compositor stopped
  drawing it. Bring it back onto a visible workspace.

Read `references/seat.md` before changing how the seat or the pointer works — it
records which approaches were tried and why the surviving one won.
