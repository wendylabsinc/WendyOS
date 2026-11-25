#!/usr/bin/env python3
"""GET-only paused Go2 commissioning check; never planner input or authority.

Checks a bounded observation interval, not an atomic lock on later execution.
Any later reset, placement, project change or resume needs phase-appropriate
validation. No credentials, discovery, mutations, task issuance or retries.
"""
import argparse
import hashlib
import math
import os
import re
import socket
import sys
import time

from go2_mission_observer import Refused, encode, endpoint, strict_json, wait_socket

SCHEMA = 'wendy.world.go2-scene-preflight.v1'
MAX_OUTPUT = 16 << 10
MAX_HEADERS = 32 << 10
LIMITS = {'/api/status': 64 << 10, '/api/scene': 4 << 20, '/api/scene/state': 256 << 10}
PATHS = ('/api/status', '/api/scene', '/api/scene/state', '/api/scene/state', '/api/status')
PIN_PATTERNS = {'vm_name': r'[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}',
                'source_digest': r'sha256:[0-9a-f]{64}', 'project_id': r'[0-9a-f]{32}',
                'project_revision': r'[0-9a-f]{64}', 'policy_bundle': r'[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}'}


def require(ok, reason):
    if not ok:
        raise Refused(reason)


def integer(value, minimum=0):
    return type(value) is int and minimum <= value <= (1 << 53)-1


def finite(value):
    return type(value) in (int, float) and math.isfinite(value)


def vec(value, size):
    return type(value) is list and len(value) == size and all(finite(v) for v in value)


def get(runtime, path, deadline, maximum):
    """One fixed-path request, without DNS, proxies, redirects or chunked bodies."""
    require(path in LIMITS and maximum == LIMITS[path], 'request_path_or_bound')
    port = endpoint(runtime)
    remaining = deadline-time.monotonic()
    require(remaining > 0, 'total_deadline')
    sock = None
    try:
        sock = socket.create_connection(('127.0.0.1', port), timeout=remaining)
        sock.setblocking(False)
        request = (f'GET {path} HTTP/1.1\r\nHost: 127.0.0.1:{port}\r\n'
                   'Accept: application/json\r\nConnection: close\r\n\r\n').encode('ascii')
        sent = 0
        while sent < len(request):
            wait_socket(sock, True, deadline)
            try:
                n = sock.send(request[sent:])
            except BlockingIOError:
                continue
            require(n > 0, 'transport_error')
            sent += n
        data = bytearray()
        boundary = -1
        length = None
        while True:
            wait_socket(sock, False, deadline)
            try:
                part = sock.recv(min(65536, maximum+MAX_HEADERS+1-len(data)))
            except BlockingIOError:
                continue
            require(bool(part), 'truncated_response')
            data.extend(part)
            if boundary < 0:
                at = data.find(b'\r\n\r\n')
                require(at <= MAX_HEADERS, 'header_bound')
                if at < 0:
                    require(len(data) <= MAX_HEADERS, 'header_bound')
                    continue
                lines = bytes(data[:at]).split(b'\r\n')
                require(re.fullmatch(rb'HTTP/1\.[01] 200(?: [\x20-\x7e]*)?', lines[0]) is not None, 'http_status')
                headers = {}
                for line in lines[1:]:
                    require(b':' in line and line[:1] not in (b' ', b'\t'), 'http_headers')
                    key, value = line.split(b':', 1)
                    key = key.lower()
                    require(re.fullmatch(rb'[a-z0-9-]+', key) is not None and key not in headers, 'http_headers')
                    headers[key] = value.strip()
                raw_length = headers.get(b'content-length', b'')
                require(b'transfer-encoding' not in headers and b'content-encoding' not in headers
                        and re.fullmatch(rb'[0-9]{1,8}', raw_length) is not None, 'http_framing')
                length = int(raw_length)
                require(0 < length <= maximum, 'response_bound')
                boundary = at+4
            if boundary >= 0 and len(data) >= boundary+length:
                require(len(data) == boundary+length, 'extra_response_bytes')
                require(time.monotonic() < deadline, 'total_deadline')
                return bytes(data[boundary:])
    except (socket.timeout, TimeoutError):
        raise Refused('total_deadline') from None
    except OSError:
        raise Refused('transport_error') from None
    finally:
        if sock is not None:
            sock.close()


