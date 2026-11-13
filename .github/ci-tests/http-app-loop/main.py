"""Small real service for the command-reference app lifecycle specs."""
import json
import os
import uuid
from http.server import BaseHTTPRequestHandler, HTTPServer

INSTANCE = str(uuid.uuid4())


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path != "/ready":
            self.send_error(404)
            return
        body = json.dumps({
            "app": os.environ["WENDY_APP_ID"],
            "instance": INSTANCE,
            "version": "1.0.0",
        }).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


server = HTTPServer(("0.0.0.0", int(os.environ["E2E_HTTP_PORT"])), Handler)
print(f"serving {os.environ['WENDY_APP_ID']} instance={INSTANCE}", flush=True)
server.serve_forever()
