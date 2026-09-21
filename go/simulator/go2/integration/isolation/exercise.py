#!/usr/bin/env python3
"""Verify Go2 UDP DDS confinement in disposable, internal Docker namespaces.

No VM, host networking, host ports, external interface or physical robot is
used. The selected image must already exist locally and include Python, nft,
legacy iptables for --backend legacy, and setpriv. All created resources are
removed in finally cleanup. An optional JSON output retains both backends.
"""

import argparse
from datetime import datetime, timezone
import hashlib
import json
from pathlib import Path
import re
import signal
import subprocess
import time
import uuid

from peer import UDP_PORTS, packet_key

HERE = Path(__file__).resolve().parent
RUNTIME = HERE.parents[1] / "go2_sim"
RTPS = b"RTPS\x02\x03\x01\x10" + bytes(range(16))
PREFIXED_RTPS = b"non-RTPS-prefix:" + RTPS
ORDINARY_UDP = b"wendy ordinary UDP payload"
LEGACY_CHAIN = "WENDY_GO2_DDS"
LEGACY_BINARIES = ("iptables-legacy", "ip6tables-legacy")
UNRELATED = "GO2_TEST_UNRELATED"


def docker(*args, input=None, check=True):
    result = subprocess.run(["docker", *args], input=input, capture_output=True,
                            text=True, timeout=30)
    if check and result.returncode:
        raise RuntimeError(f"docker {' '.join(args[:4])} failed: {result.stderr.strip()}")
    return result


def remote(container, *args, input=None, check=True):
    return docker("exec", "-i", container, *args, input=input, check=check)


def probe(container, action, host=None, payload=None, port=UDP_PORTS[0]):
    args = ["python3", "/checks/peer.py", action]
    if host is not None:
        args.append(host)
    if payload is not None:
        args.append(payload.hex())
    args += ["--port", str(port)]
    return json.loads(remote(container, *args).stdout)


def assert_packet(container, receiver, destination, payload, delivered, port):
    key = packet_key(port, payload)
    before = probe(receiver, "stats", "127.0.0.1")["received"].get(key, 0)
    result = probe(container, "udp", destination, payload, port)
    after = probe(receiver, "stats", "127.0.0.1")["received"].get(key, 0)
    if result["delivered"] != delivered or after - before != int(delivered):
        raise RuntimeError(f"UDP {container} -> {destination}:{port}: {result}, "
                           f"actual receipts={after - before}, expected delivered={delivered}")


def install(container, backend, check=True):
    command = ["python3", "-m", "go2_sim.isolation"]
    if backend == "legacy":
        command = ["python3", "-c", "import json; from go2_sim.isolation_legacy import install; print(json.dumps(install()))"]
    return remote(container, "env", "PYTHONPATH=/runtime", *command, check=check)


def rules(container, backend):
    if backend == "legacy":
        return {binary: remote(container, binary, "-t", "filter", "-S").stdout
                for binary in LEGACY_BINARIES}
    return json.loads(remote(container, "nft", "--json", "list", "table", "inet", "wendy_go2").stdout)


def prepare_unrelated(container, backend):
    if backend == "legacy":
        for binary in LEGACY_BINARIES:
            patch = f"*filter\n:{UNRELATED} - [0:0]\n-A {UNRELATED} -m comment --comment preserve-marker -j RETURN\n"
            patch += "".join(f"-A {chain} -m comment --comment earlier-accept -j ACCEPT\n"
                             for chain in ("INPUT", "OUTPUT", "FORWARD"))
            remote(container, binary + "-restore", "--noflush", input=patch + "COMMIT\n")
        return rules(container, backend)
    remote(container, "nft", "--file", "-", input='''table inet go2_test_unrelated {
  comment "preserve-marker"
  chain input { type filter hook input priority -20; policy accept; accept; }
  chain output { type filter hook output priority -20; policy accept; accept; }
}
''')
    return remote(container, "nft", "--json", "list", "table", "inet", "go2_test_unrelated").stdout


def verify_unrelated(container, backend, before):
    if backend == "legacy":
        after = rules(container, backend)
        for binary, previous in before.items():
            # Compare the complete original table after removing only managed
            # chain declarations/rules/jumps. This also checks policy and order.
            remaining = [line for line in after[binary].splitlines() if LEGACY_CHAIN not in line]
            if remaining != previous.splitlines():
                raise RuntimeError("Unrelated legacy rules or policies changed: " + binary)
    elif remote(container, "nft", "--json", "list", "table", "inet", "go2_test_unrelated").stdout != before:
        raise RuntimeError("Unrelated nft table changed")


