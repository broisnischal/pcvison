#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.11"
# dependencies = ["mcp[cli]>=1.9.0", "pillow>=10.1"]
# ///
"""pcvision — let Claude see this machine's screen.

Two entry points in one file:
  * `pcvision.py serve`  -> MCP stdio server (for Claude Code / Claude Desktop)
  * `pcvision.py shot|watch|record|live|input|listen|monitors|windows` -> CLI

Backend: grim (capture) + slurp (interactive region) + hyprctl (monitor/window
geometry, cursor warping) + ffmpeg (video) + wtype (keyboard) + /dev/uinput
(mouse buttons and wheel). Wayland; the input half is Hyprland-specific.
"""

from __future__ import annotations

import argparse
import fcntl
import glob
import io
import json
import os
import shutil
import signal
import struct
import subprocess
import sys
import tempfile
import time
from pathlib import Path

from PIL import Image as PILImage
from PIL import ImageDraw, ImageFont

CACHE = Path(os.environ.get("XDG_CACHE_HOME") or Path.home() / ".cache") / "pcvision"

# Long-edge pixel budget per detail level. Claude's vision tops out usefully
# around 1568px; anything bigger just burns tokens.
DETAIL = {"low": 800, "normal": 1280, "high": 1568, "full": 0}

MAX_FRAMES = 12
MAX_SECONDS = 120.0


# --------------------------------------------------------------------------- env


def bootstrap_env() -> None:
    """MCP servers are spawned without a login shell, so the Wayland/Hyprland
    handles may be missing. Recover them from the runtime dir."""
    rt = os.environ.get("XDG_RUNTIME_DIR") or f"/run/user/{os.getuid()}"
    os.environ["XDG_RUNTIME_DIR"] = rt

    if not os.environ.get("WAYLAND_DISPLAY"):
        socks = [s for s in sorted(glob.glob(f"{rt}/wayland-*")) if not s.endswith(".lock")]
        if socks:
            os.environ["WAYLAND_DISPLAY"] = os.path.basename(socks[0])

    if not os.environ.get("HYPRLAND_INSTANCE_SIGNATURE"):
        insts = sorted(
            (d for d in glob.glob(f"{rt}/hypr/*") if os.path.isdir(d)),
            key=os.path.getmtime,
            reverse=True,
        )
        if insts:
            os.environ["HYPRLAND_INSTANCE_SIGNATURE"] = os.path.basename(insts[0])


def rebind_env() -> str | None:
    """Same idea, but for a session that moved under a long-running process: a
    compositor restart hands out a fresh socket, and the old one never recovers.
    Only worth calling once captures have started failing."""
    rt = os.environ.get("XDG_RUNTIME_DIR") or f"/run/user/{os.getuid()}"
    socks = sorted((s for s in glob.glob(f"{rt}/wayland-*") if not s.endswith(".lock")),
                   key=os.path.getmtime, reverse=True)
    insts = sorted((d for d in glob.glob(f"{rt}/hypr/*") if os.path.isdir(d)),
                   key=os.path.getmtime, reverse=True)
    if insts:
        os.environ["HYPRLAND_INSTANCE_SIGNATURE"] = os.path.basename(insts[0])
    if socks:
        os.environ["WAYLAND_DISPLAY"] = os.path.basename(socks[0])
        return os.path.basename(socks[0])
    return None


# ----------------------------------------------------------------------- capture


def _run(cmd: list[str], **kw) -> subprocess.CompletedProcess:
    return subprocess.run(cmd, capture_output=True, **kw)


def grim(geometry: str | None = None, output: str | None = None, cursor: bool = False) -> bytes:
    """Capture PNG bytes. No geometry/output -> whole multi-monitor layout."""
    if not shutil.which("grim"):
        raise RuntimeError("grim is not installed (pacman -S grim)")
    cmd = ["grim"]
    if cursor:
        cmd.append("-c")
    if output:
        cmd += ["-o", output]
    if geometry:
        cmd += ["-g", geometry]
    p = _run(cmd + ["-"])
    if p.returncode != 0 or not p.stdout:
        err = p.stderr.decode(errors="replace").strip() or "empty output"
        raise RuntimeError(f"grim failed: {err} (WAYLAND_DISPLAY={os.environ.get('WAYLAND_DISPLAY')})")
    return p.stdout


def hyprctl(*args: str):
    if not shutil.which("hyprctl"):
        raise RuntimeError("hyprctl not found — this tool targets Hyprland")
    p = _run(["hyprctl", "-j", *args], text=True)
    if p.returncode != 0:
        raise RuntimeError(f"hyprctl {' '.join(args)} failed: {p.stderr.strip()}")
    return json.loads(p.stdout)


def monitors() -> list[dict]:
    out = []
    for m in hyprctl("monitors"):
        out.append(
            {
                "name": m["name"],
                "description": m.get("description", ""),
                "resolution": f'{m["width"]}x{m["height"]}',
                "position": f'{m["x"]},{m["y"]}',
                "scale": m.get("scale"),
                "refresh": round(m.get("refreshRate", 0), 1),
                "focused": m.get("focused", False),
                "workspace": (m.get("activeWorkspace") or {}).get("name"),
                "geometry": f'{m["x"]},{m["y"]} {m["width"]}x{m["height"]}',
            }
        )
    return out


def windows() -> list[dict]:
    try:
        active = (hyprctl("activewindow") or {}).get("address")
    except RuntimeError:
        active = None
    out = []
    for c in hyprctl("clients"):
        if not c.get("mapped") or c.get("hidden"):
            continue
        w, h = c["size"]
        if w <= 0 or h <= 0:
            continue
        out.append(
            {
                "address": c.get("address"),
                "title": c.get("title", ""),
                "class": c.get("class", ""),
                "workspace": (c.get("workspace") or {}).get("name"),
                "monitor": c.get("monitor"),
                "geometry": f'{c["at"][0]},{c["at"][1]} {w}x{h}',
                "floating": c.get("floating", False),
                "focused": active is not None and c.get("address") == active,
            }
        )
    return out


def find_window(query: str) -> dict:
    wins = windows()
    if not wins:
        raise RuntimeError("no mapped windows found")
    if query.lower() in ("active", "focused", "current"):
        for w in wins:
            if w["focused"]:
                return w
        raise RuntimeError("no focused window")
    q = query.lower()
    hits = [w for w in wins if q in w["title"].lower() or q in w["class"].lower()]
    if not hits:
        listing = ", ".join(f'{w["class"]}:{w["title"][:30]}' for w in wins)
        raise RuntimeError(f"no window matching {query!r}. Open windows: {listing}")
    hits.sort(key=lambda w: not w["focused"])
    return hits[0]


def slurp_region(single_window: bool = False) -> str:
    """Interactive picker — the human drags a box (or clicks a window)."""
    if not shutil.which("slurp"):
        raise RuntimeError("slurp is not installed (pacman -S slurp)")
    cmd = ["slurp"]
    if single_window:
        boxes = "\n".join(w["geometry"] for w in windows())
        p = subprocess.run(cmd, input=boxes, capture_output=True, text=True)
    else:
        p = subprocess.run(cmd, capture_output=True, text=True)
    geo = p.stdout.strip()
    if p.returncode != 0 or not geo:
        raise RuntimeError("region selection cancelled")
    return geo


def resolve_target(
    monitor: str | None, window: str | None, region: str | None
) -> tuple[str | None, str | None, str]:
    """-> (geometry, output, human label)"""
    if sum(x is not None for x in (monitor, window, region)) > 1:
        raise RuntimeError("pass at most one of monitor / window / region")
    if region:
        return region, None, f"region {region}"
    if window:
        w = find_window(window)
        return w["geometry"], None, f'window {w["class"]}: {w["title"][:60]}'
    if monitor:
        names = [m["name"] for m in monitors()]
        if monitor not in names:
            raise RuntimeError(f"unknown monitor {monitor!r}; available: {', '.join(names)}")
        return None, monitor, f"monitor {monitor}"
    return None, None, "full desktop (all monitors)"


# ------------------------------------------------------------------------ encode


def encode(png: bytes, detail: str = "normal") -> tuple[bytes, str, tuple[int, int], tuple[int, int]]:
    """Downscale + recompress so a screenshot costs ~1-2k tokens, not 30k.

    Returns (bytes, format, native size, sent size). The sent size matters for
    input control: coordinates read off the image must be scaled back up.
    """
    im = PILImage.open(io.BytesIO(png))
    native = im.size
    if detail == "full":
        return png, "png", native, native
    budget = DETAIL.get(detail, DETAIL["normal"])
    im = im.convert("RGB")
    if max(im.size) > budget:
        im.thumbnail((budget, budget), PILImage.LANCZOS)
    buf = io.BytesIO()
    im.save(buf, "JPEG", quality=72, optimize=True)
    return buf.getvalue(), "jpeg", native, im.size


def _font(size: int):
    try:
        return ImageFont.load_default(size=size)
    except TypeError:  # Pillow < 10.1
        return ImageFont.load_default()


