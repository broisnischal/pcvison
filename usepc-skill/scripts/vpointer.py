"""A pointer device the agent owns, spoken straight down the Wayland wire.

`zwlr_virtual_pointer_v1` lets a client create a pointer on a compositor's seat
without touching /dev/uinput, without root, and — crucially — against *any*
compositor socket, including one that is not the user's session. Point this at
the agent's own compositor instance and the agent gets a cursor of its own.

Pure stdlib on purpose: this has to work with the system python on a machine
where nothing was pip-installed.

Wire format recap (little-endian throughout):

    header   uint32 object_id
             uint32 (size << 16) | opcode      -- size counts the header
    uint/int 4 bytes
    fixed    int32, 24.8 signed fixed point
    string   uint32 length-including-NUL, then the bytes, padded to 4
    new_id   uint32
    object   uint32, 0 for null
"""

from __future__ import annotations

import os
import socket
import struct
import time

DISPLAY_ID = 1
WL_DISPLAY_SYNC = 0
WL_DISPLAY_GET_REGISTRY = 1
WL_DISPLAY_ERROR = 0
WL_DISPLAY_DELETE_ID = 1
WL_REGISTRY_BIND = 0
WL_REGISTRY_GLOBAL = 0

VP_MANAGER = "zwlr_virtual_pointer_manager_v1"
VP_CREATE = 0  # create_virtual_pointer(seat, id)
VP_MOTION_ABS = 1  # motion_absolute(time, x, y, x_extent, y_extent)
VP_BUTTON = 2  # button(time, button, state)
VP_AXIS = 3  # axis(time, axis, value)
VP_FRAME = 4  # frame()
VP_AXIS_SOURCE = 5  # axis_source(source)
VP_AXIS_STOP = 6  # axis_stop(time, axis)
VP_AXIS_DISCRETE = 7  # axis_discrete(time, axis, value, discrete)
VP_DESTROY = 8

# linux/input-event-codes.h
BTN = {"left": 0x110, "right": 0x111, "middle": 0x112, "side": 0x113, "extra": 0x114,
       "back": 0x116, "forward": 0x115}
AXIS_VERTICAL, AXIS_HORIZONTAL = 0, 1
AXIS_SOURCE_WHEEL = 0
NOTCH = 15.0  # one wheel click, in surface-local units, as libinput reports it


class WaylandError(RuntimeError):
    pass


def _fixed(value: float) -> int:
    return int(round(value * 256.0))


def _string(text: str) -> bytes:
    raw = text.encode() + b"\0"
    return struct.pack("<I", len(raw)) + raw + b"\0" * (-len(raw) % 4)


def socket_path(display: str | None = None) -> str:
    """Resolve a WAYLAND_DISPLAY value (or absolute path) to a socket path."""
    disp = display or os.environ.get("WAYLAND_DISPLAY")
    if not disp:
        raise WaylandError("no WAYLAND_DISPLAY set and none given")
    if disp.startswith("/"):
        return disp
    runtime = os.environ.get("XDG_RUNTIME_DIR") or f"/run/user/{os.getuid()}"
    return os.path.join(runtime, disp)