def drop_counters(container, backend):
    if backend == "legacy":
        counters = {}
        for family, binary in zip(("ipv4", "ipv6"), LEGACY_BINARIES):
            saved = remote(container, binary + "-save", "-c", "-t", "filter").stdout
            matches = re.findall(r"^\[(\d+):\d+\] -A " + LEGACY_CHAIN + r" .* -j DROP$", saved, re.MULTILINE)
            if len(matches) != 1:
                raise RuntimeError("Missing single legacy drop rule counter: " + saved)
            counters[family] = int(matches[0])
        return counters
    counters = {}
    for item in rules(container, backend)["nftables"]:
        if "rule" in item:
            for expr in item["rule"]["expr"]:
                if "counter" in expr:
                    counters[item["rule"]["chain"]] = expr["counter"]["packets"]
    return counters


def foreign_owner_check(container, backend):
    if backend == "legacy":
        # Both families have protection before this deliberate foreign marker.
        # A rejected reinstall must leave all rules byte-for-byte unchanged.
        remote(container, "ip6tables-legacy", "-A", LEGACY_CHAIN, "-m", "comment",
               "--comment", "Different-owner-preserve", "-j", "RETURN")
    else:
        remote(container, "nft", "--file", "-", input='''delete table inet wendy_go2
table inet wendy_go2 {
  comment "Different owner: preserve this table"
  chain marker { }
}
''')
    before = rules(container, backend)
    refusal = install(container, backend, check=False)
    if refusal.returncode == 0 or "unknown owner" not in refusal.stderr:
        raise RuntimeError("Foreign reserved firewall object was not refused: " + refusal.stderr)
    if rules(container, backend) != before:
        raise RuntimeError("Foreign reserved firewall object was changed")


def write_report(path, backend, result):
    if path is None:
        return
    path = Path(path)
    report = {"kind": "go2-dds-isolation", "results": {}}
    if path.exists():
        report = json.loads(path.read_text())
        if report.get("kind") != "go2-dds-isolation":
            raise RuntimeError("Refusing to overwrite an unrelated JSON report: " + str(path))
    report["results"][backend] = result
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_suffix(path.suffix + ".tmp")
    temporary.write_text(json.dumps(report, indent=2) + "\n")
    temporary.replace(path)


