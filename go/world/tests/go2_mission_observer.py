#!/usr/bin/env python3
"""Bounded GET-only Go2 simulator observations; ground truth is an oracle only.

No credentials, task issuance, discovery, ROS publications or runtime mutations.
`self-test` uses fake loopback HTTP servers and never contacts a Wendy VM.
"""
import argparse
import hashlib
import json
import math
import os
from pathlib import Path
import re
import select
import signal
import socket
import sys
import time
from urllib.parse import urlsplit

MAX_BODY = 1 << 20
MAX_HEADERS = 32768
MAX_FILE = 16 << 20
MAX_SUMMARY = 256 << 10
REQUEST_SECONDS = .250
SCHEMA = 'wendy.world.go2-mission-observer.v1'


class Refused(Exception):
    def __init__(self, reason, status=None):
        super().__init__(reason)
        self.reason = reason
        self.status = status


def endpoint(value):
    try:
        p = urlsplit(value)
        if (p.scheme != 'http' or p.hostname != '127.0.0.1' or p.username or p.password
                or p.path or p.query or p.fragment or p.port is None or not 1 <= p.port <= 65535
                or value != 'http://127.0.0.1:' + str(p.port)):
            raise ValueError()
    except (ValueError, TypeError):
        raise Refused('invalid_numeric_loopback_endpoint') from None
    return p.port


def strict_json(raw):
    def pairs(items):
        result = {}
        for k, v in items:
            if k in result:
                raise Refused('duplicate_json_field')
            result[k] = v
        return result
    def bad(_):
        raise Refused('nonfinite_json_number')
    try:
        return json.loads(raw.decode('utf-8'), object_pairs_hook=pairs, parse_constant=bad)
    except (ValueError, UnicodeError, RecursionError):
        raise Refused('invalid_json') from None


def encode(value):
    return json.dumps(value, ensure_ascii=False, allow_nan=False, separators=(',', ':')).encode('utf-8')


def wait_socket(sock, write, deadline):
    remaining = deadline - time.monotonic()
    if remaining <= 0:
        raise Refused('request_timeout')
    readable, writable, exceptional = select.select([] if write else [sock], [sock] if write else [], [sock], remaining)
    if exceptional:
        raise Refused('transport_error')
    if not (writable if write else readable):
        raise Refused('request_timeout')


def get_status(runtime, deadline):
    """Absolute deadline across connect, headers and body, including trickle reads.

    Numeric endpoint means no DNS. This small HTTP client has no proxy or redirect
    path. Fixed Content-Length is required; unsupported framing is an error.
    """
    port = endpoint(runtime)
    deadline = min(deadline, time.monotonic() + REQUEST_SECONDS)
    remaining = deadline - time.monotonic()
    if remaining <= 0:
        raise Refused('request_timeout')
    sock = None
    try:
        sock = socket.create_connection(('127.0.0.1', port), timeout=remaining)
        sock.setblocking(False)
        request = ('GET /api/status HTTP/1.1\r\nHost: 127.0.0.1:' + str(port) +
                   '\r\nAccept: application/json\r\nConnection: close\r\n\r\n').encode('ascii')
        offset = 0
        while offset < len(request):
            wait_socket(sock, True, deadline)
            try:
                n = sock.send(request[offset:])
            except BlockingIOError:
                continue
            if n <= 0:
                raise Refused('transport_error')
            offset += n
        data = bytearray()
        boundary = -1
        length = None
        while True:
            wait_socket(sock, False, deadline)
            try:
                part = sock.recv(min(65536, MAX_BODY + MAX_HEADERS + 1 - len(data)))
            except BlockingIOError:
                continue
            if not part:
                raise Refused('truncated_response')
            data.extend(part)
            if boundary < 0:
                boundary = data.find(b'\r\n\r\n')
                if boundary < 0:
                    if len(data) > MAX_HEADERS:
                        raise Refused('header_bound')
                    continue
                if boundary > MAX_HEADERS:
                    raise Refused('header_bound')
                lines = bytes(data[:boundary]).split(b'\r\n')
                match = re.fullmatch(rb'HTTP/1\.[01] ([0-9]{3})(?: [\x20-\x7e]*)?', lines[0])
                if not match:
                    raise Refused('invalid_http_status')
                status = int(match[1])
                if status != 200:
                    raise Refused('http_status', status)
                headers = {}
                for line in lines[1:]:
                    if b':' not in line or line[:1] in (b' ', b'\t'):
                        raise Refused('invalid_http_headers')
                    k, v = line.split(b':', 1)
                    k = k.lower()
                    if not re.fullmatch(rb'[a-z0-9-]+', k) or k in headers:
                        raise Refused('invalid_http_headers')
                    headers[k] = v.strip()
                raw_length = headers.get(b'content-length', b'')
                if b'transfer-encoding' in headers or not re.fullmatch(rb'[0-9]{1,8}', raw_length):
                    raise Refused('unsupported_http_framing')
                length = int(raw_length)
                if length <= 0 or length > MAX_BODY:
                    raise Refused('body_bound')
                boundary += 4
            if boundary >= 0 and length is not None and len(data) >= boundary + length:
                if len(data) != boundary + length:
                    raise Refused('extra_response_bytes')
                if time.monotonic() > deadline:
                    raise Refused('request_timeout')
                body = bytes(data[boundary:])
                return strict_json(body), {'body_bytes': len(body), 'body_sha256': hashlib.sha256(body).hexdigest()}
    except (socket.timeout, TimeoutError):
        raise Refused('request_timeout') from None
    except (OSError, OverflowError):
        raise Refused('transport_error') from None
    finally:
        if sock is not None:
            sock.close()


