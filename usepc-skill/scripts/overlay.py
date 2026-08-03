"""A translucent cursor drawn over the user's desktop.

The agent's real pointer lives in its own seat, where it has a real cursor. This
is the other half: a *see-through* cursor painted on the user's screen so they can
watch where the agent is pointing — during a takeover of their desktop, or as a
marker before a click lands.

It is a wlr-layer-shell surface on the overlay layer with an **empty input
region**, so it swallows nothing: clicks, scrolls and hovers pass straight through
to whatever is underneath. One surface per monitor, drawn only on the monitor the
point falls in.

Run as a daemon (`usepc cursor start`) and steer it by writing cursor.json;
polling a tiny file beats plumbing a socket and costs nothing measurable.
"""

from __future__ import annotations

import json
import math
import os
import sys
import time
from pathlib import Path

STATE_DIR = Path(os.environ.get("USEPC_HOME") or (Path.home() / ".cache" / "usepc"))
CURSOR_FILE = STATE_DIR / "cursor.json"
PID_FILE = STATE_DIR / "cursor.pid"
POLL_MS = 30
ACCENT = (0.478, 0.635, 0.968)  # #7aa2f7 — clearly not the user's own cursor


def write_command(**fields) -> None:
    STATE_DIR.mkdir(parents=True, exist_ok=True)
    payload = read_command()
    payload.update(fields)
    payload["at"] = time.time()
    tmp = CURSOR_FILE.with_suffix(".tmp")
    tmp.write_text(json.dumps(payload))
    tmp.replace(CURSOR_FILE)


def read_command() -> dict:
    try:
        return json.loads(CURSOR_FILE.read_text())
    except Exception:
        return {}


def daemon_pid() -> int | None:
    try:
        pid = int(PID_FILE.read_text().strip())
        os.kill(pid, 0)
        return pid
    except Exception:
        return None


def stop_daemon() -> str:
    pid = daemon_pid()
    if not pid:
        return "no cursor overlay running"
    try:
        os.kill(pid, 15)
    except Exception:
        pass
    PID_FILE.unlink(missing_ok=True)
    return f"cursor overlay stopped (pid {pid})"


# ------------------------------------------------------------------ the drawing


def _draw_cursor(cr, x: float, y: float, scale: float, alpha: float, label: str | None) -> None:
    """A pointer arrow, outlined so it reads on light and dark backgrounds alike."""
    arrow = [(0, 0), (0, 17.5), (4.3, 13.3), (7.1, 19.5), (9.9, 18.2), (7.1, 12.0),
             (12.9, 12.0)]
    cr.save()
    cr.translate(x, y)
    cr.scale(scale, scale)

    cr.move_to(*arrow[0])
    for point in arrow[1:]:
        cr.line_to(*point)
    cr.close_path()

    cr.set_source_rgba(*ACCENT, alpha)
    cr.fill_preserve()
    cr.set_line_width(1.6)
    cr.set_source_rgba(1, 1, 1, min(1.0, alpha + 0.25))
    cr.stroke()
    cr.restore()

    # A soft ring: makes the agent pointer findable on a busy screen.
    cr.save()
    cr.translate(x, y)
    cr.set_line_width(1.4)
    cr.set_source_rgba(*ACCENT, alpha * 0.5)
    cr.arc(0, 0, 15 * scale, 0, 2 * math.pi)
    cr.stroke()
    cr.restore()

    if label:
        cr.save()
        cr.translate(x + 20 * scale, y + 26 * scale)
        cr.select_font_face("sans-serif")
        cr.set_font_size(11.5)
        extents = cr.text_extents(label)
        cr.set_source_rgba(0.06, 0.07, 0.1, alpha * 0.85)
        cr.rectangle(-4, -extents.height - 4, extents.width + 8, extents.height + 8)
        cr.fill()
        cr.set_source_rgba(1, 1, 1, min(1.0, alpha + 0.3))
        cr.move_to(0, 0)
        cr.show_text(label)
        cr.restore()


