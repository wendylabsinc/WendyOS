"""Finite live cloud input for contained simulation missions.

One native reader and assembler belong to one bridge lifetime. No report replay,
publisher, HTTP request, command, arm, helper restart or deadline renewal exists
here. Runtime identity and motion authority remain the bridge/executor's job.
"""
from __future__ import annotations

from collections import OrderedDict
import argparse
import copy
import hashlib
import json
import os
import re
import selectors
import subprocess
import threading
import time

from .go2_bridge.protocol import Refused, integer, private_file, strict as strict_profile
from .go2_cloud import SOURCE_DIGEST
from .go2_cloud_capture import convert
from .go2_cloud_observer import TOPICS, output_path
from .go2_cloud_shadow import strict, ROW_FIELDS
from .go2_cloud_stream import Assembler

HELPER = '/opt/world/world-go2-cloud-live'
MAX_ROW = 256 << 10
MAX_RAW = 256 << 20
MAX_STDOUT = 576 << 20
MAX_STDERR = 64 << 10
MAX_MESSAGES = 32768
MAX_CLOUDS = 2048
MAX_ORIGINALS = 8


class _Clock:
    """Same five-millisecond read/offset contract as the bridge Clock."""
    def __init__(self, wall=time.time_ns, mono=time.monotonic_ns):
        self.wall, self.mono = wall, mono
        self.last = self.offset = None
        self.faulted = False
        self.lock = threading.Lock()

    def read(self):
        with self.lock:
            if self.faulted:
                raise Refused('clock_fault_latched')
            m0, w, m1 = self.mono(), self.wall(), self.mono()
            bad = (type(m0) is not int or type(m1) is not int or type(w) is not int or
                   m0 < 0 or m1 < m0 or w <= 0 or m1-m0 > 5_000_000)
            if not bad and self.last is not None:
                bad = m0 < self.last[0] or w < self.last[1]
            if not bad and self.offset is not None:
                bad = abs((w-m0)-self.offset) > 5_000_000
            if bad:
                self.faulted = True
                raise Refused('clock_continuity')
            self.offset = w-m0 if self.offset is None else self.offset
            self.last = (m1, w)
            return w, m1


def _code(error, fallback):
    value = str(error)
    return value if re.fullmatch('[a-z_]{1,64}', value) else fallback


