#!/usr/bin/env python3
"""Simple HTTP API service for HelloCompose example."""

from http.server import BaseHTTPRequestHandler, HTTPServer
import json
import platform
import sys

class Handler(BaseHTTPRequestHandler):
    def log_message(self, format, *args):
        print(f"[api] {format % args}", flush=True)

    def do_GET(self):
        body = json.dumps({
            "message": "Hello from the API!",
            "python": sys.version,
            "machine": platform.machine(),
        }).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

print(f"[api] Starting on port 8080 (Python {sys.version.split()[0]})", flush=True)
HTTPServer(("", 8080), Handler).serve_forever()
