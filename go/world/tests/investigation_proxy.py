"""Loopback-only acceptance fault proxy; never records credentials or request headers."""
import argparse
import hashlib
import http.client
import http.server
import json
from pathlib import Path
import socket
import threading
import time


def serve(port, upstream, directory):
    root = Path(directory)
    class Handler(http.server.BaseHTTPRequestHandler):
        def do_POST(self):
            mode = json.loads((root / 'mode.json').read_text()) if (root / 'mode.json').exists() else {}
            if mode.get('partition'):
                self.send_error(503, 'simulated partition')
                return
            length = int(self.headers.get('Content-Length', 0))
            if length > 131072:
                self.send_error(413)
                return
            body = self.rfile.read(length)
            conn = http.client.HTTPConnection('127.0.0.1', upstream, timeout=4)
            conn.request('POST', self.path, body, {'Content-Type': 'application/json', 'X-World-Authorization': self.headers.get('X-World-Authorization', '')})
            response = conn.getresponse()
            data = response.read(131073)
            event = {'path': self.path, 'body_sha256': hashlib.sha256(body).hexdigest(), 'status': response.status}
            if self.path in ('/v1/evidence/pin', '/v1/evidence/unpin'):
                event['body'] = json.loads(body)
            with (root / 'events.jsonl').open('a') as log:
                log.write(json.dumps(event, sort_keys=True) + '\n')
            conn.close()
            drop = mode.get('drop_pin') and self.path.endswith('/pin') or mode.get('drop_unpin') and self.path.endswith('/unpin')
            if drop:
                (root / 'delivered.json').write_text(json.dumps(event))
                if mode.get('wait_after_delivery'):
                    threading.Event().wait(8)
                self.connection.shutdown(socket.SHUT_RDWR)
                self.connection.close()
                return
            if self.path == '/v1/evidence/pin' and response.status == 200:
                if mode.get('changed_digest') or mode.get('changed_deadline'):
                    value=json.loads(data)
                    if mode.get('changed_digest'):value['sha256']=['0'*64]
                    if mode.get('changed_deadline'):value['retain_until_unix_ms']+=1000
                    data=json.dumps(value).encode()
                if mode.get('delay_response'):time.sleep(.5)
            self.send_response(response.status)
            self.send_header('Content-Type', 'application/json')
            self.send_header('Content-Length', str(len(data)))
            self.end_headers()
            self.wfile.write(data)
        def log_message(self, *_):
            pass
    http.server.ThreadingHTTPServer(('127.0.0.1', port), Handler).serve_forever()

if __name__ == '__main__':
    p = argparse.ArgumentParser()
    p.add_argument('--port', type=int, required=True)
    p.add_argument('--upstream', type=int, required=True)
    p.add_argument('--directory', required=True)
    a = p.parse_args()
    Path(a.directory).mkdir(mode=0o700, parents=True, exist_ok=True)
    serve(a.port, a.upstream, a.directory)