def finite(value):
    return type(value) in (int, float) and math.isfinite(value)


def vector(value, count=3):
    return isinstance(value, list) and len(value) == count and all(finite(n) for n in value)


def small_string(value):
    return value is None or isinstance(value, str) and len(value.encode('utf-8')) <= 1024


def metadata(status):
    # Original selected metadata values are retained without relabeling as trust.
    names = ('robot', 'simulation', 'profile_version', 'robot_kind', 'source_digest',
             'vm_name', 'dds_isolation', 'policy_bundle', 'world', 'clock_mode', 'seed')
    return {name: status.get(name) for name in names}


def observation(status, expected):
    if not isinstance(status, dict):
        raise Refused('status_not_object')
    if (type(status.get('epoch')) is not int or not 1 <= status['epoch'] <= (1 << 53) - 1
            or not finite(status.get('time')) or status['time'] < 0
            or not all(vector(status.get(name)) for name in ('position', 'linear_velocity_world', 'angular_velocity_body', 'command', 'applied_command'))
            or not vector(status.get('quaternion_wxyz'), 4)
            or type(status.get('armed')) is not bool or type(status.get('healthy')) is not bool
            or type(status.get('ready')) is not bool or not isinstance(status.get('mode'), str)
            or len(status['mode']) > 64 or type(status.get('ncontact')) is not int or status['ncontact'] < 0):
        raise Refused('status_fields_invalid')
    project, ros = status.get('project'), status.get('ros_commands')
    if (not isinstance(project, dict) or not isinstance(ros, dict)
            or type(ros.get('accepted')) is not int or ros['accepted'] < 0
            or type(ros.get('rejected')) is not int or ros['rejected'] < 0
            or type(ros.get('auto_control')) is not bool
            or not small_string(ros.get('owner')) or not small_string(ros.get('last_error'))
            or not small_string(status.get('error'))):
        raise Refused('status_metadata_invalid')
    pins = {'epoch': status['epoch'], 'vm_name': status.get('vm_name'),
            'source_digest': status.get('source_digest'), 'project_id': project.get('project_id'),
            'project_revision': project.get('revision'), 'policy_bundle': status.get('policy_bundle')}
    violations = sorted(k for k, wanted in expected.items() if wanted is not None and pins[k] != wanted)
    if status.get('simulation') is not True or status.get('robot_kind') != 'go2':
        violations.append('simulation_go2_identity')
    return {'pin_violations': violations, 'epoch': status['epoch'], 'simulation_time_s': status['time'],
            'mode': status['mode'], 'armed': status['armed'], 'healthy': status['healthy'], 'ready': status['ready'],
            'command_target': status['command'], 'applied_command': status['applied_command'],
            'oracle_only': {'position_m': status['position'], 'quaternion_wxyz': status['quaternion_wxyz'],
                            'linear_velocity_world_mps': status['linear_velocity_world'],
                            'angular_velocity_body_rps': status['angular_velocity_body']},
            'raw_ncontact': status['ncontact'],
            'ros_commands': {k: ros.get(k) for k in ('accepted', 'rejected', 'owner', 'auto_control', 'last_error')},
            'runtime_error_present': status.get('error') is not None, 'metadata': metadata(status),
            'project': {k: project.get(k) for k in ('project_id', 'revision', 'epoch', 'error')}}


