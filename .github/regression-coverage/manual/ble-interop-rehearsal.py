#!/usr/bin/env python3
"""Generate the physical BLE interoperability procedure for audit #2534.

This program only reads local files and writes a new evidence directory.
Usage: python3 ble-interop-rehearsal.py --cli PATH --inventory inventory.json --output NEWDIR

Inventory has tenant_uuid and exactly two boards labelled A and B. Each board has
serial, device_uuid, management_selector (direct wired host:port), controller_mac,
controller_index (integer), controller_model, architecture, and setup_recipe (a
local file describing console access, Wi-Fi isolation, backups and rollback).
Each board also has current, older and rollback objects, each containing version,
commit and binary (a local agent executable for that board's architecture).
Use an older release predating #2534 that supports the same identity protocol.
Do not use a simulated legacy TLS configuration as the physical older release.
"""
import argparse
import hashlib
import itertools
import json
import os
from pathlib import Path
import platform
import re
import shlex
import subprocess
import uuid

ROOT = Path(__file__).resolve().parents[3]


def line(value, name):
    if not isinstance(value, str) or not value.strip() or any(ord(c) < 32 for c in value) or '<' in value or '>' in value:
        raise ValueError(f'{name} must be a nonempty single line without placeholders')
    return value


def artifact(value):
    p = Path(line(value, 'artifact')).expanduser().resolve(strict=True)
    if not p.is_file() or not p.stat().st_size:
        raise ValueError('artifact must be a nonempty file')
    return {'path': str(p), 'sha256': hashlib.sha256(p.read_bytes()).hexdigest()}


def write(path, content):
    with path.open('x', encoding='utf-8') as f:
        os.chmod(path, 0o600)
        f.write(content)


