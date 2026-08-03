"""The agent's own seat on Windows: a second desktop object.

Windows has the cleanest answer of the three platforms. `CreateDesktop` makes a
real desktop with its own input queue, its own cursor position, its own foreground
window and its own window list. Processes started with `STARTUPINFO.lpDesktop` set
live there; `SetThreadDesktop` points this thread's `SendInput` calls at it.

The user's desktop — cursor included — is not involved in any of it. Nothing here
is a trick or an overlay: the isolation is what the OS provides.

Windows on a non-active desktop are still composited, so each is captured with
`PrintWindow(PW_RENDERFULLCONTENT)` and pasted onto a canvas. That works where a
plain screen-grab of an inactive desktop returns black.

Untested by the author — no Windows machine was available. The API usage follows
the documented contracts; `usepc doctor` reports what it can actually confirm.
"""

from __future__ import annotations

import ctypes
import json
import os
import struct
import subprocess
import sys
import time
import zlib
from ctypes import wintypes
from pathlib import Path

STATE_DIR = Path(os.environ.get("USEPC_HOME") or (Path.home() / ".cache" / "usepc"))
SEAT_FILE = STATE_DIR / "seat.json"
DESKTOP_NAME = os.environ.get("USEPC_DESKTOP", "usepc-agent")

GENERIC_ALL = 0x10000000
DESKTOP_ALL = 0x01FF
INPUT_MOUSE, INPUT_KEYBOARD = 0, 1
MOUSEEVENTF = {"move": 0x0001, "absolute": 0x8000, "wheel": 0x0800, "hwheel": 0x1000,
               "left_down": 0x0002, "left_up": 0x0004, "right_down": 0x0008,
               "right_up": 0x0010, "middle_down": 0x0020, "middle_up": 0x0040}
KEYEVENTF_KEYUP, KEYEVENTF_UNICODE = 0x0002, 0x0004
PW_RENDERFULLCONTENT = 0x00000002
SW_SHOW = 5

VK = {"ctrl": 0x11, "control": 0x11, "shift": 0x10, "alt": 0x12, "win": 0x5B, "super": 0x5B,
      "enter": 0x0D, "return": 0x0D, "tab": 0x09, "esc": 0x1B, "escape": 0x1B, "space": 0x20,
      "backspace": 0x08, "delete": 0x2E, "up": 0x26, "down": 0x28, "left": 0x25, "right": 0x27,
      "home": 0x24, "end": 0x23, "pgup": 0x21, "pgdn": 0x22, "insert": 0x2D}
for _n in range(1, 13):
    VK[f"f{_n}"] = 0x6F + _n


class SeatError(RuntimeError):
    pass


def _user32():
    user32 = ctypes.WinDLL("user32", use_last_error=True)
    user32.SetProcessDPIAware()
    return user32


def backend() -> str:
    return "windows-desktop" if sys.platform == "win32" else "none"


# ---------------------------------------------------------------- structures


class MouseInput(ctypes.Structure):
    _fields_ = [("dx", wintypes.LONG), ("dy", wintypes.LONG), ("mouseData", wintypes.DWORD),
                ("dwFlags", wintypes.DWORD), ("time", wintypes.DWORD),
                ("dwExtraInfo", ctypes.POINTER(ctypes.c_ulong))]


class KeyInput(ctypes.Structure):
    _fields_ = [("wVk", wintypes.WORD), ("wScan", wintypes.WORD), ("dwFlags", wintypes.DWORD),
                ("time", wintypes.DWORD), ("dwExtraInfo", ctypes.POINTER(ctypes.c_ulong))]


class InputUnion(ctypes.Union):
    _fields_ = [("mi", MouseInput), ("ki", KeyInput)]


class Input(ctypes.Structure):
    _fields_ = [("type", wintypes.DWORD), ("union", InputUnion)]


