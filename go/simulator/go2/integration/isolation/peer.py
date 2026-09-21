"""Local packet endpoints and probes for the disposable Docker isolation test."""

import argparse
import hashlib
import http.server
import json
import socket
import subprocess
import threading
import urllib.request

UDP_PORTS = (28901, 7417, 55213)
HTTP_PORT = 28902
RECEIVED = {}
RECEIVED_LOCK = threading.Lock()


def packet_key(port, payload):
    return f"{port}:{hashlib.sha256(payload).hexdigest()}"


def udp_server(family, host, port):
    sock = socket.socket(family, socket.SOCK_DGRAM)
    if family == socket.AF_INET6:
        sock.setsockopt(socket.IPPROTO_IPV6, socket.IPV6_V6ONLY, 1)
    sock.bind((host, port))

    def receive():
        while True:
            data, address = sock.recvfrom(65535)
            with RECEIVED_LOCK:
                key = packet_key(port, data)
                RECEIVED[key] = RECEIVED.get(key, 0) + 1
            # The legacy matcher searches the whole packet, so echoing RTPS
            # would also block the return path and hide an input-filter bug.
            sock.sendto(hashlib.sha256(data).hexdigest().encode(), address)

    threading.Thread(target=receive, daemon=True).start()


class HTTPHandler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        with RECEIVED_LOCK:
            value = {"received": dict(RECEIVED)} if self.path == "/stats" else {"packet_test": "ok"}
        data = json.dumps(value).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def log_message(self, *_args):
        pass


class IPv6HTTPServer(http.server.ThreadingHTTPServer):
    address_family = socket.AF_INET6

    def server_bind(self):
        self.socket.setsockopt(socket.IPPROTO_IPV6, socket.IPV6_V6ONLY, 1)
        super().server_bind()


def serve():
    for port in UDP_PORTS:
        udp_server(socket.AF_INET, "0.0.0.0", port)
        udp_server(socket.AF_INET6, "::", port)
    for server in (http.server.ThreadingHTTPServer(("0.0.0.0", HTTP_PORT), HTTPHandler),
                   IPv6HTTPServer(("::", HTTP_PORT), HTTPHandler)):
        threading.Thread(target=server.serve_forever, daemon=True).start()
    print(json.dumps({"ready": True}), flush=True)
    threading.Event().wait()


def probe_udp(host, payload, port):
    family = socket.AF_INET6 if ":" in host else socket.AF_INET
    with socket.socket(family, socket.SOCK_DGRAM) as sock:
        sock.settimeout(0.75)
        try:
            sock.sendto(payload, (host, port))
            response, _ = sock.recvfrom(65535)
        except TimeoutError:
            return {"delivered": False, "reason": "timeout"}
        except PermissionError:
            # Linux can report a local output-hook drop directly to sendto.
            # The driver also requires the actual nft drop counter to advance.
            return {"delivered": False, "reason": "local_output_denied"}
        if response != hashlib.sha256(payload).hexdigest().encode():
            raise RuntimeError("Unexpected UDP response")
        return {"delivered": True}


def probe_http(host, stats=False):
    address = "[" + host + "]" if ":" in host else host
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    with opener.open(f"http://{address}:{HTTP_PORT}/" + ("stats" if stats else ""), timeout=2) as response:
        value = json.load(response)
        if stats:
            return value
        if response.status != 200 or value != {"packet_test": "ok"}:
            raise RuntimeError("Unexpected HTTP response")
        return {"delivered": True}


def permissions(backend):
    with open("/proc/self/status") as stream:
        fields = dict(line.strip().split(":", 1) for line in stream if ":" in line)
    caps = {name: int(fields[name].strip(), 16)
            for name in ("CapEff", "CapPrm", "CapInh", "CapBnd", "CapAmb")}
    net_admin = 1 << 12
    if any(value & net_admin for value in caps.values()):
        raise RuntimeError("Runtime retained CAP_NET_ADMIN: " + repr(caps))
    commands = [["nft", "add", "table", "inet", "go2_unexpected_permission"]]
    if backend == "legacy":
        commands = [[binary, "--table", "filter", "--new-chain", "GO2_UNEXPECTED_PERMISSION"]
                    for binary in ("iptables-legacy", "ip6tables-legacy")]
    denied = []
    for command in commands:
        result = subprocess.run(command, capture_output=True, text=True, timeout=5)
        if result.returncode == 0 or not any(message in result.stderr.lower()
                                              for message in ("operation not permitted", "permission denied")):
            raise RuntimeError("Dropped runtime firewall write was not permission-denied: " + result.stderr)
        denied.append(command[0])
    return {"net_admin_dropped": True, "capabilities": caps, "denied_writers": denied}


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("serve", "udp", "http", "stats", "permissions"))
    parser.add_argument("host", nargs="?")
    parser.add_argument("payload", nargs="?")
    parser.add_argument("--port", type=int, default=UDP_PORTS[0])
    parser.add_argument("--backend", choices=("nft", "legacy"), default="nft")
    args = parser.parse_args()
    if args.action == "serve":
        serve()
    elif args.action == "udp":
        print(json.dumps(probe_udp(args.host, bytes.fromhex(args.payload), args.port)))
    elif args.action == "http":
        print(json.dumps(probe_http(args.host)))
    elif args.action == "stats":
        print(json.dumps(probe_http(args.host, stats=True)))
    else:
        print(json.dumps(permissions(args.backend)))
