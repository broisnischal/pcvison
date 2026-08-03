"""What the machine is doing right now, on any of the three platforms.

Everything here degrades instead of raising: a missing sensor becomes None and the
caller just doesn't draw it. Every field is cheap enough to poll once a second.

`Sampler` holds the previous counter read so CPU and network figures are
*instantaneous* rather than the lifetime averages `ps` and friends report. A
long-lived monitor keeps one Sampler; a one-shot `snapshot()` takes two reads
120 ms apart, which is short enough to feel immediate and long enough to be true.
"""

from __future__ import annotations

import json
import os
import platform
import shutil
import subprocess
import time
from pathlib import Path

OS = platform.system()  # "Linux" | "Darwin" | "Windows"
IS_LINUX, IS_MAC, IS_WIN = OS == "Linux", OS == "Darwin", OS == "Windows"


def _run(cmd: list[str], timeout: float = 4.0) -> str:
    try:
        proc = subprocess.run(cmd, capture_output=True, text=True, timeout=timeout)
        return proc.stdout if proc.returncode == 0 else ""
    except Exception:
        return ""


def _powershell(script: str, timeout: float = 8.0) -> str:
    exe = shutil.which("pwsh") or shutil.which("powershell")
    if not exe:
        return ""
    return _run([exe, "-NoProfile", "-NonInteractive", "-Command", script], timeout)


def human_bytes(value: float | None) -> str:
    if value is None:
        return "-"
    for unit in ("B", "K", "M", "G", "T"):
        if abs(value) < 1024 or unit == "T":
            return f"{value:.0f}{unit}" if unit in ("B", "K") else f"{value:.1f}{unit}"
        value /= 1024.0
    return f"{value:.1f}T"


def human_time(seconds: float | None) -> str:
    if not seconds:
        return "-"
    seconds = int(seconds)
    days, rem = divmod(seconds, 86400)
    hours, rem = divmod(rem, 3600)
    minutes = rem // 60
    if days:
        return f"{days}d {hours}h"
    if hours:
        return f"{hours}h {minutes}m"
    return f"{minutes}m"