class LiveCloudSensors:
    def __init__(self, source_digest, expected_epoch, profile, *, clock=None,
                 process_factory=None, start_thread=True):
        if source_digest != SOURCE_DIGEST:
            raise Refused('sensor_cloud_source')
        integer(expected_epoch, 1, (1 << 53)-1)
        exp = profile.get('cloud_experiment') if type(profile) is dict else None
        if (type(exp) is not dict or set(exp) != {'model', 'scope', 'collector_duration_ms'} or
                exp['model'] != 'coherent-multiring-slab-v1' or exp['scope'] != 'contained-simulation'):
            raise Refused('sensor_cloud_profile')
        duration = integer(exp['collector_duration_ms'], 1, 60000)
        age = integer(profile.get('observation_max_age_ms'), 1, 250)
        skew = integer(profile.get('source_skew_max_ms'), 0, 50)
        self.source_digest, self.expected_epoch = source_digest, expected_epoch
        self.duration_ms, self.age_ns, self.skew_ns = duration, age*1_000_000, skew*1_000_000
        self.clock = clock or _Clock()
        self.lock = threading.RLock()
        self.cleanup_lock = threading.Lock()
        self.stop = threading.Event()
        self.closed = False
        self.fault = ''
        self.rejected = None
        self.rejected_row = None
        self.rejected_row_meta = None
        self.latest = None
        self.originals = OrderedDict()
        self.admissions = OrderedDict()
        self.sequence = 0
        self.loss_generation = 0
        self.unavailable_reason = 'sensors_not_ready'
        self.last_clock = None
        self.process = None
        self.thread = None
        self.cleanup = None
        self.stderr = bytearray()
        self.stdout_bytes = self.raw_bytes = self.messages = self.clouds = self.unavailable = 0
        self.native_elapsed = 0
        self.native_gids, self.native_sources = {}, {}
        self.completion = None
        self.last_assembler_status = None
        wall, start = self._read_clock()
        self.started_ns, self.deadline_ns = start, start+duration*1_000_000
        self.startup_deadline_ns = start+min(3000, duration)*1_000_000
        self.assembler = Assembler(source_digest, expected_epoch, start_ns=start,
            startup_timeout_ns=min(3000, duration)*1_000_000,
            run_timeout_ns=duration*1_000_000, companion_timeout_ns=min(100, duration)*1_000_000)
        self.last_assembler_status = self.assembler.status()
        factory = process_factory or subprocess.Popen
        try:
            # Parent deadline already exists. Popen and ROS initialization use
            # that same lifetime; a late child cannot shift it forward.
            self.process = factory([HELPER, '--duration-ms', str(duration), '--max-clouds', str(MAX_CLOUDS)],
                                   stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                                   stderr=subprocess.PIPE, bufsize=0)
            self._check_time(self._read_clock()[1])
            if start_thread and not self.fault:
                self.thread = threading.Thread(target=self._run, name='world-cloud-live', daemon=True)
                self.thread.start()
            elif self.fault:
                self._cleanup()
        except Exception as error:
            self._fail('sensor_cloud_helper_start')
            self._cleanup()
            raise Refused('sensor_cloud_helper_start') from error

    def _read_clock(self):
        try:
            with self.lock:
                wall, mono = self.clock.read()
                integer(wall, 1); integer(mono)
                if self.last_clock is not None and (wall < self.last_clock[0] or mono < self.last_clock[1]):
                    raise Refused('clock_continuity')
                self.last_clock = (wall, mono)
            return wall, mono
        except Exception as error:
            self._fail('sensor_cloud_clock')
            raise Refused('sensor_cloud_clock') from error

    def _fail(self, reason, native_row=None):
        with self.lock:
            if self.closed:
                return
            if not self.fault:
                self.fault = reason
                self.rejected = {'schema': 'wendy.world.go2-rejected-sensor.v1',
                    'topic': 'cloud', 'reason': reason, 'world_epoch': self.expected_epoch,
                    'publisher_gid': None, 'captured_unix_ns': None, 'quaternion_wxyz': None,
                    'normalized_quaternion_wxyz': None, 'odom_position_xyz': None,
                    'roll_rad': None, 'pitch_rad': None}
                if native_row is not None:
                    self.rejected_row = native_row[:MAX_ROW]
                    self.rejected_row_meta = {'bytes': len(native_row),
                        'sha256': hashlib.sha256(native_row).hexdigest(),
                        'retained_bytes': len(self.rejected_row), 'truncated': len(native_row) > MAX_ROW}
            self.latest = None
            self.loss_generation += 1
            self.stop.set()

    def _missing(self, reason):
        with self.lock:
            self.latest = None
            self.loss_generation += 1
            self.unavailable += 1
            self.unavailable_reason = reason

    def _check_time(self, now):
        with self.lock:
            if now >= self.deadline_ns:
                self._fail('sensor_cloud_run_deadline')
            elif self.sequence == 0 and now >= self.startup_deadline_ns:
                self._fail('sensor_cloud_startup_deadline')
            if self.fault:
                raise Refused(self.fault)
            if self.closed:
                raise Refused('sensor_cloud_closed')

    def _fresh(self, candidate, wall, mono, companion):
        if companion and mono >= candidate.companion_deadline_ns:
            return 'sensor_cloud_companion_deadline'
        stamps = (candidate.cloud.value.capture_unix_ns, candidate.scan.value['capture_unix_ns'],
                  candidate.odom.value['capture_unix_ns'])
        if any(wall < stamp or wall-stamp >= self.age_ns for stamp in stamps):
            return 'sensor_cloud_stale'
        if abs(stamps[0]-stamps[2]) > self.skew_ns:
            return 'sensor_cloud_pair_skew'
        return None

    def _events(self, events, native_row=None):
        for event in events:
            if self.stop.is_set():
                return
            if event.kind in ('fatal', 'closed'):
                self._fail('sensor_cloud_assembler_fault', native_row)
                return
            if event.kind == 'unavailable':
                self._missing('sensor_cloud_companion_missing')
                continue
            if event.kind != 'ready' or event.candidate is None:
                self._fail('sensor_cloud_assembler_fault', native_row)
                return
            c = event.candidate
            wall, now = self._read_clock()
            self._check_time(now)
            reason = self._fresh(c, wall, now, True)
            if reason:
                self._missing(reason)
                continue
            value = convert(c, capture_sequence=self.sequence+1)
            # Construct all diagnostic references before the final real-time
            # check. This work cannot be charged after positive publication.
            original = value.original
            samples = {'cloud': original.cloud, 'scan': original.scan, 'odom': original.odom,
                       'epoch': original.epoch_sample, 'mount': original.transforms[0],
                       'cloud_mount': original.transforms[1]}
            admission = {'sequence': value.sequence, 'candidate_id': original.capture_id,
                'source_digest': self.source_digest, 'observed_epoch': original.observed_epoch,
                'assembled_monotonic_ns': original.assembled_at_ns,
                'companion_deadline_ns': original.companion_deadline_ns,
                'original_capture_stamps_ns': {'cloud': original.cloud.value.capture_unix_ns,
                    'scan': original.scan.value['capture_unix_ns'], 'odom': original.odom.value['capture_unix_ns']},
                'samples': {key: {'topic': sample.topic, 'publisher_gid': sample.publisher_gid,
                    'serialized_sha256': sample.serialized_sha256, 'serialized_bytes': len(sample.raw_cdr),
                    'source_timestamp_ns': sample.source_timestamp_ns, 'received_timestamp_ns': sample.received_timestamp_ns,
                    'helper_elapsed_ns': sample.helper_elapsed_ns, 'arrived_monotonic_ns': sample.arrived_ns}
                    for key, sample in samples.items()}}
            with self.lock:
                wall, now = self._read_clock()
                self._check_time(now)
                reason = self._fresh(original, wall, now, True)
                if reason:
                    self._missing(reason)
                    continue
                if self.fault or self.closed:
                    return
                admission.update(admission_wall_ns=wall, admission_monotonic_ns=now)
                self.sequence += 1
                self.latest = value
                self.originals[self.sequence] = original
                self.admissions[self.sequence] = admission
                while len(self.originals) > MAX_ORIGINALS:
                    self.originals.popitem(last=False)
                    self.admissions.popitem(last=False)
                self.unavailable_reason = ''

    def _ingest_line(self, raw):
        if self.stop.is_set():
            return
        try:
            wall, arrival = self._read_clock()
            self._check_time(arrival)
            row = strict(raw)
            if type(row) is not dict:
                raise Refused('sensor_cloud_row')
            if row.get('kind') == 'complete':
                valid = (set(row) == {'kind', 'reason', 'messages', 'clouds', 'serialized_bytes'} and
                         row['reason'] in ('duration', 'max_clouds', 'capture_limit', 'interrupted') and
                         type(row['messages']) is int and row['messages'] == self.messages and
                         type(row['clouds']) is int and row['clouds'] == self.clouds and
                         type(row['serialized_bytes']) is int and row['serialized_bytes'] == self.raw_bytes)
                with self.lock:
                    self.completion = dict(row) if valid else {'reason': 'invalid'}
                self._fail('sensor_cloud_helper_complete' if valid else 'sensor_cloud_completion_invalid')
                return
            if set(row) != ROW_FIELDS or row['kind'] != 'sample' or row['topic'] not in TOPICS:
                raise Refused('sensor_cloud_row')
            topic = row['topic']
            payload = row['payload_hex']
            if type(payload) is not str or not 0 < len(payload) <= TOPICS[topic]*2 or len(payload)%2:
                raise Refused('sensor_cloud_row')
            size = len(payload)//2
            if (self.messages >= MAX_MESSAGES or self.raw_bytes+size > MAX_RAW or
                    topic == '/utlidar/cloud' and self.clouds >= MAX_CLOUDS):
                raise Refused('sensor_cloud_quota')
            elapsed = integer(row['helper_elapsed_ns'])
            if elapsed < self.native_elapsed or elapsed >= self.duration_ms*1_000_000:
                raise Refused('sensor_cloud_helper_clock')
            # Assembler owns strict CDR, source/GID/stamp checks. It keeps those
            # high-water marks after buffers evict old samples.
            events = self.assembler.ingest(row, arrival)
            with self.lock:
                self.messages += 1
                self.raw_bytes += size
                self.clouds += int(topic == '/utlidar/cloud')
                self.native_elapsed = elapsed
                self.last_assembler_status = self.assembler.status()
            wall, now = self._read_clock()
            self._check_time(now)
            self._events(events, raw)
        except Exception as error:
            self._fail(_code(error, 'sensor_cloud_invalid'), raw)

    def _tick(self):
        if self.stop.is_set():
            return
        try:
            wall, now = self._read_clock()
            self._check_time(now)
            self._events(self.assembler.poll(now))
            with self.lock:
                self.last_assembler_status = self.assembler.status()
                latest = self.latest
            if latest is not None:
                reason = self._fresh(latest.original, wall, now, False)
                if reason:
                    self._missing(reason)
        except Exception as error:
            self._fail(_code(error, 'sensor_cloud_invalid'))

    def _run(self):
        pending = bytearray()
        selector = selectors.DefaultSelector()
        try:
            for stream, kind in ((self.process.stdout, 'stdout'), (self.process.stderr, 'stderr')):
                os.set_blocking(stream.fileno(), False)
                selector.register(stream, selectors.EVENT_READ, kind)
            while not self.stop.is_set():
                self._tick()
                if self.stop.is_set():
                    break
                for key, _ in selector.select(.005):
                    block = os.read(key.fileobj.fileno(), 65536)
                    if not block:
                        selector.unregister(key.fileobj)
                        if key.data == 'stdout':
                            self._fail('sensor_cloud_helper_eof')
                            break
                        continue
                    if key.data == 'stderr':
                        with self.lock:
                            remaining = MAX_STDERR-len(self.stderr)
                            self.stderr.extend(block[:remaining])
                            if len(block) > remaining:
                                self._fail('sensor_cloud_stderr_quota')
                        continue
                    with self.lock:
                        self.stdout_bytes += len(block)
                        if self.stdout_bytes > MAX_STDOUT:
                            self._fail('sensor_cloud_stdout_quota')
                    if self.stop.is_set():
                        break
                    pending.extend(block)
                    while b'\n' in pending and not self.stop.is_set():
                        at = pending.index(b'\n')+1
                        line = bytes(pending[:at]); del pending[:at]
                        self._ingest_line(line)
                    if len(pending) > MAX_ROW:
                        self._fail('sensor_cloud_row_quota')
                if self.process.poll() is not None and not self.stop.is_set():
                    self._fail('sensor_cloud_helper_exit')
        except Exception as error:
            self._fail(_code(error, 'sensor_cloud_helper_unavailable'))
        finally:
            selector.close()
            self._cleanup()

    def _cleanup(self):
        with self.cleanup_lock:
            if self.cleanup is not None or self.process is None:
                return
            actions = []
            try:
                if self.process.poll() is None:
                    actions.append('terminate'); self.process.terminate()
                    try:
                        self.process.wait(timeout=.5)
                    except subprocess.TimeoutExpired:
                        actions.append('kill'); self.process.kill()
                        self.process.wait(timeout=.5)
                else:
                    self.process.wait(timeout=.5)
                result = {'actions': actions, 'reaped': True, 'exit_code': self.process.returncode}
            except Exception:
                result = {'actions': actions, 'reaped': False, 'exit_code': self.process.poll()}
            with self.lock:
                self.cleanup = result
            for stream in (self.process.stdout, self.process.stderr):
                try:
                    if stream is not None:
                        stream.close()
                except Exception:
                    pass

    def snapshot(self):
        wall, now = self._read_clock()
        self._check_time(now)
        with self.lock:
            latest, loss = self.latest, self.loss_generation
            if latest is None:
                raise Refused('sensors_not_ready' if self.sequence == 0 else 'sensor_cloud_unavailable')
        value = latest.bridge_capture()
        value['cloud'] = latest.cloud_payload()
        with self.lock:
            wall, now = self._read_clock()
            self._check_time(now)
            reason = self._fresh(latest.original, wall, now, False)
            if reason:
                self._missing(reason)
                raise Refused(reason)
            if self.fault:
                raise Refused(self.fault)
            if self.closed or loss != self.loss_generation or self.latest is None:
                raise Refused('sensor_cloud_unavailable')
        return value

    def original_capture(self, sequence):
        """Return one owned immutable original; missing/evicted IDs stay absent.

        At most eight tuples survive. Callers serialize only a requested tuple
        under their own response bound. This lookup has no freshness/authority.
        """
        integer(sequence, 1, (1 << 53)-1)
        with self.lock:
            return self.originals.get(sequence)

    def rejected_sample(self):
        with self.lock:
            return copy.deepcopy(self.rejected)

    def capture_diagnostic(self, sequence):
        integer(sequence, 1, (1 << 53)-1)
        with self.lock:
            return copy.deepcopy(self.admissions.get(sequence))

    def rejected_native_row(self):
        """First failed row only; bytes may be a prefix, as diagnostics states."""
        with self.lock:
            return self.rejected_row

    def diagnostics(self):
        with self.lock:
            return {'schema': 'wendy.world.go2-cloud-live-diagnostics.v1', 'simulation': True,
                    'operational_authority': False, 'source_digest': self.source_digest,
                    'expected_epoch': self.expected_epoch, 'started_ns': self.started_ns,
                    'deadline_ns': self.deadline_ns, 'startup_deadline_ns': self.startup_deadline_ns,
                    'fault': self.fault, 'closed': self.closed, 'last_clock': self.last_clock,
                    'worker_alive': self.thread is not None and self.thread.is_alive(),
                    'messages': self.messages, 'clouds': self.clouds, 'serialized_bytes': self.raw_bytes,
                    'stdout_bytes': self.stdout_bytes, 'stderr_bytes': len(self.stderr),
                    'stderr_sha256': hashlib.sha256(self.stderr).hexdigest(),
                    'stderr_prefix_hex': bytes(self.stderr[:4096]).hex(),
                    'accepted_candidates': self.sequence, 'unavailable_candidates': self.unavailable,
                    'unavailable_reason': self.unavailable_reason,
                    'latest_sequence': None if self.latest is None else self.latest.sequence,
                    'retained_sequences': list(self.originals),
                    'retained_admissions': copy.deepcopy(list(self.admissions.values())),
                    'assembler': copy.deepcopy(self.last_assembler_status),
                    'rejected_native_row': copy.deepcopy(self.rejected_row_meta),
                    'completion': copy.deepcopy(self.completion), 'cleanup': copy.deepcopy(self.cleanup)}

    def close(self):
        with self.lock:
            self.closed = True
            self.latest = None
            self.loss_generation += 1
            self.stop.set()
        self._cleanup()
        if self.thread is not None and self.thread is not threading.current_thread():
            self.thread.join(timeout=1.)


