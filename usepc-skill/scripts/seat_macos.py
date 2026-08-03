"""The agent's own pointer on macOS — with an honest account of the limits.

macOS has no second desktop and no second cursor. Spaces share one cursor, and
`CGWarpMouseCursorPosition` moves the one the human is holding. Anything claiming
otherwise inside a single login session is pretending.

What macOS *does* give is `CGEventPostToPid`: an event delivered to one process's
event queue, which never warps the visible cursor and never disturbs the user's
pointer. So the agent's seat here is **a bound application** rather than a bound
screen. Clicks land in that app at the coordinates given; the human's cursor stays
exactly where it was, and their frontmost app keeps focus.

Genuinely isolated alternatives, if the app fights back (some games and DRM'd apps
ignore posted events):

  * A second login session — System Settings ▸ Users & Groups ▸ fast user
    switching. Two sessions, two cursors, real isolation. Switch back with the menu
    bar. The agent's session keeps running while yours is in front.
  * A VM (UTM, Parallels, VMware Fusion). Its own everything, at the cost of RAM.

`usepc doctor` says which of these is set up. Untested by the author — no Mac was
available — but every call below follows the documented Core Graphics contracts.
"""

from __future__ import annotations

import ctypes
import ctypes.util
import json
import os
import subprocess
import time
from pathlib import Path

STATE_DIR = Path(os.environ.get("USEPC_HOME") or (Path.home() / ".cache" / "usepc"))
SEAT_FILE = STATE_DIR / "seat.json"

KCG_HID_TAP = 0
EVENT = {"left_down": 1, "left_up": 2, "right_down": 3, "right_up": 4, "moved": 5,
         "middle_down": 25, "middle_up": 26}
BUTTON_NUMBER = {"left": 0, "right": 1, "middle": 2}
MODIFIER = {"cmd": 1 << 20, "command": 1 << 20, "shift": 1 << 17, "alt": 1 << 19,
            "option": 1 << 19, "ctrl": 1 << 18, "control": 1 << 18, "fn": 1 << 23}
KEYCODE = {"a": 0, "s": 1, "d": 2, "f": 3, "h": 4, "g": 5, "z": 6, "x": 7, "c": 8, "v": 9,
           "b": 11, "q": 12, "w": 13, "e": 14, "r": 15, "y": 16, "t": 17, "1": 18, "2": 19,
           "3": 20, "4": 21, "6": 22, "5": 23, "equal": 24, "9": 25, "7": 26, "minus": 27,
           "8": 28, "0": 29, "o": 31, "u": 32, "i": 34, "p": 35, "return": 36, "enter": 36,
           "l": 37, "j": 38, "k": 40, "n": 45, "m": 46, "tab": 48, "space": 49,
           "backspace": 51, "delete": 51, "escape": 53, "esc": 53,
           "left": 123, "right": 124, "down": 125, "up": 126, "home": 115, "end": 119,
           "pgup": 116, "pgdn": 121}
for _n in range(1, 13):
    KEYCODE[f"f{_n}"] = {1: 122, 2: 120, 3: 99, 4: 118, 5: 96, 6: 97, 7: 98, 8: 100,
                         9: 101, 10: 109, 11: 103, 12: 111}[_n]


class SeatError(RuntimeError):
    pass


class Point(ctypes.Structure):
    _fields_ = [("x", ctypes.c_double), ("y", ctypes.c_double)]


def _framework(name: str, fallback: str):
    path = ctypes.util.find_library(name) or fallback
    return ctypes.cdll.LoadLibrary(path)


def _cg():
    lib = _framework(
        "ApplicationServices",
        "/System/Library/Frameworks/ApplicationServices.framework/ApplicationServices")
    lib.CGEventCreateMouseEvent.restype = ctypes.c_void_p
    lib.CGEventCreateMouseEvent.argtypes = [ctypes.c_void_p, ctypes.c_uint32, Point, ctypes.c_uint32]
    lib.CGEventCreateKeyboardEvent.restype = ctypes.c_void_p
    lib.CGEventCreateKeyboardEvent.argtypes = [ctypes.c_void_p, ctypes.c_uint16, ctypes.c_bool]
    lib.CGEventCreateScrollWheelEvent.restype = ctypes.c_void_p
    lib.CGEventPostToPid.argtypes = [ctypes.c_int32, ctypes.c_void_p]
    lib.CGEventSetFlags.argtypes = [ctypes.c_void_p, ctypes.c_uint64]
    lib.CFRelease.argtypes = [ctypes.c_void_p]
    return lib


def backend() -> str:
    try:
        _cg()
        return "macos-cgevent-pid"
    except Exception:
        return "none"


# ---------------------------------------------------------------- state


def _read_state() -> dict:
    try:
        return json.loads(SEAT_FILE.read_text())
    except Exception:
        return {}


