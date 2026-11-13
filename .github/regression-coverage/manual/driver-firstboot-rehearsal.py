#!/usr/bin/env python3
"""Prepare exact-target audit2628 driver acceptance; never contact a device."""
import argparse
import base64
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import shlex
import subprocess
from urllib.parse import urlsplit

ROOT = Path(__file__).resolve().parents[3]
HELPER = Path(__file__).with_name("orin-firstboot-rehearsal.py")
SPEC = importlib.util.spec_from_file_location("driver_firstboot_helpers", HELPER)
COMMON = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(COMMON)
line, artifact = COMMON.line, COMMON.artifact
BUILDER_REVISION = "f72a1b354c80523dc2b092e52e38e44fe56c8d5a"
BUILDER_PATH = "drivers/wendyos-hello/wendyos_hello.c"
GCS = "https://storage.googleapis.com/wendyos-images-public/"


def public_url(value):
    parsed = urlsplit(line(value))
    if parsed.scheme != "https" or parsed.netloc != "storage.googleapis.com" or not parsed.path.startswith("/wendyos-images-public/") or parsed.fragment or (parsed.query and not re.fullmatch(r"generation=[0-9]+", parsed.query)):
        raise ValueError("require exact public artifact URL, optionally pinned by GCS generation")
    if any(part in ("latest", "nightly", "..") for part in parsed.path.split("/")):
        raise ValueError("moving or relative artifact path")
    return parsed._replace(query="").geturl()


