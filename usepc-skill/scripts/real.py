"""Touching the user's *actual* desktop — the path that needs asking first.

The agent seat covers almost everything, and nothing here runs unless `--real` was
passed. When it does run, it behaves like a guest: the pointer is put back exactly
where the human left it, and every action is written to the audit log.

There is no honest way to click on the user's desktop without briefly owning their
cursor. One seat, one cursor. So instead of pretending, this borrows it for a few
hundred milliseconds and gives it straight back. If the agent needs a pointer of
its own, that is what the seat is for.
"""

from __future__ import annotations

import os
import shutil
import subprocess
import time
from pathlib import Path

import sysinfo


class RealError(RuntimeError):
    pass


RESTORE = os.environ.get("USEPC_NO_RESTORE") != "1"


# ------------------------------------------------------------------ geometry


def screen_size() -> tuple[int, int]:
    if sysinfo.IS_LINUX:
        if shutil.which("hyprctl"):
            import json
            monitors = json.loads(subprocess.run(["hyprctl", "monitors", "-j"],
                                                 capture_output=True, text=True,
                                                 timeout=4).stdout)
            right = max(m["x"] + int(m["width"] / (m.get("scale") or 1)) for m in monitors)
            bottom = max(m["y"] + int(m["height"] / (m.get("scale") or 1)) for m in monitors)
            return right, bottom
        if shutil.which("xdotool"):
            out = subprocess.run(["xdotool", "getdisplaygeometry"], capture_output=True,
                                 text=True).stdout.split()
            return int(out[0]), int(out[1])
        raise RealError("cannot determine screen size on this Linux session")
    if sysinfo.IS_MAC:
        cg = _core_graphics()
        display = cg.CGMainDisplayID()
        return int(cg.CGDisplayPixelsWide(display)), int(cg.CGDisplayPixelsHigh(display))
    import ctypes
    user32 = ctypes.windll.user32
    user32.SetProcessDPIAware()
    return int(user32.GetSystemMetrics(78) or user32.GetSystemMetrics(0)), \
        int(user32.GetSystemMetrics(79) or user32.GetSystemMetrics(1))


def geometry() -> tuple[int, int]:
    return screen_size()


def cursor_position() -> tuple[int, int] | None:
    if sysinfo.IS_LINUX:
        if shutil.which("hyprctl"):
            raw = subprocess.run(["hyprctl", "cursorpos"], capture_output=True, text=True,
                                 timeout=4).stdout.strip()
            try:
                x, y = (int(v.strip()) for v in raw.split(","))
                return x, y
            except Exception:
                return None
        if shutil.which("xdotool"):
            out = subprocess.run(["xdotool", "getmouselocation", "--shell"],
                                 capture_output=True, text=True).stdout
            values = dict(line.split("=", 1) for line in out.strip().splitlines() if "=" in line)
            return int(values.get("X", 0)), int(values.get("Y", 0))
        return None
    if sysinfo.IS_MAC:
        cg = _core_graphics()
        import ctypes

        class Point(ctypes.Structure):
            _fields_ = [("x", ctypes.c_double), ("y", ctypes.c_double)]

        event = cg.CGEventCreate(None)
        cg.CGEventGetLocation.restype = Point
        point = cg.CGEventGetLocation(event)
        cg.CFRelease(event)
        return int(point.x), int(point.y)
    import ctypes

    class WinPoint(ctypes.Structure):
        _fields_ = [("x", ctypes.c_long), ("y", ctypes.c_long)]

    point = WinPoint()
    ctypes.windll.user32.GetCursorPos(ctypes.byref(point))
    return point.x, point.y


class _Borrowed:
    """Hold the user's cursor for the length of one action, then hand it back."""

    def __init__(self) -> None:
        self.origin = cursor_position() if RESTORE else None

    def __enter__(self) -> "_Borrowed":
        return self

    def __exit__(self, *exc) -> None:
        if self.origin:
            try:
                _warp(*self.origin)
            except Exception:
                pass


# ------------------------------------------------------------------ platform pointers


def _core_graphics():
    import ctypes
    import ctypes.util
    path = ctypes.util.find_library("ApplicationServices") or \
        "/System/Library/Frameworks/ApplicationServices.framework/ApplicationServices"
    return ctypes.cdll.LoadLibrary(path)


