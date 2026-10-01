#!/usr/bin/env python3
"""Wendy change detection sample. No model download is needed for demo mode."""

import argparse
import base64
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import io
import json
import logging
import os
from pathlib import Path
import threading
import time
import zipfile

import cv2

from change_detection.classifier import LABELS
from change_detection.pipeline import Pipeline
from change_detection.sources import CaptureSource, DemoSource, SCENARIOS

STATIC = Path(__file__).parent / "static"


def encode(image, extension=".jpg"):
    ok, buffer = cv2.imencode(extension, image)
    if not ok:
        raise ValueError("Could not encode frame")
    return buffer.tobytes()


def image_url(image, extension=".jpg"):
    mime = "png" if extension == ".png" else "jpeg"
    return f"data:image/{mime};base64," + base64.b64encode(encode(image, extension)).decode()


class Runtime:
    def __init__(self, source, pipeline=None, scene_id="session-1"):
        self.source = source
        self.pipeline = pipeline or Pipeline()
        self.scene_id = scene_id
        self.lock = threading.RLock()
        self.stop = threading.Event()
        self.paused = False
        self.error = None
        self.latest = None
        self.history = []
        self.thread = None

    def tick(self):
        with self.lock:
            frame, timestamp = self.source.read()
            result = self.pipeline.process(frame, timestamp)
            self.latest = result
            self.history.append({"time": timestamp, "coverage": result["coverage"],
                                 "liquid": max((r["scores"]["liquid"] for r in result["regions"]), default=0)})
            self.history = self.history[-120:]

    def run(self):
        while not self.stop.is_set():
            started = time.monotonic()
            try:
                with self.lock:
                    if not self.paused and not self.error:
                        self.tick()
            except Exception as error:
                logging.exception("Frame processing stopped")
                with self.lock:
                    self.error = str(error)
            self.stop.wait(max(0.01, self.source.interval - (time.monotonic() - started)))

    def start(self):
        self.tick()
        self.thread = threading.Thread(target=self.run, name="capture", daemon=True)
        self.thread.start()

    def close(self):
        self.stop.set()
        if self.thread:
            self.thread.join(timeout=3)
        if not self.thread or not self.thread.is_alive():
            self.source.close()

    def state(self):
        with self.lock:
            result = self.latest
            metadata = {"source": self.source.name, "demo": isinstance(self.source, DemoSource),
                        "scenario": getattr(self.source, "scenario", None), "paused": self.paused,
                        "error": self.error, "scene_id": self.scene_id, "labels": LABELS}
            if result is None:
                return {**metadata, "status": "starting", "regions": []}
            state = {key: value for key, value in result.items()
                     if key not in ("before", "current", "mask", "residual", "regions")}
            state.update(metadata)
            state["history"] = list(self.history)
            state["images"] = {key: image_url(result[key], ".png" if key == "mask" else ".jpg")
                               for key in ("before", "current", "mask", "residual")}
            state["regions"] = []
            for region in result["regions"]:
                item = {key: value for key, value in region.items() if key not in ("before", "after", "mask")}
                item["images"] = {key: image_url(region[key], ".png") for key in ("before", "after", "mask")}
                state["regions"].append(item)
            return state

    def control(self, action, body):
        with self.lock:
            if action == "pause":
                if not isinstance(body.get("paused"), bool):
                    raise ValueError("paused must be a boolean")
                self.paused = body["paused"]
                return
            if action == "demo":
                if not isinstance(self.source, DemoSource):
                    raise ValueError("Scenario selection is only available in demo mode")
                if body.get("scenario") not in SCENARIOS:
                    raise ValueError("Unknown demo scenario")
                self.source = DemoSource(body["scenario"])
            elif action == "reset":
                if isinstance(self.source, DemoSource):
                    self.source = DemoSource(self.source.scenario)
            else:
                raise ValueError("Unknown action")
            self.pipeline.reset()
            self.history.clear()
            self.error = None
            self.latest = None
            self.tick()

    def export(self, body):
        with self.lock:
            if body.get("label") not in LABELS:
                raise ValueError("Choose a supported label")
            if not self.latest:
                raise ValueError("No current frame")
            region = next((r for r in self.latest["regions"] if r["id"] == body.get("id")), None)
            if region is None:
                raise ValueError("Region is no longer visible; pause and select a current region")
            name = f"sample-{time.time_ns()}"
            row = {"scene_id": self.scene_id, "label": body["label"],
                   "features": region["features"], "synthetic": isinstance(self.source, DemoSource),
                   **{key: f"{name}/{key}.png" for key in ("before", "after", "mask")}}
            buffer = io.BytesIO()
            with zipfile.ZipFile(buffer, "w", zipfile.ZIP_DEFLATED) as archive:
                for key in ("before", "after", "mask"):
                    archive.writestr(f"{name}/{key}.png", encode(region[key], ".png"))
                archive.writestr(f"{name}/sample.json", json.dumps(row, indent=2))
            return buffer.getvalue(), name


