"""Thermostat, Linux half: the UNO Q's sketch runs the control loop; this app
serves the page, forwards target and on/off changes, and keeps 24 h of history."""
import json
import math
import os
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

TARGET_MIN, TARGET_MAX = 10, 30  # the sketch clamps to the same range
HISTORY_SECONDS = 24 * 3600
SAMPLE_SECONDS = 60

PAGE = """<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>UNO Q Thermostat</title>
<style>
  :root { --bg:#f5f7f7; --card:#fff; --fg:#17201f; --muted:#5d6967; --line:#d9e0de;
          --room:#0b7a73; --target:#b4531a; --heat:#f6e3d3; color-scheme: light; }
  @media (prefers-color-scheme: dark) { :root { --bg:#101615; --card:#172020; --fg:#e4ecea;
          --muted:#9aa8a5; --line:#2b3735; --room:#4cc3b8; --target:#f0965b; --heat:#3a2a1e; color-scheme: dark; } }
  * { box-sizing: border-box; }
  body { margin:0; background:var(--bg); color:var(--fg); font:15px/1.5 system-ui, sans-serif;
         padding: 24px max(16px, env(safe-area-inset-left)); }
  main { max-width: 44rem; margin: 0 auto; display: grid; gap: 16px; }
  h1 { margin: 0; font-size: 1.4rem; }
  .card { background: var(--card); border: 1px solid var(--line); border-radius: 12px; padding: 16px; }
  .now { display: flex; flex-wrap: wrap; gap: 24px; align-items: flex-end; }
  .big { font-size: 3rem; font-weight: 600; line-height: 1; font-variant-numeric: tabular-nums; }
  .label { color: var(--muted); font-size: .8rem; text-transform: uppercase; letter-spacing: .05em; }
  .target { display: flex; align-items: center; gap: 10px; }
  .target button { width: 44px; height: 44px; font-size: 1.4rem; border-radius: 50%;
                   border: 1px solid var(--line); background: var(--card); color: var(--fg); cursor: pointer; }
  .target .big { color: var(--target); min-width: 4.5ch; text-align: center; }
  .pill { display: inline-block; padding: 2px 10px; border-radius: 999px; font-size: .85rem; font-weight: 600;
          background: var(--line); color: var(--muted); }
  .pill.on { background: var(--target); color: #fff; }
  .row { display: flex; flex-wrap: wrap; gap: 12px; align-items: center; justify-content: space-between; }
  .switch { font-size: 1rem; padding: 8px 16px; border-radius: 8px; border: 1px solid var(--line);
            background: var(--card); color: var(--fg); cursor: pointer; }
  svg { width: 100%; height: auto; display: block; }
  .legend { display: flex; gap: 16px; color: var(--muted); font-size: .85rem; }
  .legend i { display: inline-block; width: 14px; height: 3px; vertical-align: middle; margin-right: 6px; }
  #error { color: var(--target); min-height: 1.5em; }
</style></head>
<body><main>
<h1>UNO Q Thermostat</h1>
<div class="card now">
  <div><div class="label">Room</div><div class="big" id="room">-</div></div>
  <div><div class="label">Target</div>
    <div class="target"><button id="down" aria-label="Lower target">&minus;</button>
      <span class="big" id="target">-</span>
      <button id="up" aria-label="Raise target">+</button></div></div>
  <div><div class="label">Humidity</div><div class="big" id="humidity" style="font-size:1.6rem">-</div></div>
</div>
<div class="card row">
  <div>Heating <span class="pill" id="heating">-</span></div>
  <button class="switch" id="power">-</button>
</div>
<div class="card">
  <div class="row"><div class="label">Last 24 hours</div>
    <div class="legend"><span><i style="background:var(--room)"></i>Room</span>
      <span><i style="background:var(--target)"></i>Target</span>
      <span><i style="background:var(--heat);height:10px"></i>Heating</span></div></div>
  <svg id="chart" viewBox="0 0 640 220" role="img" aria-label="Room temperature and target over time"></svg>
</div>
<div id="error"></div>
<script>
let state = null;
const $ = id => document.getElementById(id);
const deg = v => v === null ? "-" : v.toFixed(1) + " \\u00b0C";

async function post(path, body) {
  const r = await fetch(path, {method: "POST", headers: {"Content-Type": "application/json"}, body: JSON.stringify(body)});
  const j = await r.json();
  $("error").textContent = j.ok ? "" : j.error;
  refresh();
}
$("up").onclick = () => state && state.target !== null && post("/api/target", {target: state.target + 1});
$("down").onclick = () => state && state.target !== null && post("/api/target", {target: state.target - 1});
$("power").onclick = () => state && state.enabled !== null && post("/api/enabled", {enabled: !state.enabled});

async function refresh() {
  try { state = await (await fetch("/api/state")).json(); } catch (e) { return; }
  $("room").textContent = deg(state.celsius);
  $("target").textContent = state.target === null ? "-" : state.target + " \\u00b0C";
  $("humidity").textContent = state.humidity === null ? "-" : state.humidity.toFixed(0) + " %";
  const h = $("heating");
  h.textContent = state.heating === null ? "-" : state.enabled === false ? "off" : state.heating ? "on" : "idle";
  h.className = "pill" + (state.heating ? " on" : "");
  $("power").textContent = state.enabled === null ? "-" : state.enabled ? "Turn off" : "Turn on";
}

async function chart() {
  let pts; try { pts = await (await fetch("/api/history")).json(); } catch (e) { return; }
  const svg = $("chart"), W = 640, H = 220, L = 36, R = 8, T = 10, B = 24;
  const css = getComputedStyle(document.documentElement);
  const color = n => css.getPropertyValue(n).trim();
  if (pts.length < 2) { svg.innerHTML = `<text x="${W/2}" y="${H/2}" text-anchor="middle" fill="${color('--muted')}">Collecting data, one point per minute</text>`; return; }
  const t0 = pts[0].t, t1 = pts[pts.length - 1].t;
  const vals = pts.flatMap(p => [p.celsius, p.target]).filter(v => v !== null);
  let lo = Math.floor(Math.min(...vals) - 1), hi = Math.ceil(Math.max(...vals) + 1);
  const x = t => L + (t - t0) / Math.max(t1 - t0, 1) * (W - L - R);
  const y = v => T + (hi - v) / (hi - lo) * (H - T - B);
  let out = "";
  for (let i = 0; i < pts.length - 1; i++) if (pts[i].heating)
    out += `<rect x="${x(pts[i].t)}" y="${T}" width="${Math.max(x(pts[i+1].t) - x(pts[i].t), 1)}" height="${H-T-B}" fill="${color('--heat')}"/>`;
  const step = Math.max(1, Math.round((hi - lo) / 4));
  for (let v = lo; v <= hi; v += step)
    out += `<line x1="${L}" x2="${W-R}" y1="${y(v)}" y2="${y(v)}" stroke="${color('--line')}"/><text x="${L-6}" y="${y(v)+4}" text-anchor="end" font-size="11" fill="${color('--muted')}">${v}\\u00b0</text>`;
  [t0, (t0 + t1) / 2, t1].forEach((t, i) => {
    const d = new Date(t * 1000), s = d.getHours().toString().padStart(2, "0") + ":" + d.getMinutes().toString().padStart(2, "0");
    out += `<text x="${x(t)}" y="${H-6}" text-anchor="${["start","middle","end"][i]}" font-size="11" fill="${color('--muted')}">${s}</text>`;
  });
  const line = key => pts.filter(p => p[key] !== null).map((p, i) => (i ? "L" : "M") + x(p.t).toFixed(1) + "," + y(p[key]).toFixed(1)).join("");
  out += `<path d="${line('target')}" fill="none" stroke="${color('--target')}" stroke-width="2" stroke-dasharray="6 4"/>`;
  out += `<path d="${line('celsius')}" fill="none" stroke="${color('--room')}" stroke-width="2.5"/>`;
  svg.innerHTML = out;
}
refresh(); chart();
setInterval(refresh, 2000); setInterval(chart, 30000);
</script>
</main></body></html>
"""