def _warp(x: float, y: float) -> None:
    """Put the cursor at x,y without pressing anything."""
    if sysinfo.IS_LINUX:
        if shutil.which("hyprctl"):
            proc = subprocess.run(["hyprctl", "dispatch", "movecursor", str(int(x)), str(int(y))],
                                  capture_output=True, text=True, timeout=4)
            if "ok" not in proc.stdout:
                raise RealError(f"movecursor failed: {proc.stdout.strip()}{proc.stderr.strip()}")
            return
        if shutil.which("xdotool"):
            subprocess.run(["xdotool", "mousemove", str(int(x)), str(int(y))], check=True)
            return
        import vpointer
        with vpointer.VirtualPointer(extent=screen_size()) as pointer:
            pointer.move(x, y)
            pointer.flush()
        return
    if sysinfo.IS_MAC:
        cg = _core_graphics()
        import ctypes

        class Point(ctypes.Structure):
            _fields_ = [("x", ctypes.c_double), ("y", ctypes.c_double)]

        cg.CGWarpMouseCursorPosition(Point(float(x), float(y)))
        return
    import ctypes
    ctypes.windll.user32.SetProcessDPIAware()
    ctypes.windll.user32.SetCursorPos(int(x), int(y))


MAC_BUTTON = {"left": (1, 2, 0), "right": (3, 4, 1), "middle": (25, 26, 2)}
WIN_BUTTON = {"left": (0x0002, 0x0004), "right": (0x0008, 0x0010), "middle": (0x0020, 0x0040)}


def _press(button: str, down: bool) -> None:
    if sysinfo.IS_LINUX:
        import vpointer
        # A fresh device per press would drop the pairing, so callers hold one open.
        raise RealError("internal: linux presses go through _linux_pointer()")
    if sysinfo.IS_MAC:
        cg = _core_graphics()
        import ctypes

        class Point(ctypes.Structure):
            _fields_ = [("x", ctypes.c_double), ("y", ctypes.c_double)]

        position = cursor_position() or (0, 0)
        down_type, up_type, mouse_button = MAC_BUTTON.get(button, MAC_BUTTON["left"])
        event = cg.CGEventCreateMouseEvent(None, down_type if down else up_type,
                                          Point(float(position[0]), float(position[1])),
                                          mouse_button)
        cg.CGEventPost(0, event)  # kCGHIDEventTap
        cg.CFRelease(event)
        return
    import ctypes
    flags = WIN_BUTTON.get(button, WIN_BUTTON["left"])
    ctypes.windll.user32.mouse_event(flags[0] if down else flags[1], 0, 0, 0, 0)


class _linux_pointer:
    """One virtual pointer on the user's compositor, kept open across a gesture."""

    def __enter__(self):
        import vpointer
        self.pointer = vpointer.VirtualPointer(extent=screen_size())
        return self.pointer

    def __exit__(self, *exc):
        self.pointer.flush()
        self.pointer.close()


# ------------------------------------------------------------------ actions


def move(x: float, y: float) -> None:
    _warp(x, y)


def click(x: float | None = None, y: float | None = None, button: str = "left",
          count: int = 1) -> None:
    with _Borrowed():
        if x is not None and y is not None:
            _warp(x, y)
            time.sleep(0.12)
        if sysinfo.IS_LINUX:
            with _linux_pointer() as pointer:
                pointer.click(button, count)
        else:
            for i in range(max(1, count)):
                if i:
                    time.sleep(0.08)
                _press(button, True)
                time.sleep(0.04)
                _press(button, False)
        time.sleep(0.05)


def scroll(amount: int, x: float | None = None, y: float | None = None,
           horizontal: bool = False) -> None:
    with _Borrowed():
        if x is not None and y is not None:
            _warp(x, y)
            time.sleep(0.1)
        if sysinfo.IS_LINUX:
            with _linux_pointer() as pointer:
                pointer.scroll(amount, horizontal)
        elif sysinfo.IS_MAC:
            cg = _core_graphics()
            event = cg.CGEventCreateScrollWheelEvent(None, 1, 1, -int(amount))
            cg.CGEventPost(0, event)
            cg.CFRelease(event)
        else:
            import ctypes
            ctypes.windll.user32.mouse_event(0x1000 if horizontal else 0x0800, 0, 0,
                                            -int(amount) * 120, 0)