def main(image, backend, output=None):
    image_info = json.loads(docker("image", "inspect", image).stdout)[0]
    identity = uuid.uuid4().hex
    prefix = "wendy-go2-isolation-" + identity[:10]
    network = prefix + "-net"
    guard, peer = prefix + "-guard", prefix + "-peer"
    created = []
    network_created = False
    results = {"image": image_info["Id"], "backend": backend, "internal_network": True,
               "started_at": datetime.now(timezone.utc).isoformat(), "udp_ports": list(UDP_PORTS),
               "runtime_sources": {name: hashlib.sha256((RUNTIME / name).read_bytes()).hexdigest()
                                   for name in ("isolation.py", "isolation_legacy.py")},
               "ipv4": {}, "ipv6": {}, "passed": False}
    try:
        subnet = f"fd73:{identity[10:14]}:{identity[14:18]}::/64"
        docker("network", "create", "--internal", "--ipv6", "--subnet", subnet, network)
        network_created = True
        for name in (guard, peer):
            args = ["run", "--detach", "--pull=never", "--name", name, "--network", network,
                    "--cap-drop=ALL", "--security-opt=no-new-privileges", "--entrypoint=python3",
                    "--mount", f"type=bind,src={HERE},dst=/checks,readonly",
                    "--mount", f"type=bind,src={RUNTIME},dst=/runtime/go2_sim,readonly"]
            if name == guard:
                args += ["--cap-add=NET_ADMIN", "--cap-add=SETPCAP"]
                if backend == "legacy":
                    # libiptc uses a raw socket even for filter-table reads.
                    args += ["--cap-add=NET_RAW"]
            docker(*args, image, "/checks/peer.py", "serve")
            created.append(name)
        for name in created:
            deadline = time.monotonic() + 10
            while '"ready": true' not in docker("logs", name).stdout:
                if time.monotonic() > deadline:
                    raise RuntimeError("Packet endpoint did not become ready: " + name)
                time.sleep(0.1)
        addresses = {}
        for name in created:
            attached = json.loads(docker("inspect", name).stdout)[0]["NetworkSettings"]["Networks"][network]
            addresses[name] = {"ipv4": attached["IPAddress"], "ipv6": attached["GlobalIPv6Address"]}
            if not all(addresses[name].values()):
                raise RuntimeError("Docker did not assign both IP families")
        for family in ("ipv4", "ipv6"):
            for port in UDP_PORTS:
                for sender, receiver in ((guard, peer), (peer, guard)):
                    assert_packet(sender, receiver, addresses[receiver][family], RTPS, True, port)
                    assert_packet(sender, receiver, addresses[receiver][family], PREFIXED_RTPS, True, port)
            results[family]["baseline_rtps_and_marker_both_directions_all_ports"] = True
        unrelated = prepare_unrelated(guard, backend)
        first = json.loads(install(guard, backend).stdout)
        second = json.loads(install(guard, backend).stdout)
        if first != second or first.get("dds_isolation") != "udp-rtps-loopback":
            raise RuntimeError("Repeated install did not preserve the managed boundary")
        if backend == "legacy" and first.get("backend") != "iptables-legacy":
            raise RuntimeError("Legacy test did not install legacy rules")
        results["installation"] = first
        results["reinstall_idempotent"] = True
        verify_unrelated(guard, backend, unrelated)
        results["unrelated_rules_policies_and_earlier_accept_preserved"] = True
        for family, loopback in (("ipv4", "127.0.0.1"), ("ipv6", "::1")):
            for port in UDP_PORTS:
                for sender, receiver in ((guard, peer), (peer, guard)):
                    assert_packet(sender, receiver, addresses[receiver][family], RTPS, False, port)
                    assert_packet(sender, receiver, addresses[receiver][family], PREFIXED_RTPS, backend != "legacy", port)
                    assert_packet(sender, receiver, addresses[receiver][family], ORDINARY_UDP, True, port)
                assert_packet(guard, guard, loopback, RTPS, True, port)
                assert_packet(guard, guard, loopback, PREFIXED_RTPS, True, port)
            for sender, receiver in ((guard, peer), (peer, guard)):
                if not probe(sender, "http", addresses[receiver][family])["delivered"]:
                    raise RuntimeError("HTTP was blocked")
            results[family].update({"non_loopback_rtps_blocked_both_directions_all_ports": True,
                                   "prefixed_rtps_marker": "blocked" if backend == "legacy" else "allowed",
                                   "loopback_rtps_and_marker_allowed_all_ports": True,
                                   "ordinary_udp_allowed_both_directions_all_ports": True,
                                   "http_tcp_allowed_both_directions": True,
                                   "actual_receiver_counts_checked": True})
        counters = drop_counters(guard, backend)
        required = ("ipv4", "ipv6") if backend == "legacy" else ("input", "output")
        if any(counters.get(key, 0) < 2 * len(UDP_PORTS) for key in required):
            raise RuntimeError("Missing actual firewall drop counters: " + repr(counters))
        results["drop_counters"] = counters
        permissions = remote(guard, "setpriv", "--bounding-set=-net_admin", "--inh-caps=-net_admin",
                             "--ambient-caps=-net_admin", "python3", "/checks/peer.py", "permissions",
                             "--backend", backend)
        results["runtime_permissions"] = json.loads(permissions.stdout)
        foreign_owner_check(guard, backend)
        results["foreign_reserved_object_preserved"] = True
        results["passed"] = True
    except BaseException as error:
        results["error"] = str(error)
        raise
    finally:
        failures = []
        for name in reversed(created):
            if docker("rm", "--force", name, check=False).returncode:
                failures.append(name)
        if network_created and docker("network", "rm", network, check=False).returncode:
            failures.append(network)
        results["cleanup_complete"] = not failures
        if failures:
            results["passed"] = False
            results["cleanup_failures"] = failures
        write_report(output, backend, results)
        if failures:
            raise RuntimeError("Could not remove test resources: " + ", ".join(failures))
    print(json.dumps(results, indent=2), flush=True)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image", default="wendy-go2-build-base:dev")
    parser.add_argument("--backend", choices=("nft", "legacy"), default="nft")
    parser.add_argument("--output", help="JSON artifact; retains results keyed by backend")
    args = parser.parse_args()
    signal.signal(signal.SIGTERM, lambda *_: (_ for _ in ()).throw(KeyboardInterrupt()))
    main(args.image, args.backend, args.output)