def _number(value):
    return None if value is None or math.isnan(value) else value


class Thermostat:
    """Mirror of the sketch's state, plus history. None until the sketch reports."""

    def __init__(self, bridge, clock=time.time):
        self.bridge = bridge
        self.clock = clock
        self._lock = threading.Lock()
        self._state = {"celsius": None, "humidity": None, "target": None, "enabled": None, "heating": None}
        self._history = []
        # The sketch notifies its state every 2 s and on every change; the bridge
        # re-registers the handler on reconnect.
        bridge.provide("state", self.on_state)

    def on_state(self, celsius, humidity, target, enabled, heating):
        now = self.clock()
        point = {"t": now, "celsius": _number(celsius), "target": int(target), "heating": bool(heating)}
        with self._lock:
            # A missing Thermo reads as NaN, which JSON (and so the page) can't carry.
            self._state = {"celsius": point["celsius"], "humidity": _number(humidity),
                           "target": point["target"], "enabled": bool(enabled), "heating": point["heating"]}
            last = self._history[-1] if self._history else None
            # One point a minute, plus every change, so the chart shows each switch.
            if (last is None or now - last["t"] >= SAMPLE_SECONDS
                    or last["heating"] != point["heating"] or last["target"] != point["target"]):
                self._history.append(point)
            while self._history and now - self._history[0]["t"] > HISTORY_SECONDS:
                self._history.pop(0)

    def snapshot(self):
        with self._lock:
            return dict(self._state, target_min=TARGET_MIN, target_max=TARGET_MAX)

    def history(self):
        with self._lock:
            return list(self._history)

    def set_target(self, target):
        if isinstance(target, bool) or not isinstance(target, int) or not TARGET_MIN <= target <= TARGET_MAX:
            raise ValueError(f"target must be a whole number from {TARGET_MIN} to {TARGET_MAX}")
        return self.bridge.call("set_target", target, timeout=5)

    def set_enabled(self, enabled):
        if not isinstance(enabled, bool):
            raise ValueError("enabled must be true or false")
        return self.bridge.call("set_enabled", enabled, timeout=5)


