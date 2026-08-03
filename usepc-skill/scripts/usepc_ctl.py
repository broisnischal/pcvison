#!/usr/bin/env python3
"""usepc — drive and watch this machine without stealing the human's hands.

Two rules the whole tool is built around:

  1. Pointer and keyboard actions go to the *agent's own seat* by default. The
     user's cursor is never moved, because the agent's pointer is not on the
     user's seat at all.
  2. Touching the user's actual desktop takes an explicit `--real`, and even then
     the cursor is put back where they left it.

Everything is stdlib, so it starts in ~30 ms with the system python.
"""

from __future__ import annotations

import argparse
import json
import os
import shlex
import shutil
import subprocess
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import sysinfo  # noqa: E402

STATE_DIR = Path(os.environ.get("USEPC_HOME") or (Path.home() / ".cache" / "usepc"))
STATE_FILE = STATE_DIR / "state.json"
ACTION_LOG = STATE_DIR / "actions.log"
MONITOR_PID = STATE_DIR / "monitor.pid"
SHOT_DIR = STATE_DIR / "shots"
TMUX_SESSION = "usepc-monitor"


class Fail(RuntimeError):
    pass


def seat_module():
    if sysinfo.IS_LINUX:
        import seat_linux
        return seat_linux
    if sysinfo.IS_MAC:
        import seat_macos
        return seat_macos
    import seat_windows
    return seat_windows


def log_action(text: str) -> None:
    STATE_DIR.mkdir(parents=True, exist_ok=True)
    with ACTION_LOG.open("a") as handle:
        handle.write(f"{time.strftime('%H:%M:%S')}  {text}\n")
    if ACTION_LOG.stat().st_size > 200_000:  # keep the tail, drop the history
        lines = ACTION_LOG.read_text(errors="replace").splitlines()[-400:]
        ACTION_LOG.write_text("\n".join(lines) + "\n")


# --------------------------------------------------------------------- the seat


def cmd_seat(args) -> int:
    seat = seat_module()
    action = args.action
    if action == "start":
        state = seat.start(width=args.width, height=args.height, opacity=args.opacity,
                           workspace=args.workspace, xwayland=args.xwayland)
        log_action(f"seat start {state.get('display')} {args.width}x{args.height}")
        print(f"agent seat up on {state.get('display')} ({state.get('backend')}), "
              f"pid {state.get('pid')}")
        print("its cursor is its own — your pointer is not involved")
        return 0
    if action == "stop":
        print(seat.stop())
        log_action("seat stop")
        return 0
    if action == "status":
        state = seat.status()
        if args.json:
            print(json.dumps(state, indent=1, default=str))
            return 0
        if not state.get("running"):
            print(f"agent seat: not running (backend available: {state.get('backend')})")
            return 0
        try:
            width, height = seat.geometry()
            size = f"{width}x{height}"
        except Exception:
            size = "?"
        print(f"agent seat: running · {state['backend']} · display {state.get('display')} · "
              f"{size} · pid {state.get('pid')}")
        for window in seat.windows():
            mark = "*" if window.get("focused") else " "
            print(f" {mark} {window.get('class', '?')}: {str(window.get('title', ''))[:60]}")
        return 0
    if action == "open":
        if not args.rest:
            raise Fail("usepc seat open <command>")
        command = " ".join(args.rest)
        seat.ensure()
        pid = seat.spawn(command)
        log_action(f"seat open {command}")
        print(f"launched in the agent seat (pid {pid}): {command}")
        return 0
    if action == "handoff":
        print(seat.handoff())
        return 0
    if action == "reclaim":
        print(seat.reclaim())
        return 0
    if action == "opacity":
        value = float(args.rest[0]) if args.rest else 0.85
        print(seat.set_opacity(value))
        return 0
    raise Fail(f"unknown seat action {action!r}")


# ------------------------------------------------------------------ acting


def _resolve(args, seat) -> tuple[float, float] | tuple[None, None]:
    """Map coordinates read off a screenshot back to real ones.

    Screenshots are downscaled, so a click measured on the image lands short and
    up-left unless the sent width is declared with --from-width.
    """
    if args.x is None:
        return None, None
    x, y = float(args.x), float(args.y)
    if args.from_width:
        if args.real:
            width = _real_layout()[0]
        else:
            width = seat.geometry()[0]
        factor = width / float(args.from_width)
        x, y = x * factor, y * factor
    return x, y