def generate(inventory, output):
    data = json.loads(Path(inventory).read_text())
    def reject_secrets(value):
        if isinstance(value, dict):
            for key, child in value.items():
                if any(word in key.lower() for word in ("password", "passphrase", "private_key", "token", "eabhmac")):
                    raise ValueError("inventory must not contain credentials")
                reject_secrets(child)
        elif isinstance(value, list):
            for child in value: reject_secrets(child)
    reject_secrets(data)
    board = data["board"]
    allowed = {"jetson-orin-nano": ("nvme",), "jetson-agx-orin": ("nvme", "emmc")}
    if board.get("carrier") != "developer-kit" or board.get("storage") not in allowed.get(board.get("device"), ()):
        raise ValueError("require a supported Orin developer-kit storage pair")
    for key in ("serial", "ecid", "owner", "storage_serial", "usb_path", "serial_console"):
        line(board[key])
    if not re.fullmatch(r"[a-z][a-z0-9-]{2,54}", board["device_name"]) or board["device_name"].endswith("-"):
        raise ValueError("require a unique valid provisioned device name")
    for key in ("usb_address_before", "usb_address_after"):
        address = line(board[key])
        # USB fallback must be a real link-local endpoint, not cloud/NAT/loopback.
        import ipaddress
        host, port = address.rsplit(":", 1)
        ip = ipaddress.ip_address(host.strip("[]"))
        if not ip.is_link_local or port != "50051":
            raise ValueError("require explicit link-local USB endpoint on port50051")
    if board["usb_address_before"] == board["usb_address_after"]:
        raise ValueError("record distinct observed pre/post first-boot endpoints")
    for key in ("cli_source_commit", "agent_source_commit"):
        if not re.fullmatch(r"[a-f0-9]{40}", data[key]): raise ValueError("pin full candidate source commits")
    if type(data["disconnect_seconds"]) is not int or not 15 <= data["disconnect_seconds"] <= 120:
        raise ValueError("bound the powered USB disconnect to15..120seconds")
    keys = ("cli_binary", "agent_binary", "release_descriptor", "release_archive", "device_catalog", "driver_raw", "signature_review", "identity_record", "recovery_record", "authorization_record", "network_isolation_record")
    records = {key: artifact(data[key]) for key in keys}
    key_path = Path(line(data["signing_public_key"])).resolve(strict=True)
    if not key_path.is_file() or key_path.stat().st_size > 65536:
        raise ValueError("require the bounded actual build public-key file, including an empty placeholder")
    key_bytes = key_path.read_bytes()
    records["signing_public_key"] = {"path":str(key_path), "sha256":hashlib.sha256(key_bytes).hexdigest(), "size_bytes":len(key_bytes)}
    if not os.access(records["cli_binary"]["path"], os.X_OK): raise ValueError("CLI must be executable")
    release = json.loads(Path(records["release_descriptor"]["path"]).read_text())
    if any(release.get(k) != board[k] for k in ("device", "storage")):
        raise ValueError("flashpack descriptor must match this board/storage")
    version = release["version"]
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]*", version) or version in ("latest", "nightly"):
        raise ValueError("pin immutable OS version")
    archive = records["release_archive"]
    if release.get("sha256") != archive["sha256"] or type(release.get("size_bytes")) is not int or release["size_bytes"] != archive["size_bytes"]:
        raise ValueError("flashpack bytes differ from published descriptor")
    catalog = json.loads(Path(records["device_catalog"]["path"]).read_text())
    if catalog["device_id"] != board["device"]: raise ValueError("catalog board mismatch")
    entry = catalog["versions"][version]
    prefix = board["storage"]+"_flashpack_"
    if public_url(release["url"]) != public_url(GCS+entry[prefix+"path"]) or not release["url"].split("?", 1)[0].endswith(".flashpack.tar.zst") or entry[prefix+"checksum"] != archive["sha256"] or entry[prefix+"size_bytes"] != archive["size_bytes"]:
        raise ValueError("catalog and full flashpack identity differ")
    kernel = line(entry["devkit"]["kernel_version"])
    if not re.fullmatch(r"[A-Za-z0-9._+-]+", kernel): raise ValueError("invalid exact kernel release")
    extensions = [e for e in entry["extensions"] if e["name"] == "wendyos-hello" and e["kernel_version"] == kernel]
    if len(extensions) != 1: raise ValueError("require one published hello driver for the exact image kernel")
    driver = extensions[0]
    raw = records["driver_raw"]
    if driver["sha256"] != raw["sha256"] or type(driver["size_bytes"]) is not int or driver["size_bytes"] != raw["size_bytes"] or driver["modules_load"] != ["wendyos_hello"]:
        raise ValueError("driver bytes/module identity differ from catalog")
    public_url(GCS+driver["path"])
    signature = base64.b64decode(driver["signature"], validate=True)
    if len(signature) != 3309: raise ValueError("require a complete ML-DSA65 signature")
    review = json.loads(Path(records["signature_review"]["path"]).read_text())
    expected = {"driver_sha256":raw["sha256"], "signature_sha256":hashlib.sha256(signature).hexdigest(), "agent_sha256":records["agent_binary"]["sha256"], "public_key_sha256":records["signing_public_key"]["sha256"], "agent_signature_verification_enabled":bool(key_bytes.strip())}
    if any(review.get(k) != v for k,v in expected.items()) or type(review.get("agent_signature_verification_enabled")) is not bool:
        raise ValueError("require reviewed signature/key/build binding for this candidate")
    line(review["reviewer"])
    # Read immutable source objects, never execute the supplied builder checkout.
    module = subprocess.check_output(["git", "show", BUILDER_REVISION+":"+BUILDER_PATH], cwd=Path(data["builder_source"]).resolve(strict=True), timeout=10)
    if b'wendyos_hello: driver add-on loaded (sysext pipeline OK)' not in module:
        raise ValueError("unexpected pinned hello module provenance")
    out = Path(output).resolve()
    out.mkdir(mode=0o700, parents=True, exist_ok=False)
    cli = records["cli_binary"]["path"]
    config = out/"cli-config"
    config.mkdir(mode=0o700)
    prefix = ["env", "WENDY_CONFIG_DIR="+str(config), "WENDY_SECRET_STORE=file", "WENDY_ANALYTICS=false", "WENDY_NO_UPDATE_CHECK=1", cli]
    selection = ["--device-type",board["device"],"--storage",board["storage"],"--version",version]
    install = [*prefix,"install",*selection,"--device-name",board["device_name"],"--binary",records["agent_binary"]["path"],"--pre-enroll=false","--no-wifi"]
    target = ["--device",board["usb_address_after"]]
    commands = {"plan":[*prefix,"install","plan",*selection,"--carrier","developer-kit"], "install_with_driver":install+["--driver","wendyos-hello"], "fresh_registry_baseline":install,
                "verify":[*prefix,"install","verify","--address",board["usb_address_after"],"--expected-device-type",board["device"],"--expected-os-version",version,"--timeout","30s"],
                "info":[*prefix,"--json","device","info",*target], "wifi_status":[*prefix,"--json","device","wifi","status",*target],
                "available":[*prefix,"--json","device","drivers","list","--available",*target], "list":[*prefix,"--json","device","drivers","list",*target],
                "registry_install":[*prefix,"device","drivers","install","wendyos-hello",*target], "remove_owned_driver":[*prefix,"device","drivers","remove","wendyos-hello",*target]}
    (out/"commands.json").write_text(json.dumps(commands,indent=2)+"\n")
    guide = Path(__file__).with_suffix(".txt").read_text()
    for key,value in {"@SERIAL@":board["serial"],"@NAME@":board["device_name"],"@KERNEL@":kernel,"@DISCONNECT@":str(data["disconnect_seconds"]),"@RAW@":raw["sha256"],"@AGENT@":records["agent_binary"]["sha256"]}.items(): guide=guide.replace(key,value)
    guide += "\nExact commands, never executed by generation:\n"+"\n".join(key+": "+shlex.join(value) for key,value in commands.items())+"\n"
    (out/"procedure.txt").write_text(guide)
    sources = [Path(__file__),Path(__file__).with_suffix(".txt"),HELPER]+[ROOT/p for p in ("go/internal/cli/commands/os_install_drivers.go","go/internal/cli/commands/drivers_upload.go","go/internal/cli/commands/drivers_install.go","go/internal/cli/commands/manifest.go","go/internal/cli/commands/device_organization.go","go/internal/cli/commands/os_cmd.go","go/internal/agent/services/driver_service.go","go/internal/shared/sigverify/sigverify.go")]
    evidence = {"audit_entries":["2628"],"hardware_execution":"not_run","inventory_record":artifact(str(Path(inventory).resolve())),"inventory":data,"records":records,"release":release,"driver":driver,"kernel":kernel,
                "signature_policy":{"candidate_verifier_enabled":bool(key_bytes.strip()),"generation_verifies_signature":False},
                "sources":[artifact(str(p)) for p in sources],"module_provenance":{"revision":BUILDER_REVISION,"path":BUILDER_PATH,"sha256":hashlib.sha256(module).hexdigest()},
                "generated":[artifact(str(out/p)) for p in ("commands.json","procedure.txt")],
                "cases":[{"id":case,"status":"not_run","observations":[],"reviewer":None} for case in ("identity_catalog_signature","powered_disconnect_address_change","postflash_activation","second_boot_activation","unenrolled_registry_upload","registry_second_boot","timeout_partial_success","owned_cleanup")],
                "scope":"Generation validates hashes and reviewed metadata, not ML-DSA cryptography or flashpack/rootfs contents. Physical writes, USB address changes, upload transport, module activation and second boots are unexecuted."}
    (out/"evidence.json").write_text(json.dumps(evidence,indent=2)+"\n")
    return evidence


if __name__ == "__main__":
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument("inventory",type=Path)
    parser.add_argument("output",type=Path)
    args=parser.parse_args()
    generate(args.inventory,args.output)
