"""Watching the user's real screen, continuously, without falling over.

`monitor` answers "how is this machine". This answers "what is the human
actually doing, and when". A detached recorder keeps the last few minutes of the
real desktop in a ring buffer on disk and writes one metadata line per tick, so
the cheap read is *text* — `usepc watch timeline`, a few hundred tokens — and
pixels are only spent when the agent actually asks to look.

Nothing leaves the machine. Frames sit in the user's own cache directory, age out
of the ring on their own, and `watch stop --purge` removes them.

The failure modes are the whole point, so they are handled explicitly:

  * The loop never exits on a capture error. A locked screen, a monitor being
    unplugged, DPMS blanking and a compositor restart all look like "grim
    failed", and all of them end when the user comes back — so it backs off and
    keeps trying instead of dying with a message nobody reads.
  * A Hyprland restart hands out a *new* wayland socket, so after a run of
    failures the recorder goes looking for one rather than talking to a corpse.
    It refuses to bind to the agent seat's nested socket while doing so.
  * The recorder runs under a small supervisor, so a hard crash costs a couple
    of seconds of history rather than the rest of the session.
  * Identical frames are never stored twice. An untouched desktop encodes to
    byte-identical JPEG, so idle time costs one metadata line per tick and no
    disk at all.
  * The ring is bounded by *both* time and bytes, so a long session on a 4K
    screen cannot quietly eat the disk.
"""

from __future__ import annotations

import hashlib
import json
import os
import shutil
import signal
import subprocess
import sys
import time
from pathlib import Path

import sysinfo

STATE_DIR = Path(os.environ.get("USEPC_HOME") or (Path.home() / ".cache" / "usepc"))
WATCH_DIR = STATE_DIR / "watch"
FRAME_DIR = WATCH_DIR / "frames"
INDEX = WATCH_DIR / "index.jsonl"
DAEMON = WATCH_DIR / "daemon.json"
BEAT = WATCH_DIR / "beat.json"
LOG = WATCH_DIR / "daemon.log"
SEAT_FILE = STATE_DIR / "seat.json"

SCALE = {"low": 0.30, "normal": 0.45, "high": 0.70, "full": 1.0}
QUALITY = {"low": 55, "normal": 62, "high": 76, "full": 88}

MAX_BACKOFF = 30.0
STALE_AFTER = 12.0  # a heartbeat older than this (plus one interval) means "wedged"


class WatchError(RuntimeError):
    pass


# --------------------------------------------------------------------- plumbing


def _alive(pid: int | None) -> bool:
    if not pid:
        return False
    try:
        os.kill(pid, 0)
        return True
    except OSError:
        return False


def _read_json(path: Path) -> dict | None:
    try:
        return json.loads(path.read_text())
    except Exception:
        return None


def _write_json(path: Path, payload: dict) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_suffix(path.suffix + ".tmp")
    tmp.write_text(json.dumps(payload, default=str))
    tmp.replace(path)


def _note(text: str) -> None:
    WATCH_DIR.mkdir(parents=True, exist_ok=True)
    with LOG.open("a") as handle:
        handle.write(f"{time.strftime('%Y-%m-%d %H:%M:%S')}  {text}\n")
    if LOG.stat().st_size > 120_000:
        LOG.write_text("\n".join(LOG.read_text(errors="replace").splitlines()[-300:]) + "\n")


def config() -> dict | None:
    return _read_json(DAEMON)


def beat() -> dict | None:
    return _read_json(BEAT)


def _frame_time(path: Path) -> float:
    return int(path.stem.split("-", 1)[1]) / 1000.0


def frame_files() -> list[Path]:
    try:
        return sorted(FRAME_DIR.glob("f-*.jpg"), key=_frame_time)
    except Exception:
        return []


# ------------------------------------------------------------------- capturing


def _seat_sockets() -> set[str]:
    """Never record the agent's own nested seat by accident."""
    seat = _read_json(SEAT_FILE) or {}
    return {str(seat.get("display") or ""), str(seat.get("signature") or "")} - {""}