def drag(x1: float, y1: float, x2: float, y2: float, button: str = "left",
         steps: int = 24) -> None:
    with _Borrowed():
        if sysinfo.IS_LINUX and shutil.which("hyprctl"):
            with _linux_pointer() as pointer:
                _warp(x1, y1)
                time.sleep(0.1)
                pointer.button(button, True)
                try:
                    for i in range(1, max(2, steps) + 1):
                        _warp(x1 + (x2 - x1) * i / steps, y1 + (y2 - y1) * i / steps)
                        time.sleep(0.015)
                finally:
                    pointer.button(button, False)
            return
        _warp(x1, y1)
        time.sleep(0.1)
        _press(button, True)
        try:
            for i in range(1, max(2, steps) + 1):
                _warp(x1 + (x2 - x1) * i / steps, y1 + (y2 - y1) * i / steps)
                time.sleep(0.015)
        finally:
            _press(button, False)


def type_text(text: str, delay_ms: int = 12) -> None:
    if sysinfo.IS_LINUX:
        if shutil.which("wtype"):
            proc = subprocess.run(["wtype", "-d", str(delay_ms), "-"], input=text,
                                  capture_output=True, text=True)
            if proc.returncode != 0:
                raise RealError(f"wtype failed: {(proc.stderr or proc.stdout).strip()}")
            return
        if shutil.which("xdotool"):
            subprocess.run(["xdotool", "type", "--delay", str(delay_ms), text], check=True)
            return
        raise RealError("install wtype (Wayland) or xdotool (X11) to type on the real desktop")
    if sysinfo.IS_MAC:
        cg = _core_graphics()
        import ctypes
        for char in text:
            event = cg.CGEventCreateKeyboardEvent(None, 0, True)
            buffer = ctypes.create_unicode_buffer(char)
            cg.CGEventKeyboardSetUnicodeString(event, 1, buffer)
            cg.CGEventPost(0, event)
            cg.CFRelease(event)
            time.sleep(delay_ms / 1000)
        return
    import ctypes
    for char in text:
        _win_unicode(char, ctypes)
        time.sleep(delay_ms / 1000)


def _win_unicode(char: str, ctypes) -> None:
    KEYEVENTF_UNICODE, KEYEVENTF_KEYUP, INPUT_KEYBOARD = 0x0004, 0x0002, 1

    class KeyInput(ctypes.Structure):
        _fields_ = [("wVk", ctypes.c_ushort), ("wScan", ctypes.c_ushort),
                    ("dwFlags", ctypes.c_ulong), ("time", ctypes.c_ulong),
                    ("dwExtraInfo", ctypes.POINTER(ctypes.c_ulong))]

    class Union(ctypes.Union):
        _fields_ = [("ki", KeyInput)]

    class Input(ctypes.Structure):
        _fields_ = [("type", ctypes.c_ulong), ("union", Union)]

    for flags in (KEYEVENTF_UNICODE, KEYEVENTF_UNICODE | KEYEVENTF_KEYUP):
        event = Input(INPUT_KEYBOARD, Union(KeyInput(0, ord(char), flags, 0, None)))
        ctypes.windll.user32.SendInput(1, ctypes.byref(event), ctypes.sizeof(Input))


LINUX_MODS = {"ctrl": "ctrl", "control": "ctrl", "shift": "shift", "alt": "alt",
              "super": "logo", "cmd": "logo", "win": "logo", "meta": "logo"}
MAC_KEYCODES = {"return": 36, "enter": 36, "tab": 48, "space": 49, "delete": 51,
                "escape": 53, "esc": 53, "left": 123, "right": 124, "down": 125, "up": 126,
                "a": 0, "c": 8, "v": 9, "s": 1, "t": 17, "w": 13, "q": 12, "l": 37, "n": 45}
MAC_MODS = {"cmd": 1 << 20, "command": 1 << 20, "shift": 1 << 17, "alt": 1 << 19,
            "option": 1 << 19, "ctrl": 1 << 18, "control": 1 << 18}
WIN_VK = {"ctrl": 0x11, "control": 0x11, "shift": 0x10, "alt": 0x12, "win": 0x5B,
          "super": 0x5B, "enter": 0x0D, "return": 0x0D, "tab": 0x09, "esc": 0x1B,
          "escape": 0x1B, "space": 0x20, "backspace": 0x08, "delete": 0x2E,
          "up": 0x26, "down": 0x28, "left": 0x25, "right": 0x27, "home": 0x24, "end": 0x23}