def create_file(path):
    return os.fdopen(os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600), 'wb')


def add_duration(stats, value):
    stats['count'] += 1
    stats['sum_ms'] += value
    stats['max_ms'] = max(stats['max_ms'], value)


def run_observer(args, fetch=get_status, stop=None):
    endpoint(args.runtime)
    if not finite(args.duration) or not 0 < args.duration <= 120 or not 50 <= args.interval_ms <= 1000:
        raise Refused('invalid_observation_bounds')
    expected = {'epoch': args.expected_epoch, 'vm_name': args.expected_vm,
                'source_digest': args.expected_source_digest, 'project_id': args.expected_project_id,
                'project_revision': args.expected_project_revision, 'policy_bundle': args.expected_policy_bundle}
    if type(expected['epoch']) is not int or not 1 <= expected['epoch'] <= (1 << 53) - 1:
        raise Refused('explicit_epoch_required')
    for name, pattern in [('source_digest', r'sha256:[0-9a-f]{64}'), ('project_id', r'[0-9a-f]{32}'), ('project_revision', r'[0-9a-f]{64}'), ('vm_name', r'[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}'), ('policy_bundle', r'[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}')]:
        if expected[name] is not None and (not isinstance(expected[name], str) or not re.fullmatch(pattern, expected[name])):
            raise Refused('invalid_expected_pin')
    out = Path(args.out)
    out.mkdir(mode=0o700)  # Existing directories are never overwritten or cleaned.
    sample_path = out / 'samples.ndjson'
    started = time.monotonic()
    until = started + args.duration
    interval = args.interval_ms / 1000
    wall_started = time.time_ns()
    summary = {'schema': SCHEMA, 'runtime': args.runtime, 'expected': expected,
               'requested_duration_s': args.duration, 'interval_ms': args.interval_ms,
               'request_timeout_ms': 250, 'body_limit_bytes': MAX_BODY, 'sample_file_limit_bytes': MAX_FILE,
               'host_wall_started_unix_ns': wall_started, 'observer_pid': os.getpid(),
               'python_version': sys.version.split()[0], 'ground_truth_role': 'verification_oracle_only_never_planner_input',
               'contact_interpretation': 'raw_contact_count_not_collision_classification',
               'physical_stopping_proven': False, 'runtime_instance_verified': False,
               'oracle_coordinate_frame': 'runtime_native_world_Z_up_not_World_site',
               'command_components': ['body_vx_mps', 'body_vy_mps', 'yaw_rate_rps'],
               'applied_command_role': 'runtime_slew_target_not_measured_body_velocity',
               'network_deadline_scope': 'absolute_per_request_and_original_sampling_window',
               'file_io_hard_deadline': False,
               'completed_requested_window': False, 'interrupted': False, 'file_cap_reached': False,
               'sample_attempts': 0, 'status_samples': 0, 'retained_samples': 0, 'retained_status_samples': 0,
               'last_attempt_retained': None, 'request_errors': {}, 'report_errors': {},
               'pin_violation_samples': 0, 'pin_violations': {}, 'runtime_unhealthy_samples': 0,
               'runtime_error_samples': 0, 'initial': None, 'final': None, 'mode_samples': {},
               'request_duration': {'count': 0, 'sum_ms': 0, 'max_ms': 0},
               'request_start_gaps': {'count': 0, 'sum_ms': 0, 'max_ms': 0, 'above_twice_interval_count': 0},
               'successful_completion_gaps': {'count': 0, 'sum_ms': 0, 'max_ms': 0},
               'missed_schedule_slots': 0, 'sample_file_bytes': 0}
    digest = hashlib.sha256()
    next_at = started
    last_start = last_success = None
    last_epoch = last_sim_time = None
    summary['observed_epoch_changes'] = 0
    summary['same_epoch_simulation_time_regressions'] = 0
    try:
        with create_file(sample_path) as output:
            while time.monotonic() < until:
                if stop is not None and stop():
                    summary['interrupted'] = True
                    break
                delay = min(next_at, until) - time.monotonic()
                if delay > 0:
                    time.sleep(delay)
                if stop is not None and stop():
                    summary['interrupted'] = True
                    break
                began = time.monotonic()
                if began >= until:
                    break
                if last_start is not None:
                    gap = (began - last_start) * 1000
                    add_duration(summary['request_start_gaps'], gap)
                    if gap > 2 * args.interval_ms:
                        summary['request_start_gaps']['above_twice_interval_count'] += 1
                last_start = began
                row = {'sequence': summary['sample_attempts'] + 1,
                       'request_started_offset_ms': (began - started) * 1000,
                       'host_wall_request_started_unix_ns': time.time_ns()}
                summary['sample_attempts'] += 1
                summary['last_attempt_retained'] = False
                snapshot = None
                try:
                    status, body_info = fetch(args.runtime, min(until, began + REQUEST_SECONDS))
                    row.update(body_info)
                    row['status'] = status
                    row['observation'] = observed = observation(status, expected)
                    completed = time.monotonic()
                    if last_success is not None:
                        add_duration(summary['successful_completion_gaps'], (completed - last_success) * 1000)
                    last_success = completed
                    summary['status_samples'] += 1
                    mode = observed['mode']
                    summary['mode_samples'][mode] = summary['mode_samples'].get(mode, 0) + 1
                    summary['runtime_unhealthy_samples'] += not observed['healthy']
                    summary['runtime_error_samples'] += observed['runtime_error_present']
                    if last_epoch is not None and last_epoch != observed['epoch']:
                        summary['observed_epoch_changes'] += 1
                    if last_epoch == observed['epoch'] and observed['simulation_time_s'] < last_sim_time:
                        summary['same_epoch_simulation_time_regressions'] += 1
                    last_epoch, last_sim_time = observed['epoch'], observed['simulation_time_s']
                    if observed['pin_violations']:
                        summary['pin_violation_samples'] += 1
                        for p in observed['pin_violations']:
                            summary['pin_violations'][p] = summary['pin_violations'].get(p, 0) + 1
                    snapshot = {'sample_retained': False, 'request_started_offset_ms': row['request_started_offset_ms'],
                                'response_completed_offset_ms': (completed - started) * 1000, **observed}
                    if summary['initial'] is None:
                        summary['initial'] = snapshot
                    summary['final'] = snapshot
                except Refused as e:
                    row['error_class'] = e.reason
                    if e.status is not None:
                        row['http_status'] = e.status
                    summary['request_errors'][e.reason] = summary['request_errors'].get(e.reason, 0) + 1
                completed = time.monotonic()
                row['response_completed_offset_ms'] = (completed - started) * 1000
                row['request_duration_ms'] = (completed - began) * 1000
                add_duration(summary['request_duration'], row['request_duration_ms'])
                raw = encode(row) + b'\n'
                if summary['sample_file_bytes'] + len(raw) > MAX_FILE:
                    summary['file_cap_reached'] = True
                    break
                output.write(raw)
                output.flush()
                digest.update(raw)
                summary['sample_file_bytes'] += len(raw)
                summary['retained_samples'] += 1
                summary['last_attempt_retained'] = True
                if snapshot is not None:
                    snapshot['sample_retained'] = True
                    summary['retained_status_samples'] += 1
                # No catch-up burst after a slow/erroring request or file write.
                next_at += interval
                now = time.monotonic()
                if next_at < now:
                    slots = math.floor((now - next_at) / interval) + 1
                    summary['missed_schedule_slots'] += slots
                    next_at += slots * interval
            output.flush()
            os.fsync(output.fileno())
    except OSError:
        summary['report_errors']['sample_file_io'] = 1
    except (ValueError, OverflowError, RecursionError):
        summary['report_errors']['sample_encoding'] = 1
    ended = time.monotonic()
    summary['elapsed_monotonic_s'] = ended - started
    summary['host_wall_finished_unix_ns'] = time.time_ns()
    summary['sample_file_sha256'] = digest.hexdigest()
    summary['completed_requested_window'] = (ended >= until and not summary['interrupted']
                                             and not summary['file_cap_reached'] and not summary['report_errors'])
    summary['all_sampled_identities_match'] = summary['status_samples'] > 0 and summary['pin_violation_samples'] == 0
    summary['sample_errors_present'] = bool(summary['request_errors'] or summary['report_errors'])
    summary['result'] = 'observed' if (summary['completed_requested_window'] and summary['all_sampled_identities_match']
                                     and not summary['sample_errors_present'] and not summary['runtime_unhealthy_samples']
                                     and not summary['runtime_error_samples'] and not summary['same_epoch_simulation_time_regressions']) else 'incomplete_or_changed'
    try:
        raw = encode(summary) + b'\n'
        if len(raw) > MAX_SUMMARY:
            raise Refused('summary_bound')
        with create_file(out / 'summary.json') as output:
            output.write(raw)
            output.flush()
            os.fsync(output.fileno())
    except OSError:
        raise Refused('summary_file_io') from None
    return summary