def _rebind_session(cfg: dict) -> str | None:
    """Find the user's compositor again after it restarted under us."""
    if not sysinfo.IS_LINUX:
        return None
    runtime = Path(os.environ.get("XDG_RUNTIME_DIR") or f"/run/user/{os.getuid()}")
    avoid = _seat_sockets()
    found = None
    socks = [p for p in runtime.glob("wayland-*")
             if not p.name.endswith(".lock") and p.name not in avoid]
    if socks:
        wanted = cfg.get("display")
        pick = next((p for p in socks if p.name == wanted), None) or max(
            socks, key=lambda p: p.stat().st_mtime)
        os.environ["WAYLAND_DISPLAY"] = pick.name
        found = pick.name
    hypr = runtime / "hypr"
    if hypr.is_dir():
        instances = [d for d in hypr.iterdir() if d.is_dir() and d.name not in avoid]
        if instances:
            os.environ["HYPRLAND_INSTANCE_SIGNATURE"] = max(
                instances, key=lambda d: d.stat().st_mtime).name
    return found


def _capture(cfg: dict, dest: Path) -> None:
    detail = cfg.get("detail", "normal")
    if sysinfo.IS_LINUX and shutil.which("grim"):
        cmd = ["grim", "-t", "jpeg", "-q", str(QUALITY[detail]), "-s", f"{SCALE[detail]:g}"]
        if cfg.get("monitor"):
            cmd += ["-o", cfg["monitor"]]
        if cfg.get("region"):
            cmd += ["-g", cfg["region"]]
        if cfg.get("cursor"):
            cmd.append("-c")
        cmd.append(str(dest))
        proc = subprocess.run(cmd, capture_output=True, text=True, timeout=20)
        if proc.returncode != 0 or not dest.exists() or dest.stat().st_size == 0:
            raise WatchError((proc.stderr or proc.stdout).strip()[:160] or "grim produced nothing")
        return
    import real
    real.shot(dest, scale=SCALE[detail], quality=QUALITY[detail])
    if not dest.exists() or dest.stat().st_size == 0:
        raise WatchError("screen capture produced nothing")


_focus_cache: dict = {"t": 0.0, "value": {}}


def _focus(min_interval: float) -> dict:
    """Which window has the user's attention. Cached, because macOS and Windows
    both pay a process spawn for this and the answer changes slowly."""
    now = time.time()
    if now - _focus_cache["t"] < min_interval:
        return _focus_cache["value"]
    value: dict = {}
    try:
        if sysinfo.IS_LINUX and shutil.which("hyprctl"):
            raw = subprocess.run(["hyprctl", "activewindow", "-j"], capture_output=True,
                                 text=True, timeout=4).stdout
            active = json.loads(raw or "{}")
            if active.get("class") or active.get("title"):
                value = {"app": active.get("class") or "?",
                         "title": (active.get("title") or "")[:110],
                         "ws": (active.get("workspace") or {}).get("name")}
        elif sysinfo.IS_LINUX and shutil.which("xdotool"):
            title = subprocess.run(["xdotool", "getactivewindow", "getwindowname"],
                                   capture_output=True, text=True, timeout=4).stdout.strip()
            value = {"app": title.split(" - ")[-1][:40] or "?", "title": title[:110]}
        elif sysinfo.IS_MAC:
            app = subprocess.run(
                ["osascript", "-e", 'tell application "System Events" to get name of first '
                 'process whose frontmost is true'],
                capture_output=True, text=True, timeout=6).stdout.strip()
            value = {"app": app or "?", "title": ""}
        elif sysinfo.IS_WIN:
            raw = sysinfo._powershell(
                "Add-Type -Namespace W -Name A -MemberDefinition '[DllImport(\"user32.dll\")]"
                "public static extern IntPtr GetForegroundWindow();';"
                "$h=[W.A]::GetForegroundWindow();"
                "Get-Process|Where-Object{$_.MainWindowHandle -eq $h}|"
                "Select-Object -First 1 ProcessName,MainWindowTitle|ConvertTo-Json -Compress")
            parsed = json.loads(raw or "{}")
            value = {"app": parsed.get("ProcessName", "?"),
                     "title": (parsed.get("MainWindowTitle") or "")[:110]}
    except Exception:
        value = _focus_cache["value"]  # a hiccup here must never stop the recording
    _focus_cache.update({"t": now, "value": value})
    return value


# ------------------------------------------------------------------ the ring


def _append(entry: dict) -> None:
    with INDEX.open("a") as handle:
        handle.write(json.dumps(entry, separators=(",", ":")) + "\n")


def read_index(seconds: float | None = None) -> list[dict]:
    cutoff = time.time() - seconds if seconds else 0.0
    out: list[dict] = []
    try:
        with INDEX.open() as handle:
            for line in handle:
                try:
                    entry = json.loads(line)
                except Exception:
                    continue
                if entry.get("t", 0) >= cutoff:
                    out.append(entry)
    except FileNotFoundError:
        return []
    return out


