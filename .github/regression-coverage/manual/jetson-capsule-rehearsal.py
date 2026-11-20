#!/usr/bin/env python3
"""Generate audit1682 capsule-only physical steps; never execute device operations."""
import argparse
import importlib.util
import json
from pathlib import Path
import re
import shlex
from urllib.parse import urlsplit

HERE = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location("jetson_ab_plan", HERE/"jetson-ab-rehearsal.py")
AB = importlib.util.module_from_spec(spec)
spec.loader.exec_module(AB)


def unchanged(record):
    actual = AB.record(record["path"])
    if actual != record:
        raise ValueError("prepared input changed: " + record["path"])
    return actual


def generate(inventory, output):
    data = json.loads(Path(inventory).read_text())
    plan_record = AB.record(data["ab_plan"])
    plan = json.loads(Path(plan_record["path"]).read_text())
    if plan.get("audit_entries") != ["2341"] or plan.get("updater_revision") != AB.UPDATER_REVISION:
        raise ValueError("require the pinned 2341 identity/preparation plan, not an execution claim")
    # A prepared identity plan is a reusable input, never evidence of hardware success.
    for item in list(plan["records"].values()) + plan["sources"] + plan["generated"]:
        unchanged(item)
    board = plan["inventory"]["board"]
    records = plan["records"]
    if board["soc"] not in ("tegra234", "tegra264"):
        raise ValueError("only the pinned updater's capsule-capable Orin/Thor families")
    source = Path(plan["inventory"]["updater_source"])
    import subprocess
    if subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=source, text=True, timeout=10).strip() != AB.UPDATER_REVISION or subprocess.check_output(["git", "status", "--porcelain", "--untracked-files=no"], cwd=source, text=True, timeout=10).strip():
        raise ValueError("updater source must remain clean and pinned")
    transition = data["transition"]
    for key in ("from_firmware", "to_firmware", "from_esrt_version", "to_esrt_version", "esrt_firmware_guid", "vendor_qualification"):
        AB.line(transition[key])
    if transition["from_firmware"] != board["firmware"] or transition["from_firmware"] == transition["to_firmware"] or transition["from_esrt_version"] == transition["to_esrt_version"]:
        raise ValueError("require a genuine reviewed firmware/ESRT version transition")
    if not re.fullmatch(r"[a-fA-F0-9]{8}(?:-[a-fA-F0-9]{4}){3}-[a-fA-F0-9]{12}", transition["esrt_firmware_guid"]):
        raise ValueError("pin the actual ESRT firmware class GUID")
    if transition.get("antirollback_compatible") is not True or transition.get("recovery_independently_verified") is not True:
        raise ValueError("require reviewed forward transition and independent recovery qualification")
    for key, low, high in (("boot_seconds", 60, 900), ("power_off_seconds", 10, 120)):
        if type(data[key]) is not int or not low <= data[key] <= high:
            raise ValueError("require bounded physical observation windows")
    physical = {key: AB.record(data[key]) for key in ("transition_record", "recovery_record", "power_isolation_record", "authorization_record")}
    archive = AB.record(data["archive"])
    capsule = AB.record(data["capsule"])
    descriptor_record = AB.record(data["descriptor"])
    descriptor = json.loads(Path(descriptor_record["path"]).read_text())
    url = urlsplit(AB.line(descriptor["url"]))
    if url.scheme != "https" or not url.hostname or url.username or url.password or url.query or url.fragment or not url.path.endswith(".wendy") or any(part in ("latest", "nightly") for part in url.path.split("/")):
        raise ValueError("pin an immutable credential-free HTTPS .wendy artifact")
    m = AB.manifest(archive["path"])
    if descriptor.get("sha256") != archive["sha256"] or descriptor.get("size_bytes") != archive["size_bytes"] or descriptor.get("storage") != board["storage"] or m.get("compatible_devices") != [board["device_type"]]:
        raise ValueError("capsule artifact must match exact board/storage/hash/size")
    for key in ("artifact_name", "artifact_version"):
        if descriptor[key] != AB.line(m[key]) or m[key] in ("latest", "nightly"):
            raise ValueError("pin matching producer artifact identity")
    inspection_record = AB.record(data["payload_inspection"])
    inspection = json.loads(Path(inspection_record["path"]).read_text())
    expected = {"artifact_sha256": archive["sha256"], "rootfs_sha256": m["payload"]["sha256"], "agent_sha256": records["agent_binary"]["sha256"], "updater_sha256": records["updater_binary"]["sha256"], "bootloader_marker_present": True, "capsule_path": "/opt/nvidia/UpdateCapsule/tegra-bl.cap", "capsule_sha256": capsule["sha256"], "capsule_size_bytes": capsule["size_bytes"]}
    if any(inspection.get(k) != v for k, v in expected.items()) or inspection.get("bootloader_marker_present") is not True:
        raise ValueError("require independently reviewed rootfs marker and exact capsule bytes")
    AB.line(inspection["reviewer"])
    out = Path(output).resolve()
    out.mkdir(mode=0o700, parents=True, exist_ok=False)
    cli = records["cli_binary"]["path"]
    commands = {
        "agent_capsule_update": [cli, "device", "update", "--device", board["address"], "--binary", records["agent_binary"]["path"], "--artifact-url", descriptor["url"], "--yes"],
        "console_stage_without_reboot": ["/usr/bin/wendyos-update", "install", descriptor["url"]],
        "console_status": ["/usr/bin/wendyos-update", "status", "--json", "--verbose"],
        "console_readiness": ["/usr/bin/wendyos-update", "check", "--json"],
        "console_reboot_after_success": ["systemctl", "reboot"],
    }
    (out/"commands.json").write_text(json.dumps(commands, indent=2)+"\n")
    substitutions = {"@SERIAL@": board["serial"], "@ESP@": board["esp_partuuid"], "@CAPSULE@": capsule["sha256"], "@SIZE@": str(capsule["size_bytes"]), "@FROM@": transition["from_firmware"], "@TO@": transition["to_firmware"], "@GUID@": transition["esrt_firmware_guid"], "@BOOT@": str(data["boot_seconds"]), "@INSTALL@": str(plan["inventory"]["install_seconds"]), "@OFF@": str(data["power_off_seconds"]), "@AGENT@": records["agent_binary"]["sha256"], "@UPDATER@": records["updater_binary"]["sha256"], "@PAYLOAD@": m["payload"]["sha256"], "@VERSION@": m["artifact_version"]}
    guide = Path(__file__).with_suffix(".txt").read_text()
    for key, value in substitutions.items():
        guide = guide.replace(key, value)
    guide += "\nExact commands (not executed by generation):\n" + "\n".join(k+": "+shlex.join(v) for k, v in commands.items())+"\n"
    (out/"procedure.txt").write_text(guide)
    sources = [Path(__file__), Path(__file__).with_suffix(".txt"), HERE/"jetson-ab-rehearsal.py"] + [AB.ROOT/p for p in ("go/internal/agent/services/reboot.go", "go/internal/agent/services/reboot_linux.go", "go/internal/agent/osworkarounds/wdy2200.go")]
    sources += [source/p for p in ("internal/connector/tegrauefi/swap-slot.go", "internal/connector/tegrauefi/verify.go", "docs/cli-contract.md")]
    evidence = {"audit_entries": ["1682"], "hardware_execution": "not_run", "ab_plan": plan_record, "board": board, "transition": transition, "records": physical, "archive": archive, "capsule": capsule, "descriptor": descriptor_record, "payload_inspection": inspection_record, "updater_revision": AB.UPDATER_REVISION, "generation_scope": "Hashes local files and validates prepared records; never operates hardware, authenticates firmware, or promotes physical results.", "sources": [AB.record(str(p)) for p in sources], "generated": [AB.record(str(out/p)) for p in ("commands.json", "procedure.txt")], "cases": [{"id": key, "status": "not_run", "observations": [], "reviewer": None} for key in ("identity_and_capsule_baseline", "agent_capsule_reboot_and_consumption", "acknowledged_stage_power_loss", "second_boot_firmware_persistence", "agent_sync_boundary_fault_injection", "clean_shutdown_watchdog_fallback", "qualified_recovery")], "limits": ["Rootfs-only A/B success is not capsule proof.", "Current updater syncfs can mask an omitted Agent sync negative control.", "Acknowledged-stage power loss does not exercise the Agent sync-to-reboot window.", "Timing-specific cut and stuck-shutdown cases require a separately qualified rig; no rig is supplied.", "Existing pending-deployment Orin readiness refusal remains unresolved."]}
    (out/"evidence.json").write_text(json.dumps(evidence, indent=2)+"\n")
    return evidence


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("inventory")
    parser.add_argument("output")
    args = parser.parse_args()
    generate(args.inventory, args.output)
