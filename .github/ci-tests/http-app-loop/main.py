"""Small real service for the command-reference app lifecycle specs."""
import json
import os
import sys
import uuid
from http.server import BaseHTTPRequestHandler, HTTPServer

INSTANCE = str(uuid.uuid4())
VERSION = os.environ.get("E2E_HTTP_VERSION", "1.0.0")
REQUESTS = 0


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        global REQUESTS
        if self.path not in ("/ready", "/burst"):
            self.send_error(404)
            return
        REQUESTS += 1
        identity = (f"app={os.environ['WENDY_APP_ID']} instance={INSTANCE} "
                    f"version={VERSION} request={REQUESTS}")
        print(f"INFO e2e stdout {identity}", flush=True)
        print(f"ERROR e2e stderr {identity}", file=sys.stderr, flush=True)
        burst_bytes = 0
        if self.path == "/burst":
            # Bounded output exceeds a normal Linux pipe's capacity. Respond
            # only after both streams have accepted every byte.
            chunk = "x" * 32767 + "\n"
            for _ in range(64):
                sys.stdout.write(chunk)
                sys.stdout.flush()
                sys.stderr.write(chunk)
                sys.stderr.flush()
                burst_bytes += len(chunk)
        body = json.dumps({
            "app": os.environ["WENDY_APP_ID"],
            "instance": INSTANCE,
            "version": VERSION,
            "requests": REQUESTS,
            "burstBytesPerStream": burst_bytes,
        }).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *_):
        # Only the deliberate request markers enter the app's stderr stream.
        pass


server = HTTPServer(("0.0.0.0", int(os.environ["E2E_HTTP_PORT"])), Handler)
print(f"serving {os.environ['WENDY_APP_ID']} instance={INSTANCE}", flush=True)
server.serve_forever()