def _write_state(state: dict) -> None:
    STATE_DIR.mkdir(parents=True, exist_ok=True)
    tmp = SEAT_FILE.with_suffix(".tmp")
    tmp.write_text(json.dumps(state, indent=2))
    tmp.replace(SEAT_FILE)


def _alive(pid: int) -> bool:
    try:
        os.kill(int(pid), 0)
        return True
    except Exception:
        return False


def status() -> dict:
    state = _read_state()
    if not state:
        return {"running": False, "backend": backend()}
    pid = state.get("target_pid")
    if pid and not _alive(pid):
        return {"running": False, "backend": backend(), "stale": True,
                "note": "the bound app exited"}
    state["running"] = bool(pid)
    state["display"] = f"pid {pid}" if pid else "unbound"
    return state


def geometry() -> tuple[int, int]:
    """Screen size — posted events use global screen coordinates."""
    raw = subprocess.run(
        ["osascript", "-e", 'tell application "Finder" to get bounds of window of desktop'],
        capture_output=True, text=True).stdout.strip()
    try:
        parts = [int(v.strip()) for v in raw.split(",")]
        return parts[2], parts[3]
    except Exception:
        return 1920, 1080


def start(app: str | None = None, **_) -> dict:
    state = status()
    if state.get("running"):
        return state
    if not app:
        _write_state({"backend": backend(), "target_pid": None, "started": time.time()})
        raise SeatError(
            "macOS has no second cursor to hand out, so the agent's seat is a bound app.\n"
            "  usepc seat open 'Safari'     — launch it and bind to it\n"
            "  usepc seat open 'TextEdit'\n"
            "For real isolation instead, enable fast user switching and give the agent "
            "its own login session (see references/macos.md).")
    return _bind(app)


def _bind(app: str) -> dict:
    subprocess.run(["open", "-a", app], check=True)
    time.sleep(1.2)
    pid_raw = subprocess.run(
        ["osascript", "-e", f'tell application "System Events" to get unix id of '
                            f'first process whose name is "{app}"'],
        capture_output=True, text=True).stdout.strip()
    if not pid_raw.isdigit():
        raise SeatError(f"could not find a running process for {app!r} after launching it")
    state = {"backend": backend(), "target_pid": int(pid_raw), "target_app": app,
             "started": time.time(), "signature": app}
    _write_state(state)
    return status()


def stop() -> str:
    SEAT_FILE.unlink(missing_ok=True)
    return "agent seat unbound (the app was left running)"


def ensure(**opts) -> dict:
    state = status()
    if state.get("running"):
        return state
    return start(**opts)


def _target() -> int:
    state = status()
    pid = state.get("target_pid")
    if not pid:
        raise SeatError("no app is bound — `usepc seat open <App>` first")
    return int(pid)


def spawn(command: str) -> int:
    """On macOS the 'seat' is an app, so opening one also binds to it."""
    app = command.strip().strip('"')
    state = _bind(app)
    return int(state["target_pid"])


def handoff() -> str:
    state = status()
    app = state.get("target_app")
    if not app:
        raise SeatError("nothing bound to hand over")
    subprocess.run(["osascript", "-e", f'tell application "{app}" to activate'], check=True)
    return f"{app} brought to the front — it is yours; the agent still posts to its pid"


def reclaim() -> str:
    return "nothing to reclaim: posted events never took your cursor or focus"


def set_opacity(value: float) -> str:
    return "opacity is not applicable — the macOS seat is an app, not a window of ours"


# ---------------------------------------------------------------- input


def _post_mouse(kind: str, x: float, y: float, button: str = "left", clicks: int = 1) -> None:
    lib = _cg()
    event = lib.CGEventCreateMouseEvent(None, EVENT[kind], Point(float(x), float(y)),
                                        BUTTON_NUMBER.get(button, 0))
    if clicks > 1:
        # kCGMouseEventClickState = 1
        lib.CGEventSetIntegerValueField.argtypes = [ctypes.c_void_p, ctypes.c_uint32,
                                                   ctypes.c_int64]
        lib.CGEventSetIntegerValueField(event, 1, clicks)
    lib.CGEventPostToPid(_target(), event)
    lib.CFRelease(event)


def move(x: float, y: float) -> None:
    _post_mouse("moved", x, y)


def click(x: float | None = None, y: float | None = None, button: str = "left",
          count: int = 1) -> None:
    if x is None or y is None:
        raise SeatError("posted clicks need coordinates — there is no shared cursor to reuse")
    _post_mouse("moved", x, y)
    time.sleep(0.05)
    for i in range(max(1, count)):
        _post_mouse(f"{button}_down", x, y, button, i + 1)
        time.sleep(0.04)
        _post_mouse(f"{button}_up", x, y, button, i + 1)
        time.sleep(0.05)


