"""Two real operator supervisor processes against the existing Linux MuJoCo VM.

All identities, endpoints and observations are simulation-only. This creates
isolated temporary containers/networks on the existing Docker VM; no new VM,
physical device, enrolled credential, public image push or hardware command.
"""
import argparse
import json
import os
import re
import sys
from pathlib import Path
import signal
import subprocess
import tempfile
import time
import uuid

from virtual_site import ROOT

# This bounded test proxy can lose either response direction, delay an ACK
# past its signed deadline or replay its exact bytes and authorization header.
# It never signs, rewrites or logs proof/key bytes.
PROXY = r'''
import http.client, http.server, json, threading, time
from pathlib import Path
slots = threading.BoundedSemaphore(4)
printing = threading.Lock()
class Server(http.server.ThreadingHTTPServer):
    daemon_threads = True
    def process_request(self, request, address):
        if not slots.acquire(False):
            request.close(); return
        super().process_request(request, address)
    def process_request_thread(self, request, address):
        try: super().process_request_thread(request, address)
        finally: slots.release()
    def get_request(self):
        request, address = super().get_request()
        request.settimeout(1)
        return request, address
class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *_): pass
    def do_POST(self):
        if self.path not in ('/v1/tasks/supervision/challenge', '/v1/tasks/supervision/ack'):
            self.send_error(404); return
        try:
            length = int(self.headers.get('Content-Length', '-1'))
            auth = self.headers.get('X-World-Authorization', '')
            if not 0 < length <= 65536 or not 0 < len(auth) <= 65536:
                self.send_error(400); return
            body = self.rfile.read(length)
            if len(body) != length:
                self.send_error(400); return
            mode = Path('/fault/mode').read_text().strip()
            if mode == 'delay-ack' and self.path.endswith('/ack'):
                time.sleep(.7)
            conn = http.client.HTTPConnection('world-node-1', 8080, timeout=1)
            try:
                conn.request('POST', self.path, body, {'Content-Type':'application/json', 'X-World-Authorization':auth})
                response = conn.getresponse()
                status, data = response.status, response.read(65537)
                if len(data) > 65536:
                    raise ValueError('response bound')
            finally: conn.close()
            replay = {}
            if mode == 'replay-ack' and self.path.endswith('/ack'):
                duplicate = http.client.HTTPConnection('world-node-1', 8080, timeout=1)
                try:
                    # Reuse the original outer nonce/signature and exact ACK.
                    # Keep the first response intact for the legitimate client.
                    duplicate.request('POST', self.path, body, {'Content-Type':'application/json', 'X-World-Authorization':auth})
                    duplicate_response = duplicate.getresponse()
                    duplicate_status = duplicate_response.status
                    if len(duplicate_response.read(65537)) > 65536:
                        raise ValueError('duplicate response bound')
                finally: duplicate.close()
                replay['duplicate_status'] = duplicate_status
                if status == 200:
                    acknowledged = json.loads(body).get('ack', {}).get('challenge', {})
                    receipted = json.loads(data).get('receipt', {}).get('challenge', {})
                    challenge_id = receipted.get('challenge_id')
                    expires = receipted.get('expires_at_unix_ms')
                    if (not isinstance(challenge_id, str) or len(challenge_id) != 64 or
                            any(c not in '0123456789abcdef' for c in challenge_id) or
                            type(expires) is not int or expires <= 0):
                        raise ValueError('invalid receipt metadata')
                    replay.update({'original_receipt_matches_ack':acknowledged == receipted,
                                   'original_challenge_id':challenge_id,
                                   'original_expires_at_unix_ms':expires})
            dropped = ((mode == 'drop-challenge-response' and self.path.endswith('/challenge')) or
                       (mode == 'drop-ack-response' and self.path.endswith('/ack')))
            with printing:
                print(json.dumps({'path':self.path, 'mode':mode, 'upstream_status':status, 'response_dropped':dropped, **replay}), flush=True)
            if dropped:
                self.close_connection = True
                return
            self.send_response(status)
            self.send_header('Content-Type','application/json')
            self.send_header('Content-Length',str(len(data)))
            self.end_headers()
            self.wfile.write(data)
        except (OSError, ValueError, http.client.HTTPException):
            self.close_connection = True
Server(('0.0.0.0', 8099), Handler).serve_forever(poll_interval=.1)
'''


