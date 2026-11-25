"""Three real World nodes and two native viewers; synthetic spatial fixtures.

All artifacts and disposable credentials stay in the caller's private output.
Native stages invoke ordinary application callbacks, not OS mouse input.
"""
import argparse
import hashlib
import http.client
import json
import os
from pathlib import Path
import signal
import subprocess
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


def sha(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def write(path, value):
    path.write_text(json.dumps(value, indent=2) + "\n")
    path.chmod(0o600)


def run(args):
    root = args.out.resolve()
    root.mkdir(parents=True, mode=0o700, exist_ok=False)
    site = root / "private-site"
    site.mkdir(mode=0o700)
    owned, logs, checks, nodes = [], [], [], {}
    receipt = {"scope": "Actual authenticated loopback World processes; two native viewers; explicitly synthetic spatial claims, not sensor measurements or physical gas inference", "checks": checks, "processes": []}
    hold_page, reached_page, release_page = threading.Event(), threading.Event(), threading.Event()
    requests = []

    def command(argv, good=True):
        p = subprocess.run([str(x) for x in argv], capture_output=True, timeout=35)
        if (p.returncode == 0) != good:
            raise AssertionError(f"{Path(str(argv[0])).name} rc={p.returncode}: {p.stderr.decode()[-800:]}")
        return json.loads(p.stdout) if p.stdout.strip().startswith((b"{", b"[")) else p.stdout.decode()

    def start(argv, label):
        f = (root / (label + ".log")).open("wb")
        logs.append(f)
        p = subprocess.Popen([str(x) for x in argv], stdout=f, stderr=subprocess.STDOUT)
        p.receipt = {"label": label, "pid": p.pid, "started_at": time.time(), "executable_sha256": sha(Path(argv[0]))}
        receipt["processes"].append(p.receipt)
        owned.append(p)
        return p

    def stop(p, kill=False):
        if "exited_at" in p.receipt:
            return
        if p.poll() is None:
            p.send_signal(signal.SIGKILL if kill else signal.SIGTERM)
            try:
                p.wait(timeout=5)
            except subprocess.TimeoutExpired:
                p.kill()
                p.wait(timeout=5)
        p.receipt.update(exited_at=time.time(), returncode=p.returncode, killed=kill)

    class Proxy(BaseHTTPRequestHandler):
        def do_GET(self): self.forward()
        def do_POST(self): self.forward()
        def log_message(self, *_): pass
        def forward(self):
            body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
            conn = http.client.HTTPConnection("127.0.0.1", args.port, timeout=15)
            headers = {k: v for k, v in self.headers.items() if k.lower() not in ("host", "connection", "content-length")}
            try:
                conn.request(self.command, self.path, body=body, headers=headers)
                res = conn.getresponse()
                raw, status = res.read(), res.status
                request = {"path": self.path, "status": status, "time": time.time()}
                if self.path == "/v1/query":
                    if status == 200:
                        saved = root / f"raw-area-response-{len(requests):03d}.json"
                        saved.write_bytes(raw); saved.chmod(0o600)
                    parsed = json.loads(body)
                    request.update(cursor=bool(parsed.get("cursor")), region=parsed.get("selection", {}).get("region"))
                    if parsed.get("cursor") and hold_page.is_set() and status == 200:
                        reached_page.set()
                        release_page.wait(20)
                requests.append(request)
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)
            except (BrokenPipeError, ConnectionResetError): pass
            except Exception: self.send_error(503)
            finally: conn.close()

    proxy = ThreadingHTTPServer(("127.0.0.1", args.port + 3), Proxy)
    proxy.daemon_threads = True
    threading.Thread(target=proxy.serve_forever, daemon=True).start()

    def node(i):
        p = start([args.worldd, "--config", site / f"node-{i}.json"], f"node-{i}-{len(owned)}")
        nodes[i] = p
        until = time.monotonic() + 8
        while time.monotonic() < until:
            try:
                conn = http.client.HTTPConnection("127.0.0.1", args.port + i - 1, timeout=.2)
                conn.request("GET", "/healthz")
                r = conn.getresponse()
                r.read()
                conn.close()
                if r.status == 200: return
            except OSError: pass
            if p.poll() is not None: raise AssertionError("node exited at startup")
            time.sleep(.05)
        raise AssertionError("node readiness timeout")

    def publish(rows, label):
        path = root / (label + ".records.json")
        write(path, rows)
        return command([args.publisher, "--credentials", site / "publisher.json", "--records", path])

    def item(name, x=None, z=None, uncertainty=.1, ttl=180000, invalid=False):
        payload = {"label": "Synthetic area fixture", "fixture": True, "position_m": [x, 0, z], "uncertainty_m": uncertainty}
        if invalid: payload["position_m"][0] = None
        return {"id": name, "payload": payload, "ttl_ms": ttl}

    def query(i=1, selection=None, extra=(), good=True):
        return command([args.world, "query", "--credentials", site / f"viewer-{i}.json", "--selection", selection or root / "selection.json", "--page-size", "128", *extra], good)

    def await_count(count, i=1):
        end = time.monotonic() + 10
        while time.monotonic() < end:
            result = query(i)
            if result["complete"] and len(result["components"]) == count: return result
            time.sleep(.1)
        raise AssertionError(f"reader{i}: wanted{count}, got {len(result['components'])}")

    def native(stage, label, viewer=1, wait=True):
        report = root / (label + ".json")
        p = start([args.app / "Contents/MacOS/Wendy World", "--profile", site / f"profile-{viewer}.json", "--storage-directory", root / f"viewer-{viewer}-storage", "--area-test-stage", stage, "--area-report", report], label)
        if not wait: return p
        end = time.monotonic() + 30
        while not report.exists() and time.monotonic() < end and p.poll() is None: time.sleep(.1)
        if not report.exists(): stop(p); raise AssertionError(f"native timeout {label}")
        value = json.loads(report.read_text())
        stop(p)
        assert value["error"] is None, value.get("error")
        assert Path(str(report) + ".png").exists(), "native window absent"
        return value

    try:
        command([args.lab, "--out", site, "--run-id", "area-workspaces", "--base-port", args.port, "--sensors"])
        for i in (1, 2, 3):
            path = site / f"node-{i}.json"
            cfg = json.loads(path.read_text())
            cfg["peers"] = [] if i == 3 else [{"url": f"http://127.0.0.1:{args.port+i}", "audience": f"node-{i+1}"}]
            write(path, cfg)
        write(site / "publisher.json", json.loads((site / "fusion.json").read_text())["publisher"])
        for i in (1, 2):
            config = json.loads((site / f"viewer-{i}.json").read_text())
            if i == 1: config["url"] = f"http://127.0.0.1:{args.port+3}"
            write(site / f"profile-{i}.json", {"credentials": config})
        selected = {"types": ["wendy.hazard.v1"], "region": {"frame": {"id": "office-floor", "revision": "office-frame-v1"}, "min_xz": [0, 0], "max_xz": [5, 5]}}
        write(root / "selection.json", selected)
        node(3); node(2); node(1)
        publish([item("west-pump", 1, 1), item("boundary-claim", 5.2, 2, .5), item("east-store", 9, 3), item("unlocated-source", 1, 2, invalid=True)], "initial")
        await_count(3)
        first = native("create", "01-area-drawn-and-saved")
        second = native("create", "02-independent-reader", 2)
        assert first["record_count"] == second["record_count"] == 3
        assert first["located_count"] == 2 and first["unlocated_count"] == 1
        checks.append("Two native viewers save exact-frame areas; uncertainty intersects boundary; unlocated is separate")
        stop(nodes[2])
        publish([item("west-pump", 9, 9), item("east-store", 3, 3)], "movement-during-partition")
        historical = native("reopen", "03-offline-reopen")
        assert historical["record_count"] == 3 and not historical["online_read"]
        await_count(3)
        node(2)
        end = time.monotonic() + 10
        while time.monotonic() < end:
            rows = await_count(3)
            names = [c["key"]["entity"] for c in rows["components"]]
            if "east-store" in names and "west-pump" not in names: break
            time.sleep(.1)
        else: raise AssertionError("movement not repaired")
        moved = native("refresh", "04-movement-after-reconnect")
        assert moved["record_count"] == 3 and moved["online_read"]
        checks.append("Partition preserves last known area; reconnect removes departed claim and adds arrived claim")
        publish([item(f"sample-{i:02d}", .4+(i%5)*.8, .5+(i//5)*.8) for i in range(18)], "many-pages")
        await_count(21)
        hold_page.set()
        interrupted = native("refresh", "05-crash-mid-refresh", wait=False)
        assert reached_page.wait(15), "second signed query page was not reached"
        stop(interrupted, True)
        hold_page.clear(); release_page.set()
        after_crash = native("reopen", "06-crash-reopened-complete-view")
        assert after_crash["record_count"] == 3, "partial pages replaced old complete view"
        assert after_crash["records"] == moved["records"], "crash changed exact prior components or spatial deadlines"
        complete = native("refresh", "07-all-pages-committed")
        assert complete["record_count"] == 21 and complete["online_read"]
        checks.append("App SIGKILL during blocked second page preserves exact previous complete snapshot; subsequent refresh commits21 records")
        stop(nodes[1], True); node(1)
        restarted = native("refresh", "08-node-restart")
        assert restarted["record_count"] == 21
        checks.append("Node SIGKILL/restart retains scoped World state and a fresh complete query succeeds")
        page = query(extra=("--page-size", "2"))
        assert not page["complete"]
        changed = json.loads(json.dumps(selected));changed["region"]["max_xz"] = [4, 5]
        write(root / "changed-selection.json", changed)
        query(selection=root / "changed-selection.json", extra=("--cursor", page["next_cursor"]), good=False)
        query(extra=("--cursor", page["next_cursor"]+"x"), good=False)
        checks.append("Actual CLI refuses changed-region and tampered continuation cursors")
        export = root / "complete-area.json"
        command([args.world, "area", "fetch", "--name", "West service area", "--selection", root / "selection.json", "--credentials", site / "viewer-1.json", "--out", export, "--page-size", "7"])
        before = sha(export)
        command([args.world, "area", "fetch", "--name", "West service area", "--selection", root / "selection.json", "--credentials", site / "viewer-1.json", "--out", export], good=False)
        assert sha(export) == before
        command([args.world, "area", "show", "--file", export])
        checks.append("CLI exports only complete pages, reopens historical snapshot and refuses overwrite")
        # Renew only one new short-lived row; its original deadline continues while offline.
        publish([item("short-window", 2, 2, ttl=5000)], "short-expiry")
        await_count(22)
        short = native("refresh", "09-original-short-deadline")
        assert short["record_count"] == 22
        stop(nodes[1])
        time.sleep(5.1)
        expired = native("reopen", "10-offline-expiry")
        assert expired["record_count"] == 22 and expired["expired_count"] >= 1 and not expired["online_read"]
        checks.append("Offline reopening keeps historical rows but original short deadline removes current-looking markers")
        node(1)
        alternate = json.loads(json.dumps(selected))
        alternate["region"]["frame"]["revision"] = "office-frame-v2"
        write(root / "other-frame.json", alternate)
        assert query(selection=root / "other-frame.json")["components"] == []
        query(selection=root / "other-frame.json", extra=("--cursor", page["next_cursor"]), good=False)
        checks.append("Exact changed-frame selection has no implicit reprojection and cannot reuse old cursor")
        large = item("oversized-claim", 1, 1)
        large["payload"]["fixture_padding"] = "x" * 9000
        publish([large], "oversized")
        await_count(22)
        limited = json.loads(json.dumps(selected)); limited["entities"] = ["oversized-claim"]
        write(root / "oversized-selection.json", limited)
        query(selection=root / "oversized-selection.json", extra=("--max-bytes", "4096"), good=False)
        checks.append("An individual record above requested encoded page budget is refused without truncation")
        viewer = json.loads((site / "viewer-1.json").read_text())
        rev = command([args.publisher, "--issuer", site / "issuer.json", "--revoke-id", viewer["grant"]["grant"]["id"]])
        stop(nodes[1]); config = json.loads((site / "node-1.json").read_text()); config["revocations"] = rev; write(site / "node-1.json", config); node(1)
        query(good=False)
        denied = native("refresh", "11-revoked-reader-keeps-history")
        assert not denied["online_read"] and denied["record_count"] == 22 and "Previous complete snapshot retained" in denied["notice"]
        checks.append("Revoked reader cannot refresh; previously saved complete view stays explicitly historical")
        receipt["passed"] = True
    except Exception as error:
        receipt["passed"] = False
        receipt["error"] = str(error)
        raise
    finally:
        release_page.set()
        for p in reversed(owned): stop(p)
        proxy.shutdown(); proxy.server_close()
        for f in logs: f.close()
        receipt["requests"] = requests
        receipt["executables"] = {str(p): sha(p) for p in (args.world, args.worldd, args.lab, args.publisher, args.app / "Contents/MacOS/Wendy World")}
        receipt["driver_sha256"] = sha(Path(__file__))
        receipt["finished_at"] = time.time()
        write(root / "verification.json", receipt)


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    for name in ("world", "worldd", "lab", "publisher", "app", "out"):
        parser.add_argument("--" + name, type=Path, required=True)
    parser.add_argument("--port", type=int, default=19860)
    run(parser.parse_args())
