#!/usr/bin/env python3
"""Generate the physical T234 recovery rehearsal for audit #2057.

This generates commands and an evidence form; it does not flash a device.
Run each case on a disposable Orin with a backed-up NVMe/eMMC and a known
recovery cable. The output contains the target-specific erase confirmation,
hub/reconnect procedure, deadlines, expected results and recovery steps.
Repeat on macOS, Windows and Linux with their matching CLI build. Never mark
an unexecuted host/board combination as passed.

Example:
  python .github/regression-coverage/manual/t234-recovery-rehearsal.py --cli /absolute/path/wendy --board jetson-orin-nano --storage nvme --serial SERIAL --usb-port 'recorded physical path' --version VERSION --address 192.0.2.10:50051 --output /tmp/t234-rehearsal

Before running, build the CLI from this checkout with:
  go build -o /absolute/path/wendy ./go/cmd/wendy
Use the native .exe path on Windows. Use an actual reachable device address,
not the documentation address in the example. Match the CLI's displayed board,
serial/ECID and physical USB path to the supplied values before approving any
write. The CLI still owns its own interactive erase confirmation.
"""
import argparse
import hashlib
import json
import platform
from pathlib import Path
import shlex
import subprocess

ROOT = Path(__file__).resolve().parents[3]


def main():
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument('--cli', required=True, type=Path)
    p.add_argument('--board', choices=['jetson-orin-nano', 'jetson-agx-orin'], required=True)
    p.add_argument('--storage', choices=['nvme', 'emmc'], required=True)
    p.add_argument('--serial', required=True)
    p.add_argument('--usb-port', required=True)
    p.add_argument('--version', required=True)
    p.add_argument('--address', required=True)
    p.add_argument('--output', type=Path, required=True)
    a = p.parse_args()
    if a.board == 'jetson-orin-nano' and a.storage != 'nvme':
        p.error('Orin Nano recovery supports NVMe, not eMMC')
    if not a.cli.is_file():
        p.error('build the native CLI first and supply its absolute path')
    for value in [a.serial, a.usb_port, a.version, a.address]:
        if not value.strip() or any(ord(c) < 32 for c in value):
            p.error('identity/version/address values must be nonempty single-line values')
    if subprocess.check_output(['git', 'status', '--porcelain'], cwd=ROOT, text=True).strip():
        p.error('use a clean checkout so the evidence identifies an exact commit')
    commit = subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=ROOT, text=True).strip()
    cli = str(a.cli.resolve())
    args = [cli, 'install', '--device-type', a.board, '--storage', a.storage,
            '--rootfs-only=false', '--version', a.version, '--device-name', 'regression-t234',
            '--no-wifi', '--pre-enroll=false']
    verify = [cli, 'install', 'verify', '--address', a.address,
              '--expected-device-type', a.board, '--expected-os-version', a.version, '--timeout', '1m']
    quote = (lambda x: "'"+x.replace("'", "''")+"'") if platform.system() == 'Windows' else shlex.quote
    prefix = '& ' if platform.system() == 'Windows' else ''
    command = lambda xs: prefix + ' '.join(map(quote, xs))
    recovery = ('With power off, jumper pin 9 GND to pin 10 FC REC on the developer kit, '
                'connect its recovery USB-C port, then power on and remove the jumper.'
                if a.board == 'jetson-orin-nano' else
                'Connect the developer kit recovery USB port. Hold Force Recovery, press and '
                'release Reset, then release Force Recovery. Follow the carrier vendor instructions '
                'instead if this is not the NVIDIA developer kit.')
    cases = [
        dict(id='direct', setup='Connect directly to the recorded host USB port. Keep all other recovery boards disconnected.',
             action='Complete a normal recovery flash. Record the final write/flush completion and exit status before disconnecting anything.',
             expected='Exit zero only after all writes, flushes and config seeding complete; first-boot verification succeeds.'),
        dict(id='nested-hub', setup='Connect the same board through a powered nested hub. Record the new full physical USB path and hub model before approving the displayed target.',
             action='Complete recovery through the hub. Save host USB/disk inventories before RCM, during the flashing gadget and after reboot.',
             expected='Discovery correlates the correct board across RCM and UMS enumeration; no unrelated disk is selected; completion and first boot succeed.'),
        dict(id='detach-reconnect', setup='Use the same recorded hub and port with only the disposable target attached.',
             action='After the progress display confirms rootfs writing, unplug ONLY the target USB cable for five seconds, then reconnect it to the same port without powering off the board. Record timestamps. Do not attach a replacement disk.',
             expected='The installer either resumes only after verifying the same device identity or exits with an explicit failure. It must never claim success with incomplete data, select a different target, or hang indefinitely. If it fails, perform a new uninterrupted recovery before first-boot verification.'),
        dict(id='slow-transport-and-flush', setup='Use a powered USB 2 hub or a known slow disposable target. Record negotiated USB speed and storage model. Keep power stable.',
             action='Run a full recovery and record the time from final data-write progress to actual completion. Do not unplug while the final flush is pending. If no delayed flush occurs, mark delayed_flush_observed=false; do not claim that condition was exercised.',
             expected='Slow transfer/flush does not produce premature success or select another disk. Host-specific flush handling completes or reports a bounded explicit error; successful completion passes first-boot verification.')]
    text = f'''T234 physical recovery rehearsal, audit #2057
Source commit: {commit}
Host: {platform.platform()}
Board: {a.board}; serial/ECID: {a.serial}; initial USB path: {a.usb_port}
Storage erased: QSPI plus all {a.storage} data, including /data.

1. Reserve the disposable board and storage. Back up all data. Disconnect other
   recovery boards and removable storage. Connect Ethernet for first boot.
   Record board/carrier, storage and hub models, CLI checksum and OS version.
2. Start a host terminal transcript. On Windows use Start-Transcript with a new
   output file; on macOS/Linux use script with a new output file. Keep credentials
   out of the transcript. Inventory USB/disk identities with:
   macOS: system_profiler SPUSBDataType; diskutil list
   Linux: lsusb -t; lsblk -o NAME,SIZE,MODEL,SERIAL,TYPE,MOUNTPOINTS
   Windows PowerShell: Get-PnpDevice -PresentOnly; Get-Disk
3. Enter recovery: {recovery}
4. Before EACH case, compare the CLI's selected board identity and physical path
   to the recorded target. Type the following into the evidence form only after
   the owner authorizes the erase, and separately approve the CLI confirmation:
   ERASE {a.serial} QSPI AND {a.storage.upper()}
   A mismatch, ambiguous identity or missing path means stop, not choose another disk.

Install command, run manually for EACH case below:
{command(args)}

Per-case bounds: allow up to 45 minutes after confirming the erase, including
slow transfer/flush. Record progress timestamps. If no result by that deadline,
mark failed, interrupt once and allow 60 seconds for cleanup. Do not start a
second writer or unplug until the writer/helper exits. If it cannot exit, save
the transcript and shut down the dedicated test host before recovering the board.
Never treat an interrupted or timed-out run as passed.
'''
    for c in cases:
        text += f"\nCASE {c['id']}\nSetup: {c['setup']}\nAction: {c['action']}\nExpected: {c['expected']}\n"
    text += f'''
After each successful install, remove recovery jumpers, reboot normally and
allow up to five minutes for Ethernet discovery. Confirm that the board named
regression-t234 at the supplied address is physically the selected board. Run:
{command(verify)}
Save its JSON, including reported version, board and identity. A successful
writer alone does not prove first boot. A failed verification keeps the case
failed. Once identity is recorded, also repeat verify with --expected-public-key
and that exact key to rule out another device answering at the reused address.

After detach failure, record that failure before retrying. Use the same board,
known-good direct cable and a fresh uninterrupted recovery to restore the target.
At the end, stop the transcript, remove recovery jumpers, disconnect the test hub
and verify the restored OS. Attach all transcripts, inventories and verification
JSON to the evidence form. Record unsupported/unavailable hosts as not_run.
Do not store Wi-Fi passwords or cloud credentials in the evidence bundle.
'''
    a.output.mkdir(parents=True, exist_ok=False)
    (a.output/'instructions.txt').write_text(text)
    report = dict(audit_entries=['2057'], commit=commit, host=platform.platform(),
                  cli=cli, cli_sha256=hashlib.sha256(a.cli.read_bytes()).hexdigest(),
                  board=a.board, serial=a.serial, initial_usb_path=a.usb_port,
                  version=a.version, storage=a.storage, hardware_execution='not_run',
                  cases=[dict(c, result='not_run', erase_authorization='', transcript='',
                              usb_inventory='', disk_inventory='', elapsed_seconds=None,
                              write_completed_at='', flush_completed_at='', exit_code=None,
                              delayed_flush_observed=None, verification_json='', notes='') for c in cases])
    (a.output/'evidence.json').write_text(json.dumps(report,indent=2)+'\n')
    print(f'Generated {a.output}/instructions.txt and evidence.json. No hardware action was executed.')


if __name__ == '__main__':
    main()