def _prune(window: float, max_bytes: int) -> tuple[int, int]:
    """Bound the ring by age first, then by size. Returns (frames, bytes) kept."""
    files = frame_files()
    cutoff = time.time() - window
    keep: list[Path] = []
    for path in files:
        if _frame_time(path) < cutoff:
            path.unlink(missing_ok=True)
        else:
            keep.append(path)
    total = sum(p.stat().st_size for p in keep if p.exists())
    while keep and total > max_bytes:
        oldest = keep.pop(0)
        try:
            total -= oldest.stat().st_size
        except OSError:
            pass
        oldest.unlink(missing_ok=True)
    # A little slack, so the tick that wrote the oldest surviving frame survives too.
    floor = (_frame_time(keep[0]) - 1.0) if keep else time.time()
    entries = [e for e in read_index() if e.get("t", 0) >= floor]
    tmp = INDEX.with_suffix(".tmp")
    tmp.write_text("".join(json.dumps(e, separators=(",", ":")) + "\n" for e in entries))
    tmp.replace(INDEX)
    return len(keep), total


# ------------------------------------------------------------------ the recorder


def record(cfg: dict) -> int:
    """The capture loop. Returns 0 only when asked to stop."""
    FRAME_DIR.mkdir(parents=True, exist_ok=True)
    interval = 1.0 / max(0.05, float(cfg["fps"]))
    window = float(cfg["seconds"])
    max_bytes = int(float(cfg["max_mb"]) * 1024 * 1024)
    focus_interval = 1.0 if sysinfo.IS_LINUX else 5.0
    stop = {"now": False}

    def _bye(*_):
        stop["now"] = True

    for sig in (signal.SIGTERM, signal.SIGINT):
        signal.signal(sig, _bye)

    scratch = WATCH_DIR / f"pending-{os.getpid()}.jpg"
    previous_hash: str | None = None
    previous_name: str | None = None
    fails = 0
    last_prune = 0.0
    captured = 0
    next_tick = time.monotonic()

    while not stop["now"]:
        tick = time.time()
        try:
            _capture(cfg, scratch)
            payload = scratch.read_bytes()
        except Exception as exc:
            fails += 1
            reason = str(exc)[:160]
            if fails in (3, 10) or fails % 30 == 0:
                rebound = _rebind_session(cfg)
                _note(f"capture failing ({fails}): {reason}"
                      + (f" — rebound to {rebound}" if rebound else ""))
            _write_json(BEAT, {"t": tick, "pid": os.getpid(), "healthy": False, "fails": fails,
                               "reason": reason, "captured": captured,
                               "state": "waiting for the screen to come back"})
            # Screens come back. Back off, stay alive, keep the history we have.
            delay = min(MAX_BACKOFF, interval * (2 ** min(fails, 5)))
            end = time.monotonic() + delay
            while time.monotonic() < end and not stop["now"]:
                time.sleep(0.2)
            next_tick = time.monotonic()
            continue

        fails = 0
        digest = hashlib.blake2b(payload, digest_size=12).hexdigest()
        entry = {"t": round(tick, 3), **_focus(focus_interval)}
        if digest == previous_hash and previous_name:
            entry.update({"f": previous_name, "new": False})  # nothing moved: no new file
        else:
            name = f"f-{int(tick * 1000)}.jpg"
            (FRAME_DIR / name).write_bytes(payload)
            entry.update({"f": name, "new": True, "n": len(payload)})
            previous_hash, previous_name = digest, name
            captured += 1
        _append(entry)

        if tick - last_prune >= max(5.0, interval):
            kept, total = _prune(window, max_bytes)
            last_prune = tick
        else:
            kept, total = None, None

        state = {"t": tick, "pid": os.getpid(), "healthy": True, "fails": 0, "captured": captured,
                 "state": "recording", "last_frame": entry["f"], "changed": entry["new"]}
        if kept is not None:
            state.update({"frames": kept, "bytes": total})
        _write_json(BEAT, state)

        next_tick += interval
        sleep_for = next_tick - time.monotonic()
        if sleep_for < -interval:  # the machine slept, or we fell far behind
            next_tick = time.monotonic()
            sleep_for = 0.0
        end = time.monotonic() + max(0.0, sleep_for)
        while time.monotonic() < end and not stop["now"]:
            time.sleep(min(0.2, max(0.01, end - time.monotonic())))

    scratch.unlink(missing_ok=True)
    _note("recorder asked to stop")
    return 0