def parser():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--runtime', required=True)
    p.add_argument('--duration', type=float, required=True)
    p.add_argument('--interval-ms', type=int, default=100)
    p.add_argument('--out', required=True)
    p.add_argument('--expected-epoch', type=int, required=True)
    p.add_argument('--expected-vm')
    p.add_argument('--expected-source-digest')
    p.add_argument('--expected-project-id')
    p.add_argument('--expected-project-revision')
    p.add_argument('--expected-policy-bundle')
    return p


def main():
    if sys.argv[1:] == ['self-test']:
        return self_test()
    args = parser().parse_args()
    stopped = [False]
    for sig in (signal.SIGINT, signal.SIGTERM):
        signal.signal(sig, lambda *_: stopped.__setitem__(0, True))
    try:
        summary = run_observer(args, stop=lambda: stopped[0])
        print(json.dumps({'schema': SCHEMA, 'result': summary['result'],
                          'sample_attempts': summary['sample_attempts'], 'status_samples': summary['status_samples'],
                          'request_errors': summary['request_errors'], 'pin_violation_samples': summary['pin_violation_samples']}))
        return 0 if summary['result'] == 'observed' else 1
    except (Refused, OSError) as e:
        print(json.dumps({'schema': SCHEMA, 'result': 'refused',
                          'error_class': e.reason if isinstance(e, Refused) else 'output_io'}), file=sys.stderr)
        return 1