def _real_layout() -> tuple[int, int]:
    if sysinfo.IS_LINUX and shutil.which("hyprctl"):
        monitors = json.loads(subprocess.run(["hyprctl", "monitors", "-j"], capture_output=True,
                                             text=True, timeout=4).stdout)
        right = max(m["x"] + int(m["width"] / (m.get("scale") or 1)) for m in monitors)
        bottom = max(m["y"] + int(m["height"] / (m.get("scale") or 1)) for m in monitors)
        return right, bottom
    import real
    return real.screen_size()


def cmd_act(args) -> int:
    verb = args.verb
    if args.real:
        import real
        target, where = real, "the user's desktop"
    else:
        target, where = seat_module(), "the agent seat"
        target.ensure()

    x, y = _resolve(args, target)
    if verb == "move":
        target.move(x, y)
        note = f"{x:.0f},{y:.0f}"
    elif verb == "click":
        target.click(x, y, button=args.button, count=args.count)
        note = f"{args.button}×{args.count} at {'cursor' if x is None else f'{x:.0f},{y:.0f}'}"
    elif verb == "scroll":
        target.scroll(args.amount, x, y, horizontal=args.horizontal)
        note = f"{args.amount:+d}{' horizontal' if args.horizontal else ''}"
    elif verb == "drag":
        if args.to is None:
            raise Fail("drag needs --to X,Y")
        x2, y2 = (float(v) for v in args.to.split(","))
        if args.from_width:
            factor = (target.geometry()[0] if not args.real else _real_layout()[0]) / float(args.from_width)
            x2, y2 = x2 * factor, y2 * factor
        target.drag(x, y, x2, y2, button=args.button)
        note = f"{x:.0f},{y:.0f} → {x2:.0f},{y2:.0f}"
    elif verb == "type":
        if not args.rest:
            raise Fail("usepc type <text>")
        text = " ".join(args.rest)
        target.type_text(text, delay_ms=args.delay)
        note = f"{len(text)} chars"
    elif verb == "keys":
        if not args.rest:
            raise Fail("usepc keys ctrl+shift+t")
        for combo in args.rest:
            target.press(combo)
        note = " ".join(args.rest)
    elif verb == "paste":
        if not args.rest:
            raise Fail("usepc paste <text>")
        text = " ".join(args.rest)
        target.paste(text)
        note = f"{len(text)} chars via clipboard"
    else:
        raise Fail(f"unknown verb {verb!r}")

    log_action(f"{'REAL ' if args.real else ''}{verb} {note}")
    print(f"{verb}: {note} — in {where}")
    if args.shot:
        print(_capture(args, target))
    return 0


def _capture(args, target) -> str:
    SHOT_DIR.mkdir(parents=True, exist_ok=True)
    dest = SHOT_DIR / f"{'real' if args.real else 'seat'}-{time.strftime('%H%M%S')}.jpg"
    scale = {"low": 0.42, "normal": 0.66, "high": 0.85, "full": 1.0}[args.detail]
    if args.real:
        import real
        return str(real.shot(dest, scale=scale))
    return str(target.shot(dest, scale=scale, window=args.window, region=args.region))


def cmd_shot(args) -> int:
    target = seat_module()
    if not args.real:
        target.ensure()
    path = _capture(args, target)
    sent_width = None
    try:
        width = (_real_layout() if args.real else target.geometry())[0]
        sent_width = int(width * {"low": 0.42, "normal": 0.66, "high": 0.85, "full": 1.0}[args.detail])
    except Exception:
        pass
    print(path)
    if sent_width:
        print(f"# {sent_width}px wide — pass --from-width {sent_width} when clicking off this image")
    return 0


def cmd_windows(args) -> int:
    if args.real:
        info = sysinfo.desktop()
        print(f"{info.get('compositor', '?')} · {info.get('window_count', '?')} windows · "
              f"focus: {info.get('active', '?')}")
        for line in info.get("windows", []):
            print(f"  {line}")
        return 0
    seat = seat_module()
    seat.ensure()
    windows = seat.windows()
    if not windows:
        print("the agent seat is empty — `usepc seat open <app>` to put something in it")
        return 0
    for window in windows:
        mark = "*" if window.get("focused") else " "
        print(f"{mark} {window.get('class', '?')}: {str(window.get('title', ''))[:70]}  "
              f"at {window.get('at')} size {window.get('size')}")
    return 0


