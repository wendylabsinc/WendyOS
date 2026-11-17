#!/usr/bin/env python3
"""Prepare audit2246 nested-hub USB arm acceptance without device I/O."""
import argparse
from datetime import datetime
import hashlib
import json
from pathlib import Path
import re
import shlex
import subprocess
import uuid

ROOT = Path(__file__).resolve().parents[3]
CASES = ('nested-hub-inventory', 'save-name', 'unplug', 'same-port-replug',
         'changed-tty-and-hub-port', 'unnamed-rebind', 'agent-restart', 'forget-owned-binding')


def line(value):
    if not isinstance(value, str) or not value.strip() or any(ord(c) < 32 for c in value):
        raise ValueError('required nonempty single-line identity')
    return value


def artifact(value):
    p = Path(value)
    if p.is_symlink() or not p.is_file():
        raise ValueError('provenance must be an existing regular file')
    return {'path': str(p.resolve()), 'sha256': hashlib.sha256(p.read_bytes()).hexdigest()}


def generate(inventory, output):
    raw = json.loads(inventory.read_text())
    if raw['platform'] not in ('macos', 'linux'):
        raise ValueError('requires native Mac Agent or native Linux Agent')
    for key in ('hostname', 'target', 'os_version', 'agent_version', 'owner', 'captured_at'):
        line(raw[key])
    if datetime.fromisoformat(raw['captured_at'].replace('Z', '+00:00')).tzinfo is None:
        raise ValueError('captured_at must include its timezone')
    adapter = raw['adapter']
    for key in ('id', 'usb_location', 'usb_interface', 'device_path', 'physical_label'):
        line(adapter[key])
    for key in ('vendor_id', 'product_id'):
        if not re.fullmatch('[0-9a-f]{4}', adapter[key]):
            raise ValueError('USB IDs must be exact lowercase hex')
    path_pattern = r'/dev/cu\.[A-Za-z0-9_.-]+' if raw['platform'] == 'macos' else r'/dev/tty(?:USB|ACM)[0-9]+'
    if not re.fullmatch(path_pattern, adapter['device_path']):
        raise ValueError('require exact native tty path')
    serial = adapter['serial_number']
    if not isinstance(serial, str): raise ValueError('missing serial is an explicit empty string')
    if serial: line(serial)
    kind, key = ('s', serial) if serial else ('l', adapter['usb_location'])
    expected = 'usb:%s:%s:%s:%s:%s' % (adapter['vendor_id'], adapter['product_id'],
                kind, key.encode().hex(), adapter['usb_interface'])
    if adapter['id'] != expected:
        raise ValueError('stable serial/interface identity does not match inventory ID')
    hubs = raw['nested_hubs']
    if len(hubs) < 2:
        raise ValueError('two physically nested USB hubs are required')
    for h in hubs:
        for key in ('physical_label', 'native_location', 'vendor_id', 'product_id'):
            line(h[key])
    if len({h['native_location'] for h in hubs}) != len(hubs):
        raise ValueError('hub ancestry must name distinct native locations')
    records = {k: artifact(raw[k]) for k in ('cli', 'agent_binary', 'native_inventory',
                'baseline_list', 'authorization', 'restart_procedure')}
    baseline = json.loads(Path(raw['baseline_list']).read_text())['devices']
    matches = [d for d in baseline if d['id'] == adapter['id']]
    if len(matches) != 1:
        raise ValueError('selected endpoint must be unique in actual Agent inventory')
    d = matches[0]
    for key in ('serial_number', 'usb_location', 'usb_interface', 'device_path', 'vendor_id', 'product_id'):
        if d.get(key) != adapter[key]:
            raise ValueError('native adapter and Agent baseline disagree: ' + key)
    if d.get('identity_kind') != ('usb_serial' if serial else 'usb_location') or d.get('connected') is not True or d.get('configured') is not False or d.get('verified') is not False:
        raise ValueError('requires present, unconfigured, unverified adapter')
    out = output.resolve()
    if out == ROOT or ROOT in out.parents:
        raise ValueError('output must be new and outside the checkout')
    out.mkdir(mode=0o700, parents=False, exist_ok=False)
    name = 'ci2246-' + uuid.uuid4().hex[:12]
    common = [records['cli']['path'], '--device', raw['target'], '--json', 'device', 'pair', '--usb-arms']
    commands = {'list': common + ['--list'], 'bind': common + ['--bind', adapter['id'], '--arm-name', name],
                'rebind': common + ['--bind', adapter['id']], 'forget': common + ['--forget', adapter['id']]}
    if not serial:
        commands['bind'] += ['--allow-location-binding']
        commands['rebind'] += ['--allow-location-binding']
        commands['refuse-location-bind'] = common + ['--bind', adapter['id'], '--arm-name', name]
    sources = ['go/internal/agent/robotarm/inventory.go', 'go/internal/agent/robotarm/store.go',
               'go/internal/agent/services/robot_arm_service.go',
               'swift/WendyAgentCore/Sources/WendyAgent/Services/Platform/USBSerialInventory.swift',
               'swift/WendyAgentCore/Sources/WendyAgent/Services/RobotArmService.swift',
               'go/internal/cli/commands/device_pair_usb_arms.go']
    plan = {'audit_entry': '2246', 'inventory': raw, 'records': records,
            'source_head': subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=ROOT, text=True).strip(),
            'source_sha256': {p: artifact(ROOT / p)['sha256'] for p in sources},
            'saved_name': name, 'commands': commands, 'hardware_execution': 'not_run',
            'cases': [{'id': case, 'status': 'not_run', 'evidence': [], 'reviewer': None} for case in CASES]}
    (out / 'plan.json').write_text(json.dumps(plan, indent=2) + '\n')
    guide = Path(__file__).with_suffix('.txt').read_text()
    for key, value in {'HOST': raw['hostname'], 'TARGET': raw['target'], 'ID': adapter['id'],
                       'NAME': name, 'PORT': adapter['device_path'], 'PLATFORM': raw['platform']}.items():
        guide = guide.replace('@'+key+'@', value)
    guide += '\nExact reviewed CLI commands, run individually at the specified steps:\n'
    guide += '\n'.join(key + ': ' + shlex.join(value) for key, value in commands.items()) + '\n'
    (out / 'procedure.txt').write_text(guide)
    return plan