def probe(source_digest, expected_epoch, profile, sample_count):
    """Bounded passive probe; every snapshot failure contributes to its report."""
    integer(sample_count, 1, 512)
    live = None
    result = {'schema': 'wendy.world.go2-cloud-live-probe.v1', 'simulation': True,
        'operational_authority': False, 'commands_published': 0, 'runtime_http_calls': 0,
        'source_digest': source_digest, 'expected_epoch': expected_epoch, 'requested_samples': sample_count,
        'samples': [], 'snapshot_failures': 0, 'failure_counts': {}, 'first_failures': [], 'last_failures': [],
        'missed_snapshot_sequences': 0, 'complete': False, 'error': None,
        'time_limit': 'Uncalibrated machine wall time; no runtime identity or qualified clearance claim.'}
    def failure(reason):
        reason = _code(reason, 'sensor_cloud_probe_error')
        counts = result['failure_counts']
        if reason not in counts and len(counts) >= 31:
            reason = 'sensor_cloud_other_error'
        counts[reason] = counts.get(reason, 0)+1
        result['snapshot_failures'] += 1
        event = {'reason': reason, 'monotonic_ns': time.monotonic_ns()}
        if len(result['first_failures']) < 64:
            result['first_failures'].append(event)
        result['last_failures'].append(event)
        del result['last_failures'][:-64]
    try:
        live = LiveCloudSensors(source_digest, expected_epoch, profile)
        previous = 0
        while len(result['samples']) < sample_count:
            try:
                value = live.snapshot()
            except Refused as error:
                failure(error)
                if live.fault or live.closed:
                    result['error'] = live.fault or 'sensor_cloud_closed'
                    break
            else:
                sequence = value['capture_sequence']
                if sequence != previous:
                    evidence = live.capture_diagnostic(sequence)
                    if evidence is None:
                        failure('sensor_cloud_probe_evidence_evicted')
                        result['error'] = 'sensor_cloud_probe_evidence_evicted'
                        break
                    wall, mono = live.clock.read()
                    evidence['probe_observed_wall_ns'] = wall
                    evidence['probe_observed_monotonic_ns'] = mono
                    result['missed_snapshot_sequences'] += sequence-previous-1
                    result['samples'].append(evidence)
                    previous = sequence
            if time.monotonic_ns() >= live.deadline_ns:
                failure('sensor_cloud_probe_deadline')
                result['error'] = 'sensor_cloud_probe_deadline'
                break
            time.sleep(.005)
    except Exception as error:
        result['error'] = _code(error, 'sensor_cloud_probe_error')
        failure(result['error'])
    finally:
        if live is not None:
            result['before_close'] = live.diagnostics()
            if result['before_close']['fault'] and result['error'] is None:
                result['error'] = result['before_close']['fault']
                failure(result['error'])
            live.close()
            result['after_close'] = live.diagnostics()
            if result['after_close']['fault'] and result['error'] is None:
                result['error'] = result['after_close']['fault']
                failure(result['error'])
            if (not result['after_close']['cleanup'] or not result['after_close']['cleanup']['reaped'] or
                    result['after_close']['worker_alive']):
                if result['error'] is None:
                    result['error'] = 'sensor_cloud_cleanup_incomplete'
                    failure(result['error'])
            result['complete'] = (len(result['samples']) == sample_count and result['error'] is None and
                not result['before_close']['fault'] and not result['after_close']['fault'])
    return result


