#!/usr/bin/env python3
"""Execute documented PAIR invitations against two real pinned cluster managers."""
import argparse
from contextlib import ExitStack
import importlib.util
import json
import hashlib
import ipaddress
import shutil
import os
from pathlib import Path
import queue
import re
import shlex
import signal
import socket
import subprocess
import tempfile
import threading
import time

REVISION = "13b68115fa2c9c1d94f1ead1358f8d5a527cfecf"


def run(argv, **kwargs):
    return subprocess.run(argv, check=True, capture_output=True, text=True, timeout=180, **kwargs)


def documented_rpc(raw):
    raw = raw.replace("\\\n", " ")
    commands = {}
    for line in raw.splitlines():
        if line.startswith("wendy ") and " -- pairctl rpc " in line:
            argv = shlex.split(line)
            rpc = argv[argv.index("pairctl") + 1:]
            if rpc[0] == "rpc":
                commands[rpc[1]] = json.loads(rpc[2]) if len(rpc) > 2 else {}
    required = {"cluster:invite-node", "cluster:respond-to-invite", "cluster:invite-status", "nodes:get-initial"}
    if not required <= commands.keys():
        raise ValueError("README is missing a documented invitation command")
    return commands


def documented_states(raw):
    # Bind the response claims to the method they describe, then compare with
    # actual service output. A generic 'check paired' is the original ambiguity.
    invite = re.search(r'Check\s+the\s+`([^`]+)`\s+response\s+for\s+`state:\s*"([^"`]+)"`', raw)
    member = re.search(r'`nodes:get-initial`\s+response\s+lists\s+cluster\s+members\s+as\s+`([^`]+)`', raw)
    if not invite or not member:
        raise ValueError("README must identify the invitation response separately from membership")
    return invite.group(1), invite.group(2), member.group(1)


class Manager:
    def __init__(self, binary, directory):
        self.directory = directory
        self.messages = queue.Queue(maxsize=256)
        self.events = []
        self.next_id = 0
        self.closed = False
        with socket.socket() as reservation:
            reservation.bind(("127.0.0.1", 0))
            self.port = reservation.getsockname()[1]
        self.log = (directory.parent / (directory.name + ".log")).open("w+")
        environment = {k: v for k, v in os.environ.items() if not k.startswith(("NVPAIR_", "PAIR_"))}
        environment.update(HTTP_PROXY="", HTTPS_PROXY="", ALL_PROXY="", NO_PROXY="127.0.0.1,localhost",
                           http_proxy="", https_proxy="", all_proxy="", no_proxy="127.0.0.1,localhost")
        self.process = subprocess.Popen([str(binary), "--config-dir", str(directory), "--port", str(self.port)],
                                        stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=self.log, env=environment)
        def read():
            try:
                while line := self.process.stdout.readline(1024 * 1024 + 1):
                    if len(line) > 1024 * 1024:
                        raise ValueError("PAIR RPC response exceeds fixture limit")
                    self.messages.put(json.loads(line), timeout=2)
            except Exception as error:
                self.messages.put(error, timeout=2)
        self.reader = threading.Thread(target=read, daemon=True)
        self.reader.start()
        try:
            # The owned process must confirm its bind. A listener belonging to
            # somebody else on a raced ephemeral port cannot satisfy this check.
            deadline = time.monotonic() + 5
            while time.monotonic() < deadline:
                self.log.flush()
                text = Path(self.log.name).read_text()
                if f"inter-node server listening on :{self.port} " in text:
                    break
                if self.process.poll() is not None:
                    raise RuntimeError("owned PAIR manager exited before binding")
                time.sleep(.02)
            else:
                raise TimeoutError("owned PAIR manager did not bind")
        except BaseException:
            self.close()
            raise

    def call(self, method, params):
        self.next_id += 1
        identifier = self.next_id
        wire = {"jsonrpc": "2.0", "id": identifier, "method": method, "params": params}
        self.process.stdin.write((json.dumps(wire) + "\n").encode())
        self.process.stdin.flush()
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline:
            message = self.messages.get(timeout=max(.001, deadline - time.monotonic()))
            if isinstance(message, Exception): raise message
            if message.get("id") == identifier:
                if "error" in message: raise ValueError(f"PAIR rejected {method}: {message['error']}")
                return message["result"]
            self.events.append(message)
        raise TimeoutError("PAIR response deadline expired")

    def close(self):
        if self.closed:
            return
        if self.process.stdin and not self.process.stdin.closed:
            try: self.process.stdin.close()
            except BrokenPipeError: pass
        try:
            self.process.wait(timeout=3)
        except subprocess.TimeoutExpired:
            self.process.terminate()
            try: self.process.wait(timeout=3)
            except subprocess.TimeoutExpired:
                self.process.kill()
                self.process.wait(timeout=3)
        self.reader.join(timeout=2)
        self.process.stdout.close()
        self.log.close()
        self.closed = True


