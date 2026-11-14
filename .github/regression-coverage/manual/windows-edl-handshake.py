#!/usr/bin/env python3
r"""Physical Windows EDL regression for audit #1930.

Run from a clean checkout in a Windows terminal, with Go installed. Use a
Dragonwing IQ-8275 or IQ-9075 and its matching extracted, trusted flash bundle.
Disconnect every other Qualcomm EDL board. Back up work on the selected board:
this test resets it, but sends no program/patch commands to its storage.

Power the board off. Set IQ-8275 S2-8 ON, or IQ-9075 MODE switch 3 ON. Connect a
data-capable USB cable to the recovery port and power on. In PowerShell obtain
the exact instance, retaining the serial suffix:
  Get-PnpDevice -PresentOnly | Where-Object InstanceId -like 'USB\VID_05C6&PID_9008*' | Format-List FriendlyName,Status,InstanceId

The selected device needs Wendy's compatible signed WinUSB binding. If access
is refused, build the CLI and run ONLY its driver helper in elevated PowerShell:
  New-Item -ItemType Directory -Force build
  go build -o .\build\wendy-regression.exe ./go/cmd/wendy
  .\build\wendy-regression.exe __usb-driver 'USB\VID_05C6&PID_9008\<exact-instance>'
This explicitly installs/updates that device's host driver; it does not flash.
Then return to the ordinary terminal and repeat this script.

Example, substituting the exact instance and a checksum obtained from your
trusted matching bundle before execution:
  python .github/regression-coverage/manual/windows-edl-handshake.py --board iq9075 --instance 'USB\VID_05C6&PID_9008\SERIAL' --bundle C:\fixtures\iq9075 --programmer-sha256 <64-hex-digits>

Pass requires the same-connection chip read/upload/configure/reset test to
report PASS, with no skips. Preserve report.json and handshake.log. The script
records the source commit, board, instance, programmer digest and toolchain.
On failure, preserve those files, set the recovery switch OFF and power-cycle;
do not substitute a different target or programmer to obtain a passing result.
After success also set the named switch OFF and power-cycle into the existing
OS. Verify its expected identity using `wendy discover --json`; attach that
output to the report. A passed handshake alone does not prove a full flash.
"""

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile

ROOT = Path(__file__).resolve().parents[3]
BOARDS = {"iq8275": ("0x002e70e1", "S2-8"), "iq9075": ("0x002eb0e1", "MODE switch 3")}


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--board", choices=BOARDS, required=True)
    parser.add_argument("--instance", required=True)
    parser.add_argument("--bundle", type=Path, required=True)
    parser.add_argument("--programmer-sha256", required=True)
    args = parser.parse_args()
    if os.name != "nt":
        parser.error("this physical regression requires a native Windows host")
    if not re.fullmatch(r"USB\\VID_05C6&PID_9008[^\\]*\\[^\r\n]+", args.instance, re.I):
        parser.error("supply the complete Qualcomm EDL PnP instance, including its serial suffix")
    expected = args.programmer_sha256.lower()
    if not re.fullmatch(r"[a-f0-9]{64}", expected):
        parser.error("programmer checksum must contain 64 hexadecimal digits")
    programmer = args.bundle.resolve() / "prog_firehose_ddr.elf"
    if not programmer.is_file() or hashlib.sha256(programmer.read_bytes()).hexdigest() != expected:
        parser.error("the programmer is missing or differs from the expected checksum")
    dirty = subprocess.check_output(["git", "status", "--porcelain"], cwd=ROOT, text=True)
    if dirty.strip():
        parser.error("use a clean checkout so the hardware result identifies an exact source commit")
    commit = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip()
    version = subprocess.check_output(["go", "version"], cwd=ROOT, text=True).strip()
    msm, switch = BOARDS[args.board]
    print(f"Board: {args.board}; instance: {args.instance}; programmer SHA-256: {expected}")
    print("This uploads to RAM and resets the selected board. No storage programming is performed.")
    if input(f"Type RESET {args.instance} to run: ").strip() != f"RESET {args.instance}":
        parser.error("reset was not confirmed")
    output = Path(tempfile.mkdtemp(prefix="wendy-edl-regression-"))
    env = dict(os.environ, WENDY_HW_EDL_BUNDLE=str(args.bundle.resolve()),
               WENDY_HW_EDL_INSTANCE=args.instance, WENDY_HW_EDL_EXPECT_MSM=msm,
               WENDY_HW_EDL_PROGRAMMER_SHA256=expected)
    command = ["go", "test", "./go/internal/cli/qdl", "-run", "^TestHardwareEDLHandshake$", "-v", "-count=1", "-timeout=120s"]
    with subprocess.Popen(command, cwd=ROOT, env=env, text=True, encoding="utf-8", errors="replace",
                          stdout=subprocess.PIPE, stderr=subprocess.STDOUT) as process:
        try:
            log, _ = process.communicate(timeout=600)
            code = process.returncode
        except subprocess.TimeoutExpired:
            subprocess.run(["taskkill", "/PID", str(process.pid), "/T", "/F"],
                           stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=False)
            log, _ = process.communicate()
            log += "\nHardware test exceeded its outer 600-second deadline.\n"
            code = 124
    (output / "handshake.log").write_text(log, encoding="utf-8")
    passed = code == 0 and "--- PASS: TestHardwareEDLHandshake " in log and "--- SKIP:" not in log
    (output / "report.json").write_text(json.dumps(dict(audit_entries=["1930"], commit=commit,
        board=args.board, instance=args.instance, programmer_sha256=expected, go=version,
        command=command, passed=passed, storage_programmed=False), indent=2)+"\n", encoding="utf-8")
    print(log)
    print(f"Evidence: {output}")
    print(f"Set {switch} OFF and power-cycle. Verify the existing OS identity before using the board again.")
    return 0 if passed else 1


if __name__ == "__main__":
    sys.exit(main())