def validate_status(status, pins):
    require(type(status) is dict and type(status.get('project')) is dict
            and type(status.get('ros_commands')) is dict, 'runtime_shape')
    project, commands = status['project'], status['ros_commands']
    require('error' in status and 'error' in project and 'owner' in commands,
            'runtime_shape')
    actual = {k: status.get(k) for k in ('epoch', 'vm_name', 'source_digest', 'policy_bundle')}
    actual.update(project_id=project.get('project_id'), project_revision=project.get('revision'))
    # Project epoch records the project load. Runtime reset advances the world
    # epoch without reloading that project, so these two counters can differ.
    require(integer(status.get('epoch'), 1) and actual == pins
            and integer(project.get('epoch'), 1), 'runtime_pins')
    require(status.get('simulation') is True and status.get('robot_kind') == 'go2'
            and status.get('mode') == 'paused' and status.get('armed') is False
            and status.get('healthy') is True and status.get('error') is None
            and project.get('error') is None and commands.get('owner') is None
            and commands.get('auto_control') is False, 'paused_runtime_required')
    require(all(vec(status.get(k), 3) and status[k] == [0, 0, 0]
                for k in ('command', 'applied_command')), 'nonzero_command')
    require(finite(status.get('time')) and status['time'] >= 0, 'runtime_time')


def validate_scene(scene, states, statuses, body_name, position, tolerance,
                   pins, scene_id=None, generation=None):
    require(type(scene) is dict and type(scene.get('id')) is str
            and re.fullmatch('[0-9a-f]{64}', scene['id']) is not None, 'scene_identity')
    require(scene_id is None or scene['id'] == scene_id, 'scene_pin')
    bodies = scene.get('bodies')
    require(type(bodies) is list and 1 <= len(bodies) <= 4096, 'scene_bodies')
    ids, names = set(), set()
    for body in bodies:
        require(type(body) is dict and integer(body.get('id'))
                and body['id'] < len(bodies) and type(body.get('name')) is str
                and 0 < len(body['name']) <= 128, 'scene_body_shape')
        require(body['id'] not in ids and body['name'] not in names, 'ambiguous_scene_body')
        ids.add(body['id']); names.add(body['name'])
    matches = [body['id'] for body in bodies if body['name'] == body_name]
    require(len(matches) == 1, 'body_not_found')
    body_id = matches[0]
    for state in states:
        require(type(state) is dict and state.get('scene_id') == scene['id']
                and integer(state.get('epoch'), 1) and state['epoch'] == pins['epoch']
                and integer(state.get('generation')) and state.get('mode') == 'paused'
                and state.get('valid') is True, 'scene_state_identity')
        require(generation is None or state['generation'] == generation, 'generation_pin')
        require(finite(state.get('time')) and state['time'] >= 0
                and vec(state.get('positions'), 3*len(bodies))
                and vec(state.get('quaternions'), 4*len(bodies)), 'scene_state_shape')
        actual = state['positions'][3*body_id:3*body_id+3]
        require(all(abs(a-b) <= tolerance for a,b in zip(actual,position)), 'obstacle_position_mismatch')
    require(states[0] == states[1], 'scene_state_changed')
    require(statuses[0]['time'] == statuses[1]['time'] == states[0]['time'], 'paused_time_changed')
    return {'scene_id': scene['id'], 'epoch': states[0]['epoch'], 'generation': states[0]['generation'],
            'mode': states[0]['mode'], 'valid': states[0]['valid'], 'simulation_time_s': states[0]['time'],
            'body_name': body_name, 'body_id': body_id,
            'position_runtime_world_z_up_m': states[0]['positions'][3*body_id:3*body_id+3]}


