#!/usr/bin/env python3
"""Extend the existing Go2 ROS image gate with actual Patrol CLI/DDS checks."""
import hashlib
import json
import os
from pathlib import Path

CASES = ('strict', 'gaps', 'arrival', 'route', 'invalid')
CONTROLS = (
    ('drop-gaps-flag', 'gaps', 'allow_scan_gaps=args.allow_scan_gaps',
     'allow_scan_gaps=False', 'PATROL_GAPS_FLAG_FORWARD'),
    ('drop-arrival-flag', 'arrival', 'ignore_capture_age=args.ignore_capture_age',
     'ignore_capture_age=False', 'PATROL_ARRIVAL_FLAG_FORWARD'),
    ('skip-stamp-admission', 'invalid',
     '        wall_ns = sensor_wall_ns(time.time_ns() if wall_ns is None else wall_ns)',
     '        if self.ignore_capture_age:\n            return time.monotonic()\n'
     '        wall_ns = sensor_wall_ns(time.time_ns() if wall_ns is None else wall_ns)',
     'PATROL_ARRIVAL_REJECTS_ZERO'),
)


def plans(source):
    rows = [('normal', None, source, None)]
    for name, case, before, after, marker in CONTROLS:
        if source.count(before) != 1:
            raise RuntimeError('Patrol source control drift: ' + name)
        rows.append((name, case, source.replace(before, after), marker))
    return rows


def accept(name, code, record, log, failure=None):
    if name == 'normal':
        return (code == 0 and record.get('status') == 'passed' and
                [c['case'] for c in record.get('cases', [])] == list(CASES) and
                'PATROL_ROS_TRANSPORT_VERIFIED' in log)
    control = next(row for row in CONTROLS if row[0] == name)
    marker = control[-1]
    return (code == 1 and record.get('status') == 'failed' and
            record.get('assertion', '').startswith(marker) and
            failure is not None and failure.get('assertion', '').startswith(marker) and
            failure.get('subscriptions') == [1, 1] and
            # The permissive stamp control must actually deliver motion over
            # DDS, rather than merely fail during initialization or teardown.
            (name != 'skip-stamp-admission' or
             any(command['x'] == .55 for _, command in failure.get('commands', []))) and
            'PATROL_ROS_TRANSPORT_VERIFIED' not in log)


def run_cases(root, destination, image, network, nonce, containers, run, output):
    source = root / 'go/simulator/go2/examples/patrol'
    fixture = root / 'go/ros2/go2-bridge/tests/patrol_transport_test.py'
    summary = {'status': 'failed', 'physical_robot': False, 'image': image, 'cases': [],
               'source_sha256': {str(p.relative_to(root)): hashlib.sha256(p.read_bytes()).hexdigest()
                   for p in (source/'ros_app.py', source/'patrol.py', source/'go2_io.py', fixture,
                             Path(__file__).resolve())}}
    target = destination / 'patrol'
    target.mkdir()
    try:
        for name, case, text, marker in plans((source / 'ros_app.py').read_text()):
            work = target / name
            work.mkdir()
            application = work / 'source'
            application.mkdir()
            (application / 'ros_app.py').write_text(text)
            for file in ('patrol.py', 'go2_io.py'):
                (application / file).write_bytes((source / file).read_bytes())
            evidence = work / 'evidence'
            evidence.mkdir()
            container = 'wendy-go2-' + nonce + '-patrol-' + name
            containers.append(container)
            command = ['docker', 'create', '--name', container,
                       '--label', 'dev.wendy.regression.go2=' + nonce, '--network', network,
                       '--cap-drop=ALL', '--security-opt=no-new-privileges', '--read-only',
                       '--tmpfs', '/tmp:rw,nosuid,nodev,size=64m', '--user', f'{os.getuid()}:{os.getgid()}',
                       '-e', 'HOME=/tmp', '-e', 'ROS_LOCALHOST_ONLY=1', '-e', 'ROS_DOMAIN_ID=38',
                       '-e', 'RMW_IMPLEMENTATION=rmw_cyclonedds_cpp',
                       '-e', 'PYTHONDONTWRITEBYTECODE=1', '-e', 'GO2_SENSOR_CLOCK_OFFSET_SECONDS=0',
                       '-v', str(application) + ':/patrol:ro', '-v', str(fixture) + ':/check.py:ro',
                       '-v', str(evidence) + ':/evidence:rw', image, '/bin/bash', '-c',
                       'source /opt/go2-bridge/ros_ws/install/setup.bash && '
                       'exec python3 /check.py --source /patrol --output /evidence' +
                       (' --case ' + case if case else '')]
            output(command)
            # Normal uses five real application processes. Each has bounded
            # discovery, sensor assertions and SIGINT/wait cleanup. The Docker
            # owner handles any unknown process drain at the outer deadline.
            expected = 0 if name == 'normal' else 1
            code = run('patrol-' + name, ['docker', 'start', '--attach', container],
                140 if name == 'normal' else 35, expected=expected)
            record = json.loads((evidence / 'result.json').read_text())
            failure = json.loads((evidence / 'failure-state.json').read_text()) if name != 'normal' else None
            log = (destination / ('patrol-' + name + '.log')).read_text()
            if not accept(name, code, record, log, failure):
                raise RuntimeError('Patrol check failed outside its intended boundary: ' + name)
            summary['cases'].append({'name': name, 'exit_code': code, 'expected': True})
        summary['status'] = 'passed'
    finally:
        (target / 'summary.json').write_text(json.dumps(summary, indent=2) + '\n')
    print('PATROL_ROS_MATRIX_VERIFIED: native DDS and three source controls', flush=True)