class Sampler:
    """Keeps the previous counters so rates can be differenced."""

    def __init__(self) -> None:
        self.prev_cpu: tuple | None = None
        self.prev_cores: list[tuple] | None = None
        self.prev_net: tuple[float, int, int] | None = None
        self.prev_procs: dict[int, float] | None = None
        self.prev_time: float | None = None

    # ---- cpu ---------------------------------------------------------------

    def cpu(self) -> dict:
        if IS_LINUX:
            return self._cpu_linux()
        if IS_MAC:
            return self._cpu_mac()
        return self._cpu_win()

    def _read_proc_stat(self) -> tuple[tuple, list[tuple]]:
        total: tuple = ()
        cores: list[tuple] = []
        for line in Path("/proc/stat").read_text().splitlines():
            if not line.startswith("cpu"):
                break
            parts = line.split()
            values = tuple(int(v) for v in parts[1:11])
            if parts[0] == "cpu":
                total = values
            else:
                cores.append(values)
        return total, cores

    @staticmethod
    def _busy(prev: tuple, now: tuple) -> float | None:
        if not prev or not now:
            return None
        idle_prev, idle_now = prev[3] + prev[4], now[3] + now[4]
        total_prev, total_now = sum(prev), sum(now)
        delta = total_now - total_prev
        if delta <= 0:
            return None
        return max(0.0, min(100.0, 100.0 * (1.0 - (idle_now - idle_prev) / delta)))

    def _cpu_linux(self) -> dict:
        total, cores = self._read_proc_stat()
        percent = self._busy(self.prev_cpu, total) if self.prev_cpu else None
        per_core = None
        if self.prev_cores and len(self.prev_cores) == len(cores):
            per_core = [self._busy(p, c) for p, c in zip(self.prev_cores, cores)]
        self.prev_cpu, self.prev_cores = total, cores
        freq = None
        try:
            freqs = [int(p.read_text()) / 1000.0
                     for p in Path("/sys/devices/system/cpu").glob("cpu[0-9]*/cpufreq/scaling_cur_freq")]
            freq = sum(freqs) / len(freqs) if freqs else None
        except Exception:
            pass
        return {"percent": percent, "per_core": per_core, "cores": len(cores),
                "freq_mhz": freq, "temp_c": self._temp_linux()}

    @staticmethod
    def _temp_linux() -> float | None:
        best = None
        for zone in Path("/sys/class/thermal").glob("thermal_zone*"):
            try:
                kind = (zone / "type").read_text().strip()
                value = int((zone / "temp").read_text()) / 1000.0
            except Exception:
                continue
            if 0 < value < 130 and (kind.startswith(("x86_pkg", "cpu", "acpitz")) or best is None):
                best = value if best is None else max(best, value)
        if best is None:
            for hwmon in Path("/sys/class/hwmon").glob("hwmon*/temp1_input"):
                try:
                    value = int(hwmon.read_text()) / 1000.0
                    if 0 < value < 130:
                        best = value
                        break
                except Exception:
                    continue
        return best

    def _cpu_mac(self) -> dict:
        out = _run(["top", "-l", "1", "-s", "0", "-n", "0"], timeout=6)
        percent = None
        for line in out.splitlines():
            if line.startswith("CPU usage"):
                try:
                    idle = float(line.split(",")[-1].strip().split("%")[0])
                    percent = max(0.0, 100.0 - idle)
                except Exception:
                    pass
                break
        cores = os.cpu_count() or 0
        temp = None
        return {"percent": percent, "per_core": None, "cores": cores,
                "freq_mhz": None, "temp_c": temp}

    def _cpu_win(self) -> dict:
        raw = _powershell(
            "$c=Get-CimInstance Win32_Processor;"
            "[pscustomobject]@{p=($c|Measure-Object -Property LoadPercentage -Average).Average;"
            "n=($c|Measure-Object -Property NumberOfLogicalProcessors -Sum).Sum;"
            "f=($c|Measure-Object -Property CurrentClockSpeed -Average).Average}|ConvertTo-Json -Compress")
        try:
            data = json.loads(raw)
            return {"percent": float(data.get("p") or 0), "per_core": None,
                    "cores": int(data.get("n") or 0),
                    "freq_mhz": float(data.get("f") or 0) or None, "temp_c": None}
        except Exception:
            return {"percent": None, "per_core": None, "cores": os.cpu_count() or 0,
                    "freq_mhz": None, "temp_c": None}

    # ---- memory ------------------------------------------------------------

    def memory(self) -> dict:
        if IS_LINUX:
            info = {}
            for line in Path("/proc/meminfo").read_text().splitlines():
                key, _, rest = line.partition(":")
                info[key] = int(rest.strip().split()[0]) * 1024
            total = info.get("MemTotal", 0)
            available = info.get("MemAvailable", 0)
            swap_total = info.get("SwapTotal", 0)
            swap_used = swap_total - info.get("SwapFree", 0)
            return {"total": total, "used": total - available,
                    "percent": 100.0 * (total - available) / total if total else None,
                    "swap_total": swap_total, "swap_used": swap_used,
                    "cached": info.get("Cached", 0)}
        if IS_MAC:
            total = int(_run(["sysctl", "-n", "hw.memsize"]).strip() or 0)
            page = 4096
            stats = {}
            for line in _run(["vm_stat"]).splitlines():
                key, _, rest = line.partition(":")
                digits = rest.strip().rstrip(".")
                if digits.isdigit():
                    stats[key.strip()] = int(digits) * page
                if "page size of" in line:
                    try:
                        page = int(line.split("page size of")[1].split("bytes")[0].strip())
                    except Exception:
                        pass
            free = stats.get("Pages free", 0) + stats.get("Pages inactive", 0)
            used = max(0, total - free)
            swap_line = _run(["sysctl", "-n", "vm.swapusage"])
            swap_total = swap_used = 0
            try:
                parts = swap_line.replace("M", "").split()
                swap_total = int(float(parts[2]) * 1024 * 1024)
                swap_used = int(float(parts[5]) * 1024 * 1024)
            except Exception:
                pass
            return {"total": total, "used": used,
                    "percent": 100.0 * used / total if total else None,
                    "swap_total": swap_total, "swap_used": swap_used, "cached": 0}
        raw = _powershell(
            "$o=Get-CimInstance Win32_OperatingSystem;"
            "[pscustomobject]@{t=$o.TotalVisibleMemorySize;f=$o.FreePhysicalMemory;"
            "st=$o.TotalVirtualMemorySize;sf=$o.FreeVirtualMemory}|ConvertTo-Json -Compress")
        try:
            data = json.loads(raw)
            total = int(data["t"]) * 1024
            used = total - int(data["f"]) * 1024
            return {"total": total, "used": used,
                    "percent": 100.0 * used / total if total else None,
                    "swap_total": int(data.get("st", 0)) * 1024,
                    "swap_used": (int(data.get("st", 0)) - int(data.get("sf", 0))) * 1024,
                    "cached": 0}
        except Exception:
            return {"total": 0, "used": 0, "percent": None, "swap_total": 0,
                    "swap_used": 0, "cached": 0}

    # ---- network -----------------------------------------------------------

    def network(self) -> dict:
        now = time.monotonic()
        rx = tx = 0
        if IS_LINUX:
            try:
                for line in Path("/proc/net/dev").read_text().splitlines()[2:]:
                    name, _, rest = line.partition(":")
                    if name.strip() in ("lo",) or name.strip().startswith(("veth", "docker", "br-")):
                        continue
                    fields = rest.split()
                    rx += int(fields[0])
                    tx += int(fields[8])
            except Exception:
                return {"rx_rate": None, "tx_rate": None}
        elif IS_MAC:
            for line in _run(["netstat", "-ib"]).splitlines()[1:]:
                fields = line.split()
                if len(fields) > 9 and fields[0] != "lo0" and "Link" in fields[2]:
                    try:
                        rx += int(fields[6])
                        tx += int(fields[9])
                    except (ValueError, IndexError):
                        continue
        else:
            raw = _powershell(
                "$s=Get-NetAdapterStatistics -ErrorAction SilentlyContinue|"
                "Measure-Object -Property ReceivedBytes,SentBytes -Sum;"
                "[pscustomobject]@{rx=$s[0].Sum;tx=$s[1].Sum}|ConvertTo-Json -Compress")
            try:
                data = json.loads(raw)
                rx, tx = int(data.get("rx") or 0), int(data.get("tx") or 0)
            except Exception:
                return {"rx_rate": None, "tx_rate": None}
        rates: dict = {"rx_rate": None, "tx_rate": None, "rx_total": rx, "tx_total": tx}
        if self.prev_net:
            prev_time, prev_rx, prev_tx = self.prev_net
            span = now - prev_time
            if span > 0.01:
                rates["rx_rate"] = max(0, (rx - prev_rx) / span)
                rates["tx_rate"] = max(0, (tx - prev_tx) / span)
        self.prev_net = (now, rx, tx)
        return rates

    # ---- processes ---------------------------------------------------------

    def top_processes(self, limit: int = 6) -> list[dict]:
        """Instantaneous CPU share per process, differenced between samples."""
        if not IS_LINUX:
            return self._top_processes_portable(limit)
        now = time.monotonic()
        ticks = os.sysconf("SC_CLK_TCK")
        current: dict[int, float] = {}
        names: dict[int, str] = {}
        rss: dict[int, int] = {}
        for entry in Path("/proc").iterdir():
            if not entry.name.isdigit():
                continue
            try:
                fields = (entry / "stat").read_text().rsplit(") ", 1)
                name = fields[0].split("(", 1)[1]
                rest = fields[1].split()
                cpu_time = (int(rest[11]) + int(rest[12])) / ticks
                current[int(entry.name)] = cpu_time
                names[int(entry.name)] = name
                rss[int(entry.name)] = int(rest[21]) * os.sysconf("SC_PAGE_SIZE")
            except Exception:
                continue
        result: list[dict] = []
        if self.prev_procs and self.prev_time:
            span = now - self.prev_time
            if span > 0.01:
                for pid, cpu_time in current.items():
                    delta = cpu_time - self.prev_procs.get(pid, cpu_time)
                    if delta > 0:
                        result.append({"pid": pid, "name": names.get(pid, "?"),
                                       "cpu": 100.0 * delta / span, "rss": rss.get(pid, 0)})
        self.prev_procs, self.prev_time = current, now
        result.sort(key=lambda p: p["cpu"], reverse=True)
        return result[:limit]

    @staticmethod
    def _top_processes_portable(limit: int) -> list[dict]:
        if IS_MAC:
            out = _run(["ps", "-Ao", "pid,pcpu,rss,comm", "-r"])
            rows = []
            for line in out.splitlines()[1:limit + 1]:
                parts = line.split(None, 3)
                if len(parts) == 4:
                    rows.append({"pid": int(parts[0]), "cpu": float(parts[1]),
                                 "rss": int(parts[2]) * 1024,
                                 "name": Path(parts[3]).name})
            return rows
        raw = _powershell(
            f"Get-Process|Sort-Object CPU -Descending|Select-Object -First {limit} "
            "Id,ProcessName,WorkingSet64,CPU|ConvertTo-Json -Compress")
        try:
            data = json.loads(raw)
            if isinstance(data, dict):
                data = [data]
            return [{"pid": d.get("Id"), "name": d.get("ProcessName"),
                     "cpu": float(d.get("CPU") or 0), "rss": int(d.get("WorkingSet64") or 0)}
                    for d in data]
        except Exception:
            return []


