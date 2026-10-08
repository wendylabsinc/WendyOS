import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
import threading
import time
import urllib.parse

from telemetry import emit

settings = json.loads(Path("settings.json").read_text())


def compute(value, challenge):
    started = time.time_ns()
    error = ""
    result = None
    try:
        # The fixture supplies the expected calibration and response-time limit.
        result = value * value / settings["calibration_divisor"]
        time.sleep(settings["worker_delay_ms"] / 1000)
    except Exception as exc:
        error = str(exc)
    ended = time.time_ns()
    emit(settings, challenge, started, ended, error)
    return (500 if error else 200), {"result": result, "challenge": challenge, "nonce": settings["nonce"]}


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/health":
            status, body = 200, {k: settings[k] for k in ("app_id", "version", "nonce")}
        else:
            query = urllib.parse.parse_qs(urllib.parse.urlsplit(self.path).query)
            status, body = compute(int(query["value"][0]), query["challenge"][0])
        payload = json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)


def background():
    while True:
        try:
            compute(3, "background")
        except Exception as exc:
            print("OTLP export failed:", exc, flush=True)
        time.sleep(1)


threading.Thread(target=background, daemon=True).start()
ThreadingHTTPServer(("0.0.0.0", settings["port"]), Handler).serve_forever()