def command(args, timeout=45):
    result = subprocess.run(args, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                            cwd=ROOT, timeout=timeout)
    if result.returncode:
        # Never include subprocess input, imported credentials or wire proofs.
        raise RuntimeError(f"command failed ({args[0]}): {result.stderr.decode()[-1500:]}")
    return result.stdout


def sanitized_diagnostics(names):
    diagnostics = []
    for name in names:
        item = {"container": name}
        try:
            state = subprocess.run(["docker", "inspect", "--format", "{{json .State}}", name],
                                   capture_output=True, timeout=3)
            if state.returncode == 0:
                value = json.loads(state.stdout)
                item["state"] = {key: value.get(key) for key in ("Status", "Running", "ExitCode", "OOMKilled")}
            logs = subprocess.run(["docker", "logs", "--tail", "8", name], capture_output=True, timeout=3)
            lines = (logs.stdout + logs.stderr)[-6000:].decode(errors="replace").splitlines()
            safe = []
            for line in lines:
                lowered = line.lower()
                if any(marker in lowered for marker in ("private_key", "pemprivatekey", "x-world-authorization", "begin private", "bearer ")):
                    safe.append("[credential-shaped log line omitted]")
                    continue
                # Strip long encoded material, including tokens, signatures and
                # digests; preserve bounded operational reasons and lease times.
                line = re.sub(r"[A-Za-z0-9_+/=-]{40,}", "[encoded value omitted]", line)
                safe.append(line[:700])
            item["recent_logs"] = safe
        except (subprocess.TimeoutExpired, OSError, ValueError):
            item["diagnostic_status"] = "unavailable within bound"
        diagnostics.append(item)
    print(json.dumps({"supervision_diagnostics": diagnostics}), file=sys.stderr)


def private_json(path, value):
    data = json.dumps(value, separators=(",", ":")).encode()
    with open(path, "xb") as stream:
        os.chmod(path, 0o600)
        stream.write(data)


