"""Tiny real HTTP app for the Apps & Deployments Swift E2E draft.

VERSION is baked into the image, never taken from deployment configuration.
Every other response field is nonsecret fixture configuration.
"""

import json
import os
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path

VERSION = Path("/app/version.txt").read_text().strip()
# Benign per-device failure for the mixed fleet outcome. Linux boot id is a
# public, read-only kernel value shared with its containers. The same release
# can therefore fail only on the explicitly selected dedicated target.
fail_on_boot = os.environ.get("E2E_FAIL_ON_BOOT_ID", "")
if fail_on_boot and Path("/proc/sys/kernel/random/boot_id").read_text().strip() == fail_on_boot:
    raise SystemExit("Intentional E2E startup failure on the declared target")
if VERSION == "3.0.0":
    raise SystemExit("Intentional E2E startup failure")


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        payload = json.dumps(
            {
                "app": os.environ["E2E_APP_ID"],
                "attempt": os.environ["E2E_ATTEMPT"],
                "version": VERSION,
                "unit": os.environ["DISPLAY_UNIT"],
                "fileOnly": os.environ["FILE_ONLY"],
            },
            sort_keys=True,
        ).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)


HTTPServer(("0.0.0.0", int(os.environ["E2E_HTTP_PORT"])), Handler).serve_forever()