def cmd_focus(args) -> int:
    seat = seat_module()
    seat.ensure()
    print(seat.focus(" ".join(args.rest)))
    return 0


# ------------------------------------------------------------------ the monitor


def cmd_monitor(args) -> int:
    if args.action == "run":
        import monitor
        return monitor.run(args.interval)

    if args.action == "start":
        if _monitor_alive():
            print(f"monitor already running (pid {MONITOR_PID.read_text().strip()})")
            return 0
        return _monitor_start(args)

    if args.action == "stop":
        pid = _monitor_alive()
        if shutil.which("tmux"):
            subprocess.run(["tmux", "kill-session", "-t", TMUX_SESSION], capture_output=True)
        if pid:
            try:
                os.kill(pid, 15)
            except Exception:
                pass
        MONITOR_PID.unlink(missing_ok=True)
        print("monitor stopped" if pid else "monitor was not running")
        return 0

    if args.action == "attach":
        if not shutil.which("tmux"):
            raise Fail("no tmux — run `usepc monitor run` in a shell instead")
        os.execvp("tmux", ["tmux", "attach", "-t", TMUX_SESSION])

    # status
    pid = _monitor_alive()
    age = None
    if STATE_FILE.exists():
        age = time.time() - STATE_FILE.stat().st_mtime
    if pid:
        print(f"monitor running (pid {pid})" + (f", state {age:.1f}s old" if age else ""))
    else:
        print("monitor not running" + (f" (last state {age:.0f}s old)" if age else ""))
    return 0


def _monitor_alive() -> int | None:
    try:
        pid = int(MONITOR_PID.read_text().strip())
        os.kill(pid, 0)
        return pid
    except Exception:
        return None


def _terminal_command(inner: list[str]) -> list[str] | None:
    for term, flag in (("alacritty", "-e"), ("foot", "-e"), ("kitty", "-e"),
                       ("ghostty", "-e"), ("wezterm", "start"), ("xterm", "-e"),
                       ("gnome-terminal", "--")):
        if shutil.which(term):
            if term == "wezterm":
                return [term, "start", "--"] + inner
            return [term, flag] + inner
    return None