def run(node_image, actuator_image):
    suffix = uuid.uuid4().hex[:10]
    run_id = "supervision-" + suffix
    control, peer = "supervision-control-" + suffix, "supervision-peer-" + suffix
    nodes = [f"supervision-node-{i}-{suffix}" for i in range(1, 4)]
    actuator, executor, proxy = ["supervision-" + part + "-" + suffix for part in ("actuator", "executor", "proxy")]
    created, networks = [], []
    user = f"{os.getuid()}:{os.getgid()}"
    report = {"schema": "wendy.world.supervision-site.v1", "run_id": run_id,
              "mode": "isolated Linux containers on the existing Docker VM",
              "authority": "generated Ed25519 simulation grants", "physical_hardware": False,
              "checks": {}}
    with tempfile.TemporaryDirectory(prefix="wendy-supervision-") as temporary:
        folder = Path(temporary)
        try:
            command(["go", "run", "./cmd/world-lab", "--out", str(folder), "--runtime-dir", "/state",
                     "--containers", "--missions", "--run-id", run_id])
            for network in (control, peer):
                networks.append(network)
                command(["docker", "network", "create", "--internal", network])
            for i, name in enumerate(nodes, 1):
                state = folder / f"state-{i}"
                state.mkdir(mode=0o700)
                created.append(name)
                command(["docker", "run", "-d", "--pull=never", "--name", name,
                         "--network", control if i == 1 else peer, "--network-alias", f"world-node-{i}",
                         "--user", user, "--mount", f"type=bind,src={folder / f'node-{i}.json'},dst=/config.json,readonly",
                         "--mount", f"type=bind,src={state},dst=/state", node_image, "--config", "/config.json"])
            command(["docker", "network", "connect", "--alias", "world-node-1", peer, nodes[0]])
            created.append(actuator)
            command(["docker", "run", "-d", "--pull=never", "--name", actuator, "--network", control,
                     "--network-alias", "actuator", "--user", user,
                     "--mount", f"type=bind,src={folder / 'adapter.token'},dst=/token,readonly", actuator_image,
                     "examples/mission-office/world.json", "--token-file", "/token", "--host", "0.0.0.0",
                     "--allow-network-simulation", "--allow-test-faults"])
            fault = folder / "fault"
            fault.mkdir(mode=0o700)
            (fault / "mode").write_text("pass")
            script = folder / "proxy.py"
            script.write_text(PROXY)
            created.append(proxy)
            command(["docker", "run", "-d", "--pull=never", "--name", proxy, "--network", control,
                     "--network-alias", "supervision-proxy", "--user", user, "--entrypoint", "python",
                     "--mount", f"type=bind,src={script},dst=/proxy.py,readonly",
                     "--mount", f"type=bind,src={fault},dst=/fault,readonly", actuator_image, "/proxy.py"])
            proxy_credentials = json.loads((folder / "supervisor-a.json").read_text())
            proxy_credentials["url"] = "http://supervision-proxy:8099"
            private_json(folder / "supervisor-proxy.json", proxy_credentials)
            del proxy_credentials
            server_principal = json.loads((folder / "node-1.json").read_text())["peer_grant"]["grant"]["principal"]

            def until(fn, predicate, timeout=12):
                end, last = time.monotonic() + timeout, None
                while time.monotonic() < end:
                    try:
                        last = fn()
                        if predicate(last):
                            return last
                    except (RuntimeError, OSError, KeyError, StopIteration, json.JSONDecodeError) as error:
                        last = type(error).__name__
                    time.sleep(.04)
                raise AssertionError(f"condition not met: {last}")

            def physics():
                code = """import urllib.request
r=urllib.request.Request('http://127.0.0.1:8091/v1/state',headers={'Authorization':'Bearer '+open('/token').read()})
with urllib.request.urlopen(r,timeout=1) as response: print(response.read().decode())
"""
                return json.loads(command(["docker", "exec", actuator, "python", "-c", code], timeout=5))

            def cli(verb, *args, credentials="operator-1.json", spec=None):
                mounts = ["--mount", f"type=bind,src={folder / credentials},dst=/operator.json,readonly"]
                if spec is not None:
                    mounts += ["--mount", f"type=bind,src={spec},dst=/spec.json,readonly"]
                    args += ("--spec", "/spec.json")
                return json.loads(command(["docker", "run", "--rm", "--pull=never", "--network", control,
                    "--user", user, "--entrypoint", "/world", *mounts, node_image,
                    "task", verb, "--credentials", "/operator.json", "--json", *args]))

            def local_task(task_id):
                state = json.loads((folder / "state-1" / "node-1.tasks").read_text())["state"]
                saved = state["records"].get(task_id)
                return saved["record"] if saved else state["stops"].get(task_id)

            def phase(task_id):
                record = local_task(task_id)
                return record["status"]["Phase"] if record else None

            def stopped(state):
                return abs(state["velocity_mps"][0]) < .001 and abs(state["commanded_velocity_mps"]) < .001

            def hold_position():
                before = until(physics, stopped, timeout=2)
                time.sleep(.35)
                after = physics()
                assert stopped(after) and abs(after["position_m"][0] - before["position_m"][0]) < .005
                return after

            def submit(task_id, mode="supervised", target=None):
                position = physics()["position_m"][0]
                if target is None:
                    target = -1.8 if position > 0 else 1.8
                spec = {"id": task_id, "type": "wendy.nav.go_to.v1", "site": "synthetic-office",
                        "assignee": "sim-robot/navigation", "domain": "simulation", "class": "MOVE", "mode": mode,
                        "frame": {"id": "office-floor", "revision": "office-frame-v1"},
                        "not_after_unix_ms": int(time.time() * 1000) + 120000,
                        "area": {"min": [-2.1, .1, -.1], "max": [2.1, .3, .1]},
                        "required_sensors": ["gas", "local-obstacles"], "minimum_energy": .2, "fallback": "hold",
                        "target": [target, .2, 0], "speed_mps": .2, "tolerance_m": .03, "stop_speed_mps": .03}
                path = folder / (task_id + ".json")
                private_json(path, spec)
                cli("send", spec=path)
                return path

            def supervise(task_id, spec, credentials="supervisor-a.json"):
                name = "supervision-client-" + uuid.uuid4().hex[:10]
                created.append(name)
                command(["docker", "run", "-d", "--pull=never", "--name", name, "--network", control,
                         "--user", user, "--entrypoint", "/world",
                         "--mount", f"type=bind,src={folder / credentials},dst=/operator.json,readonly",
                         "--mount", f"type=bind,src={folder / 'site-trust.json'},dst=/trust.json,readonly",
                         "--mount", f"type=bind,src={spec},dst=/spec.json,readonly", node_image,
                         "task", "supervise", "--credentials", "/operator.json", "--trust", "/trust.json",
                         "--spec", "/spec.json", "--run-id", run_id, "--assignee", "sim-robot/navigation",
                         "--gateway-principal", server_principal, "--duration", "60s", "--json"])
                return name

            def lease_log(name):
                lines = command(["docker", "logs", "--tail", "30", name]).decode().splitlines()
                return [value for line in lines if line.startswith("{")
                        for value in [json.loads(line)] if value.get("schema") == "wendy.world.supervision-lease.v1"]

            def process(name):
                return json.loads(command(["docker", "inspect", "--format", "{{json .State}}", name]))

            def ready(name):
                return until(lambda: lease_log(name), bool, timeout=6)

            def finish(name):
                command(["docker", "stop", "--time", "2", name], timeout=5)

            def moving(task_id):
                until(lambda: phase(task_id), lambda p: p == "running")
                return until(physics, lambda s: abs(s["velocity_mps"][0]) > .1)

            def held(task_id):
                start = time.monotonic()
                until(lambda: phase(task_id), lambda p: p == "held", timeout=2)
                state = hold_position()
                return {"held_and_stopped_seconds": round(time.monotonic() - start, 3), "position_x": state["position_m"][0]}

            until(lambda: cli("list")["tasks"], lambda value: not value)
            until(physics, lambda s: s["synthetic"])
            created.append(executor)
            command(["docker", "run", "-d", "--pull=never", "--name", executor, "--network", control,
                     "--user", user, "--entrypoint", "/world-executor",
                     "--mount", f"type=bind,src={folder / 'executor-1.json'},dst=/client.json,readonly",
                     "--mount", f"type=bind,src={folder / 'adapter.token'},dst=/token,readonly", node_image,
                     "--credentials", "/client.json", "--adapter", "http://actuator:8091", "--adapter-token-file", "/token"])
            until(physics, lambda s: s["sequence"] > 0)

            # One operator's disappearance does not remove the other's lease/stop.
            spec = submit("two-operators")
            a = supervise("two-operators", spec); ready(a)
            b = supervise("two-operators", spec, "supervisor-b.json"); ready(b)
            first = moving("two-operators")
            finish(a)
            time.sleep(.8)
            continued = physics()
            assert phase("two-operators") == "running" and abs(continued["position_m"][0] - first["position_m"][0]) > .08
            cli("stop", "--id", "two-operators", credentials="supervisor-b.json")
            hold_position(); finish(b)
            assert phase("two-operators") == "canceled"
            report["checks"]["one_operator_lost"] = {"continued_distance_m": abs(continued["position_m"][0]-first["position_m"][0]), "surviving_operator_stop": "canceled and measured stationary"}

            spec = submit("last-operator-lost")
            a = supervise("last-operator-lost", spec); ready(a)
            b = supervise("last-operator-lost", spec, "supervisor-b.json"); ready(b); moving("last-operator-lost")
            finish(a); time.sleep(.6)
            assert phase("last-operator-lost") == "running"
            finish(b)
            report["checks"]["last_operator_lost"] = held("last-operator-lost")
            retry = supervise("last-operator-lost", spec)
            exit_state = until(lambda: process(retry), lambda s: not s["Running"], timeout=4)
            assert exit_state["ExitCode"] != 0 and not lease_log(retry)
            hold_position(); assert phase("last-operator-lost") == "held"
            report["checks"]["held_never_resumes"] = True
            cli("stop", "--id", "last-operator-lost")

            def fault_mode(value):
                temporary_mode = fault / "next-mode"
                temporary_mode.write_text(value)
                temporary_mode.replace(fault / "mode")

            def proxy_log():
                return [json.loads(line) for line in command(["docker", "logs", "--tail", "60", proxy]).decode().splitlines() if line.startswith("{")]

            # A duplicate outer proof cannot issue another receipt or extend its
            # original lease. Keep returning the first valid receipt so fresh
            # subsequent rounds can continue supervising the running task.
            fault_mode("pass")
            spec = submit("replay-ack")
            a = supervise("replay-ack", spec, "supervisor-proxy.json"); ready(a)
            first = moving("replay-ack")
            fault_mode("replay-ack")
            replayed = until(proxy_log, lambda values: any(v.get("mode") == "replay-ack" and
                             "duplicate_status" in v for v in values), timeout=4)
            replayed = [v for v in replayed if v.get("mode") == "replay-ack" and "duplicate_status" in v][-1]
            assert replayed["upstream_status"] == 200 and replayed["duplicate_status"] == 401, replayed
            assert replayed["original_receipt_matches_ack"] and not replayed["response_dropped"], replayed
            leases = until(lambda: lease_log(a), lambda values: any(
                v["challenge_id"] == replayed["original_challenge_id"] for v in values), timeout=3)
            original = next(v for v in leases if v["challenge_id"] == replayed["original_challenge_id"])
            assert original["expires_at_unix_ms"] == replayed["original_expires_at_unix_ms"]
            assert 0 < original["expires_at_unix_ms"] - original["issued_at_unix_ms"] <= 500
            time.sleep(.8)
            continued = physics()
            assert phase("replay-ack") == "running" and process(a)["Running"]
            assert abs(continued["position_m"][0] - first["position_m"][0]) > .08
            later = lease_log(a)
            assert any(v["issued_at_unix_ms"] > original["issued_at_unix_ms"] for v in later)
            replay_events = [v for v in proxy_log() if v.get("mode") == "replay-ack" and "duplicate_status" in v]
            assert replay_events and all(v["upstream_status"] == 200 and v["duplicate_status"] == 401 and
                                         v["original_receipt_matches_ack"] for v in replay_events)
            cli("stop", "--id", "replay-ack", credentials="supervisor-a.json")
            hold_position(); finish(a)
            assert phase("replay-ack") == "canceled"
            fault_mode("pass")
            report["checks"]["replay_ack"] = {"proxy":replayed, "duplicate_rejections":len(replay_events),
                "continued_distance_m":abs(continued["position_m"][0] - first["position_m"][0]),
                "original_expiry_preserved":True, "operator_stop":"canceled and measured stationary"}

            for mode in ("drop-challenge-response", "drop-ack-response", "delay-ack"):
                fault_mode("pass")
                spec = submit(mode)
                a = supervise(mode, spec, "supervisor-proxy.json"); ready(a); moving(mode)
                fault_mode(mode)
                item = until(proxy_log, lambda values: any(v["mode"] == mode and
                             (v["path"].endswith("/ack") if mode != "drop-challenge-response" else v["path"].endswith("/challenge")) for v in values), timeout=4)
                matching = [v for v in item if v["mode"] == mode and (v["path"].endswith("/ack") if mode != "drop-challenge-response" else v["path"].endswith("/challenge"))][-1]
                if mode == "delay-ack":
                    assert matching["upstream_status"] in (401, 403, 409), matching
                else:
                    assert matching["upstream_status"] == 200 and matching["response_dropped"], matching
                result = held(mode)
                exited = until(lambda: process(a), lambda s: not s["Running"], timeout=4)
                assert exited["ExitCode"] != 0
                result["proxy"] = matching
                report["checks"][mode] = result
                fault_mode("pass"); hold_position(); assert phase(mode) == "held"
                cli("stop", "--id", mode)

            # Reissue only a generated lab supervisor under the existing lab root.
            short_path = folder / "supervisor-expiring.json"
            command(["go", "run", "./tests/supervision_fixture", "--issuer", str(folder / "issuer.json"),
                     "--credentials", str(folder / "supervisor-b.json"), "--out", str(short_path), "--ttl", "5s"])
            expiry = json.loads(short_path.read_text())["grant"]["grant"]["not_after_unix_ms"]
            spec = submit("grant-expiry")
            b = supervise("grant-expiry", spec, short_path.name); ready(b); moving("grant-expiry")
            until(lambda: phase("grant-expiry"), lambda p: p == "held", timeout=7)
            hold_position()
            assert int(time.time()*1000) >= expiry - 500
            report["checks"]["grant_expiry"] = "held and stationary without lease renewal"
            cli("stop", "--id", "grant-expiry")

            spec = submit("executor-restart")
            a = supervise("executor-restart", spec); ready(a); moving("executor-restart")
            command(["docker", "kill", "--signal", "KILL", executor])
            watchdog = hold_position()
            command(["docker", "start", executor])
            until(lambda: phase("executor-restart"), lambda p: p == "failed")
            until(lambda: process(a), lambda state: not state["Running"], timeout=4)
            hold_position()
            retry = supervise("executor-restart", spec)
            exited = until(lambda: process(retry), lambda state: not state["Running"], timeout=4)
            assert exited["ExitCode"] != 0 and not lease_log(retry)
            report["checks"]["executor_restart"] = {"watchdog_position_x": watchdog["position_m"][0],
                "after_restart": "new executor generation failed interrupted task; no lease or motion resume"}

            spec = submit("gateway-restart")
            a = supervise("gateway-restart", spec); ready(a); moving("gateway-restart")
            command(["docker", "kill", "--signal", "KILL", nodes[0]])
            hold_position()
            command(["docker", "start", nodes[0]])
            until(lambda: phase("gateway-restart"), lambda p: p == "failed")
            hold_position()
            retry = supervise("gateway-restart", spec)
            exited = until(lambda: process(retry), lambda s: not s["Running"], timeout=4)
            assert exited["ExitCode"] != 0 and not lease_log(retry)
            report["checks"]["gateway_restart"] = "watchdog stopped motion; failed after reopen; no lease restore"
            # The existing executor pins the old gateway boot. Start a new
            # executor generation explicitly, only after proving no auto-resume.
            prior_epoch = json.loads((folder / "state-1" / "node-1.tasks").read_text())["state"]["executor_epoch"]
            prior_session = physics()["session"]
            command(["docker", "restart", "--time", "2", executor])
            until(lambda: json.loads((folder / "state-1" / "node-1.tasks").read_text())["state"]["executor_epoch"],
                  lambda value: value > prior_epoch)
            until(physics, lambda state: state["session"] != prior_session)
            hold_position(); assert phase("gateway-restart") == "failed"
            report["checks"]["explicit_executor_restart_after_gateway_boot"] = True

            spec = submit("supervisor-revoked")
            a = supervise("supervisor-revoked", spec); ready(a)
            b = supervise("supervisor-revoked", spec, "supervisor-b.json"); ready(b); moving("supervisor-revoked")
            revoked = folder / "revoke-supervisor-b.json"
            command(["go", "run", "./tests/supervision_fixture", "--issuer", str(folder / "issuer.json"),
                     "--credentials", str(folder / "supervisor-b.json"), "--out", str(revoked), "--revoke"])
            post = """import urllib.request
r=urllib.request.Request('http://world-node-1:8080/v1/revocations',data=open('/revocations.json','rb').read(),headers={'Content-Type':'application/json'})
with urllib.request.urlopen(r,timeout=2) as response: print(response.status)
"""
            command(["docker", "run", "--rm", "--pull=never", "--network", control, "--user", user,
                     "--entrypoint", "python", "--mount", f"type=bind,src={revoked},dst=/revocations.json,readonly",
                     actuator_image, "-c", post])
            until(lambda: process(b), lambda s: not s["Running"], timeout=4)
            time.sleep(.6); assert phase("supervisor-revoked") == "running"
            finish(a); held("supervisor-revoked")
            report["checks"]["supervisor_revocation"] = "other operator kept task running; loss of final nonrevoked operator held motion"
            cli("stop", "--id", "supervisor-revoked")

            # Autonomous tasks still use their local envelope, not remote presence.
            start_x = physics()["position_m"][0]
            target = start_x - .6 if start_x > 0 else start_x + .6
            target = max(-1.8, min(1.8, target))
            submit("autonomous-regression", mode="autonomous", target=target)
            moving("autonomous-regression")
            command(["docker", "network", "disconnect", peer, nodes[0]])
            until(lambda: phase("autonomous-regression"), lambda p: p == "succeeded", timeout=10)
            final = hold_position()
            assert abs(final["position_m"][0]-target) <= .031
            report["checks"]["autonomous_partition"] = {"start_x":start_x,"target_x":target,"final_x":final["position_m"][0],"status":"succeeded without supervisors"}

            # Last case: revoking the original mission issuer must fail a running
            # task even while a different operator remains authorized. Never
            # remove an earlier revocation to make another scenario possible.
            task_id = "mission-issuer-revoked"
            issuer_profile = json.loads((folder / "operator-1.json").read_text())
            supervisor_profile = json.loads((folder / "supervisor-a.json").read_text())
            issuer_principal = issuer_profile["grant"]["grant"]["principal"]
            supervisor_principal = supervisor_profile["grant"]["grant"]["principal"]
            assert issuer_principal != supervisor_principal
            current_revocations = json.loads(revoked.read_text())
            current_denyset = current_revocations["revocations"].get("principals", [])
            assert issuer_principal not in current_denyset and supervisor_principal not in current_denyset
            updated_issuer = json.loads((folder / "issuer.json").read_text())
            updated_issuer["revocations"] = current_revocations
            updated_issuer_path = folder / "issuer-after-supervisor-revocation.json"
            private_json(updated_issuer_path, updated_issuer)
            issuer_revoked = folder / "revoke-mission-issuer.json"
            command(["go", "run", "./tests/supervision_fixture", "--issuer", str(updated_issuer_path),
                     "--credentials", str(folder / "operator-1.json"), "--out", str(issuer_revoked),
                     "--revoke-mission-issuer"])
            issuer_update = json.loads(issuer_revoked.read_text())["revocations"]
            assert issuer_update["revision"] == current_revocations["revocations"]["revision"] + 1
            assert set(issuer_update["principals"]) == set(current_denyset) | {issuer_principal}
            assert supervisor_principal not in issuer_update["principals"]
            for field in ("grant_ids", "key_ids", "expires_at_unix_ms"):
                assert issuer_update[field] == current_revocations["revocations"][field]

            spec = submit(task_id)
            a = supervise(task_id, spec); ready(a); moving(task_id)
            running_record = local_task(task_id)
            assert running_record["status"]["Phase"] == "running"
            assert running_record["proof"]["grant"]["grant"]["principal"] == issuer_principal
            fresh_lease = until(lambda: lease_log(a), lambda values: bool(values) and
                values[-1]["operator_principal"] == supervisor_principal and
                values[-1]["expires_at_unix_ms"] > int(time.time() * 1000), timeout=2)[-1]
            assert process(a)["Running"]
            revoke_started = time.monotonic()
            response = command(["docker", "run", "--rm", "--pull=never", "--network", control, "--user", user,
                     "--entrypoint", "python", "--mount", f"type=bind,src={issuer_revoked},dst=/revocations.json,readonly",
                     actuator_image, "-c", post])
            assert response.strip() == b"200", response
            until(lambda: phase(task_id), lambda value: value == "failed", timeout=2)
            stationary = hold_position()
            failed_and_stopped = round(time.monotonic() - revoke_started, 3)
            failed_record = local_task(task_id)
            assert failed_record["spec"] == running_record["spec"]
            assert failed_record["proof"] == running_record["proof"]
            assert "authority" in failed_record["status"]["Reason"]
            # A fresh authenticated read proves the distinct supervisor grant
            # remains usable after installation of the issuer revocation.
            visible = cli("list", credentials="supervisor-a.json")["tasks"]
            assert next(item for item in visible if item["spec"]["id"] == task_id) == failed_record
            finish(a)
            command(["docker", "kill", "--signal", "KILL", nodes[0]])
            hold_position()
            command(["docker", "start", nodes[0]])
            reopened = until(lambda: cli("list", credentials="supervisor-a.json")["tasks"],
                lambda values: any(item["spec"]["id"] == task_id for item in values))
            assert next(item for item in reopened if item["spec"]["id"] == task_id) == failed_record
            assert local_task(task_id) == failed_record
            hold_position()
            report["checks"]["mission_issuer_revocation"] = {
                "issuer_principal": issuer_principal, "supervisor_principal": supervisor_principal,
                "distinct_supervisor_fresh_lease_before_revocation": {
                    "issued_at_unix_ms": fresh_lease["issued_at_unix_ms"],
                    "expires_at_unix_ms": fresh_lease["expires_at_unix_ms"]},
                "supervisor_authenticated_read_after_revocation": True,
                "prior_revocations_preserved": True, "revocation_revision": issuer_update["revision"],
                "terminal_phase": failed_record["status"]["Phase"],
                "terminal_reason": failed_record["status"]["Reason"],
                "revoke_request_to_failed_and_stationary_seconds": failed_and_stopped,
                "stationary_position_x": stationary["position_m"][0],
                "gateway_restart_preserved_status_spec_and_proof": True,
                "scope": "live issuer revocation; no deterministic injection inside a task write"}
            report["result"] = "passed"
            report["limits"] = ["single-axis synthetic MuJoCo robot and ideal braking, not physical stop certification",
                                "cleartext simulation HTTP with lab signatures, not production mesh or enrolled authority",
                                "shared-host wall time; lease enforcement uses gateway monotonic deadlines",
                                "one run of each fault, not latency percentiles or the full network impairment matrix"]
            return report
        except BaseException:
            sanitized_diagnostics(created)
            raise
        finally:
            cleanup_failures = []
            for name in reversed(created):
                try:
                    result = subprocess.run(["docker", "rm", "-f", name], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=10)
                    if result.returncode:
                        # A failed start can leave no container; confirm before reporting.
                        found = subprocess.run(["docker", "inspect", name], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=5)
                        if found.returncode == 0:
                            cleanup_failures.append(name)
                except (subprocess.TimeoutExpired, OSError):
                    cleanup_failures.append(name)
            for network in reversed(networks):
                try:
                    result = subprocess.run(["docker", "network", "rm", network], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=10)
                    if result.returncode:
                        found = subprocess.run(["docker", "network", "inspect", network], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=5)
                        if found.returncode == 0:
                            cleanup_failures.append(network)
                except (subprocess.TimeoutExpired, OSError):
                    cleanup_failures.append(network)
            if cleanup_failures:
                raise RuntimeError("fixture cleanup incomplete: " + ", ".join(cleanup_failures))


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--node-image", default="wendy-world-node:local")
    parser.add_argument("--actuator-image", default="wendy-world-actuator:local")
    args = parser.parse_args()
    def interrupted(_signum, _frame):
        raise KeyboardInterrupt("supervision fixture interrupted")
    signal.signal(signal.SIGTERM, interrupted)
    print(json.dumps(run(args.node_image, args.actuator_image), indent=2))