def supervise(cfg: dict) -> int:
    """Keep a recorder alive. Cheap insurance against the one failure the loop
    itself cannot catch: the process going away entirely."""
    stop = {"now": False}
    child: subprocess.Popen | None = None

    def _bye(*_):
        stop["now"] = True
        if child and child.poll() is None:
            child.terminate()

    for sig in (signal.SIGTERM, signal.SIGINT):
        signal.signal(sig, _bye)

    restarts = 0
    while not stop["now"]:
        argv = [sys.executable, str(Path(__file__).resolve().parent / "usepc_ctl.py"),
                "watch", "_run", "--config", json.dumps(cfg)]
        with LOG.open("ab") as log:
            child = subprocess.Popen(argv, stdin=subprocess.DEVNULL, stdout=log, stderr=log)
        record_daemon(cfg, supervisor=os.getpid(), worker=child.pid, restarts=restarts)
        code = child.wait()
        if stop["now"] or code == 0:
            break
        restarts += 1
        _note(f"recorder exited with {code}; restarting (#{restarts})")
        time.sleep(min(20.0, 2.0 * restarts))
    _note("supervisor stopped")
    return 0


def record_daemon(cfg: dict, supervisor: int, worker: int, restarts: int) -> None:
    _write_json(DAEMON, {**cfg, "pid": supervisor, "worker": worker, "restarts": restarts})


# ------------------------------------------------------------------ lifecycle


def start(fps: float = 1.0, seconds: float = 300.0, detail: str = "normal",
          monitor: str | None = None, region: str | None = None, cursor: bool = False,
          max_mb: float = 256.0, restart: bool = False) -> dict:
    existing = config()
    if existing and _alive(existing.get("pid")):
        if not restart:
            return {"already_running": True, **status()}
        stop()

    if detail not in SCALE:
        raise WatchError(f"detail must be one of {', '.join(SCALE)}")
    if sysinfo.IS_LINUX and not shutil.which("grim"):
        raise WatchError("no grim — install grim to record a Wayland desktop")

    FRAME_DIR.mkdir(parents=True, exist_ok=True)
    cfg = {
        "fps": max(0.05, min(float(fps), 4.0)),
        "seconds": max(10.0, float(seconds)),
        "detail": detail,
        "monitor": monitor,
        "region": region,
        "cursor": bool(cursor),
        "max_mb": max(16.0, float(max_mb)),
        "display": os.environ.get("WAYLAND_DISPLAY") or os.environ.get("DISPLAY"),
        "started": time.time(),
    }
    argv = [sys.executable, str(Path(__file__).resolve().parent / "usepc_ctl.py"),
            "watch", "_supervise", "--config", json.dumps(cfg)]
    with LOG.open("ab") as log:
        proc = subprocess.Popen(argv, stdin=subprocess.DEVNULL, stdout=log, stderr=log,
                                start_new_session=True)
    record_daemon(cfg, supervisor=proc.pid, worker=0, restarts=0)

    for _ in range(80):  # hand back a buffer the caller can actually use
        time.sleep(0.1)
        if (beat() or {}).get("captured"):
            break
        if proc.poll() is not None:
            raise WatchError(f"recorder died on startup — see {LOG}")
    return {"started": True, **status()}


def stop(purge: bool = False) -> dict:
    cfg = config() or {}
    pid = cfg.get("pid")
    if _alive(pid):
        os.kill(pid, signal.SIGTERM)
        for _ in range(40):
            if not _alive(pid):
                break
            time.sleep(0.1)
        if _alive(pid):
            os.kill(pid, signal.SIGKILL)
    worker = cfg.get("worker")
    if _alive(worker):
        try:
            os.kill(worker, signal.SIGTERM)
        except OSError:
            pass
    DAEMON.unlink(missing_ok=True)
    BEAT.unlink(missing_ok=True)
    removed = clear() if purge else 0
    return {"stopped": bool(pid), "pid": pid, "frames_deleted": removed,
            "frames_left": 0 if purge else len(frame_files())}


def clear() -> int:
    files = frame_files()
    for path in files:
        path.unlink(missing_ok=True)
    INDEX.unlink(missing_ok=True)
    return len(files)


