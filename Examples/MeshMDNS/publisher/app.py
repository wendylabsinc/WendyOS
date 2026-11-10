import json
import signal
import socket
import threading
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from zeroconf import IPVersion, ServiceInfo, Zeroconf
from network import interface_ipv4

instance = "Workshop API " + uuid.uuid4().hex[:8]


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        body = json.dumps({"instance": instance, "status": "ok"}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


def main():
    address = interface_ipv4()
    stopped = threading.Event()
    for sig in (signal.SIGINT, signal.SIGTERM):
        signal.signal(sig, lambda *_: stopped.set())
    server = ThreadingHTTPServer(("0.0.0.0", 8080), Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    zc = Zeroconf(interfaces=[address], ip_version=IPVersion.V4Only)
    info = ServiceInfo(
        "_http._tcp.local.",
        f"{instance}._http._tcp.local.",
        addresses=[socket.inet_aton(address)],
        port=8080,
        properties={"demo": "learn-mesh", "path": "/"},
        server=f"workshop-{uuid.uuid4().hex[:8]}.local.",
    )
    registered = False
    try:
        zc.register_service(info, allow_name_change=True)
        registered = True
        print(f"Advertising {info.name} at {address}:8080", flush=True)
        stopped.wait()
    finally:
        if registered:
            zc.unregister_service(info)
        zc.close()
        server.shutdown()
        server.server_close()
        thread.join()


if __name__ == "__main__":
    main()