# ---------------------------------------------------------------- static-ish facts


def disks() -> list[dict]:
    rows: list[dict] = []
    if IS_WIN:
        raw = _powershell(
            "Get-CimInstance Win32_LogicalDisk -Filter 'DriveType=3'|"
            "Select-Object DeviceID,Size,FreeSpace|ConvertTo-Json -Compress")
        try:
            data = json.loads(raw)
            if isinstance(data, dict):
                data = [data]
            for entry in data:
                total = int(entry.get("Size") or 0)
                free = int(entry.get("FreeSpace") or 0)
                if total:
                    rows.append({"mount": entry.get("DeviceID"), "total": total,
                                 "used": total - free,
                                 "percent": 100.0 * (total - free) / total})
        except Exception:
            pass
        return rows
    for mount in ("/", str(Path.home())):
        try:
            usage = shutil.disk_usage(mount)
            rows.append({"mount": mount, "total": usage.total, "used": usage.used,
                         "percent": 100.0 * usage.used / usage.total if usage.total else None})
        except Exception:
            continue
    # de-duplicate when $HOME is on /
    if len(rows) == 2 and rows[0]["total"] == rows[1]["total"] and rows[0]["used"] == rows[1]["used"]:
        rows.pop()
    return rows


def battery() -> dict | None:
    if IS_LINUX:
        for supply in sorted(Path("/sys/class/power_supply").glob("BAT*")):
            try:
                percent = int((supply / "capacity").read_text().strip())
                state = (supply / "status").read_text().strip()
                return {"percent": percent, "state": state.lower()}
            except Exception:
                continue
        return None
    if IS_MAC:
        out = _run(["pmset", "-g", "batt"])
        for line in out.splitlines():
            if "%" in line:
                try:
                    percent = int(line.split("\t")[1].split("%")[0])
                    state = "charging" if "charging" in line else (
                        "charged" if "charged" in line else "discharging")
                    return {"percent": percent, "state": state}
                except Exception:
                    return None
        return None
    raw = _powershell("$b=Get-CimInstance Win32_Battery|Select-Object -First 1;"
                      "if($b){[pscustomobject]@{p=$b.EstimatedChargeRemaining;"
                      "s=$b.BatteryStatus}|ConvertTo-Json -Compress}")
    try:
        data = json.loads(raw)
        return {"percent": int(data["p"]),
                "state": "charging" if int(data.get("s", 1)) == 2 else "discharging"}
    except Exception:
        return None