def healthy() -> tuple[bool, str]:
    cfg = config()
    if not cfg:
        return False, "never started"
    if not _alive(cfg.get("pid")):
        return False, "daemon is gone"
    pulse = beat()
    if not pulse:
        return False, "no heartbeat yet"
    age = time.time() - pulse.get("t", 0)
    if age > STALE_AFTER + 1.0 / max(0.05, cfg.get("fps", 1.0)):
        return False, f"heartbeat {age:.0f}s stale"
    if not pulse.get("healthy"):
        return False, pulse.get("reason") or "capture failing"
    return True, "recording"


def ensure_running() -> dict | None:
    """Self-heal on use. Only ever restarts something the user started before —
    it will not begin watching a screen on its own."""
    cfg = config()
    if not cfg:
        return None
    ok, why = healthy()
    if ok:
        return None
    if _alive(cfg.get("pid")) and "stale" not in why:
        return None
    _note(f"self-healing: {why}")
    stop()
    _write_json(DAEMON, cfg)  # keep the config so start() can reuse it
    revived = start(fps=cfg["fps"], seconds=cfg["seconds"], detail=cfg["detail"],
                    monitor=cfg.get("monitor"), region=cfg.get("region"),
                    cursor=cfg.get("cursor"), max_mb=cfg["max_mb"], restart=True)
    return {"restarted": True, "was": why, **revived}


def status() -> dict:
    cfg = config()
    files = frame_files()
    ok, why = healthy()
    out: dict = {"running": bool(cfg and _alive(cfg.get("pid"))), "healthy": ok, "state": why,
                 "frames_on_disk": len(files), "dir": str(WATCH_DIR)}
    if cfg:
        out.update({k: cfg.get(k) for k in
                    ("fps", "seconds", "detail", "monitor", "region", "cursor", "max_mb",
                     "pid", "worker", "restarts")})
        out["started"] = time.strftime("%H:%M:%S", time.localtime(cfg.get("started", 0)))
    if files:
        out["span_seconds"] = round(_frame_time(files[-1]) - _frame_time(files[0]), 1)
        out["bytes"] = sum(p.stat().st_size for p in files if p.exists())
    entries = read_index()
    if entries:
        out["newest_age_seconds"] = round(time.time() - entries[-1]["t"], 1)
        out["ticks"] = len(entries)
    pulse = beat()
    if pulse:
        out["captured_total"] = pulse.get("captured")
        if not pulse.get("healthy"):
            out["failing_because"] = pulse.get("reason")
    return out


# ------------------------------------------------------------------ reading it back


def _span(seconds: float) -> list[dict]:
    entries = read_index(seconds)
    if not entries:
        raise WatchError(
            "nothing recorded for that window — `usepc watch start` first, "
            "or `usepc watch status` to see why it stopped")
    return entries


def segments(seconds: float = 300.0, min_seconds: float = 0.0) -> list[dict]:
    """Consecutive ticks sharing a focused window, collapsed into stretches."""
    entries = _span(seconds)
    out: list[dict] = []
    for entry in entries:
        key = (entry.get("app"), entry.get("title"))
        if out and (out[-1]["app"], out[-1]["title"]) == key:
            current = out[-1]
            current["end"] = entry["t"]
            current["ticks"] += 1
            current["changed"] += 1 if entry.get("new") else 0
            current["last_frame"] = entry.get("f") or current["last_frame"]
        else:
            out.append({"app": entry.get("app"), "title": entry.get("title"),
                        "ws": entry.get("ws"), "start": entry["t"], "end": entry["t"],
                        "ticks": 1, "changed": 1 if entry.get("new") else 0,
                        "first_frame": entry.get("f"), "last_frame": entry.get("f")})
    now = time.time()
    for seg in out:
        seg["seconds"] = round(max(seg["end"] - seg["start"], 0.0), 1)
        seg["idle"] = seg["changed"] <= 1 and seg["seconds"] >= 5
        seg["ago"] = round(now - seg["end"], 1)
    return [s for s in out if s["seconds"] >= min_seconds] or out


