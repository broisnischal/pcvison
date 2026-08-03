"""The monitor that lives in a shell.

Two audiences, one loop. The human gets a redrawing dashboard; the agent gets
`state.json` rewritten every tick, which is a ~300 token read instead of a
screenshot. Plain ANSI rather than curses so the same code runs in Windows
Terminal, and stdlib-only so it starts instantly.
"""

from __future__ import annotations

import json
import os
import shutil
import signal
import sys
import time
from pathlib import Path

import sysinfo

STATE_DIR = Path(os.environ.get("USEPC_HOME") or (Path.home() / ".cache" / "usepc"))
STATE_FILE = STATE_DIR / "state.json"
ACTION_LOG = STATE_DIR / "actions.log"

CSI = "\033["
HIDE, SHOW = f"{CSI}?25l", f"{CSI}?25h"
ALT_ON, ALT_OFF = f"{CSI}?1049h", f"{CSI}?1049l"
CLEAR, HOME = f"{CSI}2J", f"{CSI}H"
RESET = f"{CSI}0m"
DIM, BOLD = f"{CSI}2m", f"{CSI}1m"
FG = {"cyan": f"{CSI}36m", "green": f"{CSI}32m", "yellow": f"{CSI}33m",
      "red": f"{CSI}31m", "blue": f"{CSI}34m", "magenta": f"{CSI}35m",
      "grey": f"{CSI}90m", "white": f"{CSI}97m"}

BLOCKS = " ▏▎▍▌▋▊▉█"


def _colour_for(percent: float | None) -> str:
    if percent is None:
        return FG["grey"]
    if percent >= 90:
        return FG["red"]
    if percent >= 70:
        return FG["yellow"]
    return FG["green"]


def bar(percent: float | None, width: int = 20, colour: str | None = None) -> str:
    if percent is None:
        return f"{FG['grey']}{'·' * width}{RESET}"
    percent = max(0.0, min(100.0, percent))
    filled = percent / 100.0 * width
    whole = int(filled)
    remainder = filled - whole
    tail = BLOCKS[int(remainder * 8)] if whole < width else ""
    body = "█" * whole + tail
    body = body.ljust(width, "·")
    return f"{colour or _colour_for(percent)}{body}{RESET}"


def sparkline(values: list[float]) -> str:
    marks = "▁▂▃▄▅▆▇█"
    return "".join(marks[min(len(marks) - 1, int(max(0.0, min(100.0, v)) / 100.0 * (len(marks) - 1)))]
                   for v in values)


class KeyReader:
    """Non-blocking single keypress, without dragging in curses."""

    def __init__(self) -> None:
        self.enabled = sys.stdin.isatty()
        self._restore = None
        if not self.enabled or sysinfo.IS_WIN:
            return
        try:
            import termios
            import tty
            self._termios = termios
            self._fd = sys.stdin.fileno()
            self._restore = termios.tcgetattr(self._fd)
            tty.setcbreak(self._fd)
        except Exception:
            self.enabled = False

    def get(self) -> str | None:
        if not self.enabled:
            return None
        try:
            if sysinfo.IS_WIN:
                import msvcrt
                return msvcrt.getwch() if msvcrt.kbhit() else None
            import select
            if select.select([sys.stdin], [], [], 0)[0]:
                return sys.stdin.read(1)
        except Exception:
            return None
        return None

    def close(self) -> None:
        if self._restore is not None:
            try:
                self._termios.tcsetattr(self._fd, self._termios.TCSADRAIN, self._restore)
            except Exception:
                pass


def _seat_state() -> dict:
    try:
        if sysinfo.IS_LINUX:
            import seat_linux as seat
        elif sysinfo.IS_MAC:
            import seat_macos as seat
        else:
            import seat_windows as seat
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
        return {"running": False, "error": str(exc)[:60]}


def _recent_actions(limit: int = 5) -> list[str]:
    try:
        lines = ACTION_LOG.read_text(errors="replace").splitlines()
        return lines[-limit:]
    except Exception:
        return []


