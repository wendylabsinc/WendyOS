import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlsplit


def serve(message: str, port: int) -> None:
    class Handler(BaseHTTPRequestHandler):
        def do_GET(self) -> None:
            path = urlsplit(self.path).path
            if path == "/":
                status, payload = 200, {"message": message}
            elif path == "/health":
                status, payload = 200, {"status": "ok"}
            else:
                status, payload = 404, {"error": "not found"}

            body = json.dumps(payload).encode("utf-8")
            self.send_response(status)
            self.send_header("Content-Type", "application/json; charset=utf-8")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

    with ThreadingHTTPServer(("0.0.0.0", port), Handler) as server:
        print(f"Mojo-configured HTTP server listening on port {port}", flush=True)
        server.serve_forever()