def scroll(notches: int, x: float | None = None, y: float | None = None,
           horizontal: bool = False) -> None:
    lib = _cg()
    if x is not None and y is not None:
        _post_mouse("moved", x, y)
    lib.CGEventCreateScrollWheelEvent.argtypes = [ctypes.c_void_p, ctypes.c_uint32,
                                                  ctypes.c_uint32, ctypes.c_int32]
    event = lib.CGEventCreateScrollWheelEvent(None, 1, 1, -int(notches))
    lib.CGEventPostToPid(_target(), event)
    lib.CFRelease(event)


def drag(x1: float, y1: float, x2: float, y2: float, button: str = "left",
         steps: int = 24) -> None:
    _post_mouse("moved", x1, y1)
    _post_mouse(f"{button}_down", x1, y1, button)
    try:
        for i in range(1, max(2, steps) + 1):
            _post_mouse("moved", x1 + (x2 - x1) * i / steps, y1 + (y2 - y1) * i / steps)
            time.sleep(0.015)
    finally:
        _post_mouse(f"{button}_up", x2, y2, button)


def type_text(text: str, delay_ms: int = 12) -> None:
    lib = _cg()
    lib.CGEventKeyboardSetUnicodeString.argtypes = [ctypes.c_void_p, ctypes.c_ulong,
                                                    ctypes.c_wchar_p]
    pid = _target()
    for char in text:
        for down in (True, False):
            event = lib.CGEventCreateKeyboardEvent(None, 0, down)
            lib.CGEventKeyboardSetUnicodeString(event, 1, char)
            lib.CGEventPostToPid(pid, event)
            lib.CFRelease(event)
        time.sleep(max(0, delay_ms) / 1000)


def press(combo: str) -> None:
    parts = [p for p in combo.replace(" ", "").split("+") if p]
    if not parts:
        raise SeatError("empty key combo")
    flags = 0
    for part in parts[:-1]:
        flags |= MODIFIER.get(part.lower(), 0)
    key = KEYCODE.get(parts[-1].lower())
    if key is None:
        raise SeatError(f"no macOS keycode known for {parts[-1]!r}")
    lib = _cg()
    pid = _target()
    for down in (True, False):
        event = lib.CGEventCreateKeyboardEvent(None, key, down)
        if flags:
            lib.CGEventSetFlags(event, flags)
        lib.CGEventPostToPid(pid, event)
        lib.CFRelease(event)
        time.sleep(0.02)


def paste(text: str, combo: str = "cmd+v") -> None:
    subprocess.run(["pbcopy"], input=text, text=True, check=True)
    time.sleep(0.15)
    press(combo)


# ---------------------------------------------------------------- looking


def windows() -> list[dict]:
    state = status()
    app = state.get("target_app")
    if not app:
        return []
    script = (f'tell application "System Events" to tell process "{app}" to get '
              '{name, position, size} of every window')
    raw = subprocess.run(["osascript", "-e", script], capture_output=True, text=True).stdout
    parts = [p.strip() for p in raw.strip().split(",")]
    result: list[dict] = []
    # AppleScript flattens the lists, so walk it in stride: name, x, y, w, h
    for i in range(0, max(0, len(parts) - 4), 5):
        try:
            result.append({"class": app, "title": parts[i], "pid": state.get("target_pid"),
                           "at": [int(parts[i + 1]), int(parts[i + 2])],
                           "size": [int(parts[i + 3]), int(parts[i + 4])],
                           "focused": i == 0, "address": parts[i]})
        except (ValueError, IndexError):
            continue
    return result


def focus(match: str) -> str:
    state = status()
    app = state.get("target_app")
    subprocess.run(["osascript", "-e", f'tell application "{app}" to activate'], check=True)
    return f"{app} activated (macOS focuses apps, not agent-private windows)"


def shot(dest: Path, scale: float = 0.66, quality: int = 62,
         region: str | None = None, window: str | None = None) -> Path:
    """Capture the bound app's window. Needs Screen Recording permission."""
    dest.parent.mkdir(parents=True, exist_ok=True)
    args = ["screencapture", "-x", "-o", "-t", "jpg"]
    if region:
        args += ["-R", region.replace(" ", ",").replace("x", ",")]
    else:
        found = windows()
        if window:
            found = [w for w in found if window.lower() in str(w.get("title", "")).lower()]
        if found:
            x, y = found[0]["at"]
            w, h = found[0]["size"]
            args += ["-R", f"{x},{y},{w},{h}"]
    args.append(str(dest))
    proc = subprocess.run(args, capture_output=True, text=True)
    if proc.returncode != 0 or not dest.exists():
        raise SeatError(f"screencapture failed: {(proc.stderr or proc.stdout).strip()} "
                        "(grant Screen Recording permission in System Settings ▸ Privacy)")
    return dest