def generate(args):
    raw = json.loads(args.inventory.read_text())
    tenant = str(uuid.UUID(raw['tenant_uuid']))
    if len(raw['boards']) != 2 or {b['label'] for b in raw['boards']} != {'A', 'B'}:
        raise ValueError('provide exactly two boards labelled A and B')
    boards = []
    for b in sorted(raw['boards'], key=lambda b: b['label']):
        item = {k: line(b[k], k) for k in ('label', 'serial', 'management_selector', 'controller_mac', 'controller_model', 'architecture')}
        item['device_uuid'] = str(uuid.UUID(b['device_uuid']))
        if item['management_selector'].startswith(('cloud:', 'mesh:', 'bluetooth:', 'wendy-lite:')):
            raise ValueError('management_selector must use direct wired host:port')
        if not re.fullmatch(r'(?:[0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}', item['controller_mac']):
            raise ValueError('controller_mac must be an exact Bluetooth address')
        if type(b['controller_index']) is not int or b['controller_index'] < 0:
            raise ValueError('controller_index must be a nonnegative integer')
        item['controller_index'] = b['controller_index']
        item['setup_recipe'] = artifact(b['setup_recipe'])
        if len(Path(item['setup_recipe']['path']).read_text().strip()) < 60:
            raise ValueError('setup_recipe must specify console access, network isolation, backups and rollback')
        for version in ('current', 'older', 'rollback'):
            item[version] = {k: line(b[version][k], k) for k in ('version', 'commit')}
            item[version]['binary'] = artifact(b[version]['binary'])
        if item['current']['binary']['sha256'] == item['older']['binary']['sha256'] or item['current']['commit'] == item['older']['commit']:
            raise ValueError('current and older must be distinct binaries and revisions')
        boards.append(item)
    for key in ('serial', 'device_uuid', 'management_selector', 'controller_mac'):
        if len({b[key] for b in boards}) != 2:
            raise ValueError(f'boards must have distinct {key}')
    cli = artifact(str(args.cli))
    commit = subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=ROOT, text=True).strip()
    dirty = bool(subprocess.check_output(['git', 'status', '--porcelain'], cwd=ROOT, text=True).strip())
    out = args.output.resolve()
    out.mkdir(mode=0o700, parents=True, exist_ok=False)
    cmd = lambda *xs: ' '.join(shlex.quote(str(x)) for x in (cli['path'], *xs))
    a, b = boards
    instructions = [f'''Physical BLE issuer interoperability, audit #2534
Source commit: {commit}; dirty checkout: {dirty}
CLI SHA-256: {cli['sha256']}; host: {platform.platform()}
Tenant: {tenant}

Generated instructions only. No device has been accessed. Use a POSIX management
shell and two disposable Linux agents with real BLE controllers and separate USB
serial consoles. Regenerate from a clean committed checkout before execution.

Preflight
1. Record owner authorization to replace each agent binary and change its mesh
   settings. Confirm the serial, device UUID, controller MAC and architecture in
   this inventory against the physical boards. Preserve their original settings,
   running executable hashes and console recovery recipes. Do not reset enrollment.
2. Both device certificates must be valid for tenant {tenant}, with the same full
   issuing chain. Record public certificate fingerprints and issuer chain length
   without copying private keys. If the issuers differ, that is a separate fallback
   case and cannot prove shared-issuer omission. Isolate unrelated nearby devices.
3. The older binary must predate #2534 but support the same enrollment/mesh protocol.
   Record release provenance and confirm that revision in source history. Both sides
   must run each real release; do not replace an older executable with a TLS mock.
   An unavailable old release is an unavailable case, not a pass.
4. Verify systemctl status wendy-agent on each serial console and inspect its actual
   ExecStart. The stock unit uses /usr/bin/wendy-agent. Use its confirmed executable
   path for sha256sum after replacement, especially when the old agent cannot report
   its running hash. A push-agent exit code of zero alone is insufficient then.
5. Record kernel and BlueZ versions with uname -a and btmon --version. Confirm local
   btmon --help supports --index and --write. Match the captured HCI controller MAC
   to the inventory, since an hci index may change after reboot. For btmon options:
   https://github.com/bluez/bluez/blob/master/doc/btmon.rst
6. Use an isolated operator configuration signed into this test tenant. Record CLI
   version and public operator principal. Disable the management host's Bluetooth
   and disconnect it from other Wendy Net routers. Attach host and A to a dedicated
   wired LAN. B will retain only its USB serial console after configuration. The
   console must not create an IP route. Do not enable Internet sharing on either.
7. Capture UTC start/end, exit code, topology, both agent logs and both HCI traces per
   case. Device info has a 30-second deadline; agent replacement has five minutes;
   path discovery/recovery has two minutes. Interrupt timed-out commands, mark fail,
   and preserve evidence before recovery. Never overwrite an earlier failed attempt.
''']
    for board in boards:
        write(out / f"{board['label']}-setup.txt", Path(board['setup_recipe']['path']).read_text())
        instructions.append(f'''Board {board['label']}: serial {board['serial']}; UUID {board['device_uuid']}
Wired management: {board['management_selector']}; architecture: {board['architecture']}
Controller: {board['controller_mac']}, index {board['controller_index']}, model {board['controller_model']}
Recipe: {board['label']}-setup.txt; SHA-256 {board['setup_recipe']['sha256']}
Before each case, start on this board's serial console in a new case directory:
  sudo btmon --index {board['controller_index']} --write hci.btsnoop
Keep this foreground capture running in a separate console, stop with Ctrl+C at the
case deadline. Record its actual file path and any dropped-packet indication.
Afterward export the same UTC interval, using the recorded start/end timestamps:
  sudo journalctl -u wendy-agent --since 'CASE_START_UTC' --until 'CASE_END_UTC' --no-pager
Recheck the controller address after power changes; do not silently capture hci0.
''')
    cases = []
    for av, bv in itertools.product(('older', 'current'), repeat=2):
        case = f'A-{av}-B-{bv}'
        instructions.append(f'''Matrix {case}
1. Restore both wired management links. Verify the exact identities before updates.
   With the recorded owner's approval, install these architecture-matched binaries:
   {cmd('device', 'push-agent', a[av]['binary']['path'], '--device', a['management_selector'])}
   A expected version {a[av]['version']}, revision {a[av]['commit']}, SHA-256 {a[av]['binary']['sha256']}
   {cmd('device', 'push-agent', b[bv]['binary']['path'], '--device', b['management_selector'])}
   B expected version {b[bv]['version']}, revision {b[bv]['commit']}, SHA-256 {b[bv]['binary']['sha256']}
   Check the running executable hash from the serial console if not reported by RPC.
   Confirm UUID and tenant are unchanged; stop on any replacement or identity mismatch.
2. Configure A for host attachment over Ethernet plus BLE, and B for BLE only:
   {cmd('device', 'wendy-net', 'configure', '--participate=true', '--ble=true', '--ethernet=true', '--nan=false', '--infrastructure-wifi=false', '--roam=false', '--share-uplink=false', '--device', a['management_selector'])}
   {cmd('device', 'wendy-net', 'configure', '--participate=true', '--ble=true', '--ethernet=false', '--nan=false', '--infrastructure-wifi=false', '--roam=false', '--share-uplink=false', '--device', b['management_selector'])}
   {cmd('--json', 'device', 'wendy-net', 'status', '--device', a['management_selector'])}
   {cmd('--json', 'device', 'wendy-net', 'status', '--device', b['management_selector'])}
   Verify saved values. If the old API cannot express them, use only a documented
   equivalent in its copied recipe and verify the same state; otherwise unavailable.
3. Physically remove B's Ethernet. Disable its infrastructure Wi-Fi using its exact
   recorded console recipe. Confirm B has no external IP path, no Cloud tunnel and
   no other mesh carrier. Keep only its BLE radio and serial console. A stays wired.
4. Restart both agents from their serial consoles with sudo systemctl restart
   wendy-agent. This clears in-process session state for the cold case. Start both
   HCI captures first and record restart times; preserve identity files untouched.
   {cmd('device', 'wendy-net', 'connect')}
   {cmd('--json', 'device', 'wendy-net', 'topology', '--device', a['management_selector'])}
   {cmd('--json', 'device', 'info', '--read-only', '--device', 'mesh:' + b['device_uuid'])}
   Require exact B UUID/tenant/version in the successful info response. Topology and
   the two HCI captures must show A-to-B over BLE. A direct or Cloud fallback, a mere
   discovery row or a TLS log without an application response does not pass.
5. Match inbound/outbound TLS-established events to the same controller connection,
   peer UUID and timestamps. Record which board dialed and which accepted. Record
   admission_wait, handshake_time, resumed and peer_certificates on current agents.
   Cold current/current with matching issuers must report resumed=false and one peer
   certificate at BOTH ends. Mixed releases must successfully exchange full chains;
   the current endpoint must observe the full chain length recorded in preflight.
   Older/older is the baseline application-success case. Missing old log fields
   cannot be invented; retain the current endpoint plus HCI and RPC evidence.
6. Repeat the info request ten times, then cold-power-cycle B for ten seconds while
   A remains running. Recheck B's serial/controller identity, restart its capture,
   and repeat topology plus ten info requests. Require recovery within two minutes,
   the same UUID, no enrollment and no alternate transport. Record actual TLS
   resumption state rather than requiring it: power loss may discard session keys.
   The pinned reconnect must still authenticate the intended device.
7. Repeat steps 3-6 with A isolated and B wired to the management host: enable only
   B's Ethernet mesh setting, disable only A's Ethernet mesh setting, unplug A's
   Ethernet and disable its Wi-Fi by its recipe. Request mesh:{a['device_uuid']}.
   Record observed TLS roles. Routing direction alone is not proof of a different
   TLS initiator; swapping old/current binaries across the UUIDs in this matrix
   supplies old-client and old-server coverage with the actual observed dial policy.
8. Stop captures, record outcomes for both directions below, and restore both wired
   links before the next matrix. Preserve every first failure and subsequent recovery.
''')
        for direction in ('A-to-B', 'B-to-A'):
            for phase in ('cold', 'pinned-reconnect'):
                cases.append({'id': f'{case}-{direction}-{phase}', 'status': 'not_run', 'versions': {'A': av, 'B': bv}, 'direction': direction, 'phase': phase, 'started_utc': None, 'finished_utc': None, 'observed_tls_client': None, 'observed_tls_server': None, 'peer_certificate_counts': {}, 'handshake_times': {}, 'evidence_files': [], 'notes': ''})
    instructions.append(f'''Slow physical link observation and cleanup
- For current/current, repeat the cold case in a controlled attenuated RF setup or
  at a recorded fixed separation that still maintains the connection. Record the
  setup and actual HCI traffic/time, TLS admission and handshake times. Require a
  successful application response and one peer certificate per end. Do not claim
  exactly 1 KB/s or 300 ms latency without measurement; the deterministic CI fixture
  separately enforces that modeled budget. Do not introduce uncontrolled interference.
- Inbound current TLS has a 30-second handshake budget after admission; outbound has
  60 seconds including peer admission. Compare log durations to those distinct
  limits. A timeout within the budget is a failure, not proof of interoperability.
- On failure preserve kernel, agent and HCI logs before restoring wired management.
  Use each copied console recipe if the agent is unreachable. Do not delete keys,
  enrollment or bonds to make a failing case pass. Record any necessary repair.
- With the recorded owner's approval restore each original binary:
  {cmd('device', 'push-agent', a['rollback']['binary']['path'], '--device', a['management_selector'])}
  {cmd('device', 'push-agent', b['rollback']['binary']['path'], '--device', b['management_selector'])}
  Verify A SHA-256 {a['rollback']['binary']['sha256']} and B SHA-256 {b['rollback']['binary']['sha256']}.
  Restore saved mesh, network and host Bluetooth settings from the recipes. Verify
  the original UUID/tenant and authenticated direct device info. Stop only test
  captures and disconnect the test operator with:
  {cmd('device', 'wendy-net', 'disconnect')}
- HCI traces can contain sensitive Bluetooth data. Keep raw captures access-limited,
  use only the disposable test devices, and publish sanitized observations. Never
  attach enrollment private keys or the operator configuration.
- Fill each evidence.json result as pass, fail or unavailable with actual versions,
  observed TLS roles, exact timestamps and evidence paths. Script readiness alone
  proves no hardware case. The cross-version job is complete only when every required
  version/direction/phase passes or the unsupported case is explicitly reviewed.
''')
    cases.append({'id': 'current-current-controlled-slow-link', 'status': 'not_run', 'evidence_files': [], 'notes': ''})
    write(out / 'instructions.txt', '\n'.join(instructions))
    write(out / 'evidence.json', json.dumps({'audit_entries': ['2534'], 'source_commit': commit, 'source_dirty': dirty, 'cli': cli, 'tenant_uuid': tenant, 'boards': boards, 'hardware_execution': 'not_run', 'authorization': None, 'cases': cases}, indent=2) + '\n')
    print(f'Wrote {len(cases)} unexecuted physical cases to {out}')


def main():
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument('--cli', type=Path, required=True)
    p.add_argument('--inventory', type=Path, required=True)
    p.add_argument('--output', type=Path, required=True)
    try:
        generate(p.parse_args())
    except (ValueError, KeyError, OSError, subprocess.CalledProcessError) as error:
        p.error(str(error))


if __name__ == '__main__':
    main()