class Dashboard:
    def __init__(self, interval: float = 1.0) -> None:
        self.interval = interval
        self.sampler = sysinfo.Sampler()
        self.cpu_history: list[float] = []
        self.net_history: list[float] = []
        self.paused = False
        self.started = time.time()

    # ---- rendering -----------------------------------------------------

    def render(self, data: dict, width: int, height: int) -> str:
        out: list[str] = []
        host = data["host"]
        seat = data.get("seat") or {}
        clock = time.strftime("%H:%M:%S")

        title = f" usepc monitor {DIM}·{RESET}{FG['white']} {host['hostname']}{RESET}"
        right = f"{DIM}{host['pretty']} · up {sysinfo.human_time(data.get('uptime'))} · {clock}{RESET}"
        out.append(self._justify(f"{BOLD}{FG['cyan']}{title}", right, width))
        out.append(f"{FG['grey']}{'─' * width}{RESET}")

        cpu = data["cpu"]
        percent = cpu.get("percent")
        self.cpu_history = (self.cpu_history + [percent or 0.0])[-max(8, min(width - 46, 40)):]
        freq = f" {cpu['freq_mhz'] / 1000:.1f}GHz" if cpu.get("freq_mhz") else ""
        temp = f" {cpu['temp_c']:.0f}°C" if cpu.get("temp_c") else ""
        out.append(f" {FG['white']}cpu{RESET}  {bar(percent, 22)} "
                   f"{self._pct(percent)} {DIM}{cpu.get('cores', '?')}c{freq}{temp}{RESET} "
                   f"{FG['blue']}{sparkline(self.cpu_history)}{RESET}")

        per_core = cpu.get("per_core")
        if per_core and height > 20:
            cores = "".join(
                f"{_colour_for(value)}{BLOCKS[min(8, int((value or 0) / 100.0 * 8))]}{RESET}"
                for value in per_core)
            out.append(f" {DIM}core{RESET} {cores}")

        mem = data["memory"]
        out.append(f" {FG['white']}mem{RESET}  {bar(mem.get('percent'), 22)} "
                   f"{self._pct(mem.get('percent'))} "
                   f"{DIM}{sysinfo.human_bytes(mem.get('used'))}/"
                   f"{sysinfo.human_bytes(mem.get('total'))}{RESET}")
        if mem.get("swap_total"):
            swap_percent = 100.0 * mem["swap_used"] / mem["swap_total"]
            out.append(f" {DIM}swap{RESET} {bar(swap_percent, 22)} {self._pct(swap_percent)} "
                       f"{DIM}{sysinfo.human_bytes(mem.get('swap_used'))}{RESET}")

        if data.get("gpu"):
            gpu = data["gpu"]
            extra = ""
            if gpu.get("mem_total"):
                extra = (f"{sysinfo.human_bytes(gpu['mem_used'])}/"
                         f"{sysinfo.human_bytes(gpu['mem_total'])}")
            if gpu.get("temp_c"):
                extra += f" {gpu['temp_c']:.0f}°C"
            out.append(f" {FG['white']}gpu{RESET}  {bar(gpu.get('percent'), 22)} "
                       f"{self._pct(gpu.get('percent'))} {DIM}{extra}{RESET}")

        net = data["network"]
        rx, tx = net.get("rx_rate"), net.get("tx_rate")
        self.net_history = (self.net_history + [min(100.0, ((rx or 0) + (tx or 0)) / 1e6 * 20)])[-30:]
        out.append(f" {FG['white']}net{RESET}  {FG['green']}↓{sysinfo.human_bytes(rx):>7}/s{RESET}  "
                   f"{FG['magenta']}↑{sysinfo.human_bytes(tx):>7}/s{RESET}  "
                   f"{FG['blue']}{sparkline(self.net_history)}{RESET}")

        for disk in data.get("disks", [])[:2]:
            out.append(f" {DIM}disk{RESET} {bar(disk.get('percent'), 22)} "
                       f"{self._pct(disk.get('percent'))} "
                       f"{DIM}{disk['mount']} {sysinfo.human_bytes(disk.get('total'))}{RESET}")

        extras = []
        if data.get("load"):
            extras.append("load " + " ".join(f"{v:.2f}" for v in data["load"]))
        if data.get("battery"):
            battery = data["battery"]
            glyph = "⚡" if battery["state"] in ("charging", "charged") else "▮"
            extras.append(f"{glyph} {battery['percent']}% {battery['state']}")
        if extras:
            out.append(f" {DIM}{' · '.join(extras)}{RESET}")

        out.append("")
        out.append(f" {BOLD}{FG['cyan']}processes{RESET} {FG['grey']}{'─' * max(0, width - 12)}{RESET}")
        for proc in data.get("processes", [])[:6]:
            out.append(f"  {_colour_for(proc['cpu'])}{proc['cpu']:5.1f}%{RESET} "
                       f"{DIM}{sysinfo.human_bytes(proc['rss']):>7}{RESET}  "
                       f"{proc['name'][:28]:<28} {DIM}{proc['pid']}{RESET}")

        desktop = data.get("desktop") or {}
        out.append("")
        out.append(f" {BOLD}{FG['cyan']}desktop{RESET} {FG['grey']}{'─' * max(0, width - 10)}{RESET}")
        monitors = ", ".join(f"{m['name']} {m['resolution']}" for m in desktop.get("monitors", []))
        out.append(f"  {DIM}session{RESET} {desktop.get('compositor', '?')}"
                   f"{'  ' + monitors if monitors else ''}"
                   f"  {DIM}{desktop.get('window_count', '?')} windows{RESET}")
        if desktop.get("active"):
            out.append(f"  {DIM}focus{RESET}   {desktop['active'][:width - 12]}")
        if desktop.get("workspace"):
            out.append(f"  {DIM}wksp{RESET}    {desktop['workspace']}")

        out.append("")
        state_colour = FG["green"] if seat.get("running") else FG["grey"]
        out.append(f" {BOLD}{FG['cyan']}agent seat{RESET} {FG['grey']}{'─' * max(0, width - 13)}{RESET}")
        if seat.get("running"):
            out.append(f"  {state_colour}●{RESET} own cursor, own keyboard, own clipboard "
                       f"{DIM}({seat.get('backend')}){RESET}")
            out.append(f"  {DIM}display{RESET} {seat.get('display')}  "
                       f"{DIM}size{RESET} {seat.get('geometry', '?')}  "
                       f"{DIM}pid{RESET} {seat.get('pid')}  "
                       f"{DIM}windows{RESET} {seat.get('windows', 0)}")
            out.append(f"  {DIM}the user's pointer is untouched by anything the agent does{RESET}")
        else:
            out.append(f"  {FG['grey']}○ not running — starts by itself on the agent's "
                       f"first action{RESET}")

        actions = _recent_actions(4)
        if actions:
            out.append("")
            out.append(f" {BOLD}{FG['cyan']}agent actions{RESET} "
                       f"{FG['grey']}{'─' * max(0, width - 16)}{RESET}")
            for line in actions:
                out.append(f"  {DIM}{line[:width - 4]}{RESET}")

        footer = (f"{DIM} q quit · p {'resume' if self.paused else 'pause'} · "
                  f"+/- interval ({self.interval:g}s){'  ⏸ PAUSED' if self.paused else ''}{RESET}")
        body = out[:max(1, height - 2)]
        body += [""] * max(0, height - 2 - len(body))
        body.append(footer)
        return "\n".join(line[:width + 40] for line in body)

    @staticmethod
    def _pct(value: float | None) -> str:
        return f"{FG['grey']}  --{RESET}" if value is None else f"{value:4.0f}%"

    @staticmethod
    def _justify(left: str, right: str, width: int) -> str:
        visible = len(_strip_ansi(left)) + len(_strip_ansi(right))
        return left + " " * max(1, width - visible) + right

    # ---- the loop ------------------------------------------------------

    def run(self) -> int:
        keys = KeyReader()
        stop = {"now": False}

        def _bye(*_):
            stop["now"] = True

        for sig in (signal.SIGINT, signal.SIGTERM):
            try:
                signal.signal(sig, _bye)
            except Exception:
                pass

        sys.stdout.write(ALT_ON + HIDE + CLEAR)
        # Prime the counters so the first frame already shows real rates.
        self.sampler.cpu()
        self.sampler.network()
        self.sampler.top_processes()
        try:
            while not stop["now"]:
                if not self.paused:
                    data = sysinfo.snapshot(self.sampler, full=True, settle=0)
                    data["seat"] = _seat_state()
                    write_state(data)
                    size = shutil.get_terminal_size((100, 30))
                    sys.stdout.write(HOME + CLEAR + self.render(data, size.columns, size.lines))
                    sys.stdout.flush()
                deadline = time.monotonic() + self.interval
                while time.monotonic() < deadline and not stop["now"]:
                    key = keys.get()
                    if key:
                        if key in ("q", "Q", "\x03"):
                            stop["now"] = True
                        elif key in ("p", "P", " "):
                            self.paused = not self.paused
                            break
                        elif key == "+":
                            self.interval = min(10.0, self.interval + 0.5)
                            break
                        elif key == "-":
                            self.interval = max(0.25, self.interval - 0.5)
                            break
                    time.sleep(0.04)
        finally:
            keys.close()
            sys.stdout.write(SHOW + ALT_OFF + RESET)
            sys.stdout.flush()
        return 0


def _strip_ansi(text: str) -> str:
    out, i = [], 0
    while i < len(text):
        if text[i] == "\033":
            while i < len(text) and text[i] not in "m":
                i += 1
            i += 1
            continue
        out.append(text[i])
        i += 1
    return "".join(out)


def write_state(data: dict) -> None:
    STATE_DIR.mkdir(parents=True, exist_ok=True)
    tmp = STATE_FILE.with_suffix(".tmp")
    tmp.write_text(json.dumps(data, indent=1, default=str))
    tmp.replace(STATE_FILE)


def run(interval: float = 1.0) -> int:
    return Dashboard(interval).run()