def make_server(runtime, host="127.0.0.1", port=8000):
    class Handler(BaseHTTPRequestHandler):
        def reply(self, status, content_type, data, filename=None):
            self.send_response(status)
            self.send_header("Content-Type", content_type)
            self.send_header("Content-Length", str(len(data)))
            self.send_header("Cache-Control", "no-store")
            self.send_header("X-Content-Type-Options", "nosniff")
            if filename:
                self.send_header("Content-Disposition", f'attachment; filename="{filename}"')
            self.end_headers()
            self.wfile.write(data)

        def json(self, status, value):
            self.reply(status, "application/json", json.dumps(value, allow_nan=False).encode())

        def do_GET(self):
            if self.path == "/api/state":
                self.json(200, runtime.state())
            elif self.path == "/health":
                self.json(503 if runtime.error else 200, {"status": "error" if runtime.error else "ok",
                                                        "error": runtime.error})
            elif self.path in ("/", "/app.js", "/style.css"):
                name, mime = {"/": ("index.html", "text/html; charset=utf-8"),
                              "/app.js": ("app.js", "text/javascript"),
                              "/style.css": ("style.css", "text/css")}[self.path]
                self.reply(200, mime, (STATIC / name).read_bytes())
            else:
                self.json(404, {"error": "Not found"})

        def do_POST(self):
            if self.path not in ("/api/pause", "/api/reset", "/api/demo", "/api/export"):
                self.json(404, {"error": "Not found"})
                return
            # JSON-only same-origin controls prevent a web page elsewhere from
            # changing the shared camera reference with a cross-origin form.
            if self.headers.get("Content-Type", "").split(";")[0].strip() != "application/json":
                self.json(415, {"error": "Expected application/json"})
                return
            try:
                length = int(self.headers.get("Content-Length", "0"))
                if not 0 < length <= 4096:
                    raise ValueError("JSON body must contain 1 to 4096 bytes")
                body = json.loads(self.rfile.read(length))
                if not isinstance(body, dict):
                    raise ValueError("Expected a JSON object")
                if self.path == "/api/export":
                    data, name = runtime.export(body)
                    self.reply(200, "application/zip", data, f"{name}.zip")
                else:
                    runtime.control(self.path.rsplit("/", 1)[1], body)
                    self.json(200, {"ok": True})
            except (ValueError, TypeError, KeyError) as error:
                self.json(400, {"error": str(error)})
            except Exception as error:
                logging.exception("Control request failed")
                with runtime.lock:
                    runtime.error = str(error)
                self.json(500, {"error": "Input failed; check the app status"})

        def log_message(self, *_):
            pass

    return ThreadingHTTPServer((host, port), Handler)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source", default=os.environ.get("SOURCE", "demo"), help="demo, camera index, video file, or stream URL")
    parser.add_argument("--scenario", choices=SCENARIOS, default="liquid")
    parser.add_argument("--checkpoint", default=os.environ.get("CHECKPOINT"))
    parser.add_argument("--scene-id", default=os.environ.get("SCENE_ID", "session-1"))
    parser.add_argument("--fps", type=float, default=2)
    parser.add_argument("--host", default=os.environ.get("HOST", "127.0.0.1"))
    parser.add_argument("--port", type=int, default=int(os.environ.get("PORT", "8000")))
    args = parser.parse_args()
    cv2.setNumThreads(1)
    classifier = None
    if args.checkpoint:
        from change_detection.siamese import SiameseClassifier
        classifier = SiameseClassifier(args.checkpoint)
    source = DemoSource(args.scenario) if args.source == "demo" else CaptureSource(args.source, args.fps)
    runtime = Runtime(source, Pipeline(classifier), args.scene_id)
    server = make_server(runtime, args.host, args.port)
    try:
        runtime.start()
        print(f"Change detection ready at http://{args.host}:{server.server_port}", flush=True)
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()
        runtime.close()


if __name__ == "__main__":
    main()
