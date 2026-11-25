"""Fixture-only read relay to one already running MuJoCo source.

This is not World transport. It lets B's ordinary camera/audio capture apps and source
clock read A's single simulation incarnation before B signs that observation.
"""
import argparse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import ipaddress
from urllib.error import HTTPError
from urllib.parse import urlsplit
from urllib.request import Request, build_opener, ProxyHandler, HTTPRedirectHandler

PATHS = ('/v1/identity', '/v1/clock', '/v1/captures/camera-b', '/v1/captures/audio-1')
LIMIT = 4 << 20


class NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None


def server(bind, port, origin):
    ipaddress.IPv4Address(bind)
    target = urlsplit(origin)
    ipaddress.IPv4Address(target.hostname)
    assert target.scheme == 'http' and target.port == 8091 and not any(
        (target.path, target.query, target.fragment, target.username, target.password))
    assert type(port) is int and 0 <= port <= 65535

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def do_GET(self):
            if self.path not in PATHS:
                self.send_error(404)
                return
            headers = {key: self.headers[key] for key in ('Authorization', 'X-Wendy-Clock-Nonce')
                       if key in self.headers}
            request = Request(origin + self.path, headers=headers)
            opener = build_opener(ProxyHandler({}), NoRedirect())
            try:
                try:
                    response = opener.open(request, timeout=3)
                except HTTPError as error:
                    response = error
                with response:
                    raw = response.read(LIMIT + 1)
                    if len(raw) > LIMIT:
                        raise ValueError('source response limit')
                    code = response.code
                self.send_response(code)
                self.send_header('Content-Type', 'application/json')
                self.send_header('Content-Length', str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)
            except (OSError, ValueError):
                self.send_error(502)

    result = ThreadingHTTPServer((bind, port), Handler)
    result.daemon_threads = True
    return result


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--bind', required=True)
    parser.add_argument('--port', type=int, default=8092)
    parser.add_argument('--origin', required=True)
    args = parser.parse_args()
    server(args.bind, args.port, args.origin).serve_forever(poll_interval=.2)