def pick_frames(seconds: float = 60.0, count: int = 6, changes_only: bool = True) -> list[dict]:
    """Frames worth looking at: prefer moments where the screen actually changed,
    always include the newest, and spread the rest across the window."""
    entries = _span(seconds)
    pool = [e for e in entries if e.get("new")] if changes_only else list(entries)
    if not pool:
        pool = [entries[-1]]
    if entries[-1] not in pool:
        pool.append(entries[-1])
    seen, unique = set(), []
    for entry in pool:
        if entry.get("f") and entry["f"] not in seen and (FRAME_DIR / entry["f"]).exists():
            seen.add(entry["f"])
            unique.append(entry)
    if not unique:
        raise WatchError("the frames for that window have already aged out of the ring")
    count = max(1, int(count))
    if count == 1:
        unique = unique[-1:]  # "one frame" always means the freshest one
    elif len(unique) > count:
        step = (len(unique) - 1) / (count - 1)
        unique = [unique[round(i * step)] for i in range(count)]
    now = time.time()
    return [{**e, "path": str(FRAME_DIR / e["f"]), "ago": round(now - e["t"], 1),
             "clock": time.strftime("%H:%M:%S", time.localtime(e["t"]))} for e in unique]


def _montage() -> list[str] | None:
    if shutil.which("montage"):
        return ["montage"]
    if shutil.which("magick"):
        return ["magick", "montage"]
    return None


def sheet(picks: list[dict], columns: int = 3) -> Path | None:
    """One labelled contact sheet, so a look at five minutes costs one image."""
    tool = _montage()
    if not tool or len(picks) < 2:
        return None
    dest = WATCH_DIR / f"sheet-{time.strftime('%H%M%S')}.jpg"
    argv = tool + ["-background", "#141414", "-fill", "#e8e8e8", "-pointsize", "13",
                   "-tile", f"{max(1, columns)}x", "-geometry", "+5+5"]
    for pick in picks:
        label = f"{pick['clock']}  -{pick['ago']:.0f}s"
        if pick.get("app"):
            label += f"  {pick['app']}"
        argv += ["-label", label, pick["path"]]
    argv.append(str(dest))
    proc = subprocess.run(argv, capture_output=True, text=True, timeout=90)
    if proc.returncode != 0 or not dest.exists():
        return None
    return dest


def clip(seconds: float = 300.0, speed: float = 10.0, fps: float = 12.0) -> Path:
    """A scrubbable mp4 of the window, time-compressed. For the human, mostly."""
    if not shutil.which("ffmpeg"):
        raise WatchError("no ffmpeg — `usepc watch view` gives a contact sheet instead")
    entries = [e for e in _span(seconds) if e.get("f")]
    frames: list[tuple[float, Path]] = []
    for entry in entries:
        path = FRAME_DIR / entry["f"]
        if path.exists() and (not frames or frames[-1][1] != path):
            frames.append((entry["t"], path))
    if len(frames) < 2:
        raise WatchError("not enough distinct frames in that window to build a clip")
    speed = max(1.0, float(speed))
    lines = []
    for i, (stamp, path) in enumerate(frames):
        nxt = frames[i + 1][0] if i + 1 < len(frames) else stamp + 1.0
        lines.append(f"file '{path}'\nduration {max(0.04, (nxt - stamp) / speed):.3f}")
    lines.append(f"file '{frames[-1][1]}'")
    listing = WATCH_DIR / "clip.txt"
    listing.write_text("\n".join(lines) + "\n")
    dest = WATCH_DIR / f"clip-{time.strftime('%Y%m%d-%H%M%S')}.mp4"
    proc = subprocess.run(
        ["ffmpeg", "-y", "-loglevel", "error", "-f", "concat", "-safe", "0", "-i", str(listing),
         "-vf", "scale=trunc(iw/2)*2:trunc(ih/2)*2", "-r", str(fps), "-pix_fmt", "yuv420p",
         "-movflags", "+faststart", str(dest)],
        capture_output=True, text=True, timeout=300)
    if proc.returncode != 0 or not dest.exists():
        raise WatchError(f"ffmpeg failed: {proc.stderr.strip()[-200:]}")
    return dest


def brief() -> dict:
    """The one-line summary other parts of usepc show."""
    cfg = config()
    if not cfg:
        return {"running": False}
    ok, why = healthy()
    entries = read_index(120.0)
    latest = entries[-1] if entries else None
    return {"running": _alive(cfg.get("pid")), "healthy": ok, "state": why,
            "fps": cfg.get("fps"), "window": cfg.get("seconds"),
            "frames": len(frame_files()),
            "doing": f"{latest.get('app')} — {latest.get('title')}".strip(" —") if latest else None,
            "newest_age": round(time.time() - latest["t"], 1) if latest else None}