def main(argv=None):
    p = argparse.ArgumentParser(description='Passive bounded live collector probe; no commands or runtime HTTP.')
    p.add_argument('--source-digest', required=True)
    p.add_argument('--expected-epoch', required=True, type=int)
    p.add_argument('--profile', required=True)
    p.add_argument('--samples', required=True, type=int)
    p.add_argument('--out', required=True)
    a = p.parse_args(argv)
    path = output_path(a.out)
    raw_profile = private_file(a.profile, 16384)
    profile = strict_profile(raw_profile, 16384)
    result = probe(a.source_digest, a.expected_epoch, profile, a.samples)
    result['profile_sha256'] = hashlib.sha256(raw_profile).hexdigest()
    raw = (json.dumps(result, sort_keys=True, separators=(',', ':'), allow_nan=False)+'\n').encode()
    if len(raw) > 4 << 20:
        raise Refused('sensor_cloud_probe_report_bound')
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, 'wb') as stream:
        stream.write(raw); stream.flush(); os.fsync(stream.fileno())
    print(json.dumps({'schema': result['schema'], 'complete': result['complete'],
        'samples': len(result['samples']), 'snapshot_failures': result['snapshot_failures'],
        'error': result['error'], 'commands_published': 0, 'runtime_http_calls': 0}), flush=True)
    return 0 if result['complete'] else 1


if __name__ == '__main__':
    try:
        raise SystemExit(main())
    except (OSError, ValueError):
        print('passive_cloud_live_probe_refused', file=__import__('sys').stderr)
        raise SystemExit(1)
