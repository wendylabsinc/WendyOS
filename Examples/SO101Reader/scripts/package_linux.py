#!/usr/bin/env python3
"""Stage a self-contained ordinary Linux Wendy project without editing source config."""

import argparse
import json
from pathlib import Path
import re
import shutil

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--port", help="Explicit Linux container USB tty path, e.g. /dev/ttyACM0")
parser.add_argument("--mock", action="store_true", help="Synthetic transport; no serial entitlement")
parser.add_argument("--http-port", type=int, default=8876)
args = parser.parse_args()
if args.mock and args.port:
    parser.error("--mock and --port are mutually exclusive")
if not 1 <= args.http_port <= 65535:
    parser.error("--http-port must be 1..65535")
if args.port and not re.fullmatch(r"/dev/(ttyACM|ttyUSB)[0-9]+", args.port):
    parser.error("Wendy serial entitlement requires /dev/ttyACM<N> or /dev/ttyUSB<N>; resolve by-id on the Linux host first")
root = Path(__file__).resolve().parents[1]
dest = root / ".build" / "linux"
dest.mkdir(parents=True, exist_ok=True)
for name in ("reader.py", "protocol.py", "transport.py", "identity.py", "Dockerfile", ".dockerignore"):
    shutil.copy2(root / name, dest / name)
config = json.loads((root / "wendy.linux.json").read_text())
config["env"].update(SO101_MODE="mock" if args.mock else "physical", SO101_HTTP_PORT=str(args.http_port))
config["entitlements"][1]["port"] = args.http_port
config["readiness"]["tcpSocket"]["port"] = args.http_port
if args.port:
    config["env"]["SO101_PORT"] = args.port
    config["entitlements"].append({"type": "serial", "device": args.port.removeprefix("/dev/")})
(dest / "wendy.json").write_text(json.dumps(config, indent=2) + "\n")
print(dest)
