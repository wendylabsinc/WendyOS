import json
import queue
import time
from urllib.request import ProxyHandler, build_opener

from zeroconf import IPVersion, ServiceBrowser, ServiceStateChange, Zeroconf
from network import interface_ipv4


def main():
    events = queue.Queue()

    def changed(zeroconf, service_type, name, state_change):
        if state_change in (ServiceStateChange.Added, ServiceStateChange.Updated):
            events.put((service_type, name))

    zc = Zeroconf(interfaces=[interface_ipv4()], ip_version=IPVersion.V4Only)
    browser = ServiceBrowser(zc, "_http._tcp.local.", handlers=[changed])
    deadline = time.monotonic() + 60
    # Use the discovered mesh endpoint directly, without an HTTP proxy.
    http = build_opener(ProxyHandler({}))
    try:
        while time.monotonic() < deadline:
            try:
                kind, name = events.get(timeout=1)
            except queue.Empty:
                continue
            info = zc.get_service_info(kind, name, timeout=2000)
            if not info or info.properties.get(b"demo") != b"learn-mesh":
                continue
            for address in info.parsed_addresses(IPVersion.V4Only):
                url = f"http://{address}:{info.port}/"
                try:
                    with http.open(url, timeout=5) as response:
                        data = json.load(response)
                    if data.get("status") != "ok" or not data.get("instance"):
                        raise ValueError("Unexpected demo response")
                    print(f"Resolved {name} -> {url}", flush=True)
                    print(json.dumps(data), flush=True)
                    return
                except (OSError, ValueError) as error:
                    print(f"Request failed: {error}", flush=True)
        raise SystemExit("No reachable demo service after 60 seconds. Check mesh peers and publisher logs; retry.")
    finally:
        browser.cancel()
        zc.close()


if __name__ == "__main__":
    main()