def self_test():
    import copy
    from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
    import tempfile
    import threading
    import unittest
    from unittest.mock import patch
    fixture = {'time': 1., 'epoch': 2, 'mode': 'standing', 'armed': False, 'healthy': True,
               'ready': True, 'position': [1., 2., .3], 'quaternion_wxyz': [1., 0., 0., 0.],
               'linear_velocity_world': [.01, .02, 0.], 'angular_velocity_body': [0., 0., .03],
               'command': [.2, 0., 0.], 'applied_command': [.1, 0., 0.], 'ncontact': 4,
               'simulation': True, 'robot_kind': 'go2', 'vm_name': 'test', 'source_digest': 'sha256:' + 'a'*64,
               'policy_bundle': 'go2-policy', 'project': {'project_id': 'b'*32, 'revision': 'c'*64, 'epoch': 2, 'error': None},
               'ros_commands': {'accepted': 4, 'rejected': 1, 'auto_control': False, 'owner': 'test-owner', 'last_error': None},
               'error': None}

    class Handler(BaseHTTPRequestHandler):
        protocol_version = 'HTTP/1.1'
        requests = []
        mode = 'good'
        count = 0
        def log_message(self, *_):
            pass
        def do_GET(self):
            Handler.requests.append((self.command, self.path))
            Handler.count += 1
            if Handler.mode == 'errors':
                self.send_response(503)
                self.send_header('Content-Length', '0')
                self.end_headers()
                return
            if Handler.mode == 'redirect':
                self.send_response(302)
                self.send_header('Location', '/mutate')
                self.send_header('Content-Length', '0')
                self.end_headers()
                return
            if Handler.mode == 'oversized':
                self.send_response(200)
                self.send_header('Content-Length', str(MAX_BODY + 1))
                self.end_headers()
                return
            if Handler.mode == 'trickle':
                self.send_response(200)
                self.send_header('Content-Length', '100')
                self.end_headers()
                try:
                    for _ in range(20):
                        self.wfile.write(b' ')
                        self.wfile.flush()
                        time.sleep(.04)
                except OSError:
                    pass
                return
            data = copy.deepcopy(fixture)
            if Handler.mode == 'changed' and Handler.count > 1:
                data['epoch'] = 3
                data['project']['epoch'] = 3
            raw = encode(data)
            self.send_response(200)
            self.send_header('Content-Length', str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)

    class Checks(unittest.TestCase):
        def setUp(self):
            Handler.requests, Handler.count, Handler.mode = [], 0, 'good'
            self.server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
            self.server.daemon_threads = True
            self.thread = threading.Thread(target=self.server.serve_forever, kwargs={'poll_interval': .01}, daemon=True)
            self.thread.start()
            self.addCleanup(self.server.server_close)
            self.addCleanup(self.server.shutdown)
            self.temp = tempfile.TemporaryDirectory()
            self.addCleanup(self.temp.cleanup)
            self.url = 'http://127.0.0.1:' + str(self.server.server_port)
            self.args = argparse.Namespace(runtime=self.url, duration=.16, interval_ms=50,
                                           out=str(Path(self.temp.name)/'capture'), expected_epoch=2,
                                           expected_vm='test', expected_source_digest='sha256:'+'a'*64,
                                           expected_project_id='b'*32, expected_project_revision='c'*64,
                                           expected_policy_bundle='go2-policy')
        def test_get_only_oracle_separation_and_metadata(self):
            with patch.dict(os.environ, {'HTTP_PROXY': 'http://127.0.0.1:1', 'http_proxy': 'http://127.0.0.1:1'}):
                result = run_observer(self.args)
            self.assertEqual(result['result'], 'observed')
            self.assertTrue(Handler.requests)
            self.assertEqual(set(Handler.requests), {('GET', '/api/status')})
            self.assertEqual(result['initial']['command_target'], [.2, 0., 0.])
            self.assertEqual(result['initial']['applied_command'], [.1, 0., 0.])
            self.assertEqual(result['initial']['oracle_only']['linear_velocity_world_mps'], [.01, .02, 0.])
            self.assertFalse(result['physical_stopping_proven'])
            self.assertEqual(os.stat(Path(self.args.out)/'samples.ndjson').st_mode & 0o777, 0o600)
            raw = (Path(self.args.out)/'samples.ndjson').read_bytes()
            self.assertEqual(hashlib.sha256(raw).hexdigest(), result['sample_file_sha256'])
            self.assertEqual(strict_json(raw.splitlines()[0])['status'], fixture)
        def test_repeated_http_errors_are_not_success(self):
            Handler.mode = 'errors'
            result = run_observer(self.args)
            self.assertGreaterEqual(result['request_errors']['http_status'], 2)
            self.assertEqual(result['status_samples'], 0)
            self.assertIsNone(result['final'])
            self.assertEqual(result['result'], 'incomplete_or_changed')
            rows = [strict_json(r) for r in (Path(self.args.out)/'samples.ndjson').read_bytes().splitlines()]
            self.assertTrue(all(r['http_status'] == 503 for r in rows))
        def test_epoch_change_retained_without_rebinding(self):
            Handler.mode = 'changed'
            result = run_observer(self.args)
            self.assertEqual(result['observed_epoch_changes'], 1)
            self.assertGreater(result['pin_violation_samples'], 0)
            self.assertEqual(result['initial']['epoch'], 2)
            self.assertEqual(result['final']['epoch'], 3)
            self.assertEqual(result['expected']['epoch'], 2)
            self.assertEqual(result['result'], 'incomplete_or_changed')
        def test_redirect_refused_not_followed(self):
            Handler.mode = 'redirect'
            with self.assertRaises(Refused) as found:
                get_status(self.url, time.monotonic()+1)
            self.assertEqual(found.exception.status, 302)
            self.assertEqual(Handler.requests, [('GET', '/api/status')])
        def test_response_body_cap_checked_before_reading(self):
            Handler.mode = 'oversized'
            with self.assertRaises(Refused) as found:
                get_status(self.url, time.monotonic()+1)
            self.assertEqual(found.exception.reason, 'body_bound')
        def test_absolute_request_deadline_even_for_trickle(self):
            Handler.mode = 'trickle'
            start = time.monotonic()
            with self.assertRaises(Refused) as found:
                get_status(self.url, start+1)
            self.assertEqual(found.exception.reason, 'request_timeout')
            self.assertLess(time.monotonic()-start, .4)
        def test_total_duration_cancels_final_trickle(self):
            Handler.mode = 'trickle'
            start = time.monotonic()
            result = run_observer(self.args)
            self.assertLess(time.monotonic()-start, .35)
            self.assertEqual(result['request_errors'], {'request_timeout': 1})
            self.assertEqual(result['result'], 'incomplete_or_changed')
        def test_caps_and_existing_outputs_refused(self):
            with patch.dict(globals(), {'MAX_FILE': 1}):
                result = run_observer(self.args)
            self.assertTrue(result['file_cap_reached'])
            self.assertEqual(result['sample_file_bytes'], 0)
            self.assertEqual(result['result'], 'incomplete_or_changed')
            with self.assertRaises(FileExistsError):
                run_observer(self.args)
            self.assertTrue((Path(self.args.out)/'summary.json').is_file())
        def test_interruption_retains_prior_samples(self):
            result = run_observer(self.args, stop=lambda: bool(Handler.requests))
            self.assertTrue(result['interrupted'])
            self.assertFalse(result['completed_requested_window'])
            self.assertEqual(result['sample_attempts'], 1)
            self.assertEqual(result['retained_status_samples'], 1)
            self.assertEqual(result['result'], 'incomplete_or_changed')
        def test_report_error_not_mistaken_for_network_success(self):
            with patch('os.fsync', side_effect=OSError('injected file failure')):
                with self.assertRaises(Refused) as found:
                    run_observer(self.args)
            self.assertEqual(found.exception.reason, 'summary_file_io')
            # Both partial artifacts remain; there is no cleanup/retry.
            self.assertTrue((Path(self.args.out)/'samples.ndjson').is_file())
            self.assertTrue((Path(self.args.out)/'summary.json').is_file())
        def test_endpoint_and_schema_refusals(self):
            for value in ('http://localhost:8890', 'http://127.0.0.1:8890/', 'https://127.0.0.1:8890', 'http://user@127.0.0.1:8890', 'http://127.0.0.1:8890?q=1'):
                with self.assertRaises(Refused):
                    endpoint(value)
            with self.assertRaises(Refused):
                strict_json(b'{"epoch":1,"epoch":2}')
            bad = copy.deepcopy(fixture)
            bad['linear_velocity_world'] = None
            with self.assertRaises(Refused):
                observation(bad, {'epoch': 2})
            self.assertEqual(Handler.requests, [])

    result = unittest.TextTestRunner(verbosity=2).run(unittest.defaultTestLoader.loadTestsFromTestCase(Checks))
    return 0 if result.wasSuccessful() else 1


if __name__ == '__main__':
    sys.exit(main())
