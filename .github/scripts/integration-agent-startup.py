#!/usr/bin/env python3
"""Check real setup startup in Docker with a Linux wendy-agent binary (CGO disabled).

Build with CGO_ENABLED=0 GOOS=linux go build -o /tmp/wendy-agent ./go/cmd/wendy-agent
Run with python3 .github/scripts/integration-agent-startup.py --agent /tmp/wendy-agent
The binary must match Docker's architecture. No host ports or devices are exposed.
"""

import argparse
import json
import os
from pathlib import Path
import socket
import subprocess
import time
import uuid
import xml.etree.ElementTree as ET


CASES = ("success", "failure", "stall")


def grpc_is_serving():
    # TCP connect alone succeeds on a bound socket even before Serve starts.
    # Require the HTTP/2 SETTINGS frame from the actual gRPC server instead.
    try:
        with socket.create_connection(("127.0.0.1", 51051), timeout=0.25) as conn:
            conn.sendall(b"PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n" + bytes(3) + b"\x04" + bytes(5))
            header = b""
            while len(header) < 9:
                part = conn.recv(9 - len(header))
                if not part:
                    return False
                header += part
            return header[3] == 4 and header[5:9] == bytes(4)
    except OSError:
        return False


def inside_container(case):
    config = Path("/etc/wendy-agent")
    config.mkdir(parents=True)
    # Recorded enrollment with a missing key is the incident's startup path.
    state = json.dumps({"enrolled": True, "orgId": 42, "assetId": 100,
                        "cloudHost": "cloud.invalid", "certPem": "fixture-cert"})
    (config / "provisioning.json").write_text(state)
    service = Path("/etc/avahi/services/wendyos-mdns.service")
    service.parent.mkdir(parents=True)
    service.write_text("""<service-group><name>Startup fixture</name>
  <service><type>_wendyos._udp</type><port>51052</port>
    <txt-record>tls=true</txt-record><txt-record>orgid=42</txt-record>
    <txt-record>assetid=100</txt-record><txt-record>id=stable-device-id</txt-record>
    <txt-record>name=startup-fixture</txt-record></service>
  <service><type>_http._tcp</type><port>8080</port></service>
</service-group>""")

    # Only replace the external service manager. Exercise the real entry
    # point, provisioning loader, advertisement writer and gRPC server.
    result = {"success": "exit 0", "failure": "exit 1", "stall": "sleep 60"}[case]
    manager = Path("/usr/bin/systemctl")
    manager.write_text('''#!/bin/sh
if [ "$1" = restart ] && [ "$2" = avahi-daemon ]; then
    touch /tmp/avahi-restart-requested
    ''' + result + '''
fi
exit 1
''')
    manager.chmod(0o755)
    marker = Path("/tmp/avahi-restart-requested")
    log_path = Path("/tmp/agent.log")
    env = dict(os.environ, WENDY_AGENT_PORT="51051", WENDY_LOCAL_SOCKET="off",
               WENDY_CONTAINERD_ADDR="/tmp/no-containerd.sock")
    with log_path.open("w") as log:
        agent = subprocess.Popen(["/test-agent"], env=env, stdout=log, stderr=log)
    try:
        deadline = time.monotonic() + 75
        restart_started = None
        while not grpc_is_serving():
            if agent.poll() is not None:
                raise AssertionError(f"agent exited with {agent.returncode}")
            now = time.monotonic()
            if marker.exists() and restart_started is None:
                restart_started = now
            if restart_started is not None and now - restart_started > 10:
                raise AssertionError("Avahi restart blocked setup RPCs for over 10 seconds")
            if now > deadline:
                raise AssertionError("agent did not start serving setup RPCs")
            time.sleep(0.05)

        assert marker.exists(), "startup did not reconcile the Avahi advertisement"
        services = {s.findtext("type"): s for s in ET.parse(service).findall("service")}
        setup = services["_wendyos._udp"]
        assert setup.findtext("port") == "51051", "stale mTLS port retained"
        txt = {r.text for r in setup.findall("txt-record")}
        assert {"tls=false", "id=stable-device-id", "name=startup-fixture"} <= txt, txt
        assert not any(r.startswith(("orgid=", "assetid=")) or r == "tls=true" for r in txt), txt
        assert services["_http._tcp"].findtext("port") == "8080"
        assert (config / "provisioning.json").read_text() == state, "enrollment was modified"
        assert not (config / "device-key.pem").exists(), "missing key was replaced"
        if case != "success":
            logs = log_path.read_text()
            assert "systemctl restart avahi-daemon failed" in logs
            if case == "stall":
                assert "context deadline exceeded" in logs
        print(f"{case}: setup gRPC serves; advertisement corrected; enrollment preserved", flush=True)
    except BaseException:
        print(log_path.read_text(), flush=True)
        raise
    finally:
        agent.kill()
        agent.wait(timeout=5)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--agent", type=Path)
    parser.add_argument("--case", choices=CASES)
    parser.add_argument("--inside", choices=CASES, help=argparse.SUPPRESS)
    args = parser.parse_args()
    if args.inside:
        inside_container(args.inside)
        return
    if args.agent is None or not args.agent.is_file():
        parser.error("--agent must name a Linux wendy-agent binary (CGO disabled)")
    for case in (args.case,) if args.case else CASES:
        name = "wendy-startup-test-" + uuid.uuid4().hex[:12]
        try:
            subprocess.run([
                "docker", "run", "--rm", "--name", name, "--network", "none",
                "--mount", f"type=bind,src={args.agent.resolve()},dst=/test-agent,readonly",
                "--mount", f"type=bind,src={Path(__file__).resolve()},dst=/startup-test.py,readonly",
                "python:3.13-slim", "python3", "/startup-test.py", "--inside", case,
            ], check=True, timeout=100)
        finally:
            subprocess.run(["docker", "rm", "-f", name], stdout=subprocess.DEVNULL,
                           stderr=subprocess.DEVNULL, timeout=15)


if __name__ == "__main__":
    main()