def check_claims(raw, transcript):
    method, paired, member = documented_states(raw)
    if method != transcript["status_method"] or paired != transcript["invitation_state"]:
        raise ValueError("documented invitation state does not match the actual invitation response")
    if not transcript["member_states"] or any(state != member for state in transcript["member_states"]):
        raise ValueError("documented membership state does not match actual nodes:get-initial")


def exercise(binary, root, commands, raw, network, default_port):
    with ExitStack() as cleanup:
        a = Manager(binary, root / "a")
        cleanup.callback(a.close)
        b = Manager(binary, root / "b")
        cleanup.callback(b.close)
        a_id = a.call("cluster:get-node-id", {})["nodeUuid"]
        b_id = b.call("cluster:get-node-id", {})["nodeUuid"]
        invite_params = dict(commands["cluster:invite-node"])
        if ipaddress.ip_address(invite_params["address"]) not in ipaddress.ip_network(network["serviceCIDR"]):
            raise ValueError("documented invite address is outside the example mesh service CIDR")
        if invite_params.get("port") != default_port or not any(
                port["host"] == default_port == port["container"] and port["protocol"] == "tcp"
                for port in network["ports"]):
            raise ValueError("documented invite port differs from the real manager/default published TCP port")
        invite_params.update(address="127.0.0.1", port=b.port)
        invitation = a.call("cluster:invite-node", invite_params)
        if invitation["state"] != "pending" or len(invitation["pin"]) != 6:
            raise ValueError("real invitation did not enter pending with a PIN")
        response = dict(commands["cluster:respond-to-invite"])
        if response.get("inviteId") != "REPLACE_WITH_INVITE_ID" or response.get("pin") != "REPLACE_WITH_PIN":
            raise ValueError("documented invitation placeholders changed")
        response.update(inviteId=invitation["inviteId"], pin=invitation["pin"])
        accepted = b.call("cluster:respond-to-invite", response)
        status_params = dict(commands["cluster:invite-status"])
        if status_params.get("inviteId") != "REPLACE_WITH_INVITE_ID":
            raise ValueError("documented status placeholder changed")
        status_params["inviteId"] = invitation["inviteId"]
        status = a.call("cluster:invite-status", status_params)
        members = a.call("nodes:get-initial", commands["nodes:get-initial"])["nodes"]
        if accepted["state"] != "paired" or status["state"] != "paired":
            raise ValueError("real PIN acceptance did not produce a paired invitation")
        if {node["nodeUuid"] for node in members} != {a_id, b_id}:
            raise ValueError("paired nodes did not become actual cluster members")
        transcript = {"source_revision": REVISION, "status_method": "cluster:invite-status",
                      "invitation_state": status["state"], "member_states": [node["state"] for node in members]}
        check_claims(raw, transcript)
        # The old explanation did not identify which response contains paired.
        original = raw.replace('Check the `cluster:invite-status` response for', 'Check for', 1)
        wrong = raw.replace('response for `state: "paired"`', 'response for `state: "member"`', 1)
        for name, control in (("ambiguous original explanation", original), ("membership state substituted for invitation", wrong)):
            if control == raw: raise ValueError("negative control was not applied")
            try: check_claims(control, transcript)
            except ValueError: print("PASS negative control:", name)
            else: raise ValueError("incorrect PAIR explanation passed: " + name)
        # Close B and reopen the same fresh fixture config. No user config or
        # global keychain is consulted; actual persisted identity/membership is
        # observed after an independent process starts.
        b.close()
        restarted = Manager(binary, root / "b")
        cleanup.callback(restarted.close)
        if restarted.call("cluster:get-node-id", {})["nodeUuid"] != b_id:
            raise ValueError("PAIR node identity changed after restart")
        restored = restarted.call("nodes:get-initial", {})["nodes"]
        if {node["nodeUuid"] for node in restored} != {a_id, b_id} or any(node["state"] != "member" for node in restored):
            raise ValueError("PAIR membership was not retained after restart")
        transcript["restart_identity_and_membership_retained"] = True
        return transcript


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--pair-source", type=Path, required=True)
    parser.add_argument("--repo-root", type=Path, default=Path(__file__).resolve().parents[6])
    args = parser.parse_args()
    source = args.pair_source.resolve()
    if run(["git", "-C", str(source), "rev-parse", "HEAD"]).stdout.strip() != REVISION:
        raise ValueError("PAIR checkout must match the example Dockerfile's exact audited revision")
    if run(["git", "-C", str(source), "status", "--porcelain", "--untracked-files=all"]).stdout.strip():
        raise ValueError("PAIR tracked source must be clean")
    example = args.repo_root.resolve() / "Examples/NvidiaPAIR"
    dockerfile = (example / "Dockerfile").read_text()
    if f"ARG PAIR_REVISION={REVISION}\n" not in dockerfile:
        raise ValueError("example PAIR pin changed; refresh the process contract")
    raw = (example / "README.md").read_text()
    commands = documented_rpc(raw)
    networks = [e for e in json.loads((example / "wendy.json").read_text())["entitlements"] if e["type"] == "network" and e.get("mode") == "mesh"]
    if len(networks) != 1:
        raise ValueError("example must have one concrete mesh network contract")
    with tempfile.TemporaryDirectory(prefix="wendy-pair-docs-") as temporary:
        temporary = Path(temporary)
        service = source / "services/nvpair-cluster-manager"
        # Apply the example's actual production patch in disposable files and
        # overlay those files into the pinned tree. The upstream checkout stays
        # clean, and the resulting protocol implementation matches the image.
        patch = example / "patches/wendy-mesh-addresses.patch"
        paths = re.findall(r"(?m)^diff --git a/(\S+) b/(\S+)$", patch.read_text())
        if not paths:
            raise ValueError("example patch contains no source changes")
        patched = temporary / "patched"
        replacements = {}
        for before_path, after_path in paths:
            if before_path != after_path or not after_path.startswith("services/") or ".." in Path(after_path).parts or not after_path.endswith(".go"):
                raise ValueError("unexpected source path in PAIR example patch")
            target = patched / after_path
            target.parent.mkdir(parents=True, exist_ok=True)
            if (source / before_path).exists():
                shutil.copyfile(source / before_path, target)
            replacements[str(source / after_path)] = str(target)
        run(["git", "apply", "--check", str(patch)], cwd=patched)
        run(["git", "apply", str(patch)], cwd=patched)
        original = service / "httpserver.go"
        replacement = Path(replacements[str(original)])
        contents = replacement.read_text()
        before = 'net.Listen("tcp", fmt.Sprintf(":%d", m.port))'
        if contents.count(before) != 1:
            raise ValueError("loopback-only bind overlay no longer identifies one source site")
        replacement.write_text(contents.replace(before, 'net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", m.port))'))
        overlay = temporary / "overlay.json"
        overlay.write_text(json.dumps({"Replace": replacements}))
        binary = temporary / "cluster-manager"
        run(["go", "build", "-overlay", str(overlay), "-o", str(binary), "."], cwd=service)
        help_text = run([str(binary), "--help"]).stderr
        port_flag = re.search(r"(?m)^\s*-port int\n\s+[^\n]+\(default (\d+)\)", help_text)
        if not port_flag:
            raise ValueError("actual manager no longer declares its default inter-node port")
        default_port = int(port_flag[1])
        # No manager starts before the loopback-only build finishes. SIGTERM or
        # interruption unwinds ExitStack and terminates only these owned PIDs.
        def interrupted(*_): raise KeyboardInterrupt()
        signal.signal(signal.SIGTERM, interrupted)
        spec = importlib.util.spec_from_file_location("hosted_deadline", Path(__file__).with_name("probe-hosted-export.py"))
        timeout = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(timeout)
        with timeout.wall_timeout(60):
            transcript = exercise(binary, temporary, commands, raw, networks[0], default_port)
            transcript["example_patch_sha256"] = hashlib.sha256(patch.read_bytes()).hexdigest()
            print(json.dumps(transcript, indent=2))


if __name__ == "__main__": main()