def gpu() -> dict | None:
    if shutil.which("nvidia-smi"):
        out = _run(["nvidia-smi", "--query-gpu=utilization.gpu,memory.used,memory.total,temperature.gpu",
                    "--format=csv,noheader,nounits"], timeout=3)
        line = out.strip().splitlines()[0] if out.strip() else ""
        parts = [p.strip() for p in line.split(",")]
        if len(parts) == 4:
            try:
                return {"percent": float(parts[0]), "mem_used": float(parts[1]) * 1024 * 1024,
                        "mem_total": float(parts[2]) * 1024 * 1024, "temp_c": float(parts[3])}
            except ValueError:
                pass
    if IS_LINUX:
        for card in sorted(Path("/sys/class/drm").glob("card[0-9]/device/gpu_busy_percent")):
            try:
                return {"percent": float(card.read_text().strip())}
            except Exception:
                continue
    return None


def uptime() -> float | None:
    if IS_LINUX:
        try:
            return float(Path("/proc/uptime").read_text().split()[0])
        except Exception:
            return None
    if IS_MAC:
        raw = _run(["sysctl", "-n", "kern.boottime"])
        try:
            boot = int(raw.split("sec = ")[1].split(",")[0])
            return time.time() - boot
        except Exception:
            return None
    raw = _powershell("[int]((Get-Date)-(Get-CimInstance Win32_OperatingSystem).LastBootUpTime)"
                      ".TotalSeconds")
    try:
        return float(raw.strip())
    except Exception:
        return None


