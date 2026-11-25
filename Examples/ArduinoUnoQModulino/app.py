"""Modulino demo, Linux half: the UNO Q's sketch reads the Knob and Thermo and
drives the Buzzer; this app shows the readings and beeps through Arduino's Bridge."""
import json
import math
import os
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PAGE = """<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>UNO Q Modulino</title>
<style>
  body { font-family: system-ui, sans-serif; margin: 2rem; max-width: 28rem; }
  dl { display: grid; grid-template-columns: max-content 1fr; gap: .5rem 1rem; }
  dt { color: #666; } dd { margin: 0; font-size: 1.5rem; }
  button { font-size: 1.1rem; padding: .5rem 1.2rem; }
</style></head>
<body>
<h1>UNO Q Modulino</h1>
<dl>
  <dt>Temperature</dt><dd id="celsius">-</dd>
  <dt>Humidity</dt><dd id="humidity">-</dd>
  <dt>Knob</dt><dd id="knob">-</dd>
</dl>
<button id="beep">Beep</button> <span id="status"></span>
<script>
const fmt = (v, unit) => v === null ? "-" : v + unit;
async function refresh() {
  try {
    const s = await (await fetch("/api/state")).json();
    document.getElementById("celsius").textContent = fmt(s.celsius === null ? null : s.celsius.toFixed(1), " °C");
    document.getElementById("humidity").textContent = fmt(s.humidity === null ? null : s.humidity.toFixed(0), " %");
    document.getElementById("knob").textContent = s.knob === null ? "-" : s.knob + (s.pressed ? " (pressed)" : "");
  } catch (e) {}
}
document.getElementById("beep").onclick = async () => {
  const r = await (await fetch("/api/buzz", {method: "POST"})).json();
  document.getElementById("status").textContent = r.ok ? "" : r.error;
};
setInterval(refresh, 1000); refresh();
</script>
</body></html>
"""


def _number(value):
    return None if value is None or math.isnan(value) else value


class Readings:
    """Latest values the sketch sent; None until the first notification."""

    def __init__(self):
        self._lock = threading.Lock()
        self._data = {"knob": None, "pressed": None, "celsius": None, "humidity": None}

    def knob(self, value, pressed):
        with self._lock:
            self._data.update(knob=value, pressed=bool(pressed))

    def climate(self, celsius, humidity):
        # A missing Thermo reads as NaN, which JSON (and so the page) can't carry.
        with self._lock:
            self._data.update(celsius=_number(celsius), humidity=_number(humidity))

    def snapshot(self):
        with self._lock:
            return dict(self._data)


class Demo:
    def __init__(self, bridge):
        self.bridge = bridge
        self.readings = Readings()
        # The sketch notifies these; the bridge re-registers them on reconnect.
        bridge.provide("knob", self.readings.knob)
        bridge.provide("climate", self.readings.climate)

    def buzz(self, freq=880, ms=200):
        self.bridge.call("buzz", freq, ms, timeout=5)


def make_server(demo, port):
    class Handler(BaseHTTPRequestHandler):
        def do_GET(self):
            if self.path == "/health":
                self._send(200, "text/plain", b"ok")
            elif self.path == "/api/state":
                self._json(200, demo.readings.snapshot())
            elif self.path == "/":
                self._send(200, "text/html; charset=utf-8", PAGE.encode())
            else:
                self._send(404, "text/plain", b"not found")

        def do_POST(self):
            if self.path != "/api/buzz":
                self._send(404, "text/plain", b"not found")
                return
            try:
                demo.buzz()
            except Exception as e:  # a sketch that doesn't answer must not kill the page
                self._json(502, {"ok": False, "error": str(e)})
                return
            self._json(200, {"ok": True})

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
    demo = Demo(bridge)
    if not bridge.connect(timeout=10):
        print("Arduino router not reachable yet; the bridge keeps retrying", flush=True)
    port = int(os.environ.get("PORT", "8000"))
    print(f"Serving on port {port}", flush=True)
    make_server(demo, port).serve_forever()


if __name__ == "__main__":
    main()
