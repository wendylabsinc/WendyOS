"""Actual World MCP stdio against a durable gateway in the existing Docker VM.

Creates temporary local containers/network only. Does not connect to Go2 or
start an executor. Lab credentials stay in the temporary private directory.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import select
import subprocess
import tempfile
import time
import uuid

from virtual_site import command


class Protocol:
    def __init__(self, args, stderr):
        self.process = subprocess.Popen(args, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                        stderr=stderr, bufsize=0)
        self.buffer = b""
        self.next_id = 0
        self.frames = []

    def send(self, method, params=None, notification=False):
        message = {"jsonrpc": "2.0", "method": method}
        if params is not None:
            message["params"] = params
        if not notification:
            self.next_id += 1
            message["id"] = self.next_id
        data = json.dumps(message, separators=(",", ":")).encode() + b"\n"
        assert len(data) <= 32 << 10
        self.process.stdin.write(data)
        self.process.stdin.flush()
        if notification:
            return None
        deadline = time.monotonic() + 12
        while True:
            while b"\n" in self.buffer:
                line, self.buffer = self.buffer.split(b"\n", 1)
                assert len(line) <= 128 << 10
                response = json.loads(line)
                self.frames.append(response)
                if response.get("id") == message["id"]:
                    return response
            remaining = deadline - time.monotonic()
            if remaining <= 0 or not select.select([self.process.stdout], [], [], remaining)[0]:
                raise AssertionError("MCP response deadline exceeded")
            chunk = os.read(self.process.stdout.fileno(), 65536)
            if not chunk:
                raise AssertionError("MCP exited before a protocol response")
            self.buffer += chunk
            assert len(self.buffer) <= 256 << 10

    def call(self, name, arguments, *, error=False):
        response = self.send("tools/call", {"name": name, "arguments": arguments})
        assert "error" not in response, response
        result = response["result"]
        assert bool(result.get("isError")) == error, result
        assert len(json.dumps(result, separators=(",", ":")).encode()) <= 64 << 10
        value = result["structuredContent"]
        assert json.loads(result["content"][0]["text"]) == value
        return value

    def close(self):
        if self.process.poll() is None:
            self.process.stdin.close()
            try:
                self.process.wait(timeout=8)
            except subprocess.TimeoutExpired:
                self.process.terminate()
                self.process.wait(timeout=5)


def run(image):
    suffix = uuid.uuid4().hex[:10]
    network, node_name, mcp_name = ("world-mcp-" + suffix + s for s in ("-net", "-node", "-stdio"))
    user = f"{os.getuid()}:{os.getgid()}"
    created = []
    made_network = False
    protocol = None
    with tempfile.TemporaryDirectory(prefix="wendy-world-mcp-") as tmp:
        folder = Path(tmp)
        try:
            command(["go", "run", "./cmd/world-lab", "--out", str(folder), "--runtime-dir", "/state",
                     "--containers", "--missions", "--proposals", "--run-id", "mcp-test"])
            config_path = folder / "node-1.json"
            config = json.loads(config_path.read_text())
            config["peers"] = []
            config_path.write_text(json.dumps(config))
            state = folder / "state"
            state.mkdir(mode=0o700)
            command(["docker", "network", "create", "--internal", network])
            made_network = True
            command(["docker", "run", "-d", "--name", node_name, "--network", network,
                     "--network-alias", "world-node-1", "--user", user,
                     "--mount", f"type=bind,src={config_path},dst=/config.json,readonly",
                     "--mount", f"type=bind,src={state},dst=/state", image, "--config", "/config.json"])
            created.append(node_name)

            def cli(*args, credentials="operator-1.json"):
                return json.loads(command(["docker", "run", "--rm", "--network", network,
                    "--user", user, "--entrypoint", "/world", "--mount",
                    f"type=bind,src={folder},dst=/lab,readonly", image, "task", *args,
                    "--credentials", "/lab/" + credentials, "--json"]))

            def ready():
                deadline = time.monotonic() + 20
                while time.monotonic() < deadline:
                    try:
                        return cli("catalog")
                    except RuntimeError:
                        time.sleep(.2)
                raise AssertionError("gateway did not become ready")

            ready()
            assert cli("list")["tasks"] == []
            with (folder / "mcp-stderr.log").open("wb") as stderr:
                protocol = Protocol(["docker", "run", "--rm", "-i", "--name", mcp_name,
                    "--network", network, "--user", user, "--entrypoint", "/world-mcp",
                    "--mount", f"type=bind,src={folder/'agent-1.json'},dst=/agent.json,readonly",
                    image, "--credentials", "/agent.json"], stderr)
                created.append(mcp_name)
                initialized = protocol.send("initialize", {"protocolVersion": "2025-11-25",
                    "capabilities": {}, "clientInfo": {"name": "world-linux-check", "version": "1"}})
                assert "result" in initialized
                protocol.send("notifications/initialized", notification=True)
                tools = protocol.send("tools/list", {})["result"]["tools"]
                names = sorted(t["name"] for t in tools)
                assert names == ["task_catalog", "task_propose", "task_status", "world_query", "world_watch"]
                assert all(t["annotations"]["readOnlyHint"] == (t["name"] != "task_propose") for t in tools)
                snapshot = protocol.call("world_query", {"limit": 1})
                assert snapshot["data"]["components"] == [] and not snapshot["truncated"]
                cursor = snapshot["data"]["cursor"]
                started = time.monotonic()
                watch = protocol.call("world_watch", {"cursor": cursor, "timeout_ms": 100})
                watch_seconds = time.monotonic() - started
                assert not watch["data"]["changed"] and watch_seconds < 6
                catalog = protocol.call("task_catalog", {})["data"]
                assert catalog["assignee"] == "sim-robot/navigation" and catalog["run_id"] == "mcp-test"
                request = {"id": "proposal-one", "assignee": catalog["assignee"],
                    "type": "wendy.nav.go_to.v1", "parameters": {"target_m": [1, 0, 0]},
                    "summary": "Simulation suggestion; operator must prepare the operating envelope.",
                    "not_after_unix_ms": int(time.time()*1000) + 600_000}
                receipt = protocol.call("task_propose", request)["data"]
                assert receipt["pending_human_review"] and not receipt["adapter_validated"] and receipt["class"] == "MOVE"
                duplicate = protocol.call("task_propose", request)["data"]
                assert receipt == duplicate
                retained = cli("proposal", "get", "--id", request["id"])
                record = retained["record"]
                assert record["proposal"]["summary"] == request["summary"] and record["kind"] == "agent"
                assert record["body_sha256"] == receipt["body_sha256"]
                proposal_path = folder / "proposal.json"
                proposal_path.write_text(json.dumps(record["proposal"]))
                cli_retry = cli("propose", "--proposal", "/lab/proposal.json", credentials="agent-1.json")
                assert cli_retry == retained
                assert cli("proposal", "list", "--limit", "1")["proposals"][0]["id"] == request["id"]
                changed = dict(request, summary="changed content cannot replace this ID")
                protocol.call("task_propose", changed, error=True)
                protocol.call("world_query", {"url": "http://elsewhere.invalid"}, error=True)
                protocol.call("world_query", {"limit": 101}, error=True)
                protocol.call("task_status", {"task_id": request["id"]}, error=True)
                assert "error" in protocol.send("tools/call", {"name": "task_send", "arguments": {}})
                assert cli("list")["tasks"] == []
                command(["docker", "kill", "--signal", "KILL", node_name])
                unavailable = protocol.call("world_query", {}, error=True)
                assert unavailable["unavailable"]
                command(["docker", "start", node_name])
                ready()
                assert cli("proposal", "get", "--id", request["id"]) == retained
                assert cli("list")["tasks"] == []
                assert protocol.call("task_propose", request)["data"] == receipt
                protocol.close()
                assert protocol.process.returncode == 0
            # Operator credentials cannot start an agent-facing MCP server.
            refused = subprocess.run(["docker", "run", "--rm", "--network", "none", "--user", user,
                "--entrypoint", "/world-mcp", "--mount",
                f"type=bind,src={folder/'operator-1.json'},dst=/agent.json,readonly",
                image, "--credentials", "/agent.json"], capture_output=True, timeout=15)
            assert refused.returncode != 0 and refused.stdout == b"" and b"private_key" not in refused.stderr
            transcript = protocol.frames
            encoded = json.dumps(transcript, separators=(",", ":")).encode()
            assert b"private_key" not in encoded and b'"proof"' not in encoded
            return {"mode": "actual stdio executable and durable gateway in isolated Linux containers on existing Docker VM",
                "image": command(["docker", "image", "inspect", image, "--format", "{{.Id}}"]).decode().strip(),
                "tools": names, "protocol_version": initialized["result"]["protocolVersion"],
                "watch_elapsed_seconds": watch_seconds, "protocol_responses": len(transcript),
                "transcript_sha256": hashlib.sha256(encoded).hexdigest(), "transcript": transcript,
                "proposal_exact_retry_and_cli_review": True, "proposal_retained_after_sigkill": True,
                "changed_proposal_refused": True, "operator_mcp_credentials_refused": True,
                "gateway_outage_explicitly_unavailable": True, "proposal_never_created_task": True,
                "limits": ["simulation authority and HTTP transport", "one local proposal store; no proposal replication",
                    "empty replica; positive task status and bounded nonempty results covered by Go tests",
                    "generic proposal parameters are not adapter validated", "no Go2 or hardware interaction"]}
        finally:
            if protocol is not None:
                protocol.close()
            for name in reversed(created):
                subprocess.run(["docker", "rm", "-f", name], capture_output=True, timeout=15)
            if made_network:
                subprocess.run(["docker", "network", "rm", network], capture_output=True, timeout=15)


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--image", required=True)
    parser.add_argument("--output", required=True)
    args = parser.parse_args()
    report = run(args.image)
    Path(args.output).write_text(json.dumps(report, indent=2) + "\n")
    print(json.dumps({k:v for k,v in report.items() if k != "transcript"}, indent=2))
