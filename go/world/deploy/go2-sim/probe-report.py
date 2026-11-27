"""Expose only the zero-command probe report, not a file browser or actuator."""
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path
import sys

MODE = sys.argv[1] if len(sys.argv) == 2 else 'commands'
PATH, PORT = {'commands': ('zero-probe.json', 18884),
              'sensors': ('sensor-probe.json', 18887)}[MODE]
REPORT = Path('/tmp', PATH).read_bytes()
print(REPORT.decode(), flush=True)

class Handler(BaseHTTPRequestHandler):
    def log_message(self, *_args):
        pass

    def do_GET(self):
        if self.path != '/' + PATH:
            self.send_error(404)
            return
        self.send_response(200)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(REPORT)))
        self.end_headers()
        self.wfile.write(REPORT)

HTTPServer(('0.0.0.0', PORT), Handler).serve_forever()