def verify(plan_path, phase, snapshot):
    """Check captured API observations only; never infer physical ancestry or a pass."""
    plan = json.loads(plan_path.read_text()); adapter = plan['inventory']['adapter']
    rows = json.loads(snapshot.read_text())['devices']
    matches = [d for d in rows if d['id'] == adapter['id']]
    if len(matches) != 1:
        raise ValueError('selected adapter must have exactly one row')
    d = matches[0]
    for field in ('vendor_id', 'product_id', 'usb_interface'):
        if d.get(field) != adapter[field]: raise ValueError('selected USB identity changed: ' + field)
    if d.get('verified') is not False or d.get('access_status') != 'unknown':
        raise ValueError('USB metadata must not claim arm or port-access verification')
    configured = phase != 'forgotten'
    if d.get('configured') is not configured:
        raise ValueError('wrong saved binding state')
    if configured and (d.get('name') != plan['saved_name'] or d.get('model') != 'so101'):
        raise ValueError('saved name/model did not survive')
    if phase in ('disconnected', 'relocated-location'):
        if d.get('connected') is not False or d.get('device_path') or d.get('connection_status') != 'disconnected':
            raise ValueError('unplug left a live or stale tty binding')
    else:
        if d.get('connected') is not True or d.get('connection_status') != 'present' or not d.get('device_path'):
            raise ValueError('adapter did not reconnect')
        if d.get('serial_number') != adapter['serial_number'] or d.get('identity_kind') != ('usb_serial' if adapter['serial_number'] else 'usb_location'):
            raise ValueError('wrong adapter identity after reconnect')
    if phase == 'relocated-location':
        other = [x for x in rows if x['id'] != adapter['id'] and x.get('vendor_id') == adapter['vendor_id'] and x.get('product_id') == adapter['product_id'] and x.get('usb_interface') == adapter['usb_interface'] and x.get('serial_number') == '' and x.get('connected') is True]
        if len(other) != 1 or other[0].get('configured') is not False or other[0].get('verified') is not False or other[0].get('usb_location') == adapter['usb_location']:
            raise ValueError('location-only move must expose a new unconfigured endpoint')
        candidate = other[0]
        expected = 'usb:%s:%s:l:%s:%s' % (adapter['vendor_id'], adapter['product_id'], candidate['usb_location'].encode().hex(), adapter['usb_interface'])
        path_pattern = r'/dev/cu\.[A-Za-z0-9_.-]+' if plan['inventory']['platform'] == 'macos' else r'/dev/tty(?:USB|ACM)[0-9]+'
        if candidate['id'] != expected or candidate.get('identity_kind') != 'usb_location' or candidate.get('connection_status') != 'present' or candidate.get('access_status') != 'unknown' or not re.fullmatch(path_pattern, candidate.get('device_path', '')):
            raise ValueError('relocated endpoint has inconsistent native identity or state')
    if phase == 'renumbered' and (d['device_path'] == adapter['device_path'] or d['usb_location'] == adapter['usb_location']):
        raise ValueError('case requires an observed changed tty AND USB location')
    baseline = json.loads(Path(plan['records']['baseline_list']['path']).read_text())['devices']
    if hashlib.sha256(Path(plan['records']['baseline_list']['path']).read_bytes()).hexdigest() != plan['records']['baseline_list']['sha256']:
        raise ValueError('baseline provenance changed')
    def bindings(items):
        return sorted((x['id'], x.get('name'), x.get('model')) for x in items if x['id'] != adapter['id'] and x.get('configured'))
    if bindings(rows) != bindings(baseline):
        raise ValueError('unrelated saved bindings changed')
    print('API observations match this phase; physical identity/ancestry and hardware acceptance remain human-reviewed.')


def main():
    p = argparse.ArgumentParser(description=__doc__); sub = p.add_subparsers(dest='mode', required=True)
    prep = sub.add_parser('prepare'); prep.add_argument('inventory', type=Path); prep.add_argument('output', type=Path)
    check = sub.add_parser('check'); check.add_argument('plan', type=Path)
    check.add_argument('phase', choices=('bound', 'disconnected', 'renumbered', 'relocated-location', 'restarted', 'forgotten'))
    check.add_argument('snapshot', type=Path)
    a = p.parse_args()
    if a.mode == 'prepare': generate(a.inventory, a.output)
    else: verify(a.plan, a.phase, a.snapshot)


if __name__ == '__main__': main()