class StartupInfo(ctypes.Structure):
    _fields_ = [("cb", wintypes.DWORD), ("lpReserved", wintypes.LPWSTR),
                ("lpDesktop", wintypes.LPWSTR), ("lpTitle", wintypes.LPWSTR),
                ("dwX", wintypes.DWORD), ("dwY", wintypes.DWORD),
                ("dwXSize", wintypes.DWORD), ("dwYSize", wintypes.DWORD),
                ("dwXCountChars", wintypes.DWORD), ("dwYCountChars", wintypes.DWORD),
                ("dwFillAttribute", wintypes.DWORD), ("dwFlags", wintypes.DWORD),
                ("wShowWindow", wintypes.WORD), ("cbReserved2", wintypes.WORD),
                ("lpReserved2", ctypes.POINTER(ctypes.c_byte)),
                ("hStdInput", wintypes.HANDLE), ("hStdOutput", wintypes.HANDLE),
                ("hStdError", wintypes.HANDLE)]


class ProcessInformation(ctypes.Structure):
    _fields_ = [("hProcess", wintypes.HANDLE), ("hThread", wintypes.HANDLE),
                ("dwProcessId", wintypes.DWORD), ("dwThreadId", wintypes.DWORD)]


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
    handle = ctypes.windll.kernel32.OpenProcess(0x1000, False, int(pid))
    if not handle:
        return False
    ctypes.windll.kernel32.CloseHandle(handle)
    return True


def status() -> dict:
    state = _read_state()
    if not state or not state.get("holder_pid"):
        return {"running": False, "backend": backend()}
    if not _alive(state["holder_pid"]):
        return {"running": False, "backend": backend(), "stale": True}
    state["running"] = True
    return state


def geometry() -> tuple[int, int]:
    user32 = _user32()
    return int(user32.GetSystemMetrics(0)), int(user32.GetSystemMetrics(1))


def _open_desktop(create: bool = False):
    user32 = _user32()
    if create:
        handle = user32.CreateDesktopW(DESKTOP_NAME, None, None, 0, GENERIC_ALL, None)
    else:
        handle = user32.OpenDesktopW(DESKTOP_NAME, 0, False, GENERIC_ALL)
    if not handle:
        raise SeatError(f"could not {'create' if create else 'open'} desktop "
                        f"{DESKTOP_NAME!r}: error {ctypes.get_last_error()}")
    return handle