def _monitor_start(args) -> int:
    STATE_DIR.mkdir(parents=True, exist_ok=True)
    self_cli = [sys.executable, str(Path(__file__).resolve()), "monitor", "run",
                "--interval", str(args.interval)]
    use_tmux = shutil.which("tmux") and not args.no_tmux

    if use_tmux:
        subprocess.run(["tmux", "kill-session", "-t", TMUX_SESSION], capture_output=True)
        subprocess.run(["tmux", "new-session", "-d", "-s", TMUX_SESSION, "-x", "120", "-y", "38",
                        shlex.join(self_cli) if hasattr(shlex, "join")
                        else " ".join(shlex.quote(part) for part in self_cli)], check=True)
        inner = ["tmux", "attach", "-t", TMUX_SESSION]
    else:
        inner = self_cli

    if args.here:
        os.execvp(inner[0], inner)

    launcher = _terminal_command(inner) if _has_display() else None
    if launcher:
        proc = subprocess.Popen(launcher, start_new_session=True,
                                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        MONITOR_PID.write_text(str(proc.pid))
        _dress_monitor_window(proc.pid, args.opacity)
        print(f"monitor spawned in {launcher[0]}"
              + (f" (tmux session {TMUX_SESSION})" if use_tmux else ""))
    elif use_tmux:
        MONITOR_PID.write_text("0")
        print(f"monitor running detached in tmux — attach with: tmux attach -t {TMUX_SESSION}")
    else:
        print("no terminal emulator and no tmux — run `usepc monitor run` in a shell yourself")
        return 1
    print(f"state file for cheap reads: {STATE_FILE}")
    return 0


def _has_display() -> bool:
    return bool(os.environ.get("WAYLAND_DISPLAY") or os.environ.get("DISPLAY")) or sysinfo.IS_WIN


def _dress_monitor_window(pid: int, opacity: float) -> None:
    """Float and dim the monitor window on Hyprland — cosmetic, best effort."""
    if not (sysinfo.IS_LINUX and shutil.which("hyprctl")):
        return
    for _ in range(15):
        time.sleep(0.25)
        try:
            clients = json.loads(subprocess.run(["hyprctl", "clients", "-j"], capture_output=True,
                                                text=True, timeout=4).stdout)
        except Exception:
            return
        match = next((c for c in clients if c.get("pid") == pid), None)
        if not match:
            continue
        target = f"address:{match['address']}"
        for step in (["dispatch", "setfloating", target],
                     ["setprop", target, "opacity", f"{opacity:.2f}", "lock"],
                     ["dispatch", "resizewindowpixel", f"exact 900 700,{target}"]):
            subprocess.run(["hyprctl"] + step, capture_output=True, timeout=4)
        return


# ------------------------------------------------------------------ state, cheap


def cmd_state(args) -> int:
    fresh = None
    if STATE_FILE.exists():
        age = time.time() - STATE_FILE.stat().st_mtime
        if age < 3.0:  # the monitor is running and its reading is current
            fresh = json.loads(STATE_FILE.read_text())
    data = fresh or sysinfo.snapshot(full=args.full)
    if not fresh:
        data["seat"] = _seat_brief()
    if args.json:
        print(json.dumps(data, indent=1, default=str))
        return 0

    cpu, mem, net = data["cpu"], data["memory"], data["network"]
    desktop = data.get("desktop") or {}
    seat = data.get("seat") or {}
    host = data["host"]
    print(f"{host['hostname']} · {host['pretty']} · up {sysinfo.human_time(data.get('uptime'))}"
          f"{'' if fresh else ' (sampled now)'}")
    load = data.get("load")
    cpu_line = f"cpu {_num(cpu.get('percent'))}%  of {cpu.get('cores')} cores"
    if cpu.get("temp_c"):
        cpu_line += "  {:.0f}°C".format(cpu["temp_c"])
    if load:
        cpu_line += "  load {:.2f}".format(load[0])
    print(cpu_line)
    mem_line = "mem {}%  {}/{}".format(_num(mem.get("percent")),
                                       sysinfo.human_bytes(mem.get("used")),
                                       sysinfo.human_bytes(mem.get("total")))
    if mem.get("swap_used"):
        mem_line += "  swap " + sysinfo.human_bytes(mem.get("swap_used"))
    print(mem_line)
    if data.get("gpu"):
        print(f"gpu {_num(data['gpu'].get('percent'))}%")
    print(f"net ↓{sysinfo.human_bytes(net.get('rx_rate'))}/s ↑{sysinfo.human_bytes(net.get('tx_rate'))}/s")
    for disk in data.get("disks", [])[:2]:
        print(f"disk {disk['mount']} {_num(disk.get('percent'))}% used "
              f"of {sysinfo.human_bytes(disk.get('total'))}")
    if data.get("battery"):
        print(f"battery {data['battery']['percent']}% {data['battery']['state']}")
    top = ", ".join(f"{p['name']} {p['cpu']:.0f}%" for p in data.get("processes", [])[:4])
    if top:
        print(f"busiest: {top}")
    print(f"desktop {desktop.get('compositor', '?')} · {desktop.get('window_count', '?')} windows "
          f"· focus: {desktop.get('active', '?')}")
    if seat.get("running"):
        print(f"agent seat running · {seat.get('display')} · {seat.get('geometry', '?')} "
              f"· {seat.get('windows', 0)} windows (own cursor)")
    else:
        print("agent seat not running (auto-starts on the first pointer action)")
    if args.full:
        for line in (desktop.get("windows") or [])[:12]:
            print(f"  {line}")
    return 0


def _num(value) -> str:
    return "--" if value is None else f"{value:.0f}"


def _seat_brief() -> dict:
    try:
        seat = seat_module()
        state = seat.status()
        if state.get("running"):
            try:
                width, height = seat.geometry()
                state["geometry"] = f"{width}x{height}"
                state["windows"] = len(seat.windows())
            except Exception:
                pass
        return state
    except Exception as exc:
        return {"running": False, "error": str(exc)[:80]}


# ------------------------------------------------------------------ overlay cursor


def cmd_cursor(args) -> int:
    import overlay
    if args.action == "start":
        if overlay.daemon_pid():
            print(f"cursor overlay already running (pid {overlay.daemon_pid()})")
            return 0
        proc = subprocess.Popen([sys.executable, str(Path(__file__).parent / "overlay.py")],
                                start_new_session=True,
                                stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
        time.sleep(1.2)
        if proc.poll() is not None:
            sys.stderr.write((proc.stderr.read() or b"").decode(errors="replace"))
            return 2
        print("translucent agent cursor running — `usepc cursor move X Y` to place it")
        return 0
    if args.action == "stop":
        overlay.write_command(quit=True)
        time.sleep(0.2)
        print(overlay.stop_daemon())
        return 0
    if args.action == "move":
        if len(args.rest) < 2:
            raise Fail("usepc cursor move X Y")
        overlay.write_command(x=float(args.rest[0]), y=float(args.rest[1]), visible=True,
                              alpha=args.alpha, label=args.label)
        print(f"agent cursor shown at {args.rest[0]},{args.rest[1]} (alpha {args.alpha})")
        return 0
    if args.action == "hide":
        overlay.write_command(visible=False)
        print("agent cursor hidden")
        return 0
    if args.action == "status":
        pid = overlay.daemon_pid()
        print(f"cursor overlay {'running, pid ' + str(pid) if pid else 'not running'}")
        return 0
    raise Fail(f"unknown cursor action {args.action!r}")


# ------------------------------------------------------------------ doctor


def cmd_doctor(args) -> int:
    print(f"platform  {sysinfo._pretty_os()} ({sysinfo.OS})")
    rows: list[tuple[str, bool, str]] = []
    if sysinfo.IS_LINUX:
        session = "wayland" if os.environ.get("WAYLAND_DISPLAY") else (
            "x11" if os.environ.get("DISPLAY") else "none")
        print(f"session   {session}")
        for tool, why in (("Hyprland", "hosts the agent's own seat (nested)"),
                          ("hyprctl", "window and monitor geometry"),
                          ("grim", "capture the seat"),
                          ("wtype", "type into the seat"),
                          ("wl-copy", "the seat's own clipboard"),
                          ("tmux", "keeps the monitor alive in the background"),
                          ("Xephyr", "agent seat on X11 desktops (alternative)"),
                          ("xdotool", "drive an X11 agent seat")):
            rows.append((tool, bool(shutil.which(tool)), why))
    elif sysinfo.IS_MAC:
        for tool, why in (("screencapture", "capture"), ("osascript", "windows and apps"),
                          ("tmux", "background monitor")):
            rows.append((tool, bool(shutil.which(tool)), why))
    else:
        for tool, why in (("powershell", "metrics"), ("pwsh", "metrics (7+)")):
            rows.append((tool, bool(shutil.which(tool)), why))

    for tool, present, why in rows:
        print(f"  {'✓' if present else '·'} {tool:<12} {why}")

    print()
    seat = seat_module()
    backend = seat.backend()
    print(f"agent seat backend: {backend}")
    if backend == "none":
        print("  ! no isolated seat available — see references/<os>.md for what to install")
    state = seat.status()
    print(f"agent seat state:   {'running' if state.get('running') else 'stopped'}")

    if sysinfo.IS_LINUX and os.environ.get("WAYLAND_DISPLAY"):
        import vpointer
        can = vpointer.supported(os.environ["WAYLAND_DISPLAY"])
        print(f"virtual pointer:    {'available' if can else 'NOT available'} on the user's session")
        print("                    (only used with --real; the seat has its own)")

    try:
        import gi  # noqa: F401
        gi.require_version("Gtk4LayerShell", "1.0")
        overlay_ok = True
    except Exception:
        overlay_ok = False
    print(f"translucent cursor: {'available' if overlay_ok else 'needs gtk4-layer-shell + pygobject'}")
    print(f"monitor:            {'running' if _monitor_alive() else 'stopped'}")
    print(f"state dir:          {STATE_DIR}")
    return 0


# ------------------------------------------------------------------ argument wiring


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="usepc",
        description="watch this machine, and drive it from a seat of the agent's own")
    subs = parser.add_subparsers(dest="command", required=True)

    def add_target_flags(sub):
        sub.add_argument("--real", action="store_true",
                         help="act on the USER'S desktop instead of the agent seat "
                              "(moves their cursor, then puts it back)")
        sub.add_argument("--from-width", type=int, default=None,
                         help="width of the screenshot the coordinates were read off")
        sub.add_argument("--shot", action="store_true", help="capture afterwards and print the path")
        sub.add_argument("--detail", choices=("low", "normal", "high", "full"), default="normal")
        sub.add_argument("--window", default=None)
        sub.add_argument("--region", default=None)

    seat_parser = subs.add_parser("seat", help="the agent's own cursor, keyboard and screen")
    seat_parser.add_argument("action", choices=("start", "stop", "status", "open", "handoff",
                                               "reclaim", "opacity"))
    seat_parser.add_argument("rest", nargs="*")
    seat_parser.add_argument("--width", type=int, default=1600)
    seat_parser.add_argument("--height", type=int, default=900)
    seat_parser.add_argument("--opacity", type=float, default=0.85)
    seat_parser.add_argument("--workspace", default=None,
                             help="park the seat window on a workspace, e.g. special:usepc")
    seat_parser.add_argument("--xwayland", action="store_true", help="allow X11 apps in the seat")
    seat_parser.add_argument("--json", action="store_true")
    seat_parser.set_defaults(func=cmd_seat)

    for verb, help_text in (("click", "click in the seat"), ("move", "move the seat's pointer"),
                            ("scroll", "scroll in the seat"), ("drag", "drag in the seat"),
                            ("type", "type text into the seat"),
                            ("keys", "press key combos in the seat"),
                            ("paste", "paste via the seat's clipboard")):
        act = subs.add_parser(verb, help=help_text)
        act.set_defaults(func=cmd_act, verb=verb)
        if verb in ("click", "move", "scroll", "drag"):
            act.add_argument("x", nargs="?", type=float)
            act.add_argument("y", nargs="?", type=float)
        if verb in ("click", "drag"):
            act.add_argument("--button", default="left",
                             choices=("left", "right", "middle", "back", "forward"))
        if verb == "click":
            act.add_argument("--count", type=int, default=1)
        if verb == "drag":
            act.add_argument("--to", default=None, help="X,Y to drag to")
            act.add_argument("--count", type=int, default=1)
        if verb == "scroll":
            act.add_argument("--amount", type=int, default=3,
                             help="wheel notches; positive scrolls down")
            act.add_argument("--horizontal", action="store_true")
        if verb in ("type", "keys", "paste"):
            act.add_argument("rest", nargs="*")
            act.add_argument("--delay", type=int, default=12)
            act.set_defaults(x=None, y=None)  # keyboard verbs carry no coordinates
        else:
            act.set_defaults(rest=[])
        add_target_flags(act)

    shot = subs.add_parser("shot", help="capture the agent seat (or --real for the user's screen)")
    shot.set_defaults(func=cmd_shot)
    add_target_flags(shot)

    windows = subs.add_parser("windows", help="what is open in the seat, or --real for the desktop")
    windows.set_defaults(func=cmd_windows)
    windows.add_argument("--real", action="store_true")

    focus = subs.add_parser("focus", help="focus a window inside the seat by name")
    focus.add_argument("rest", nargs="+")
    focus.set_defaults(func=cmd_focus)

    monitor_parser = subs.add_parser("monitor", help="the live dashboard in a shell")
    monitor_parser.add_argument("action", nargs="?", default="status",
                                choices=("start", "stop", "status", "attach", "run"))
    monitor_parser.add_argument("--interval", type=float, default=1.0)
    monitor_parser.add_argument("--here", action="store_true",
                                help="take over this terminal instead of spawning one")
    monitor_parser.add_argument("--no-tmux", action="store_true")
    monitor_parser.add_argument("--opacity", type=float, default=0.92)
    monitor_parser.set_defaults(func=cmd_monitor)

    state = subs.add_parser("state", help="one cheap textual reading of the machine")
    state.add_argument("--json", action="store_true")
    state.add_argument("--full", action="store_true")
    state.set_defaults(func=cmd_state)

    cursor = subs.add_parser("cursor", help="translucent agent cursor on the user's screen")
    cursor.add_argument("action", choices=("start", "stop", "move", "hide", "status"))
    cursor.add_argument("rest", nargs="*")
    cursor.add_argument("--alpha", type=float, default=0.45)
    cursor.add_argument("--label", default="agent")
    cursor.set_defaults(func=cmd_cursor)

    doctor = subs.add_parser("doctor", help="what works on this machine and what is missing")
    doctor.set_defaults(func=cmd_doctor)

    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return args.func(args)
    except Fail as exc:
        sys.stderr.write(f"usepc: {exc}\n")
        return 2
    except KeyboardInterrupt:
        return 130
    except Exception as exc:
        sys.stderr.write(f"usepc: {type(exc).__name__}: {exc}\n")
        return 1


if __name__ == "__main__":
    sys.exit(main())