def make_server(thermostat, port):
    class Handler(BaseHTTPRequestHandler):
        def do_GET(self):
            if self.path == "/health":
                self._send(200, "text/plain", b"ok")
            elif self.path == "/api/state":
                self._json(200, thermostat.snapshot())
            elif self.path == "/api/history":
                self._json(200, thermostat.history())
            elif self.path == "/":
                self._send(200, "text/html; charset=utf-8", PAGE.encode())
            else:
                self._send(404, "text/plain", b"not found")

        def do_POST(self):
            actions = {"/api/target": ("target", thermostat.set_target),
                       "/api/enabled": ("enabled", thermostat.set_enabled)}
            if self.path not in actions:
                self._send(404, "text/plain", b"not found")
                return
            key, action = actions[self.path]
            try:
                length = int(self.headers.get("Content-Length") or 0)
                value = json.loads(self.rfile.read(length) or b"{}").get(key)
                result = action(value)
            except (ValueError, AttributeError) as e:
                self._json(400, {"ok": False, "error": str(e)})
                return
            except Exception as e:  # a sketch that doesn't answer must not kill the page
                self._json(502, {"ok": False, "error": f"the sketch did not answer: {e}"})
                return
            self._json(200, {"ok": True, key: result})

        def _json(self, status, obj):
            self._send(status, "application/json", json.dumps(obj).encode())

        def _send(self, status, ctype, body):
            self.send_response(status)
            self.send_header("Content-Type", ctype)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, *args):
            pass

    return ThreadingHTTPServer(("0.0.0.0", port), Handler)


def main():
    from arduino.router_bridge import Bridge

    bridge = Bridge(os.environ["APP_SOCKET"])
    thermostat = Thermostat(bridge)
    if not bridge.connect(timeout=10):
        print("Arduino router not reachable yet; the bridge keeps retrying", flush=True)
    port = int(os.environ.get("PORT", "8000"))
    print(f"Serving on port {port}", flush=True)
    make_server(thermostat, port).serve_forever()


if __name__ == "__main__":
    main()
