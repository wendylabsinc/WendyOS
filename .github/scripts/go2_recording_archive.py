"""Validate archived physical CDR bytes without claiming physical acceptance."""
import hashlib
import json
from pathlib import Path
import re
import struct

LIMIT = 64 * 1024 * 1024


def require(value, message):
    if not value:
        raise ValueError(message)


def regular(path, maximum):
    require(not path.is_symlink() and path.is_file(), f'need regular non-symlink file: {path}')
    require(0 < path.stat().st_size <= maximum, f'file size outside bound: {path}')
    return path.read_bytes()


def unique(pairs):
    value = {}
    for key, item in pairs:
        require(key not in value, 'duplicate JSON key: ' + key)
        value[key] = item
    return value


def document(path, maximum=2 * 1024 * 1024):
    raw = regular(path, maximum)
    return raw, json.loads(raw, object_pairs_hook=unique)


def sha(raw):
    return hashlib.sha256(raw).hexdigest()


def identity(value, fields):
    require(isinstance(value, dict), 'identity must be an object')
    for key in fields:
        item = value.get(key)
        require(isinstance(item, str) and item.strip() and not any(ord(c) < 32 for c in item), 'missing identity: ' + key)


def validate(archive, plan_path, provenance, confirm_serial):
    archive, plan_path, provenance = Path(archive), Path(plan_path), Path(provenance)
    require(not archive.is_symlink() and archive.is_dir(), 'archive must be a real directory')
    require(not provenance.is_symlink() and provenance.is_dir(), 'provenance must be a real directory')
    plan_bytes, plan = document(plan_path)
    report_bytes, report = document(archive / 'recording.json')
    require(type(plan.get('schema_version')) is int and type(report.get('schema_version')) is int and plan['schema_version'] == 1 and report['schema_version'] == 1, 'unsupported recording schema')
    require(report.get('error') is None and 'error' in report, 'recording did not complete successfully')
    require(report.get('plan_sha256') == sha(plan_bytes), 'recording plan checksum mismatch')
    require(report.get('robot') == plan.get('robot') and report.get('target') == plan.get('target'), 'recording identity differs from sealed plan')
    require(report.get('acceptance') == 'not_run' and report.get('physical_identity') == 'operator confirmation required', 'recording report changes the recorder acceptance contract')
    robot, target = plan['robot'], plan['target']
    identity(robot, ['model', 'serial', 'firmware', 'firmware_record', 'wired_interface'])
    identity(target, ['selector', 'serial', 'owner', 'board', 'kernel', 'camera_stable_id', 'node'])
    require(robot['serial'] == confirm_serial, 'exact robot serial confirmation required')
    require(robot['model'] == 'Unitree Go2' and robot.get('topic') == '/frontvideostream' and robot.get('resolution') == 360, 'not the observed Go2 front 360p format')
    require(target['camera_stable_id'].startswith('ros2:') and re.fullmatch(r'/dev/video1(?:2[89]|[3-9][0-9])', target['node']), 'invalid observed ROS camera identity')
    require(re.fullmatch(r'[a-f0-9]{40}', plan.get('source_commit', '')), 'source commit must be pinned')
    require(re.fullmatch(r'go2-camera-[a-f0-9]{12}', plan.get('nonce', '')), 'invalid capture nonce')
    for key in ('cli', 'agent'):
        require(re.fullmatch(r'[a-f0-9]{64}', plan.get(key, {}).get('sha256', '')), 'missing pinned binary hash')
    evidence = {'firmware_record': plan['firmware_record']}
    for key in ('device_info', 'installed_agent', 'camera_list', 'kernel_module', 'ros_interfaces', 'baseline_apps', 'recovery'):
        evidence[key] = plan['records'][key]
    provenance_hashes = {}
    for key, descriptor in evidence.items():
        raw = regular(provenance / key, 16 * 1024 * 1024)
        require(sha(raw) == descriptor.get('sha256'), 'provenance checksum mismatch: ' + key)
        provenance_hashes[key] = sha(raw)
    cameras = json.loads(regular(provenance / 'camera_list', 16 * 1024 * 1024), object_pairs_hook=unique)
    matches = [row for row in cameras if row.get('stable_id') == target['camera_stable_id']]
    require(len(matches) == 1 and target['node'] == '/dev/video' + str(matches[0].get('id')), 'observed camera inventory mismatch')
    rows = report.get('samples')
    require(isinstance(rows, list) and 30 <= len(rows) <= 1800, 'recording must contain 30 through 1800 samples')
    total, previous, accepted = 0, -1, []
    for index, row in enumerate(rows):
        require(isinstance(row, dict) and all(type(row.get(key)) is int for key in ('bytes','received_monotonic_ns','firmware_timestamp','resolution','h264_bytes')), 'sample metadata must contain exact integers')
        name = f'{index:04d}.cdr'
        require(row.get('file') == name, 'recording sample order/name mismatch')
        raw = regular(archive / name, 1024 * 1024)
        total += len(raw)
        require(total <= LIMIT, 'recording exceeds 64 MiB')
        require(row.get('bytes') == len(raw) and row.get('sha256') == sha(raw), 'sample checksum/size mismatch: ' + name)
        receipt = row.get('received_monotonic_ns')
        require(type(receipt) is int and receipt > previous and receipt > 0, 'recording receipts must strictly advance')
        previous = receipt
        require(len(raw) >= 24 and raw[:2] == b'\x00\x01', 'invalid CDR encapsulation: ' + name)
        timestamp, resolution, length = struct.unpack_from('<QII', raw, 4)
        require(resolution == 360 and 5 <= length <= len(raw)-20 and len(raw)-20-length <= 3, 'invalid CDR geometry/length: ' + name)
        require((timestamp, resolution, length) == (row.get('firmware_timestamp'), row.get('resolution'), row.get('h264_bytes')), 'sample metadata differs from bytes: ' + name)
        require(raw[20:20+length].startswith((b'\x00\x00\x01', b'\x00\x00\x00\x01')), 'not Annex-B H264: ' + name)
        accepted.append({'file': name, 'sha256': sha(raw), 'received_monotonic_ns': receipt, 'bytes': len(raw)})
    require({p.name for p in archive.iterdir()} == {'recording.json', *(r['file'] for r in accepted)}, 'unexpected archive members')
    elapsed = (rows[-1]['received_monotonic_ns'] - rows[0]['received_monotonic_ns']) / 1e9
    require(1 <= elapsed <= 35, 'recording receipt interval outside bounded physical capture')
    return {'schema_version': 1, 'recording_sha256': sha(report_bytes), 'plan_sha256': sha(plan_bytes),
            'robot_serial': robot['serial'], 'firmware': robot['firmware'], 'source_commit': plan['source_commit'],
            'provenance_sha256': provenance_hashes, 'samples': accepted, 'bytes': total,
            'physical_acceptance': 'not_run', 'scope': 'archived bytes; identity and physical challenge still require human review'}