def load() -> tuple[float, float, float] | None:
    try:
        return os.getloadavg()
    except Exception:
        return None


# ---------------------------------------------------------------- the desktop


def desktop() -> dict:
    """Which windows exist and what has focus — on the *user's* session."""
    info: dict = {"platform": OS}
    if IS_LINUX:
        if shutil.which("hyprctl") and os.environ.get("HYPRLAND_INSTANCE_SIGNATURE", "1"):
            try:
                active = json.loads(_run(["hyprctl", "activewindow", "-j"]) or "{}")
                monitors = json.loads(_run(["hyprctl", "monitors", "-j"]) or "[]")
                clients = json.loads(_run(["hyprctl", "clients", "-j"]) or "[]")
                info.update({
                    "compositor": "hyprland",
                    "active": f"{active.get('class', '?')} — {active.get('title', '')}".strip(" —"),
                    "workspace": (active.get("workspace") or {}).get("name"),
                    "monitors": [{"name": m["name"], "resolution": f"{m['width']}x{m['height']}",
                                  "scale": m.get("scale"), "focused": m.get("focused")}
                                 for m in monitors],
                    "window_count": len(clients),
                    "windows": [f"{c.get('class', '?')}: {c.get('title', '')[:60]}"
                                for c in clients],
                })
                return info
            except Exception:
                pass
        if shutil.which("swaymsg"):
            info["compositor"] = "sway"
        elif os.environ.get("DISPLAY"):
            info["compositor"] = "x11"
            if shutil.which("xdotool"):
                info["active"] = _run(["xdotool", "getactivewindow", "getwindowname"]).strip()
        return info
    if IS_MAC:
        info["compositor"] = "quartz"
        info["active"] = _run(["osascript", "-e",
                               'tell application "System Events" to get name of first process '
                               'whose frontmost is true']).strip()
        info["windows"] = [line for line in _run(
            ["osascript", "-e",
             'tell application "System Events" to get name of every process whose visible is true']
        ).strip().split(", ") if line]
        info["window_count"] = len(info.get("windows") or [])
        return info
    info["compositor"] = "dwm"
    raw = _powershell(
        "Add-Type -AssemblyName Microsoft.VisualBasic;"
        "$p=Get-Process|Where-Object{$_.MainWindowTitle}|Select-Object ProcessName,MainWindowTitle;"
        "$p|ConvertTo-Json -Compress")
    try:
        data = json.loads(raw)
        if isinstance(data, dict):
            data = [data]
        info["windows"] = [f"{d['ProcessName']}: {d['MainWindowTitle'][:60]}" for d in data]
        info["window_count"] = len(data)
    except Exception:
        pass
    return info


def host() -> dict:
    return {"hostname": platform.node(), "os": OS,
            "release": platform.release(),
            "pretty": _pretty_os(), "arch": platform.machine(),
            "python": platform.python_version()}


def _pretty_os() -> str:
    if IS_LINUX:
        try:
            for line in Path("/etc/os-release").read_text().splitlines():
                if line.startswith("PRETTY_NAME="):
                    return line.split("=", 1)[1].strip().strip('"')
        except Exception:
            pass
        return f"Linux {platform.release()}"
    if IS_MAC:
        return f"macOS {platform.mac_ver()[0]}"
    return f"{platform.system()} {platform.release()}"


def snapshot(sampler: Sampler | None = None, full: bool = False, settle: float = 0.12) -> dict:
    """One complete reading. Without a warm Sampler, samples twice to get rates."""
    own = sampler is None
    sampler = sampler or Sampler()
    if own:
        sampler.cpu()
        sampler.network()
        sampler.top_processes()
        time.sleep(settle)
    data = {
        "at": time.time(),
        "host": host(),
        "uptime": uptime(),
        "load": load(),
        "cpu": sampler.cpu(),
        "memory": sampler.memory(),
        "network": sampler.network(),
        "disks": disks(),
        "battery": battery(),
        "processes": sampler.top_processes(),
        "desktop": desktop(),
    }
    if full:
        data["gpu"] = gpu()
    return data