class VirtualPointer:
    """One pointer device on the compositor reachable at `display`.

    The device lives as long as this object does, so keep it alive across a
    click-drag-release sequence and let the compositor see a coherent stream.
    """

    def __init__(self, display: str | None = None, extent: tuple[int, int] | None = None):
        path = socket_path(display)
        if not os.path.exists(path):
            raise WaylandError(f"no compositor socket at {path}")
        self.display = display or os.environ.get("WAYLAND_DISPLAY", "")
        self.extent = extent
        self._sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self._sock.settimeout(2.0)
        self._sock.connect(path)
        self._next_id = 2
        self._inbox = b""
        registry = self._alloc()
        self._request(DISPLAY_ID, WL_DISPLAY_GET_REGISTRY, struct.pack("<I", registry))
        globals_ = self._collect_globals(registry)
        if VP_MANAGER not in globals_:
            raise WaylandError(
                f"{self.display or path} does not advertise {VP_MANAGER}; "
                "this compositor cannot host an agent pointer"
            )
        name, version = globals_[VP_MANAGER]
        manager = self._alloc()
        self._request(
            registry, WL_REGISTRY_BIND,
            struct.pack("<I", name) + _string(VP_MANAGER)
            + struct.pack("<II", min(version, 2), manager),
        )
        self.id = self._alloc()
        self._request(manager, VP_CREATE, struct.pack("<II", 0, self.id))
        self._check()

    # ---- plumbing -------------------------------------------------------

    def _alloc(self) -> int:
        self._next_id += 1
        return self._next_id - 1

    def _request(self, obj: int, opcode: int, payload: bytes = b"") -> None:
        size = 8 + len(payload)
        self._sock.sendall(struct.pack("<II", obj, (size << 16) | opcode) + payload)

    def _drain(self, window: float = 0.05) -> list[tuple[int, int, bytes]]:
        deadline = time.monotonic() + window
        events: list[tuple[int, int, bytes]] = []
        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                break
            try:
                self._sock.settimeout(remaining)
                chunk = self._sock.recv(8192)
            except (socket.timeout, BlockingIOError):
                break
            if not chunk:
                break
            self._inbox += chunk
            while len(self._inbox) >= 8:
                obj, word = struct.unpack("<II", self._inbox[:8])
                size, opcode = word >> 16, word & 0xFFFF
                if size < 8 or len(self._inbox) < size:
                    break
                events.append((obj, opcode, self._inbox[8:size]))
                self._inbox = self._inbox[size:]
        return events

    def _check(self, window: float = 0.05) -> None:
        for obj, opcode, body in self._drain(window):
            if obj == DISPLAY_ID and opcode == WL_DISPLAY_ERROR:
                bad, code = struct.unpack("<II", body[:8])
                length, = struct.unpack("<I", body[8:12])
                message = body[12:12 + length - 1].decode(errors="replace")
                raise WaylandError(f"compositor error on object {bad} (code {code}): {message}")

    def _collect_globals(self, registry: int) -> dict[str, tuple[int, int]]:
        self._request(DISPLAY_ID, WL_DISPLAY_SYNC, struct.pack("<I", self._alloc()))
        found: dict[str, tuple[int, int]] = {}
        for obj, opcode, body in self._drain(0.6):
            if obj == DISPLAY_ID and opcode == WL_DISPLAY_ERROR:
                self._inbox = struct.pack("<II", obj, (8 + len(body)) << 16 | opcode) + body
                self._check()
            if obj != registry or opcode != WL_REGISTRY_GLOBAL:
                continue
            name, = struct.unpack("<I", body[:4])
            length, = struct.unpack("<I", body[4:8])
            interface = body[8:8 + length - 1].decode(errors="replace")
            padded = 8 + ((length + 3) // 4) * 4
            version, = struct.unpack("<I", body[padded:padded + 4])
            found[interface] = (name, version)
        return found

    @staticmethod
    def _now() -> int:
        return int(time.monotonic() * 1000) & 0xFFFFFFFF

    # ---- pointer actions ------------------------------------------------

    def move(self, x: float, y: float, extent: tuple[int, int] | None = None) -> None:
        """Warp to absolute x,y inside an `extent`-sized coordinate space."""
        space = extent or self.extent
        if not space:
            raise WaylandError("motion_absolute needs the size of the coordinate space")
        width, height = space
        px = max(0, min(int(round(x)), width - 1))
        py = max(0, min(int(round(y)), height - 1))
        self._request(self.id, VP_MOTION_ABS,
                      struct.pack("<IIIII", self._now(), px, py, width, height))
        self.frame()

    def button(self, name: str = "left", pressed: bool = True) -> None:
        code = BTN.get(name.lower())
        if code is None:
            raise WaylandError(f"unknown button {name!r}; pick from {', '.join(sorted(BTN))}")
        self._request(self.id, VP_BUTTON,
                      struct.pack("<III", self._now(), code, 1 if pressed else 0))
        self.frame()

    def click(self, name: str = "left", count: int = 1, hold_ms: int = 40) -> None:
        for i in range(max(1, min(int(count), 5))):
            if i:
                time.sleep(0.08)
            self.button(name, True)
            time.sleep(max(int(hold_ms), 10) / 1000)
            self.button(name, False)

    def scroll(self, notches: int, horizontal: bool = False) -> None:
        """Positive scrolls down (or right); one notch is one wheel click."""
        axis = AXIS_HORIZONTAL if horizontal else AXIS_VERTICAL
        direction = 1 if notches > 0 else -1
        for _ in range(min(abs(int(notches)), 60)):
            self._request(self.id, VP_AXIS_SOURCE, struct.pack("<I", AXIS_SOURCE_WHEEL))
            self._request(self.id, VP_AXIS,
                          struct.pack("<IIi", self._now(), axis, _fixed(NOTCH * direction)))
            self._request(self.id, VP_AXIS_DISCRETE,
                          struct.pack("<IIii", self._now(), axis,
                                      _fixed(NOTCH * direction), direction))
            self.frame()
            time.sleep(0.02)
        self._request(self.id, VP_AXIS_STOP, struct.pack("<II", self._now(), axis))
        self.frame()

    def drag(self, x1: float, y1: float, x2: float, y2: float,
             button: str = "left", steps: int = 24,
             extent: tuple[int, int] | None = None) -> None:
        self.move(x1, y1, extent)
        time.sleep(0.1)
        self.button(button, True)
        try:
            time.sleep(0.1)
            steps = max(2, min(int(steps), 120))
            for i in range(1, steps + 1):
                self.move(x1 + (x2 - x1) * i / steps, y1 + (y2 - y1) * i / steps, extent)
                time.sleep(0.012)
            time.sleep(0.1)
        finally:
            self.button(button, False)

    def frame(self) -> None:
        self._request(self.id, VP_FRAME)

    def flush(self, settle: float = 0.12) -> None:
        """Let the compositor act on what we sent, and surface any protocol error."""
        self._request(DISPLAY_ID, WL_DISPLAY_SYNC, struct.pack("<I", self._alloc()))
        self._check(settle)

    def close(self) -> None:
        try:
            self._request(self.id, VP_DESTROY)
            self.flush(0.05)
        except Exception:
            pass
        finally:
            try:
                self._sock.close()
            except Exception:
                pass

    def __enter__(self) -> "VirtualPointer":
        return self

    def __exit__(self, *exc) -> None:
        self.close()


def supported(display: str | None = None) -> bool:
    """Does the compositor at `display` allow a client to own a pointer?"""
    try:
        VirtualPointer(display, (1, 1)).close()
        return True
    except Exception:
        return False