def start(width: int = 0, height: int = 0, **_) -> dict:
    """Create the desktop and park a holder process on it to keep it alive."""
    existing = status()
    if existing.get("running"):
        return existing
    handle = _open_desktop(create=True)
    _user32().CloseDesktop(handle)
    # A desktop object dies with its last handle, so hold one open in a child.
    holder = subprocess.Popen(
        [sys.executable, str(Path(__file__).resolve()), "--hold"],
        creationflags=0x00000008 | 0x08000000,  # DETACHED_PROCESS | CREATE_NO_WINDOW
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    time.sleep(0.4)
    state = {"backend": "windows-desktop", "holder_pid": holder.pid,
             "desktop": DESKTOP_NAME, "display": DESKTOP_NAME,
             "signature": DESKTOP_NAME, "started": time.time()}
    _write_state(state)
    return status()


def stop() -> str:
    state = _read_state()
    pid = state.get("holder_pid")
    if pid and _alive(pid):
        subprocess.run(["taskkill", "/PID", str(pid), "/T", "/F"], capture_output=True)
    SEAT_FILE.unlink(missing_ok=True)
    return f"agent desktop released (holder {pid})"


def ensure(**opts) -> dict:
    state = status()
    return state if state.get("running") else start(**opts)


def _bind_thread() -> None:
    """Point this thread at the agent desktop: input and window calls follow."""
    ensure()
    handle = _open_desktop()
    if not _user32().SetThreadDesktop(handle):
        raise SeatError("SetThreadDesktop failed — the thread already owns windows or hooks")


def handoff() -> str:
    """Switch the *display* to the agent desktop so the human can look at it."""
    _bind_thread()
    handle = _open_desktop()
    if not _user32().SwitchDesktop(handle):
        raise SeatError("SwitchDesktop failed")
    return ("now showing the agent desktop. Run `usepc seat reclaim` to switch back "
            "(or press Ctrl+Alt+Del and pick your session).")


def reclaim() -> str:
    user32 = _user32()
    handle = user32.OpenDesktopW("Default", 0, False, GENERIC_ALL)
    if not handle or not user32.SwitchDesktop(handle):
        raise SeatError("could not switch back to the Default desktop")
    return "back on your own desktop"


def set_opacity(value: float) -> str:
    return "opacity does not apply to a Windows desktop object (it is not a window)"


# ---------------------------------------------------------------- input


def _send(*inputs: Input) -> None:
    _bind_thread()
    count = len(inputs)
    array = (Input * count)(*inputs)
    sent = _user32().SendInput(count, array, ctypes.sizeof(Input))
    if sent != count:
        raise SeatError(f"SendInput delivered {sent}/{count}: error {ctypes.get_last_error()}")


def _absolute(x: float, y: float) -> tuple[int, int]:
    width, height = geometry()
    return int(x * 65535 / max(1, width - 1)), int(y * 65535 / max(1, height - 1))


def move(x: float, y: float) -> None:
    ax, ay = _absolute(x, y)
    _send(Input(INPUT_MOUSE, InputUnion(mi=MouseInput(
        ax, ay, 0, MOUSEEVENTF["move"] | MOUSEEVENTF["absolute"], 0, None))))


def click(x: float | None = None, y: float | None = None, button: str = "left",
          count: int = 1) -> None:
    if x is not None and y is not None:
        move(x, y)
        time.sleep(0.1)
    down, up = MOUSEEVENTF[f"{button}_down"], MOUSEEVENTF[f"{button}_up"]
    for i in range(max(1, count)):
        if i:
            time.sleep(0.08)
        _send(Input(INPUT_MOUSE, InputUnion(mi=MouseInput(0, 0, 0, down, 0, None))))
        time.sleep(0.04)
        _send(Input(INPUT_MOUSE, InputUnion(mi=MouseInput(0, 0, 0, up, 0, None))))


def scroll(notches: int, x: float | None = None, y: float | None = None,
           horizontal: bool = False) -> None:
    if x is not None and y is not None:
        move(x, y)
        time.sleep(0.1)
    flag = MOUSEEVENTF["hwheel"] if horizontal else MOUSEEVENTF["wheel"]
    _send(Input(INPUT_MOUSE, InputUnion(mi=MouseInput(
        0, 0, ctypes.c_ulong(-int(notches) * 120).value, flag, 0, None))))


def drag(x1: float, y1: float, x2: float, y2: float, button: str = "left",
         steps: int = 24) -> None:
    move(x1, y1)
    time.sleep(0.1)
    _send(Input(INPUT_MOUSE, InputUnion(mi=MouseInput(
        0, 0, 0, MOUSEEVENTF[f"{button}_down"], 0, None))))
    try:
        for i in range(1, max(2, steps) + 1):
            move(x1 + (x2 - x1) * i / steps, y1 + (y2 - y1) * i / steps)
            time.sleep(0.015)
    finally:
        _send(Input(INPUT_MOUSE, InputUnion(mi=MouseInput(
            0, 0, 0, MOUSEEVENTF[f"{button}_up"], 0, None))))


def type_text(text: str, delay_ms: int = 12) -> None:
    for char in text:
        for flags in (KEYEVENTF_UNICODE, KEYEVENTF_UNICODE | KEYEVENTF_KEYUP):
            _send(Input(INPUT_KEYBOARD, InputUnion(ki=KeyInput(0, ord(char), flags, 0, None))))
        time.sleep(max(0, delay_ms) / 1000)


def press(combo: str) -> None:
    parts = [p for p in combo.replace(" ", "").split("+") if p]
    if not parts:
        raise SeatError("empty key combo")
    user32 = _user32()
    codes = []
    for part in parts:
        low = part.lower()
        if low in VK:
            codes.append(VK[low])
        elif len(part) == 1:
            codes.append(user32.VkKeyScanW(ord(part.upper())) & 0xFF)
        else:
            raise SeatError(f"no virtual key known for {part!r}")
    events = [Input(INPUT_KEYBOARD, InputUnion(ki=KeyInput(code, 0, 0, 0, None)))
              for code in codes]
    events += [Input(INPUT_KEYBOARD, InputUnion(ki=KeyInput(code, 0, KEYEVENTF_KEYUP, 0, None)))
               for code in reversed(codes)]
    _send(*events)


def paste(text: str, combo: str = "ctrl+v") -> None:
    """The agent desktop shares the session clipboard — this does overwrite it."""
    subprocess.run("clip", input=text, text=True, shell=True, check=True)
    time.sleep(0.15)
    press(combo)


def spawn(command: str) -> int:
    ensure()
    startup = StartupInfo()
    startup.cb = ctypes.sizeof(StartupInfo)
    startup.lpDesktop = DESKTOP_NAME
    startup.dwFlags = 0x00000001  # STARTF_USESHOWWINDOW
    startup.wShowWindow = SW_SHOW
    info = ProcessInformation()
    created = ctypes.windll.kernel32.CreateProcessW(
        None, ctypes.create_unicode_buffer(command), None, None, False,
        0x00000010, None, None, ctypes.byref(startup), ctypes.byref(info))  # CREATE_NEW_CONSOLE
    if not created:
        raise SeatError(f"CreateProcessW failed: error {ctypes.get_last_error()}")
    ctypes.windll.kernel32.CloseHandle(info.hThread)
    ctypes.windll.kernel32.CloseHandle(info.hProcess)
    return int(info.dwProcessId)


# ---------------------------------------------------------------- windows & capture


def windows() -> list[dict]:
    _bind_thread()
    user32 = _user32()
    handle = _open_desktop()
    found: list[dict] = []

    @ctypes.WINFUNCTYPE(wintypes.BOOL, wintypes.HWND, wintypes.LPARAM)
    def collect(hwnd, _lparam):
        if not user32.IsWindowVisible(hwnd):
            return True
        length = user32.GetWindowTextLengthW(hwnd)
        buffer = ctypes.create_unicode_buffer(length + 1)
        user32.GetWindowTextW(hwnd, buffer, length + 1)
        rect = wintypes.RECT()
        user32.GetWindowRect(hwnd, ctypes.byref(rect))
        pid = wintypes.DWORD()
        user32.GetWindowThreadProcessId(hwnd, ctypes.byref(pid))
        if rect.right - rect.left > 1 and buffer.value:
            found.append({"hwnd": int(hwnd), "title": buffer.value, "class": "",
                          "pid": int(pid.value),
                          "at": [rect.left, rect.top],
                          "size": [rect.right - rect.left, rect.bottom - rect.top],
                          "focused": hwnd == user32.GetForegroundWindow(),
                          "address": str(int(hwnd))})
        return True

    user32.EnumDesktopWindows(handle, collect, 0)
    return found


def focus(match: str) -> str:
    _bind_thread()
    user32 = _user32()
    for window in windows():
        if match.lower() in window["title"].lower():
            user32.SetForegroundWindow(window["hwnd"])
            return f"focused {window['title']}"
    raise SeatError(f"no window on the agent desktop matches {match!r}")


def _png(path: Path, width: int, height: int, bgra: bytes) -> Path:
    """Minimal PNG writer — avoids a Pillow dependency for one screenshot."""
    rows = bytearray()
    stride = width * 4
    for y in range(height):
        rows.append(0)
        row = bgra[y * stride:(y + 1) * stride]
        rows.extend(bytes((row[i + 2], row[i + 1], row[i]) for i in range(0, len(row), 4)))

    def chunk(kind: bytes, payload: bytes) -> bytes:
        return (struct.pack(">I", len(payload)) + kind + payload
                + struct.pack(">I", zlib.crc32(kind + payload) & 0xFFFFFFFF))

    header = struct.pack(">IIBBBBB", width, height, 8, 2, 0, 0, 0)
    path.write_bytes(b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", header)
                     + chunk(b"IDAT", zlib.compress(bytes(rows), 6))
                     + chunk(b"IEND", b""))
    return path


def shot(dest: Path, scale: float = 0.66, quality: int = 62,
         region: str | None = None, window: str | None = None) -> Path:
    """Composite the agent desktop's windows into one PNG.

    An inactive desktop does not answer a plain screen grab, so each window is
    asked to render itself with PrintWindow(PW_RENDERFULLCONTENT).
    """
    _bind_thread()
    user32 = _user32()
    gdi32 = ctypes.WinDLL("gdi32")
    width, height = geometry()
    dest = dest.with_suffix(".png")
    dest.parent.mkdir(parents=True, exist_ok=True)

    screen_dc = user32.GetDC(0)
    canvas_dc = gdi32.CreateCompatibleDC(screen_dc)
    canvas = gdi32.CreateCompatibleBitmap(screen_dc, width, height)
    gdi32.SelectObject(canvas_dc, canvas)
    gdi32.PatBlt(canvas_dc, 0, 0, width, height, 0x00000042)  # BLACKNESS

    targets = windows()
    if window:
        targets = [w for w in targets if window.lower() in w["title"].lower()]
        if not targets:
            raise SeatError(f"no window on the agent desktop matches {window!r}")
    for entry in targets:
        hwnd = entry["hwnd"]
        w, h = entry["size"]
        window_dc = gdi32.CreateCompatibleDC(screen_dc)
        bitmap = gdi32.CreateCompatibleBitmap(screen_dc, w, h)
        gdi32.SelectObject(window_dc, bitmap)
        if user32.PrintWindow(hwnd, window_dc, PW_RENDERFULLCONTENT):
            gdi32.BitBlt(canvas_dc, entry["at"][0], entry["at"][1], w, h,
                         window_dc, 0, 0, 0x00CC0020)  # SRCCOPY
        gdi32.DeleteObject(bitmap)
        gdi32.DeleteDC(window_dc)

    class BitmapInfoHeader(ctypes.Structure):
        _fields_ = [("biSize", wintypes.DWORD), ("biWidth", wintypes.LONG),
                    ("biHeight", wintypes.LONG), ("biPlanes", wintypes.WORD),
                    ("biBitCount", wintypes.WORD), ("biCompression", wintypes.DWORD),
                    ("biSizeImage", wintypes.DWORD), ("biXPelsPerMeter", wintypes.LONG),
                    ("biYPelsPerMeter", wintypes.LONG), ("biClrUsed", wintypes.DWORD),
                    ("biClrImportant", wintypes.DWORD)]

    header = BitmapInfoHeader()
    header.biSize = ctypes.sizeof(BitmapInfoHeader)
    header.biWidth, header.biHeight = width, -height  # negative: top-down rows
    header.biPlanes, header.biBitCount, header.biCompression = 1, 32, 0
    buffer = ctypes.create_string_buffer(width * height * 4)
    gdi32.GetDIBits(canvas_dc, canvas, 0, height, buffer, ctypes.byref(header), 0)

    gdi32.DeleteObject(canvas)
    gdi32.DeleteDC(canvas_dc)
    user32.ReleaseDC(0, screen_dc)
    return _png(dest, width, height, buffer.raw)


def _hold_forever() -> None:
    """Child entry point: keep a handle open so the desktop object survives."""
    handle = _open_desktop(create=True)
    try:
        while True:
            time.sleep(3600)
    finally:
        _user32().CloseDesktop(handle)


if __name__ == "__main__" and "--hold" in sys.argv:
    _hold_forever()