def run_daemon(alpha: float = 0.45, scale: float = 1.6) -> int:
    try:
        import gi
        gi.require_version("Gtk", "4.0")
        gi.require_version("Gtk4LayerShell", "1.0")
        from gi.repository import Gdk, GLib, Gtk
        from gi.repository import Gtk4LayerShell as LayerShell
        import cairo  # noqa: F401  (pycairo backs DrawingArea)
    except (ImportError, ValueError) as exc:
        sys.stderr.write(
            f"the translucent cursor needs GTK4 + gtk4-layer-shell + pycairo ({exc}).\n"
            "  Arch:   sudo pacman -S gtk4-layer-shell python-gobject python-cairo\n"
            "  Fedora: sudo dnf install gtk4-layer-shell python3-gobject python3-cairo\n"
            "The agent seat works without this; only the see-through pointer needs it.\n")
        return 2

    STATE_DIR.mkdir(parents=True, exist_ok=True)
    PID_FILE.write_text(str(os.getpid()))
    state = {"visible": False, "x": 0.0, "y": 0.0, "alpha": alpha,
             "scale": scale, "label": "agent"}

    class Surface:
        """One click-through layer surface, covering one monitor."""

        def __init__(self, monitor):
            self.monitor = monitor
            self.geometry = monitor.get_geometry()
            self.window = Gtk.Window()
            self.window.set_decorated(False)
            LayerShell.init_for_window(self.window)
            LayerShell.set_layer(self.window, LayerShell.Layer.OVERLAY)
            LayerShell.set_namespace(self.window, "usepc-cursor")
            LayerShell.set_monitor(self.window, monitor)
            LayerShell.set_keyboard_mode(self.window, LayerShell.KeyboardMode.NONE)
            for edge in (LayerShell.Edge.TOP, LayerShell.Edge.BOTTOM,
                         LayerShell.Edge.LEFT, LayerShell.Edge.RIGHT):
                LayerShell.set_anchor(self.window, edge, True)
            LayerShell.set_exclusive_zone(self.window, -1)
            self.area = Gtk.DrawingArea()
            self.area.set_draw_func(self._draw)
            self.window.set_child(self.area)
            self.window.connect("realize", self._make_click_through)
            self.window.present()

        def _make_click_through(self, *_):
            """Empty input region: the surface is visible but not clickable."""
            try:
                surface = self.window.get_surface()
                surface.set_input_region(cairo.Region())
            except Exception:
                pass

        def _draw(self, area, cr, width, height):
            cr.set_operator(cairo.OPERATOR_SOURCE)
            cr.set_source_rgba(0, 0, 0, 0)
            cr.paint()
            cr.set_operator(cairo.OPERATOR_OVER)
            if not state["visible"]:
                return
            local_x = state["x"] - self.geometry.x
            local_y = state["y"] - self.geometry.y
            if not (0 <= local_x <= self.geometry.width and 0 <= local_y <= self.geometry.height):
                return
            _draw_cursor(cr, local_x, local_y, state["scale"], state["alpha"], state["label"])

        def redraw(self):
            self.area.queue_draw()

    display = Gdk.Display.get_default()
    if display is None:
        sys.stderr.write("no Wayland display available for the cursor overlay\n")
        return 2
    surfaces = [Surface(monitor) for monitor in display.get_monitors()]

    def tick():
        command = read_command()
        changed = False
        for key in ("x", "y", "alpha", "scale", "label", "visible"):
            if key in command and command[key] != state.get(key):
                state[key] = command[key]
                changed = True
        if command.get("quit"):
            for surface in surfaces:
                surface.window.close()
            PID_FILE.unlink(missing_ok=True)
            return False
        if changed:
            for surface in surfaces:
                surface.redraw()
        return True

    GLib.timeout_add(POLL_MS, tick)
    write_command(visible=False, quit=False)

    loop = GLib.MainLoop()
    try:
        loop.run()
    except KeyboardInterrupt:
        pass
    finally:
        PID_FILE.unlink(missing_ok=True)
    return 0


if __name__ == "__main__":
    sys.exit(run_daemon())