def contact_sheet(frames: list[tuple[str, bytes]], cols: int = 3, cell_width: int = 480) -> bytes:
    """Tile labelled frames into one image — a 'video' Claude can read in one look."""
    if not frames:
        raise RuntimeError("no frames captured")
    thumbs = []
    for label, png in frames:
        im = PILImage.open(io.BytesIO(png)).convert("RGB")
        ratio = cell_width / im.width
        thumbs.append((label, im.resize((cell_width, max(1, round(im.height * ratio))), PILImage.LANCZOS)))

    cols = max(1, min(cols, len(thumbs)))
    rows = -(-len(thumbs) // cols)
    cw = cell_width
    ch = max(t.height for _, t in thumbs)
    bar, pad = 22, 6
    sheet = PILImage.new("RGB", (cols * (cw + pad) + pad, rows * (ch + bar + pad) + pad), (18, 18, 20))
    draw = ImageDraw.Draw(sheet)
    font = _font(16)
    for i, (label, t) in enumerate(thumbs):
        x = pad + (i % cols) * (cw + pad)
        y = pad + (i // cols) * (ch + bar + pad)
        draw.text((x + 2, y + 3), f"#{i + 1}  {label}", fill=(235, 235, 235), font=font)
        sheet.paste(t, (x, y + bar))
    buf = io.BytesIO()
    sheet.save(buf, "JPEG", quality=75, optimize=True)
    return buf.getvalue()


def capture_series(
    seconds: float, count: int, geometry: str | None, output: str | None, cursor: bool
) -> list[tuple[float, bytes]]:
    seconds = max(0.0, min(float(seconds), MAX_SECONDS))
    count = max(1, min(int(count), MAX_FRAMES))
    gap = seconds / max(1, count - 1) if count > 1 else 0.0
    t0 = time.monotonic()
    frames: list[tuple[float, bytes]] = []
    for i in range(count):
        due = t0 + i * gap
        now = time.monotonic()
        if now < due:
            time.sleep(due - now)
        frames.append((time.monotonic() - t0, grim(geometry, output, cursor)))
    return frames


def encode_video(frames: list[tuple[float, bytes]], dest: Path, fps: float) -> Path:
    if not shutil.which("ffmpeg"):
        raise RuntimeError("ffmpeg is not installed")
    dest.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory() as td:
        for i, (_, png) in enumerate(frames):
            (Path(td) / f"f_{i:04d}.png").write_bytes(png)
        p = _run(
            [
                "ffmpeg", "-y", "-loglevel", "error",
                "-framerate", f"{max(fps, 0.5):.3f}",
                "-i", str(Path(td) / "f_%04d.png"),
                "-vf", "scale=trunc(iw/2)*2:trunc(ih/2)*2",
                "-c:v", "libx264", "-pix_fmt", "yuv420p", "-crf", "26",
                str(dest),
            ]
        )
    if p.returncode != 0:
        raise RuntimeError(f"ffmpeg failed: {p.stderr.decode(errors='replace').strip()[-400:]}")
    return dest


# ------------------------------------------------------- live ring-buffer daemon

LIVE = CACHE / "live"
PIDFILE = LIVE / "daemon.json"


def _frame_ts(p: Path) -> float:
    return int(p.stem.split("-", 1)[1]) / 1000.0


def _buffered() -> list[Path]:
    return sorted(LIVE.glob("f-*.jpg"), key=_frame_ts)


def _meta() -> dict | None:
    try:
        return json.loads(PIDFILE.read_text())
    except Exception:
        return None


def _alive(pid: int) -> bool:
    try:
        os.kill(pid, 0)
        return True
    except OSError:
        return False


def live_status() -> dict:
    m = _meta()
    frames = _buffered()
    st: dict = {
        "running": bool(m and _alive(m["pid"])),
        "frames_buffered": len(frames),
        "buffer_dir": str(LIVE),
    }
    if m:
        st.update({k: v for k, v in m.items() if k != "pid"})
        st["pid"] = m["pid"]
    if frames:
        st["buffer_span_seconds"] = round(_frame_ts(frames[-1]) - _frame_ts(frames[0]), 1)
        st["newest_frame_age_seconds"] = round(time.time() - _frame_ts(frames[-1]), 1)
    return st


def live_start(
    fps: float = 1.0,
    window_seconds: float = 120.0,
    monitor: str | None = None,
    window: str | None = None,
    region: str | None = None,
    detail: str = "low",
    include_cursor: bool = True,
    restart: bool = False,
    keep_buffer: bool = False,
) -> dict:
    """Spawn a detached recorder that keeps the last `window_seconds` of screen."""
    m = _meta()
    if m and _alive(m["pid"]):
        if not restart:
            return {"already_running": True, **live_status()}
        live_stop()

    _, _, label = resolve_target(monitor, window, region)  # validate before detaching
    LIVE.mkdir(parents=True, exist_ok=True)
    if not keep_buffer:  # a revive keeps the history it already has
        for f in _buffered():
            f.unlink(missing_ok=True)

    fps = max(0.1, min(float(fps), 4.0))
    argv = [
        sys.executable, os.path.abspath(__file__), "live", "_run",
        "--fps", str(fps),
        "--window-seconds", str(window_seconds),
        "--detail", detail,
    ]
    for flag, val in (("-m", monitor), ("-w", window), ("-r", region)):
        if val:
            argv += [flag, val]
    if include_cursor:
        argv.append("--cursor")

    log = (LIVE / "daemon.log").open("ab")
    proc = subprocess.Popen(
        argv, stdin=subprocess.DEVNULL, stdout=log, stderr=log, start_new_session=True
    )
    PIDFILE.write_text(
        json.dumps(
            {
                "pid": proc.pid,
                "fps": fps,
                "target": label,
                "detail": detail,
                "window_seconds": window_seconds,
                "started": time.strftime("%Y-%m-%d %H:%M:%S"),
                # kept so a recorder that died can be brought back exactly as asked for
                "args": {"monitor": monitor, "window": window, "region": region,
                         "include_cursor": include_cursor},
            }
        )
    )
    for _ in range(30):  # wait for the first frame so callers get a usable buffer
        if _buffered():
            break
        if proc.poll() is not None:
            raise RuntimeError(f"recorder died immediately; see {LIVE / 'daemon.log'}")
        time.sleep(0.1)
    return {"launched": True, **live_status()}


def live_stop() -> dict:
    m = _meta()
    if not m:
        return {"running": False, "note": "no recorder registered"}
    pid = m["pid"]
    if _alive(pid):
        os.kill(pid, signal.SIGTERM)
        for _ in range(30):
            if not _alive(pid):
                break
            time.sleep(0.1)
    PIDFILE.unlink(missing_ok=True)
    return {"stopped": True, "pid": pid, "frames_left_in_buffer": len(_buffered())}


def live_run(fps: float, window_seconds: float, geo, out, cursor: bool, detail: str) -> int:
    LIVE.mkdir(parents=True, exist_ok=True)
    interval = 1.0 / max(0.1, fps)
    keep = max(window_seconds * 1.2, interval * 3)
    nxt = time.monotonic()
    fails = 0
    while True:
        try:
            data, _, _, _ = encode(grim(geo, out, cursor), detail)
            (LIVE / f"f-{int(time.time() * 1000)}.jpg").write_bytes(data)
            fails = 0
        except RuntimeError as e:  # screen locked, monitor unplugged, compositor restart
            fails += 1
            note = ""
            if fails in (3, 10) or fails % 30 == 0:
                # A restarted compositor is a new socket, not a dead machine.
                note = f" — rebound to {rebind_env()}"
            print(f"[{time.strftime('%H:%M:%S')}] capture failed ({fails}): {e}{note}",
                  file=sys.stderr, flush=True)
            # Every one of these ends when the user comes back, so wait it out
            # instead of exiting and losing the history that is already buffered.
            time.sleep(min(30.0, interval * (2 ** min(fails, 5))))
            nxt = time.monotonic()
            continue
        cutoff = time.time() - keep
        for p in _buffered():
            if _frame_ts(p) < cutoff:
                p.unlink(missing_ok=True)
        nxt += interval
        time.sleep(max(0.05, nxt - time.monotonic()))


def _sig(data: bytes) -> bytes:
    return PILImage.open(io.BytesIO(data)).convert("L").resize((32, 32), PILImage.BILINEAR).tobytes()


def _delta(a: bytes, b: bytes) -> float:
    return sum(abs(x - y) for x, y in zip(a, b)) / len(a) / 255.0


def live_revive() -> str | None:
    """Bring a recorder that died back on the same target. Only ever revives a
    watch the user already asked for — it never starts one by itself."""
    m = _meta()
    if not m or _alive(m["pid"]):
        return None
    args = m.get("args") or {}
    live_start(m.get("fps", 1.0), m.get("window_seconds", 120.0),
               args.get("monitor"), args.get("window"), args.get("region"),
               m.get("detail", "low"), args.get("include_cursor", True),
               restart=True, keep_buffer=True)
    return f"recorder had died; restarted it on {m.get('target')}"


def live_window(
    seconds: float = 30.0, count: int = 6, changes_only: bool = True, threshold: float = 0.012
) -> tuple[list[tuple[float, bytes]], int, int]:
    """Pull the buffered frames covering the last `seconds`.

    Returns (frames as (age_seconds, jpeg) newest-last, total_in_window, static_frames_dropped).
    """
    live_revive()
    now = time.time()
    picked = [(now - _frame_ts(p), p.read_bytes()) for p in _buffered() if now - _frame_ts(p) <= seconds]
    total = len(picked)
    if not picked:
        raise RuntimeError(
            "live buffer is empty for that window — is the recorder running? (live_status / live_start)"
        )
    count = max(1, min(int(count), MAX_FRAMES))
    dropped = 0
    if changes_only and total > count:
        sigs = [_sig(d) for _, d in picked]
        keep = [0]
        for i in range(1, total):
            if _delta(sigs[i], sigs[keep[-1]]) >= threshold:
                keep.append(i)
            else:
                dropped += 1
        if keep[-1] != total - 1:
            keep.append(total - 1)  # always show the newest frame
        picked = [picked[i] for i in keep]
    if len(picked) > count:
        step = (len(picked) - 1) / (count - 1) if count > 1 else 0
        picked = [picked[round(j * step)] for j in range(count)]
    return picked, total, dropped


def save_bytes(data: bytes, ext: str, tag: str = "shot") -> Path:
    CACHE.mkdir(parents=True, exist_ok=True)
    path = CACHE / f"{tag}-{time.strftime('%Y%m%d-%H%M%S')}-{os.getpid()}.{ext}"
    path.write_bytes(data)
    return path


# ------------------------------------------------------------------ input control

INPUT_LOCK = CACHE / "input-disabled"
INPUT_LOG = CACHE / "input.log"
UINPUT_DEV = "/dev/uinput"

EV_SYN, EV_KEY, EV_REL = 0, 1, 2
SYN_REPORT = 0
REL_X, REL_Y, REL_HWHEEL, REL_WHEEL = 0, 1, 6, 8
BUTTONS = {"left": 0x110, "right": 0x111, "middle": 0x112}
_EVENT = "llHHi"      # struct input_event on 64-bit
_SETUP = "HHHH80sI"   # struct uinput_setup


def _ioc(direction: int, nr: int, size: int) -> int:
    return (direction << 30) | (size << 16) | (ord("U") << 8) | nr


UI_DEV_CREATE = _ioc(0, 1, 0)
UI_DEV_DESTROY = _ioc(0, 2, 0)
UI_DEV_SETUP = _ioc(1, 3, struct.calcsize(_SETUP))
UI_SET_EVBIT = _ioc(1, 100, 4)
UI_SET_KEYBIT = _ioc(1, 101, 4)
UI_SET_RELBIT = _ioc(1, 102, 4)


def input_allowed() -> None:
    """Kill switch. Only the human can lift it — see `pcvision input enable`."""
    if INPUT_LOCK.exists():
        raise RuntimeError(
            "input control is switched OFF by the user. Ask them to run "
            "`pcvision input enable` — you cannot re-enable it yourself."
        )


def audit(action: str) -> None:
    CACHE.mkdir(parents=True, exist_ok=True)
    with INPUT_LOG.open("a") as fh:
        fh.write(f"{time.strftime('%Y-%m-%d %H:%M:%S')}  {action}\n")


class VirtualPointer:
    """A uinput mouse for buttons and scrolling.

    Positioning is deliberately NOT done here — `hyprctl dispatch movecursor`
    warps the cursor to exact layout coordinates, whereas relative uinput motion
    would be mangled by libinput's acceleration curve.
    """

    _shared: "VirtualPointer | None" = None

    @classmethod
    def shared(cls) -> "VirtualPointer":
        if cls._shared is None:
            cls._shared = cls()
        return cls._shared

    def __init__(self) -> None:
        if not os.access(UINPUT_DEV, os.W_OK):
            raise RuntimeError(
                f"no write access to {UINPUT_DEV}. Grant it for this session with:\n"
                f"  sudo setfacl -m u:{os.environ.get('USER', 'you')}:rw /dev/uinput\n"
                "or permanently with the udev rule in the README."
            )
        self.fd = os.open(UINPUT_DEV, os.O_WRONLY | os.O_NONBLOCK)
        for ev in (EV_KEY, EV_REL):
            fcntl.ioctl(self.fd, UI_SET_EVBIT, ev)
        for code in BUTTONS.values():
            fcntl.ioctl(self.fd, UI_SET_KEYBIT, code)
        for axis in (REL_X, REL_Y, REL_WHEEL, REL_HWHEEL):
            fcntl.ioctl(self.fd, UI_SET_RELBIT, axis)
        fcntl.ioctl(
            self.fd,
            UI_DEV_SETUP,
            struct.pack(_SETUP, 0x03, 0x1D1D, 0x0001, 1, b"pcvision virtual pointer", 0),
        )
        fcntl.ioctl(self.fd, UI_DEV_CREATE)
        time.sleep(0.35)  # the compositor needs a moment to bind the new device

    def _emit(self, etype: int, code: int, value: int) -> None:
        os.write(self.fd, struct.pack(_EVENT, 0, 0, etype, code, value))
        if etype != EV_SYN:
            os.write(self.fd, struct.pack(_EVENT, 0, 0, EV_SYN, SYN_REPORT, 0))

    def button(self, name: str, down: bool) -> None:
        code = BUTTONS.get(name)
        if code is None:
            raise RuntimeError(f"unknown button {name!r}; use left, right or middle")
        self._emit(EV_KEY, code, 1 if down else 0)

    def click(self, name: str = "left", count: int = 1, hold_ms: int = 40) -> None:
        for i in range(max(1, min(int(count), 5))):
            if i:
                time.sleep(0.07)
            self.button(name, True)
            time.sleep(max(int(hold_ms), 10) / 1000)
            self.button(name, False)

    def scroll(self, amount: int, horizontal: bool = False) -> None:
        axis = REL_HWHEEL if horizontal else REL_WHEEL
        step = 1 if amount > 0 else -1
        for _ in range(min(abs(int(amount)), 50)):
            self._emit(EV_REL, axis, step)
            time.sleep(0.012)


def layout_bounds() -> tuple[int, int]:
    mons = monitors()
    if not mons:
        raise RuntimeError("no monitors reported")
    right = max(int(m["position"].split(",")[0]) + int(m["resolution"].split("x")[0]) for m in mons)
    bottom = max(int(m["position"].split(",")[1]) + int(m["resolution"].split("x")[1]) for m in mons)
    return right, bottom


def to_layout(
    x: float, y: float, monitor: str | None = None, from_width: int | None = None
) -> tuple[int, int]:
    """Map coordinates read off a screenshot to Hyprland layout coordinates.

    monitor:    the shot was of this monitor, so x,y are relative to its top-left.
    from_width: width of the image you actually measured on — screenshots are
                downscaled, so without this every click lands short and up-left.
    """
    if monitor:
        m = next((mm for mm in monitors() if mm["name"] == monitor), None)
        if m is None:
            raise RuntimeError(f"unknown monitor {monitor!r}")
        native_w = int(m["resolution"].split("x")[0])
        ox, oy = (int(v) for v in m["position"].split(","))
    else:
        native_w = layout_bounds()[0]
        ox = oy = 0
    if from_width and from_width > 0:
        factor = native_w / float(from_width)
        x, y = x * factor, y * factor
    px, py = round(ox + x), round(oy + y)
    lw, lh = layout_bounds()
    if not (0 <= px < lw and 0 <= py < lh):
        raise RuntimeError(
            f"{px},{py} falls outside the {lw}x{lh} monitor layout "
            "(did you forget monitor= or from_width=?)"
        )
    return px, py


def cursor_pos() -> tuple[int, int]:
    p = _run(["hyprctl", "cursorpos"], text=True)
    try:
        x, y = (int(v.strip()) for v in p.stdout.strip().split(","))
    except Exception:
        raise RuntimeError(f"could not read cursor position: {p.stdout.strip()}{p.stderr.strip()}")
    return x, y


def move_cursor(px: int, py: int) -> None:
    p = _run(["hyprctl", "dispatch", "movecursor", str(px), str(py)], text=True)
    if p.returncode != 0 or "ok" not in p.stdout:
        raise RuntimeError(f"movecursor failed: {p.stdout.strip()} {p.stderr.strip()}")


def drag(x1: int, y1: int, x2: int, y2: int, button: str = "left", steps: int = 24) -> None:
    ptr = VirtualPointer.shared()
    move_cursor(x1, y1)
    time.sleep(0.1)
    ptr.button(button, True)
    try:
        time.sleep(0.1)
        steps = max(2, min(int(steps), 100))
        for i in range(1, steps + 1):
            move_cursor(round(x1 + (x2 - x1) * i / steps), round(y1 + (y2 - y1) * i / steps))
            time.sleep(0.015)
        time.sleep(0.1)
    finally:
        ptr.button(button, False)


MODIFIERS = {
    "ctrl": "ctrl", "control": "ctrl", "shift": "shift", "alt": "alt", "opt": "alt",
    "option": "alt", "altgr": "altgr", "super": "logo", "meta": "logo", "cmd": "logo",
    "win": "logo", "logo": "logo", "capslock": "capslock",
}

KEY_ALIASES = {
    "enter": "Return", "return": "Return", "esc": "Escape", "escape": "Escape",
    "tab": "Tab", "space": "space", "backspace": "BackSpace", "bs": "BackSpace",
    "del": "Delete", "delete": "Delete", "up": "Up", "down": "Down", "left": "Left",
    "right": "Right", "home": "Home", "end": "End", "pgup": "Prior", "pageup": "Prior",
    "pgdn": "Next", "pagedown": "Next", "insert": "Insert", "menu": "Menu",
    "print": "Print", "minus": "minus", "plus": "plus", "equal": "equal",
}


def _keysym(token: str) -> str:
    low = token.lower()
    if low in KEY_ALIASES:
        return KEY_ALIASES[low]
    if len(token) == 1:
        return token
    if low.startswith("f") and low[1:].isdigit():
        return "F" + low[1:]
    return token  # trust libxkbcommon: "XF86AudioPlay", "Hyper_L", ...


def wtype(args: list[str], stdin: str | None = None) -> None:
    if not shutil.which("wtype"):
        raise RuntimeError("wtype is not installed (pacman -S wtype)")
    p = subprocess.run(["wtype", *args], input=stdin, capture_output=True, text=True)
    if p.returncode != 0:
        raise RuntimeError(f"wtype failed: {(p.stderr or p.stdout).strip()}")


def type_text(text: str, delay_ms: int = 12) -> None:
    if not text:
        raise RuntimeError("nothing to type")
    if len(text) > 8000:
        raise RuntimeError(f"{len(text)} chars is a lot to type — use paste instead")
    wtype(["-d", str(max(0, min(int(delay_ms), 200))), "-"], stdin=text)


def press_combo(combo: str) -> None:
    """'ctrl+shift+t', 'Return', 'alt+F4' -> one keystroke with modifiers held."""
    parts = [p for p in combo.replace(" ", "").split("+") if p]
    if not parts:
        raise RuntimeError("empty key combo")
    mods: list[str] = []
    key: str | None = None
    for i, part in enumerate(parts):
        mod = MODIFIERS.get(part.lower())
        if mod and i < len(parts) - 1:
            mods.append(mod)
        else:
            key = part
    if key is None:
        raise RuntimeError(f"{combo!r} has modifiers but no key")
    args: list[str] = []
    for m in mods:
        args += ["-M", m]
    args += ["-k", _keysym(key)]
    for m in reversed(mods):
        args += ["-m", m]
    wtype(args)


def press_keys(combos: list[str], gap_ms: int = 60) -> None:
    for i, combo in enumerate(combos):
        if i:
            time.sleep(max(0, gap_ms) / 1000)
        press_combo(combo)


TERMINAL_CLASSES = {
    "alacritty", "foot", "footclient", "kitty", "ghostty", "com.mitchellh.ghostty",
    "org.wezfurlong.wezterm", "xterm", "urxvt", "st", "konsole", "gnome-terminal",
    "wezterm", "rio", "contour",
}


def paste_text(text: str, combo: str | None = None) -> None:
    """Clipboard + paste shortcut: instant and exact, unlike typing thousands of
    characters. Terminals use ctrl+shift+v, everything else ctrl+v — detected from
    the focused window unless `combo` overrides it. Replaces the clipboard."""
    if not shutil.which("wl-copy"):
        raise RuntimeError("wl-clipboard is not installed (pacman -S wl-clipboard)")
    # wl-copy forks a clipboard server that inherits our pipes, so capturing its
    # output would block until the clipboard is replaced. Discard them instead.
    p = subprocess.run(
        ["wl-copy"], input=text.encode(), stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL
    )
    if p.returncode != 0:
        raise RuntimeError(f"wl-copy exited {p.returncode}")
    time.sleep(0.08)
    if combo is None:
        cls = (next((w for w in windows() if w["focused"]), {}) or {}).get("class", "").lower()
        combo = "ctrl+shift+v" if cls in TERMINAL_CLASSES or "term" in cls else "ctrl+v"
    press_combo(combo)


def focused_monitor() -> str | None:
    return next((m["name"] for m in monitors() if m["focused"]), None)


def focus_window_impl(query: str) -> dict:
    w = find_window(query)
    if not w.get("address"):
        raise RuntimeError(f"no address for window {w['title'][:40]!r}")
    p = _run(["hyprctl", "dispatch", "focuswindow", f"address:{w['address']}"], text=True)
    if p.returncode != 0 or "ok" not in p.stdout:
        raise RuntimeError(f"focuswindow failed: {p.stdout.strip()} {p.stderr.strip()}")
    return w


def window_at(px: int, py: int) -> dict | None:
    """Topmost-ish window containing a layout point; focused wins ties."""
    hits = []
    for w in windows():
        ox, oy = (int(v) for v in w["geometry"].split(" ")[0].split(","))
        ww, wh = (int(v) for v in w["geometry"].split(" ")[1].split("x"))
        if ox <= px < ox + ww and oy <= py < oy + wh:
            hits.append(w)
    if not hits:
        return None
    hits.sort(key=lambda w: not w["focused"])
    return hits[0]


def ensure_focus(query: str, tries: int = 3) -> dict:
    """Focus a window and CONFIRM it holds focus. Typing blind is how keystrokes
    end up in someone's URL bar, so refuse rather than guess."""
    current = None
    for attempt in range(tries):
        want = focus_window_impl(query)
        # Hyprland reports the focus change before the client has taken keyboard
        # focus. Poll until it is stable, then give the toolkit a moment more —
        # otherwise wtype delivers to the previously focused surface.
        for _ in range(12):
            time.sleep(0.08)
            current = next((w for w in windows() if w["focused"]), None)
            if current and current.get("address") == want.get("address"):
                break
        if current and current.get("address") == want.get("address"):
            time.sleep(0.25 + 0.1 * attempt)
            if next((w for w in windows() if w["focused"]), {}).get("address") == want.get("address"):
                return current
    raise RuntimeError(
        f"could not confirm focus on {query!r} — focus is on "
        f"{current['class'] if current else 'nothing'}. Refusing to type blind."
    )


def goto_workspace(name: str) -> None:
    p = _run(["hyprctl", "dispatch", "workspace", str(name)], text=True)
    if p.returncode != 0 or "ok" not in p.stdout:
        raise RuntimeError(f"workspace failed: {p.stdout.strip()} {p.stderr.strip()}")


# ------------------------------------------------------------------------ audio

AUDIO_DIR = CACHE / "audio"
LISTEN_DIR = CACHE / "listen"
LISTEN_PID = LISTEN_DIR / "daemon.json"
LISTEN_LOG = LISTEN_DIR / "transcript.jsonl"
_WHISPER: dict = {}


def default_monitor() -> str:
    """The monitor source of the default output = what the user is hearing."""
    sink = _run(["pactl", "get-default-sink"], text=True).stdout.strip()
    if sink and not sink.startswith("Failure"):
        return f"{sink}.monitor"
    for line in _run(["pactl", "list", "short", "sources"], text=True).stdout.splitlines():
        parts = line.split()
        if len(parts) > 1 and parts[1].endswith(".monitor"):
            return parts[1]
    raise RuntimeError("no PulseAudio/PipeWire monitor source found")


def audio_sources() -> list[dict]:
    out = []
    default = ""
    try:
        default = default_monitor()
    except RuntimeError:
        pass
    for line in _run(["pactl", "list", "short", "sources"], text=True).stdout.splitlines():
        parts = line.split()
        if len(parts) < 2:
            continue
        out.append({
            "name": parts[1],
            "kind": "output monitor (what you hear)" if parts[1].endswith(".monitor") else "input (microphone)",
            "state": parts[-1],
            "default_for_listening": parts[1] == default,
        })
    return out


def record_wav(seconds: float, source: str | None = None, path: Path | None = None) -> Path:
    src = source or default_monitor()
    AUDIO_DIR.mkdir(parents=True, exist_ok=True)
    path = path or AUDIO_DIR / f"rec-{int(time.time() * 1000)}.wav"
    p = _run([
        "ffmpeg", "-hide_banner", "-loglevel", "error", "-f", "pulse", "-i", src,
        "-t", f"{max(0.5, float(seconds)):.2f}", "-ac", "1", "-ar", "16000", "-y", str(path),
    ])
    if p.returncode != 0 or not path.exists() or path.stat().st_size < 1000:
        raise RuntimeError(f"audio capture failed from {src!r}: {p.stderr.decode(errors='replace').strip()[-300:]}")
    return path


def whisper_model(name: str = "base"):
    if name not in _WHISPER:
        try:
            from faster_whisper import WhisperModel
        except ImportError:
            raise RuntimeError("faster-whisper is not installed (uv pip install faster-whisper)")
        _WHISPER[name] = WhisperModel(name, device="cpu", compute_type="int8")
    return _WHISPER[name]


def transcribe(path: Path, model: str = "base", language: str | None = None, offset: float = 0.0) -> list[dict]:
    segs, _ = whisper_model(model).transcribe(
        str(path), language=language, vad_filter=True, beam_size=1, condition_on_previous_text=False
    )
    out = []
    for sg in segs:
        text = sg.text.strip()
        if text:
            out.append({"start": round(offset + sg.start, 1), "end": round(offset + sg.end, 1), "text": text})
    return out


def listen_once(seconds: float = 15.0, source: str | None = None, model: str = "base",
                language: str | None = None, keep: bool = False) -> list[dict]:
    wav = record_wav(seconds, source)
    try:
        return transcribe(wav, model, language)
    finally:
        if not keep:
            wav.unlink(missing_ok=True)


def listen_status() -> dict:
    try:
        meta = json.loads(LISTEN_PID.read_text())
    except Exception:
        meta = None
    lines = LISTEN_LOG.read_text().splitlines() if LISTEN_LOG.exists() else []
    st = {"running": bool(meta and _alive(meta["pid"])), "segments_buffered": len(lines),
          "transcript": str(LISTEN_LOG)}
    if meta:
        st.update(meta)
    if lines:
        try:
            st["newest_segment_age_seconds"] = round(time.time() - json.loads(lines[-1])["epoch"], 1)
        except Exception:
            pass
    return st


def listen_start(chunk_seconds: float = 20.0, source: str | None = None, model: str = "base",
                 window_seconds: float = 900.0, language: str | None = None, restart: bool = False) -> dict:
    meta = None
    try:
        meta = json.loads(LISTEN_PID.read_text())
    except Exception:
        pass
    if meta and _alive(meta["pid"]):
        if not restart:
            return {"already_running": True, **listen_status()}
        listen_stop()
    src = source or default_monitor()
    LISTEN_DIR.mkdir(parents=True, exist_ok=True)
    LISTEN_LOG.write_text("")
    argv = [sys.executable, os.path.abspath(__file__), "listen", "_run",
            "--chunk", str(chunk_seconds), "--source", src, "--model", model,
            "--window-seconds", str(window_seconds)]
    if language:
        argv += ["--language", language]
    log = (LISTEN_DIR / "daemon.log").open("ab")
    proc = subprocess.Popen(argv, stdin=subprocess.DEVNULL, stdout=log, stderr=log, start_new_session=True)
    LISTEN_PID.write_text(json.dumps({"pid": proc.pid, "chunk_seconds": chunk_seconds, "source": src,
                                      "model": model, "started": time.strftime("%Y-%m-%d %H:%M:%S")}))
    return {"launched": True, **listen_status()}


def listen_stop() -> dict:
    try:
        meta = json.loads(LISTEN_PID.read_text())
    except Exception:
        return {"running": False, "note": "no listener registered"}
    if _alive(meta["pid"]):
        os.kill(meta["pid"], signal.SIGTERM)
        for _ in range(30):
            if not _alive(meta["pid"]):
                break
            time.sleep(0.1)
    LISTEN_PID.unlink(missing_ok=True)
    return {"stopped": True, "pid": meta["pid"]}


def listen_run(chunk: float, source: str, model: str, window_seconds: float, language: str | None) -> int:
    LISTEN_DIR.mkdir(parents=True, exist_ok=True)
    whisper_model(model)  # load once, before the first chunk
    fails = 0
    while True:
        started = time.time()
        try:
            wav = record_wav(chunk, source, LISTEN_DIR / "chunk.wav")
            for seg in transcribe(wav, model, language):
                seg["epoch"] = round(started + seg["start"], 1)
                with LISTEN_LOG.open("a") as fh:
                    fh.write(json.dumps(seg) + "\n")
            fails = 0
        except RuntimeError as e:
            fails += 1
            print(f"[{time.strftime('%H:%M:%S')}] chunk failed ({fails}): {e}", file=sys.stderr, flush=True)
            if fails >= 20:
                return 1
            time.sleep(1)
        if LISTEN_LOG.exists():  # prune to the retention window
            cutoff = time.time() - window_seconds
            kept = [l for l in LISTEN_LOG.read_text().splitlines()
                    if l.strip() and json.loads(l).get("epoch", 0) >= cutoff]
            LISTEN_LOG.write_text("\n".join(kept) + ("\n" if kept else ""))


def listen_transcript(seconds: float = 120.0) -> list[dict]:
    if not LISTEN_LOG.exists():
        raise RuntimeError("no transcript yet — start the listener with listen_start")
    now = time.time()
    out = []
    for line in LISTEN_LOG.read_text().splitlines():
        if not line.strip():
            continue
        seg = json.loads(line)
        if now - seg.get("epoch", 0) <= seconds:
            seg["ago"] = round(now - seg["epoch"], 1)
            out.append(seg)
    return out


# --------------------------------------------------------------------------- MCP

INSTRUCTIONS = """Vision onto the user's Linux desktop (Hyprland/Wayland).

- `screenshot` for a single look; pick a target with monitor= / window= / region=.
- `watch_screen` to see change over time (returns several timestamped frames).
- `record_clip` for a dense clip: returns one contact-sheet image plus an .mp4 path.
- `list_monitors` / `list_windows` first when you don't know what to aim at.

Keep `detail` at "low" or "normal" unless you must read small text ("high").

You can also DRIVE this machine: `mouse_click`, `mouse_move`, `mouse_drag`,
`mouse_scroll`, `keyboard_type`, `keyboard_paste`, `keyboard_keys`, `focus_window`,
`switch_workspace`. These move the user's real cursor and type into whatever holds
focus, so:

- Look before you act. Coordinates come from a screenshot, and screenshots are
  downscaled — pass `from_width` (the sent width, printed in every screenshot
  note) and `monitor` (shots of one monitor are monitor-relative), or the click
  lands short and up-left.
- Input tools return an "after" screenshot by default. Check it actually worked
  instead of assuming, and stop if the screen is not what you expected.
- Only act when the user asked for it. Do not click through consent dialogs,
  payment confirmations, or destructive prompts on your own initiative — describe
  what you see and let them decide.
- `keyboard_paste` overwrites their clipboard. Prefer it over typing long text.

Everything you capture is the user's live screen: treat visible credentials,
messages and private data as confidential and do not repeat them unnecessarily.
"""


def build_server():
    try:  # mcp >= 2.0
        from mcp.server.mcpserver import Image as MCPImage
        from mcp.server.mcpserver import MCPServer as Server
    except ImportError:  # mcp 1.x
        from mcp.server.fastmcp import FastMCP as Server
        from mcp.server.fastmcp import Image as MCPImage

    app = Server("pcvision", instructions=INSTRUCTIONS)

    @app.tool()
    def list_monitors() -> str:
        """List connected monitors: name, resolution, layout position, focus, workspace."""
        return json.dumps(monitors(), indent=2)

    @app.tool()
    def list_windows() -> str:
        """List open windows with class, title, workspace and geometry (targets for screenshot)."""
        return json.dumps(windows(), indent=2)

    @app.tool()
    def screenshot(
        monitor: str | None = None,
        window: str | None = None,
        region: str | None = None,
        detail: str = "normal",
        include_cursor: bool = False,
        save_file: bool = False,
    ) -> list:
        """Capture the screen right now and return it as an image.

        monitor: monitor name from list_monitors (e.g. "DP-1").
        window: window class/title substring, or "active" for the focused window.
        region: "X,Y WxH" in layout coordinates.
        Omit all three to capture every monitor as one wide image.
        detail: low (800px) | normal (1280px) | high (1568px, for small text) | full (native PNG).
        """
        geo, out, label = resolve_target(monitor, window, region)
        png = grim(geo, out, include_cursor)
        data, fmt, native, sent = encode(png, detail)
        note = (f"{label} — captured {native[0]}x{native[1]}, sent as {fmt} {sent[0]}x{sent[1]}"
                f" (detail={detail}). To click something here: pass the x,y you read off THIS image"
                f" with from_width={sent[0]}" + (f", monitor={monitor!r}" if monitor else "") + ".")
        if save_file:
            note += f"\nsaved: {save_bytes(png, 'png')}"
        return [note, MCPImage(data=data, format=fmt)]

    @app.tool()
    def screenshot_each_monitor(detail: str = "low", include_cursor: bool = False) -> list:
        """Capture every monitor separately — one labelled image per screen."""
        parts: list = []
        for m in monitors():
            data, fmt, native, sent = encode(grim(None, m["name"], include_cursor), detail)
            focus = " (focused)" if m["focused"] else ""
            parts.append(f'{m["name"]}{focus} — native {native[0]}x{native[1]}, sent {sent[0]}x{sent[1]}'
                         f' (from_width={sent[0]}, monitor={m["name"]!r}), workspace {m["workspace"]}')
            parts.append(MCPImage(data=data, format=fmt))
        return parts or ["no monitors reported by hyprctl"]

    @app.tool()
    def watch_screen(
        seconds: float = 6.0,
        frames: int = 4,
        monitor: str | None = None,
        window: str | None = None,
        region: str | None = None,
        detail: str = "low",
        include_cursor: bool = True,
    ) -> list:
        """Watch the screen over a time window: capture `frames` shots spread across
        `seconds` and return them in order. Use this to see what the user is doing,
        whether something finished, or how the UI animates. Max 12 frames / 120s.
        This blocks for `seconds`, so keep it short.
        """
        geo, out, label = resolve_target(monitor, window, region)
        series = capture_series(seconds, frames, geo, out, include_cursor)
        parts: list = [f"{label} — {len(series)} frames over {series[-1][0]:.1f}s"]
        for i, (ts, png) in enumerate(series, 1):
            data, fmt, _, _ = encode(png, detail)
            parts.append(f"frame {i} @ t={ts:.1f}s")
            parts.append(MCPImage(data=data, format=fmt))
        return parts

    @app.tool()
    def record_clip(
        seconds: float = 8.0,
        fps: float = 2.0,
        monitor: str | None = None,
        window: str | None = None,
        region: str | None = None,
        columns: int = 3,
        include_cursor: bool = True,
        keep_video: bool = True,
    ) -> list:
        """Record a short clip and return it as ONE contact-sheet image (timestamped
        tiles), plus the path to an .mp4 the user can play. Best when you need many
        moments at once without spending tokens on many separate images.
        """
        geo, out, label = resolve_target(monitor, window, region)
        fps = max(0.5, min(float(fps), 4.0))
        seconds = max(1.0, min(float(seconds), MAX_SECONDS))
        count = max(2, min(int(round(seconds * fps)), MAX_FRAMES))
        series = capture_series(seconds, count, geo, out, include_cursor)
        sheet = contact_sheet([(f"t = {ts:.1f}s", png) for ts, png in series], cols=columns)
        note = f"{label} — {len(series)} frames over {series[-1][0]:.1f}s (~{count / max(series[-1][0], 0.1):.1f} fps)"
        if keep_video:
            try:
                mp4 = encode_video(series, CACHE / f"clip-{time.strftime('%Y%m%d-%H%M%S')}.mp4", fps)
                note += f"\nvideo: {mp4}"
            except RuntimeError as e:
                note += f"\nvideo unavailable: {e}"
        return [note, MCPImage(data=sheet, format="jpeg")]

    @app.tool(name="live_start")
    def _live_start_tool(
        fps: float = 1.0,
        window_seconds: float = 120.0,
        monitor: str | None = None,
        window: str | None = None,
        region: str | None = None,
        detail: str = "low",
        restart: bool = False,
    ) -> str:
        """Start a background recorder that continuously keeps the last `window_seconds`
        of the user's screen in a ring buffer on disk. Nothing is sent to you until you
        call `live_view` / `live_latest` — this just means no activity is missed between
        your looks. Returns immediately (does NOT block). Costs ~1 capture per second.
        """
        return json.dumps(
            live_start(fps, window_seconds, monitor, window, region, detail, True, restart), indent=2
        )

    @app.tool(name="live_stop")
    def _live_stop_tool() -> str:
        """Stop the background screen recorder."""
        return json.dumps(live_stop(), indent=2)

    @app.tool(name="live_status")
    def _live_status_tool() -> str:
        """Is the recorder running? How much screen history is buffered, and how fresh?"""
        return json.dumps(live_status(), indent=2)

    @app.tool()
    def live_view(
        seconds: float = 30.0, frames: int = 6, columns: int = 3, changes_only: bool = True
    ) -> list:
        """See what happened on screen over the last `seconds`, from the recorder's buffer.
        Returns ONE contact sheet labelled with how long ago each frame was captured
        (-0.0s = now). With changes_only, near-identical frames are dropped so the tiles
        show actual activity instead of a static desktop. Instant — no waiting.
        """
        picked, total, dropped = live_window(seconds, frames, changes_only)
        sheet = contact_sheet([(f"-{age:.1f}s", d) for age, d in picked], cols=columns)
        note = (
            f"last {seconds:.0f}s — {total} buffered frames, showing {len(picked)}"
            + (f", {dropped} skipped as unchanged" if dropped else "")
            + f"; newest is {picked[-1][0]:.1f}s old"
        )
        return [note, MCPImage(data=sheet, format="jpeg")]

    @app.tool()
    def live_latest() -> list:
        """The single freshest frame from the recorder's buffer — the cheapest 'what's on
        screen right now', with no capture latency."""
        picked, _, _ = live_window(seconds=10.0, count=1, changes_only=False)
        age, data = picked[-1]
        return [f"newest buffered frame, {age:.1f}s old", MCPImage(data=data, format="jpeg")]

    @app.tool()
    def audio_sources_list() -> str:
        """Audio sources available for listening. Monitor sources capture what the
        user is HEARING; input sources are microphones."""
        return json.dumps(audio_sources(), indent=2)

    @app.tool()
    def listen(
        seconds: float = 15.0,
        source: str | None = None,
        model: str = "base",
        language: str | None = None,
    ) -> str:
        """LISTEN to the machine: record `seconds` of audio and return it transcribed
        with timestamps. Defaults to the monitor of the default output, i.e. whatever
        the user is currently hearing (video, call, music). BLOCKS for `seconds` plus
        transcription time (roughly 0.3x realtime on CPU with the base model).
        model: tiny | base | small | medium — bigger is slower but more accurate."""
        segs = listen_once(seconds, source, model, language)
        if not segs:
            return f"(silence — nothing recognised in {seconds:.0f}s of audio)"
        return "\n".join(f"[{s['start']:6.1f}s] {s['text']}" for s in segs)

    @app.tool(name="listen_start")
    def _listen_start_tool(
        chunk_seconds: float = 20.0,
        source: str | None = None,
        model: str = "base",
        window_seconds: float = 900.0,
        language: str | None = None,
        restart: bool = False,
    ) -> str:
        """Start continuously transcribing what the machine plays, in the background,
        into a rolling transcript. The audio equivalent of live_start: nothing is
        missed between your looks. Read it with `listen_transcript`."""
        return json.dumps(listen_start(chunk_seconds, source, model, window_seconds, language, restart), indent=2)

    @app.tool(name="listen_stop")
    def _listen_stop_tool() -> str:
        """Stop the background listener."""
        return json.dumps(listen_stop(), indent=2)

    @app.tool(name="listen_status")
    def _listen_status_tool() -> str:
        """Is the listener running, and how much transcript is buffered?"""
        return json.dumps(listen_status(), indent=2)

    @app.tool(name="listen_transcript")
    def _listen_transcript_tool(seconds: float = 120.0) -> str:
        """What was SAID in the last `seconds`, from the background listener's buffer.
        Cheap and instant — pair with live_view to get picture plus sound."""
        segs = listen_transcript(seconds)
        if not segs:
            return f"(nothing transcribed in the last {seconds:.0f}s)"
        return "\n".join(f"[-{s['ago']:6.1f}s] {s['text']}" for s in segs)

    # ---------------------------------------------------------------- input control

    def _after(action: str, verify: bool, detail: str = "normal") -> list:
        audit(action)
        parts: list = [action]
        if verify:
            time.sleep(0.3)  # let the UI react before looking
            mon = focused_monitor()
            data, fmt, _, sent = encode(grim(None, mon, True), detail)
            parts.append(
                f"after — monitor {mon}, sent {sent[0]}x{sent[1]} "
                f"(reuse with from_width={sent[0]}, monitor={mon!r})"
            )
            parts.append(MCPImage(data=data, format=fmt))
        return parts

    @app.tool()
    def cursor_position() -> str:
        """Where the mouse pointer currently is, in layout coordinates."""
        x, y = cursor_pos()
        lw, lh = layout_bounds()
        return json.dumps({"x": x, "y": y, "layout": f"{lw}x{lh}"})

    @app.tool()
    def mouse_move(
        x: float,
        y: float,
        monitor: str | None = None,
        from_width: int | None = None,
        verify: bool = False,
    ) -> list:
        """Move the pointer. Coordinates are as you read them off a screenshot:
        pass `monitor` if the shot was of one monitor, and `from_width` = the width
        of the image you measured on (printed in every screenshot note)."""
        input_allowed()
        px, py = to_layout(x, y, monitor, from_width)
        move_cursor(px, py)
        return _after(f"mouse_move -> {px},{py}", verify, "low")

    @app.tool()
    def mouse_click(
        x: float | None = None,
        y: float | None = None,
        button: str = "left",
        count: int = 1,
        monitor: str | None = None,
        from_width: int | None = None,
        expect_window: str | None = None,
        verify: bool = True,
    ) -> list:
        """Click. With x,y the pointer moves there first; without them it clicks
        wherever the pointer already is. count=2 double-clicks. See mouse_move for
        how coordinates map. Pass `expect_window` (class/title substring) to abort if
        the pointer is not over the window you think it is — cheap insurance against
        a stale screenshot. Returns an "after" screenshot; check it."""
        input_allowed()
        where = "at current position"
        px, py = cursor_pos()
        if x is not None and y is not None:
            px, py = to_layout(x, y, monitor, from_width)
            move_cursor(px, py)
            time.sleep(0.12)
            where = f"at {px},{py}"
        under = window_at(px, py)
        if expect_window:
            got = f'{under["class"]}: {under["title"]}' if under else "nothing"
            if not under or expect_window.lower() not in got.lower():
                raise RuntimeError(
                    f"{px},{py} is over {got!r}, not {expect_window!r} — not clicking. "
                    "Take a fresh screenshot; the layout moved."
                )
        VirtualPointer.shared().click(button, count)
        on = f' on {under["class"]}' if under else ""
        return _after(f"{button} click x{max(1, int(count))} {where}{on}", verify)

    @app.tool()
    def mouse_scroll(
        amount: int,
        horizontal: bool = False,
        x: float | None = None,
        y: float | None = None,
        monitor: str | None = None,
        from_width: int | None = None,
        verify: bool = True,
    ) -> list:
        """Scroll the wheel `amount` notches — positive is up (or right). Give x,y to
        park the pointer over the thing you want to scroll first."""
        input_allowed()
        if x is not None and y is not None:
            move_cursor(*to_layout(x, y, monitor, from_width))
            time.sleep(0.1)
        VirtualPointer.shared().scroll(amount, horizontal)
        axis = "horizontal" if horizontal else "vertical"
        return _after(f"scroll {amount:+d} ({axis})", verify)

    @app.tool()
    def mouse_drag(
        from_x: float,
        from_y: float,
        to_x: float,
        to_y: float,
        button: str = "left",
        monitor: str | None = None,
        from_width: int | None = None,
        verify: bool = True,
    ) -> list:
        """Press at one point, move, release at another — sliders, selections,
        resizing, drag-and-drop. Best effort: some apps ignore warped drags, in which
        case use keyboard selection instead."""
        input_allowed()
        x1, y1 = to_layout(from_x, from_y, monitor, from_width)
        x2, y2 = to_layout(to_x, to_y, monitor, from_width)
        drag(x1, y1, x2, y2, button)
        return _after(f"drag {button} {x1},{y1} -> {x2},{y2}", verify)

    @app.tool()
    def keyboard_type(
        text: str, window: str | None = None, delay_ms: int = 12, verify: bool = True
    ) -> list:
        """Type text, character by character.

        ALWAYS pass `window` (a class/title substring) unless you have just verified
        focus yourself: it focuses that window and confirms it before typing, and
        refuses if it cannot. Without it, keystrokes go to whatever holds focus,
        which may not be what you saw in your last screenshot. For long text or
        code, use keyboard_paste."""
        input_allowed()
        target = f' into {ensure_focus(window)["class"]}' if window else ''
        type_text(text, delay_ms)
        preview = text if len(text) <= 60 else text[:57] + "..."
        return _after(f"typed {len(text)} chars{target}: {preview!r}", verify)

    @app.tool()
    def keyboard_paste(text: str, window: str | None = None, verify: bool = True) -> list:
        """Put text on the clipboard and press ctrl+v — instant and exact for long
        text or code. Pass `window` to focus and verify the target first. This
        REPLACES the user's clipboard contents."""
        input_allowed()
        target = f' into {ensure_focus(window)["class"]}' if window else ''
        paste_text(text)
        return _after(f"pasted {len(text)} chars via clipboard{target}", verify)

    @app.tool()
    def keyboard_keys(keys: str, window: str | None = None, verify: bool = True) -> list:
        """Press key combos. One combo ("ctrl+shift+t", "Return", "alt+F4", "Escape")
        or several space-separated, pressed in order ("ctrl+a Delete").

        Pass `window` to focus and VERIFY the target first — without it the combo goes
        to whatever holds focus, which is how ctrl+t ends up opening a tab in the wrong
        browser."""
        input_allowed()
        combos = [k for k in keys.split() if k]
        target = f" in {ensure_focus(window)['class']}" if window else ""
        press_keys(combos)
        return _after(f"keys: {' '.join(combos)}{target}", verify)

    @app.tool()
    def focus_window(query: str, verify: bool = True) -> list:
        """Focus a window by class/title substring before typing into it."""
        input_allowed()
        w = focus_window_impl(query)
        return _after(f'focused {w["class"]}: {w["title"][:60]}', verify)

    @app.tool()
    def switch_workspace(workspace: str, verify: bool = True) -> list:
        """Switch to a workspace by name/number (Hyprland workspace dispatcher)."""
        input_allowed()
        goto_workspace(workspace)
        return _after(f"workspace -> {workspace}", verify)

    @app.tool()
    def input_disable() -> str:
        """Switch OFF all mouse/keyboard control immediately. Use this if the user
        says stop, or if you are unsure whether acting is safe. You cannot undo it —
        only the user can, with `pcvision input enable`."""
        CACHE.mkdir(parents=True, exist_ok=True)
        INPUT_LOCK.write_text(f"disabled {time.strftime('%Y-%m-%d %H:%M:%S')}\n")
        audit("INPUT DISABLED")
        return "Input control is now off. The user re-enables it with: pcvision input enable"

    @app.tool()
    def screenshot_user_selection(pick_window: bool = False, detail: str = "normal") -> list:
        """Ask the human to select what to capture (slurp drag box, or click a window
        when pick_window=true), then return that capture. Blocks until they pick or
        press Escape — only use it when the user offered to point at something.
        """
        geo = slurp_region(single_window=pick_window)
        data, fmt, native, _ = encode(grim(geo, None, False), detail)
        return [f"user-selected {geo} — {native[0]}x{native[1]}", MCPImage(data=data, format=fmt)]

    return app


# --------------------------------------------------------------------------- CLI


def cli(argv: list[str]) -> int:
    ap = argparse.ArgumentParser(prog="pcvision", description="Screen capture for humans and for Claude.")
    sub = ap.add_subparsers(dest="cmd", required=True)

    sub.add_parser("serve", help="run as an MCP stdio server")
    sub.add_parser("monitors", help="list monitors as JSON")
    sub.add_parser("windows", help="list windows as JSON")
    pr = sub.add_parser("region", help="pick a region interactively, print geometry")
    pr.add_argument("--window", action="store_true", help="click a window instead of dragging")

    def targets(p):
        p.add_argument("-m", "--monitor")
        p.add_argument("-w", "--window", help='class/title substring, or "active"')
        p.add_argument("-r", "--region", help='"X,Y WxH"')
        p.add_argument("--cursor", action="store_true")

    ps = sub.add_parser("shot", help="single screenshot")
    targets(ps)
    ps.add_argument("-o", "--out", help="output file (default: cache dir)")
    ps.add_argument("-d", "--detail", default="full", choices=list(DETAIL))
    ps.add_argument("--pick", action="store_true", help="select region interactively first")

    pw = sub.add_parser("watch", help="N frames over T seconds")
    targets(pw)
    pw.add_argument("-s", "--seconds", type=float, default=6.0)
    pw.add_argument("-n", "--frames", type=int, default=4)
    pw.add_argument("--sheet", help="also write a contact sheet to this file")
    pw.add_argument("--out-dir", help="directory for frames (default: cache dir)")

    pa = sub.add_parser("listen", help="hear the machine: record + transcribe")
    asub = pa.add_subparsers(dest="listen_cmd", required=True)
    asub.add_parser("sources", help="list audio sources")
    aon = asub.add_parser("once", help="record N seconds and transcribe")
    aon.add_argument("-s", "--seconds", type=float, default=15.0)
    ast_ = asub.add_parser("start", help="continuous background transcription")
    ast_.add_argument("--chunk", type=float, default=20.0, help="seconds per transcription chunk")
    ast_.add_argument("--window-seconds", type=float, default=900.0, help="transcript retention")
    ast_.add_argument("--restart", action="store_true")
    asub.add_parser("stop", help="stop the listener")
    asub.add_parser("status", help="listener state as JSON")
    avw = asub.add_parser("view", help="what was said recently")
    avw.add_argument("-s", "--seconds", type=float, default=120.0)
    arn = asub.add_parser("_run", help=argparse.SUPPRESS)
    arn.add_argument("--chunk", type=float, default=20.0)
    arn.add_argument("--window-seconds", type=float, default=900.0)
    for sp in (aon, ast_, arn):
        sp.add_argument("--source", help="audio source (default: monitor of default output)")
        sp.add_argument("--model", default="base", help="tiny|base|small|medium")
        sp.add_argument("--language", help="force a language, e.g. en")

    pi = sub.add_parser("input", help="control the mouse and keyboard")
    isub = pi.add_subparsers(dest="input_cmd", required=True)
    isub.add_parser("status", help="is input control enabled?")
    isub.add_parser("enable", help="allow Claude to use mouse/keyboard")
    isub.add_parser("disable", help="kill switch: block all mouse/keyboard control")
    isub.add_parser("log", help="show the input audit trail")
    imv = isub.add_parser("move", help="move the pointer")
    imv.add_argument("x", type=float)
    imv.add_argument("y", type=float)
    icl = isub.add_parser("click", help="click (optionally at x y first)")
    icl.add_argument("x", type=float, nargs="?")
    icl.add_argument("y", type=float, nargs="?")
    icl.add_argument("-b", "--button", default="left", choices=list(BUTTONS))
    icl.add_argument("-n", "--count", type=int, default=1)
    icl.add_argument("-e", "--expect-window", help="abort unless the pointer is over this window")
    isc = isub.add_parser("scroll", help="scroll the wheel (+ is up)")
    isc.add_argument("amount", type=int)
    isc.add_argument("--horizontal", action="store_true")
    idr = isub.add_parser("drag", help="press, move, release")
    for name in ("x1", "y1", "x2", "y2"):
        idr.add_argument(name, type=float)
    idr.add_argument("-b", "--button", default="left", choices=list(BUTTONS))
    ity = isub.add_parser("type", help="type text into the focused window")
    ity.add_argument("text")
    ity.add_argument("--delay", type=int, default=12, help="ms between keystrokes")
    ity.add_argument("-w", "--window", help="focus and VERIFY this window first")
    ike = isub.add_parser("keys", help='key combos, e.g. ctrl+shift+t Return')
    ike.add_argument("combo", nargs="+")
    ike.add_argument("-w", "--window", help="focus and VERIFY this window first")
    ipa = isub.add_parser("paste", help="clipboard + ctrl+v")
    ipa.add_argument("text")
    ipa.add_argument("-w", "--window", help="focus and VERIFY this window first")
    ifo = isub.add_parser("focus", help="focus a window by class/title")
    ifo.add_argument("query")
    iws = isub.add_parser("workspace", help="switch workspace")
    iws.add_argument("name")
    for sp in (imv, icl, idr):
        sp.add_argument("-m", "--monitor", help="coordinates are relative to this monitor")
        sp.add_argument("--from-width", type=int, help="width of the image you measured on")

    pl = sub.add_parser("live", help="continuous background recorder (ring buffer)")
    lsub = pl.add_subparsers(dest="live_cmd", required=True)
    lstart = lsub.add_parser("start", help="start the recorder")
    targets(lstart)
    lstart.add_argument("--fps", type=float, default=1.0)
    lstart.add_argument("--window-seconds", type=float, default=120.0, help="history to keep")
    lstart.add_argument("-d", "--detail", default="low", choices=list(DETAIL))
    lstart.add_argument("--restart", action="store_true")
    lsub.add_parser("stop", help="stop the recorder")
    lsub.add_parser("status", help="recorder + buffer state as JSON")
    lview = lsub.add_parser("view", help="contact sheet of the last N seconds")
    lview.add_argument("-s", "--seconds", type=float, default=30.0)
    lview.add_argument("-n", "--frames", type=int, default=6)
    lview.add_argument("-c", "--columns", type=int, default=3)
    lview.add_argument("--all-frames", action="store_true", help="keep unchanged frames too")
    lview.add_argument("-o", "--out", help="sheet path (default: cache dir)")
    lrun = lsub.add_parser("_run", help=argparse.SUPPRESS)
    targets(lrun)
    lrun.add_argument("--fps", type=float, default=1.0)
    lrun.add_argument("--window-seconds", type=float, default=120.0)
    lrun.add_argument("-d", "--detail", default="low")

    prec = sub.add_parser("record", help="short clip -> mp4 (+ contact sheet)")
    targets(prec)
    prec.add_argument("-s", "--seconds", type=float, default=8.0)
    prec.add_argument("-f", "--fps", type=float, default=2.0)
    prec.add_argument("-o", "--out", default=None, help="mp4 path")
    prec.add_argument("--sheet", help="contact sheet path")

    a = ap.parse_args(argv)

    if a.cmd == "serve":
        build_server().run()
        return 0
    if a.cmd == "monitors":
        print(json.dumps(monitors(), indent=2))
        return 0
    if a.cmd == "windows":
        print(json.dumps(windows(), indent=2))
        return 0
    if a.cmd == "region":
        print(slurp_region(single_window=a.window))
        return 0

    if a.cmd == "listen":
        c = a.listen_cmd
        if c == "sources":
            print(json.dumps(audio_sources(), indent=2))
        elif c == "once":
            segs = listen_once(a.seconds, a.source, a.model, a.language)
            print("\n".join(f"[{x['start']:6.1f}s] {x['text']}" for x in segs)
                  or f"(silence — nothing recognised in {a.seconds:.0f}s)")
        elif c == "start":
            print(json.dumps(listen_start(a.chunk, a.source, a.model, a.window_seconds,
                                          a.language, a.restart), indent=2))
        elif c == "stop":
            print(json.dumps(listen_stop(), indent=2))
        elif c == "status":
            print(json.dumps(listen_status(), indent=2))
        elif c == "view":
            segs = listen_transcript(a.seconds)
            print("\n".join(f"[-{x['ago']:6.1f}s] {x['text']}" for x in segs)
                  or f"(nothing transcribed in the last {a.seconds:.0f}s)")
        elif c == "_run":
            return listen_run(a.chunk, a.source or default_monitor(), a.model,
                              a.window_seconds, a.language)
        return 0

    if a.cmd == "input":
        c = a.input_cmd
        if c == "status":
            print(json.dumps({
                "enabled": not INPUT_LOCK.exists(),
                "uinput_writable": os.access(UINPUT_DEV, os.W_OK),
                "wtype": bool(shutil.which("wtype")),
                "wl_copy": bool(shutil.which("wl-copy")),
                "audit_log": str(INPUT_LOG),
            }, indent=2))
            return 0
        if c == "enable":
            INPUT_LOCK.unlink(missing_ok=True)
            audit("INPUT ENABLED by user")
            print("input control enabled")
            return 0
        if c == "disable":
            CACHE.mkdir(parents=True, exist_ok=True)
            INPUT_LOCK.write_text(f"disabled {time.strftime('%Y-%m-%d %H:%M:%S')}\n")
            audit("INPUT DISABLED by user")
            print("input control disabled — Claude can no longer move the mouse or type")
            return 0
        if c == "log":
            print(INPUT_LOG.read_text() if INPUT_LOG.exists() else "(no input actions logged)")
            return 0

        input_allowed()
        mon = getattr(a, "monitor", None)
        fw = getattr(a, "from_width", None)
        if c == "move":
            px, py = to_layout(a.x, a.y, mon, fw)
            move_cursor(px, py)
            audit(f"mouse_move -> {px},{py}")
            print(f"cursor at {px},{py}")
        elif c == "click":
            where = "current position"
            px, py = cursor_pos()
            if a.x is not None and a.y is not None:
                px, py = to_layout(a.x, a.y, mon, fw)
                move_cursor(px, py)
                time.sleep(0.12)
                where = f"{px},{py}"
            under = window_at(px, py)
            if a.expect_window:
                got = f"{under['class']}: {under['title']}" if under else "nothing"
                if not under or a.expect_window.lower() not in got.lower():
                    raise RuntimeError(f"{px},{py} is over {got!r}, not {a.expect_window!r} — not clicking")
            where += f" ({under['class']})" if under else ""
            VirtualPointer.shared().click(a.button, a.count)
            audit(f"{a.button} click x{a.count} at {where}")
            print(f"{a.button} click x{a.count} at {where}")
        elif c == "scroll":
            VirtualPointer.shared().scroll(a.amount, a.horizontal)
            audit(f"scroll {a.amount:+d}")
            print(f"scrolled {a.amount:+d}")
        elif c == "drag":
            x1, y1 = to_layout(a.x1, a.y1, mon, fw)
            x2, y2 = to_layout(a.x2, a.y2, mon, fw)
            drag(x1, y1, x2, y2, a.button)
            audit(f"drag {a.button} {x1},{y1} -> {x2},{y2}")
            print(f"dragged {x1},{y1} -> {x2},{y2}")
        elif c == "type":
            if a.window:
                print(f"focused {ensure_focus(a.window)['class']}")
            type_text(a.text, a.delay)
            audit(f"typed {len(a.text)} chars")
            print(f"typed {len(a.text)} chars")
        elif c == "keys":
            if a.window:
                print(f"focused {ensure_focus(a.window)['class']}")
            press_keys(a.combo)
            audit(f"keys: {' '.join(a.combo)}")
            print(f"pressed {' '.join(a.combo)}")
        elif c == "paste":
            if a.window:
                print(f"focused {ensure_focus(a.window)['class']}")
            paste_text(a.text)
            audit(f"pasted {len(a.text)} chars")
            print(f"pasted {len(a.text)} chars")
        elif c == "focus":
            w = focus_window_impl(a.query)
            audit(f"focused {w['class']}")
            print(f"focused {w['class']}: {w['title'][:60]}")
        elif c == "workspace":
            goto_workspace(a.name)
            audit(f"workspace -> {a.name}")
            print(f"workspace {a.name}")
        return 0

    if a.cmd == "live":
        if a.live_cmd == "stop":
            print(json.dumps(live_stop(), indent=2))
        elif a.live_cmd == "status":
            print(json.dumps(live_status(), indent=2))
        elif a.live_cmd == "start":
            print(json.dumps(
                live_start(a.fps, a.window_seconds, a.monitor, a.window, a.region,
                           a.detail, a.cursor, a.restart), indent=2))
        elif a.live_cmd == "view":
            picked, total, dropped = live_window(a.seconds, a.frames, not a.all_frames)
            sheet = contact_sheet([(f"-{age:.1f}s", d) for age, d in picked], cols=a.columns)
            path = Path(a.out).expanduser() if a.out else None
            if path:
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_bytes(sheet)
            else:
                path = save_bytes(sheet, "jpg", "live")
            print(f"{path}\n{total} buffered in last {a.seconds:.0f}s, showing {len(picked)}"
                  f"{f', {dropped} unchanged skipped' if dropped else ''}"
                  f"; newest {picked[-1][0]:.1f}s old")
        elif a.live_cmd == "_run":
            geo, out, _ = resolve_target(a.monitor, a.window, a.region)
            return live_run(a.fps, a.window_seconds, geo, out, a.cursor, a.detail)
        return 0

    geo, out, label = resolve_target(a.monitor, a.window, a.region)

    if a.cmd == "shot":
        if getattr(a, "pick", False):
            geo, out, label = slurp_region(), None, "user selection"
        png = grim(geo, out, a.cursor)
        data, fmt, native, _ = encode(png, a.detail)
        path = Path(a.out).expanduser() if a.out else save_bytes(data, fmt)
        if a.out:
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(data)
        print(f"{path}  ({label}, {native[0]}x{native[1]} -> {fmt})")
        return 0

    if a.cmd == "watch":
        series = capture_series(a.seconds, a.frames, geo, out, a.cursor)
        d = Path(a.out_dir).expanduser() if a.out_dir else CACHE
        d.mkdir(parents=True, exist_ok=True)
        for i, (ts, png) in enumerate(series, 1):
            p = d / f"watch-{time.strftime('%H%M%S')}-{i:02d}.png"
            p.write_bytes(png)
            print(f"{p}  t={ts:.1f}s")
        if a.sheet:
            Path(a.sheet).expanduser().write_bytes(
                contact_sheet([(f"t = {ts:.1f}s", png) for ts, png in series]))
            print(a.sheet)
        return 0

    if a.cmd == "record":
        fps = max(0.5, min(a.fps, 4.0))
        count = max(2, min(int(round(a.seconds * fps)), MAX_FRAMES))
        series = capture_series(a.seconds, count, geo, out, a.cursor)
        dest = Path(a.out).expanduser() if a.out else CACHE / f"clip-{time.strftime('%Y%m%d-%H%M%S')}.mp4"
        print(encode_video(series, dest, fps))
        if a.sheet:
            Path(a.sheet).expanduser().write_bytes(
                contact_sheet([(f"t = {ts:.1f}s", png) for ts, png in series]))
            print(a.sheet)
        return 0

    return 1


def main() -> int:
    bootstrap_env()
    argv = sys.argv[1:] or ["serve"]
    try:
        return cli(argv)
    except RuntimeError as e:
        print(f"pcvision: {e}", file=sys.stderr)
        return 2
    except KeyboardInterrupt:
        return 130


if __name__ == "__main__":
    raise SystemExit(main())