def preflight(runtime, pins, body_name, position, *, timeout_s=2.0, tolerance_m=1e-6,
              scene_id=None, generation=None, fetch=get, clock=time.monotonic):
    endpoint(runtime)
    require(type(pins) is dict and set(pins) == set(PIN_PATTERNS)|{'epoch'}
            and integer(pins.get('epoch'), 1), 'explicit_runtime_pins_required')
    for key, pattern in PIN_PATTERNS.items():
        require(type(pins[key]) is str and re.fullmatch(pattern, pins[key]) is not None, 'invalid_runtime_pin')
    require(type(body_name) is str and re.fullmatch('[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}', body_name) is not None
            and vec(position, 3), 'expected_body_position')
    require(finite(timeout_s) and .05 <= timeout_s <= 5
            and finite(tolerance_m) and 0 <= tolerance_m <= .001, 'preflight_bounds')
    require(scene_id is None or type(scene_id) is str and re.fullmatch('[0-9a-f]{64}', scene_id) is not None, 'scene_pin')
    require(generation is None or integer(generation), 'generation_pin')
    started = clock(); deadline = started+timeout_s
    values, receipts = [], []
    for path in PATHS:
        before = clock()
        require(started <= before < deadline, 'total_deadline')
        raw = fetch(runtime, path, deadline, LIMITS[path])
        require(type(raw) is bytes and 0 < len(raw) <= LIMITS[path], 'response_bound')
        value = strict_json(raw)
        after = clock()
        require(before <= after < deadline, 'total_deadline')
        values.append(value)
        receipts.append({'path': path, 'bytes': len(raw), 'sha256': hashlib.sha256(raw).hexdigest(),
                         'started_elapsed_ms': (before-started)*1000, 'finished_elapsed_ms': (after-started)*1000})
    statuses = (values[0], values[4])
    for status in statuses:
        validate_status(status, pins)
    require(statuses[0]['project']['epoch'] == statuses[1]['project']['epoch'], 'project_epoch_changed')
    matched = validate_scene(values[1], values[2:4], statuses, body_name, position,
                             tolerance_m, pins, scene_id, generation)
    report = {'schema': SCHEMA, 'accepted': True, 'simulation': True,
              'role': 'commissioning_observation_only_never_planner_input_or_authority',
              'operational_authority': False, 'runtime': runtime, 'expected_pins': dict(pins),
              'expected_position_runtime_world_z_up_m': list(position), 'tolerance_m': tolerance_m,
              'observed_project_load_epoch': statuses[0]['project']['epoch'],
              'observed': matched, 'requests': receipts, 'total_deadline_s': timeout_s,
              'elapsed_ms': (clock()-started)*1000,
              'limits': ['The paused read interval is not an atomic lock on later state.',
                         'Any later reset, placement, project change or resume needs phase-appropriate validation.',
                         'Response times are local observations; the API supplies no authenticated capture freshness.',
                         'The total deadline includes reads, validation and report encoding; filesystem write and fsync are not hard bounded.']}
    bounded_output(report)
    require(clock() < deadline, 'total_deadline')
    return report


def bounded_output(report):
    raw = encode(report)+b'\n'
    require(len(raw) <= MAX_OUTPUT, 'output_bound')
    return raw


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--runtime', required=True)
    parser.add_argument('--expected-epoch', type=int, required=True)
    for key in PIN_PATTERNS:
        parser.add_argument('--expected-'+key.replace('_','-'), required=True)
    parser.add_argument('--body', required=True)
    parser.add_argument('--position', nargs=3, type=float, required=True, metavar=('X','Y','Z'))
    parser.add_argument('--expected-scene-id')
    parser.add_argument('--expected-generation', type=int)
    parser.add_argument('--timeout-s', type=float, default=2.0)
    parser.add_argument('--tolerance-m', type=float, default=1e-6)
    parser.add_argument('--out', required=True, help='new private report file; never overwritten')
    args = parser.parse_args(argv)
    try:
        # Claim output before network access. Failure remains a refusal artifact.
        fd = os.open(args.out, os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW, 0o600)
        with os.fdopen(fd, 'wb') as stream:
            try:
                pins = {key:getattr(args,'expected_'+key) for key in (*PIN_PATTERNS,'epoch')}
                report = preflight(args.runtime, pins, args.body, args.position,
                    timeout_s=args.timeout_s, tolerance_m=args.tolerance_m,
                    scene_id=args.expected_scene_id, generation=args.expected_generation)
            except Refused as error:
                report = {'schema':SCHEMA,'accepted':False,'simulation':True,
                          'operational_authority':False,'reason':error.reason}
            stream.write(bounded_output(report));stream.flush();os.fsync(stream.fileno())
        print(encode({'schema':SCHEMA,'accepted':report['accepted'],'reason':report.get('reason')}).decode())
        return 0 if report['accepted'] else 1
    except OSError:
        print('scene_preflight_io_refused', file=sys.stderr)
        return 1


if __name__ == '__main__':
    raise SystemExit(main())