def press(combo: str) -> None:
    parts = [p for p in combo.replace(" ", "").split("+") if p]
    if not parts:
        raise RealError("empty key combo")
    if sysinfo.IS_LINUX:
        if shutil.which("wtype"):
            import seat_linux
            mods = [LINUX_MODS[p.lower()] for p in parts[:-1] if p.lower() in LINUX_MODS]
            args: list[str] = []
            for mod in mods:
                args += ["-M", mod]
            args += ["-k", seat_linux._keysym(parts[-1])]
            for mod in reversed(mods):
                args += ["-m", mod]
            proc = subprocess.run(["wtype"] + args, capture_output=True, text=True)
            if proc.returncode != 0:
                raise RealError(f"wtype failed: {(proc.stderr or proc.stdout).strip()}")
            return
        if shutil.which("xdotool"):
            subprocess.run(["xdotool", "key", "+".join(parts)], check=True)
            return
        raise RealError("install wtype or xdotool to send keys to the real desktop")
    if sysinfo.IS_MAC:
        cg = _core_graphics()
        flags = 0
        for part in parts[:-1]:
            flags |= MAC_MODS.get(part.lower(), 0)
        key = MAC_KEYCODES.get(parts[-1].lower())
        if key is None:
            raise RealError(f"no macOS keycode known for {parts[-1]!r}")
        for down in (True, False):
            event = cg.CGEventCreateKeyboardEvent(None, key, down)
            if flags:
                cg.CGEventSetFlags(event, flags)
            cg.CGEventPost(0, event)
            cg.CFRelease(event)
            time.sleep(0.02)
        return
    import ctypes
    codes = []
    for part in parts:
        low = part.lower()
        if low in WIN_VK:
            codes.append(WIN_VK[low])
        elif len(part) == 1:
            codes.append(ctypes.windll.user32.VkKeyScanW(ord(part.upper())) & 0xFF)
        else:
            raise RealError(f"no Windows virtual key known for {part!r}")
    for code in codes:
        ctypes.windll.user32.keybd_event(code, 0, 0, 0)
    for code in reversed(codes):
        ctypes.windll.user32.keybd_event(code, 0, 2, 0)


def paste(text: str, combo: str | None = None) -> None:
    """Note: this *does* overwrite the user's clipboard. The seat has its own."""
    if sysinfo.IS_LINUX:
        tool = ["wl-copy"] if shutil.which("wl-copy") else (
            ["xclip", "-selection", "clipboard"] if shutil.which("xclip") else None)
        if not tool:
            raise RealError("install wl-clipboard or xclip to paste")
        subprocess.run(tool, input=text, text=True, check=True)
        combo = combo or "ctrl+v"
    elif sysinfo.IS_MAC:
        subprocess.run(["pbcopy"], input=text, text=True, check=True)
        combo = combo or "cmd+v"
    else:
        subprocess.run(["clip"], input=text, text=True, shell=True, check=True)
        combo = combo or "ctrl+v"
    time.sleep(0.15)
    press(combo)


def shot(dest: Path, scale: float = 0.66, quality: int = 62, **_) -> Path:
    dest.parent.mkdir(parents=True, exist_ok=True)
    if sysinfo.IS_LINUX:
        if shutil.which("grim"):
            proc = subprocess.run(["grim", "-t", "jpeg", "-q", str(quality), "-s", f"{scale:g}",
                                   str(dest)], capture_output=True, text=True)
            if proc.returncode != 0:
                raise RealError(f"grim failed: {(proc.stderr or proc.stdout).strip()}")
            return dest
        if shutil.which("import"):
            subprocess.run(["import", "-window", "root", str(dest)], check=True)
            return dest
        raise RealError("no screen capture tool (grim on Wayland, imagemagick on X11)")
    if sysinfo.IS_MAC:
        subprocess.run(["screencapture", "-x", "-t", "jpg", str(dest)], check=True)
        return dest
    script = (
        "Add-Type -AssemblyName System.Windows.Forms,System.Drawing;"
        "$b=[System.Windows.Forms.SystemInformation]::VirtualScreen;"
        "$bmp=New-Object System.Drawing.Bitmap $b.Width,$b.Height;"
        "$g=[System.Drawing.Graphics]::FromImage($bmp);"
        "$g.CopyFromScreen($b.Left,$b.Top,0,0,$bmp.Size);"
        f"$bmp.Save('{dest}',[System.Drawing.Imaging.ImageFormat]::Jpeg)")
    if not sysinfo._powershell(script):
        if not dest.exists():
            raise RealError("screen capture via PowerShell failed")
    return dest
