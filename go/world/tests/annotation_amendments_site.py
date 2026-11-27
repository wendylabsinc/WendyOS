"""Actual three-node and two-native-view annotation amendment acceptance.

Uses explicit private artifacts and built executables. Fault proxies only delay
acknowledgements or omit selected repair records; they never forge origin proofs.
"""
import argparse
import base64
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


def digest(raw):
    return hashlib.sha256(raw).hexdigest()


def write(path, value):
    path.write_text(json.dumps(value, indent=2) + "\n")
    path.chmod(0o600)


def run(args):
    root = args.out.resolve()
    root.mkdir(mode=0o700, parents=True, exist_ok=False)
    site = root / "private-site"
    site.mkdir(mode=0o700)
    checks, processes, nodes, streams, owned_processes = [], [], {}, [], []
    receipts = {"schema": "wendy.annotation-amendments.actual.v1", "scope": "Loopback simulation credentials, actual World processes and scripted native application callbacks; no OS mouse input or hardware", "checks": checks, "processes": processes}
    hold = threading.Event()
    delivered = threading.Event()
    release = threading.Event()
    only_withdrawal = threading.Event()
    observed_posts = []

    def command(argv, expected=0):
        # The actual gateway permits at most one clock exchange per50ms.
        # Space scripted CLI actions; do not relax the product limit.
        time.sleep(.08)
        p = subprocess.run([str(x) for x in argv], capture_output=True, timeout=25)
        if p.returncode != expected:
            raise AssertionError(f"command {Path(str(argv[0])).name} failed: {p.stderr.decode()[-1500:]}")
        return json.loads(p.stdout) if p.stdout.strip().startswith((b"{", b"[")) else p.stdout.decode()

    def start(argv, label):
        log = (root / (label + ".log")).open("wb")
        streams.append(log)
        p = subprocess.Popen([str(x) for x in argv], stdout=log, stderr=subprocess.STDOUT)
        record = {"label": label, "pid": p.pid, "started_at": time.time(), "executable_sha256": digest(Path(argv[0]).read_bytes())}
        processes.append(record)
        p.receipt = record
        owned_processes.append(p)
        return p

    def stop(p, killed=False):
        if "exited_at" in p.receipt: return
        if p.poll() is None:
            p.send_signal(signal.SIGKILL if killed else signal.SIGTERM)
            try:
                p.wait(timeout=5)
            except subprocess.TimeoutExpired:
                p.kill(); p.wait(timeout=5)
        p.receipt.update(exited_at=time.time(), returncode=p.returncode, killed=killed)

    class Proxy(BaseHTTPRequestHandler):
        def do_GET(self): self.forward()
        def do_POST(self): self.forward()
        def log_message(self, *_): pass
        def forward(self):
            body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
            conn = http.client.HTTPConnection("127.0.0.1", self.server.target, timeout=10)
            headers = {k:v for k,v in self.headers.items() if k.lower() not in ("host", "connection", "content-length")}
            try:
                conn.request(self.command, self.path, body=body, headers=headers)
                r = conn.getresponse(); raw = r.read(); status = r.status
                if self.server.kind == "author" and self.path == "/v1/annotations":
                    observed_posts.append({"sha256": digest(body), "status": status, "at": time.time()})
                    if status == 200 and hold.is_set():
                        delivered.set(); release.wait(15)
                if self.server.kind == "repair" and self.path == "/v1/repair/fetch" and status == 200 and only_withdrawal.is_set():
                    obj = json.loads(raw); kept = []
                    for rec in obj["records"]:
                        c = rec["record"]["component"]
                        if c["key"]["type"] == "wendy.annotation.amendment.v1" and c["payload"]["action"] == "withdraw":
                            kept.append(rec)
                        else:
                            obj["unavailable"].append(digest(json.dumps(rec["entry"]["key"], separators=(",", ":")).encode()))
                    obj["records"] = kept
                    raw = json.dumps(obj, separators=(",", ":")).encode()
                self.send_response(status); self.send_header("Content-Type", "application/json"); self.send_header("Content-Length", str(len(raw))); self.end_headers(); self.wfile.write(raw)
            except (BrokenPipeError, ConnectionResetError): pass
            except Exception:
                self.send_error(503)
            finally: conn.close()

    proxies = []
    def proxy(port, target, kind):
        server = ThreadingHTTPServer(("127.0.0.1", port), Proxy)
        server.daemon_threads = True
        server.target, server.kind = target, kind
        threading.Thread(target=server.serve_forever, daemon=True).start()
        proxies.append(server)

    def node(i):
        p = start([args.worldd, "--config", site / f"node-{i}.json"], f"node-{i}-{len(processes)}")
        nodes[i] = p
        until = time.monotonic() + 8
        while time.monotonic() < until:
            try:
                conn = http.client.HTTPConnection("127.0.0.1", args.port+i-1, timeout=.2)
                conn.request("GET", "/healthz"); response=conn.getresponse(); response.read(); conn.close()
                if response.status == 200: return
            except OSError: pass
            if p.poll() is not None: raise AssertionError("node exited during startup")
            time.sleep(.05)
        raise AssertionError("node did not become ready")

    def listing(i):
        return command([args.world, "annotation", "list", "--credentials", site / (f"viewer-{i}.json" if i != 3 else "contributor-macos.json")])

    def await_state(i, state):
        end = time.monotonic() + 10
        while time.monotonic() < end:
            value = listing(i)
            rows = value if isinstance(value, list) else value.get("views", [])
            if any(r["state"] == state for r in rows): return rows
            time.sleep(.1)
        raise AssertionError(f"node {i} did not reach {state}: {value}")

    def native(stage, label, viewer=False, wait=True):
        report=root/(label+".json")
        profile=site/("reader-profile.json" if viewer else "author-profile.json")
        storage=root/("reader-storage" if viewer else "author-storage")
        p=start([args.app / "Contents/MacOS/Wendy World", "--profile", profile, "--storage-directory", storage, "--amendment-test-stage", stage, "--amendment-report", report],label)
        if not wait: return p
        until=time.monotonic()+25
        while not report.exists() and time.monotonic()<until and p.poll() is None: time.sleep(.1)
        if not report.exists(): stop(p); raise AssertionError(f"native report timeout: {label}")
        result=json.loads(report.read_text()); stop(p)
        assert result["error"] is None, {k:result.get(k) for k in ("error","notice","offline","connection_state")}
        assert Path(str(report)+".png").exists(), "native window was not captured"
        return result

    try:
        command([args.lab, "--out", site, "--run-id", "annotation-amendments", "--base-port", args.port, "--sensors"])
        for i in (1,2,3):
            p=site/f"node-{i}.json"; cfg=json.loads(p.read_text())
            cfg["peers"] = ([] if i==3 else [{"url":f"http://127.0.0.1:{args.port+4 if i==1 else args.port+2}","audience":f"node-{i+1}"}])
            write(p,cfg)
        author=json.loads((site/"contributor-macos.json").read_text())
        author["url"]=f"http://127.0.0.1:{args.port+3}"
        write(site/"author-profile.json", {"credentials":author,"annotation_context":{"run_id":"annotation-amendments","frame":{"id":"office-floor","revision":"office-frame-v1"}}})
        write(site/"reader-profile.json", {"credentials":json.loads((site/"viewer-2.json").read_text())})
        proxy(args.port+3,args.port+2,"author"); proxy(args.port+4,args.port+1,"repair")
        node(3); node(2)
        created=native("create","01-original")
        assert created["originals"][0]["state"]=="published",created
        original=base64.b64decode(created["originals"][0]["signedRecord"])
        (root/"original.signed.json").write_bytes(original); (root/"original.signed.json").chmod(0o600)
        await_state(2,"original"); native("view","02-reader-original",True)
        checks.append("Original native signed note reaches independent reader")
        stop(nodes[2]); prepared=native("correct","03-correction-saved")
        body=base64.b64decode(prepared["amendments"][0]["signedRecord"]); signed_hash=digest(body)
        hold.set(); p=native("send","04-lost-ack",wait=False)
        assert delivered.wait(15),"source never accepted correction"
        stop(p,True); hold.clear(); release.set()
        reopened=native("reopen","05-crash-reopen")
        assert reopened["amendments"][0]["state"]=="pending"
        assert digest(base64.b64decode(reopened["amendments"][0]["signedRecord"]))==signed_hash
        stop(nodes[3],True); node(3)
        retried=native("send","06-exact-retry")
        assert retried["amendments"][0]["state"]=="published",retried
        assert digest(base64.b64decode(retried["amendments"][0]["signedRecord"]))==signed_hash
        assert len([x for x in observed_posts if x["sha256"]==signed_hash and x["status"]==200])==2
        checks.append("Partition, lost acknowledgement, native SIGKILL and source SIGKILL retain identical signed retry")
        node(2); await_state(2,"corrected"); native("view","07-reader-corrected",True)
        checks.append("Reader repairs corrected text after partition without editing original signed bytes")
        denied=root/"other-author.signed.json"
        command([args.world,"annotation","amend","--credentials",site/"contributor-apple.json","--original",root/"original.signed.json","--text","Unauthorized author","--out",denied],expected=1)
        assert not denied.exists()
        command([args.world,"annotation","amend","--credentials",site/"viewer-2.json","--original",root/"original.signed.json","--text","Reader cannot write","--out",denied],expected=1)
        assert not denied.exists()
        checks.append("Different author/key and read-only profile cannot prepare this author's amendment")
        tampered=json.loads(body)
        signature=bytearray(base64.b64decode(tampered["signature"]));signature[0]^=1
        tampered["signature"]=base64.b64encode(signature).decode()
        invalid=root/"tampered.signed.json";write(invalid,tampered)
        command([args.world,"annotation","send","--credentials",site/"contributor-macos.json","--record",invalid],expected=1)
        assert any(r["state"]=="corrected" for r in listing(3))
        checks.append("Changed signed envelope is rejected without replacing the accepted correction")
        fork=root/"fork.signed.json"
        command([args.world,"annotation","amend","--credentials",site/"contributor-macos.json","--original",root/"original.signed.json","--text","Independent correction: south entry","--out",fork])
        command([args.world,"annotation","send","--credentials",site/"contributor-macos.json","--record",fork])
        await_state(2,"conflict"); native("view","08-reader-conflict",True)
        checks.append("Concurrent signed root corrections remain an explicit conflict")
        native("withdraw","09-withdrawal-saved"); sent=native("send","10-withdrawal-sent")
        assert sent["amendments"][-1]["state"]=="published",sent
        await_state(2,"withdrawn"); native("view","11-reader-withdrawn",True)
        only_withdrawal.set(); node(1); await_state(1,"withdrawn")
        proof=json.loads((site/"node-1.proofs").read_text())
        assert len(proof["records"])==1 and proof["records"][0]["component"]["payload"]["action"]=="withdraw",proof
        checks.append("Third node receives terminal withdrawal before original or corrections")
        only_withdrawal.clear()
        end=time.monotonic()+10
        while time.monotonic()<end:
            proof=json.loads((site/"node-1.proofs").read_text())
            if len(proof["records"])>=4: break
            time.sleep(.1)
        assert len(proof["records"])>=4
        await_state(1,"withdrawn"); stop(nodes[1],True); node(1); await_state(1,"withdrawn")
        checks.append("Late original and predecessor replay plus replica SIGKILL cannot resurrect withdrawn text")
        for i in (1,2,3):
            proofs=json.loads((site/f"node-{i}.proofs").read_text())["records"]
            match=[r for r in proofs if r["component"]["key"]["type"] in ("wendy.annotation.note.v1","wendy.annotation.note.v2")]
            expected=json.loads(original)
            assert len(match)==1 and match[0]==expected
        short=native("create-short","12-short-original")
        short_raw=base64.b64decode(short["originals"][-1]["signedRecord"])
        short_path=root/"short.signed.json";short_path.write_bytes(short_raw);short_path.chmod(0o600)
        short_amend=root/"short-correction.signed.json"
        command([args.world,"annotation","amend","--credentials",site/"contributor-macos.json","--original",short_path,"--text","Short-lived correction","--out",short_amend])
        command([args.world,"annotation","send","--credentials",site/"contributor-macos.json","--record",short_amend])
        await_state(2,"corrected")
        deadline=json.loads(short_raw)["component"]["payload"]["expires_at_unix_ms"]/1000
        time.sleep(max(0,deadline-time.time()+.15))
        await_state(2,"expired");native("view","13-expired-reader",True)
        command([args.world,"annotation","send","--credentials",site/"contributor-macos.json","--record",short_amend],expected=1)
        assert any(r["state"]=="withdrawn" for r in listing(2))
        checks.append("Original eight-second expiry suppresses its correction and retry while unrelated withdrawal remains terminal")
        receipts["original_sha256"]=digest(original)
        receipts["correction_sha256"]=signed_hash
        receipts["request_receipts"]=observed_posts
        receipts["final_views"]={str(i):listing(i) for i in (1,2,3)}
        receipts["passed"]=True
    except BaseException as e:
        receipts.update(passed=False,error=str(e))
        raise
    finally:
        release.set()
        for p in owned_processes: stop(p)
        for s in proxies: s.shutdown();s.server_close()
        for s in streams:s.close()
        receipts["finished_at"]=time.time()
        receipts["driver_sha256"]=digest(Path(__file__).read_bytes())
        receipts["executables"]={str(p):digest(p.read_bytes()) for p in (args.world,args.worldd,args.lab,args.app/"Contents/MacOS/Wendy World")}
        write(root/"verification.json",receipts)


if __name__=="__main__":
    p=argparse.ArgumentParser(description=__doc__)
    for x in ("world","worldd","lab","app","out"):p.add_argument("--"+x,type=Path,required=True)
    p.add_argument("--port",type=int,default=19910)
    run(p.parse_args())
