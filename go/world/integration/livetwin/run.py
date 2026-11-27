"""Show a current ordinary native replica in a separately owned macOS twin.

Uses the existing two-Agent remote-media fixture and its normal app deployment.
Only that fixture's MuJoCo source accepts controls. No physical-device selector,
new VM, replay fallback or local fusion is exposed.
"""
from __future__ import annotations

import argparse
from concurrent.futures import ThreadPoolExecutor
import json
import os
from pathlib import Path
import signal
import subprocess
import time

from integration.remotemediafusion import model
from integration.remotemediafusion.run import Runner as RemoteRunner
from integration.livetwin.collector import ReaderCursor
from integration.livetwin.clock import host_clock
from integration.livetwin.media import EvidenceCache
from integration.livetwin.pipe import docker_reader
from sim.macos_demo_bridge import SessionFiles
from sim.native_replica_bridge import NativeReplicaBridge, MAX_DISPLAY_AGE_NS, timestamp


def bridge_config(runner, presentation, clock, *, display_clock=None):
    principals = {key: runner.pair['nodes'][index]['public']['principal']
                  for index, key in enumerate(('a', 'b'))}
    config = dict(version=1, run_id=runner.inputs['run_id'], tenant=runner.pair['tenant'],
        site=runner.scene['site'], frame=runner.scene['frame'],
        model_sha256=presentation['geometry']['model_sha256'],
        anchor_unix_ns=runner.scene['anchor_unix_ns'], clock_domain=runner.scene['clock_domain'],
        reader_boot_id=clock['server_boot_id'], reader_generation=runner.tasks['b/reader']['generation'],
        replica_node='gateway-b',
        source=dict(device=principals['a'], app=model.APP, service='fusion', stream=model.STREAM),
        input_sources={sensor: dict(device=principals['b' if sensor in model.REMOTE_SENSORS else 'a'],
            app=model.APP, service=sensor, stream=sensor) for sensor in model.SENSORS},
        device_labels={principals['a']: 'Gateway A', principals['b']: 'Gateway B'})
    if display_clock is not None and display_clock.display_clock is not None:
        config['display_clock'] = display_clock.display_clock
    return config


def current_update(updates, presentation, anchor):
    """Do not pair a queued pre-step observation with a newer scene."""
    selected = None
    for update in updates:
        reader = update['reader']
        if reader['status'] == 'failed':
            raise RuntimeError('ordinary native reader failed')
        clock = reader.get('clock')
        if clock and timestamp(clock['sample_time']) - anchor == presentation['simulation_time_ns']:
            selected = update
    return selected


def presented_update(updates, presentations, anchor):
    """Match a newly received event to its original scene, including in-flight steps."""
    selected = None
    for update in updates:
        if update['reader']['status'] == 'failed':
            raise RuntimeError('ordinary native reader failed')
        clock = update['reader'].get('clock')
        if clock:
            presentation = presentations.get(timestamp(clock['sample_time']) - anchor)
            if presentation is not None:
                selected = update, presentation
    return selected


def retain_presented_update(updates, presentations, anchor, queued, now_ns, pending_sample_ns):
    """Keep at most one new event while its source presentation is in flight.

    A newer reader observation supersedes the queued event even when empty.
    Its original collection timestamp is never replaced or extended. Existing
    ReaderCursor and bridge checks still guard already-consumed reader ticks.
    """
    def order(update):
        reader = update['reader']
        return (reader['tick'], reader['status'] == 'ready', reader.get('startup_attempt', 0))

    newest = queued
    for update in updates:
        if update['reader']['status'] == 'failed':
            raise RuntimeError('ordinary native reader failed')
        if newest is None or order(update) > order(newest):
            newest = update
    if newest is None:
        return None, None
    age = now_ns - newest['collection_started_monotonic_ns']
    if not 0 <= age < MAX_DISPLAY_AGE_NS:
        return None, None
    clock = newest['reader'].get('clock')
    if clock is None:
        return None, None
    sample_ns = timestamp(clock['sample_time']) - anchor
    presentation = presentations.get(sample_ns)
    if presentation is not None:
        return (newest, presentation), None
    return None, newest if sample_ns == pending_sample_ns else None


class NativeWindow:
    """Own one fresh process; diagnostics use the same native model as controls."""
    def __init__(self, executable, session, files, output, walkthrough, clock, *, mission_config=None,
                 viewer_profile=None):
        self.session, self.files, self.output = session, files, output
        self.clock = clock
        argv = [str(executable), '--session', str(session)]
        if mission_config is not None:
            mission_config = Path(mission_config)
            model.require(mission_config.is_absolute() and mission_config.is_file() and
                          0 < mission_config.stat().st_size <= 16 << 10,
                          'absolute bounded mission config file required')
            argv += ['--mission-config', str(mission_config)]
        if viewer_profile is not None:
            viewer_profile = Path(viewer_profile)
            model.require(viewer_profile.is_absolute() and viewer_profile.is_file() and
                          0 < viewer_profile.stat().st_size <= 64 << 10,
                          'absolute bounded native viewer profile required')
            argv += ['--viewer-profile', str(viewer_profile)]
        self.process = subprocess.Popen(argv,
            stdout=(output / 'native.stdout').open('xb'), stderr=(output / 'native.stderr').open('xb'),
            start_new_session=True)
        self.walkthrough = walkthrough
        self.start = self.next_frame = self.pending_capture = None
        self.pending_action = None
        self.frame_count = self.action_index = 0
        self.frames, self.actions = [], []
        self.plan = [
            (2, 'floor-plan', 'camera', 'Floor plan', None),
            (5, 'camera-off', 'command', 'toggle_sensor:camera-b', None),
            (13, 'camera-on', 'command', 'toggle_sensor:camera-b', None),
            (20, 'overview', 'camera', 'Overview', None),
            (23, 'observations-off', 'toggle_layer', 'Observations', False),
            (26, 'observations-on', 'toggle_layer', 'Observations', True),
        ]

    def ready(self, state):
        return state['snapshot']['fusion']['state'] == 'hazard'

    def poll(self, state):
        now = self.clock() / 1e9
        if self.process.poll() is not None:
            raise RuntimeError('owned native window exited during live session')
        if self.start is None:
            if not (self.session / 'app-ready.json').exists() or not self.ready(state):
                return
            self.start = self.next_frame = now
        if self.pending_capture:
            path = self.session / ('app-capture-' + self.pending_capture + '.png')
            receipt_path = path.with_suffix('.png.json')
            if path.is_file() and receipt_path.is_file():
                receipt = model.strict_json(receipt_path.read_bytes(), 32 << 10)
                model.require(receipt['run_id'] == state['run_id'] and
                              receipt['png_sha256'] == model.digest(path.read_bytes()), 'native capture receipt differs')
                self.frames.append(dict(file=str(path.relative_to(self.output)),
                    receipt=str(receipt_path.relative_to(self.output)),
                    elapsed_s=receipt['capture_finished_monotonic_ns']/1e9-self.start,
                    **receipt))
                self.pending_capture = None
        if self.pending_capture is None and now >= self.next_frame:
            self.frame_count += 1
            self.pending_capture = 'live-%04d' % self.frame_count
            path = self.session / 'capture-request.json'
            self.files.write('capture-request.json', {'id': self.pending_capture}, replace=path.exists())
            self.next_frame = now + .25
        if self.pending_action:
            path = self.session / ('qa-result-' + self.pending_action + '.json')
            if path.is_file():
                result = model.strict_json(path.read_bytes(), 32 << 10)
                self.actions.append(result)
                self.pending_action = None
        if self.walkthrough and self.pending_action is None and self.action_index < len(self.plan):
            due, name, action, target, enabled = self.plan[self.action_index]
            if now-self.start >= due and (action != 'command' or state['snapshot']['controls_available']):
                request = dict(id=name, action=action, target=target)
                if enabled is not None:
                    request['enabled'] = enabled
                path = self.session / 'qa-request.json'
                self.files.write('qa-request.json', request, replace=path.exists())
                self.pending_action = name
                self.action_index += 1

    def close(self):
        # Only the exact process launched above is signaled.
        if self.process.poll() is None:
            try:
                self.process.wait(timeout=2)
            except subprocess.TimeoutExpired:
                os.killpg(self.process.pid, signal.SIGTERM)
                try:
                    self.process.wait(timeout=3)
                except subprocess.TimeoutExpired:
                    os.killpg(self.process.pid, signal.SIGKILL)
                    self.process.wait(timeout=3)
        return dict(frames=self.frames, actions=self.actions, process_exit=self.process.returncode,
                    interaction='native application-model diagnostics, not OS input',
                    capture='live native window view images, not desktop screen recording')


class LiveSession:
    """Shared live presentation loop; the following base owns normal deployment."""
    def __init__(self, prepared, output, *, executable=None, duration=42, walkthrough=False, trace_rpc=False):
        model.require(type(duration) is int and 15 <= duration <= 45, 'demo duration must be 15–45 seconds')
        if executable is not None:
            executable = Path(executable).resolve(strict=True)
            model.require(executable.is_file() and os.access(executable, os.X_OK), 'native executable required')
        model.require(not walkthrough or executable is not None, 'walkthrough requires a native window')
        super().__init__(prepared, output, trace_rpc=trace_rpc)
        self.clock = host_clock()
        model.require(executable is None or self.clock.display_clock == 'mach_absolute_time_ns',
                      'native Mac viewer requires the raw Mach display clock')
        self.executable, self.duration, self.walkthrough = executable, duration, walkthrough
        self.bridge = self.files = self.window = self.media = self.pipe = self.source_worker = None
        self.pending_step = None
        self.pending_reader_update = None
        self.hazard_failed = False
        self.media_digest = None
        self.simulation_ns = self.source_sequence = 0
        self.enabled = list(self.sensor_ids())
        self.presentation = None
        self.presentations = {}

    def sensor_ids(self):
        return model.SENSORS

    def live_bridge_config(self, presentation, clock):
        return bridge_config(self, presentation, clock, display_clock=self.clock)

    def native_window(self, session):
        return NativeWindow(self.executable, session, self.files, self.out, self.walkthrough, self.clock,
                            mission_config=getattr(self, 'mission_config', None),
                            viewer_profile=getattr(self, 'viewer_profile', None))

    def ingest_hazard_reader(self, update):
        return self.bridge.ingest(update)

    def open_additional_readers(self):
        pass

    def poll_additional_readers(self):
        pass

    def ingest_additional_readers(self):
        pass

    def close_additional_readers(self):
        pass

    def primary_reader_failure(self, error):
        raise error

    def observe_additional_layers(self, state):
        return {}

    def validate_additional_layers(self):
        return {}

    def select_reader_update(self, updates, queued):
        pending_sample = self.pending_step['request']['simulation_time_ns'] if self.pending_step else None
        return retain_presented_update(updates, self.presentations, self.scene['anchor_unix_ns'],
                                       queued, self.clock(), pending_sample)

    def poll_source_presentation(self):
        pass

    def close_source_presentation(self):
        pass

    def source_command_minimum(self):
        return self.simulation_ns+1_000_000_000

    def periodic_source_request(self):
        return dict(simulation_time_ns=self.source_command_minimum(), sensors=self.enabled)

    def read_cursor(self, offset, inode):
        return self.pipe.read(offset, inode)

    def source_io(self, request):
        response = model.strict_json(self.nodes['a'].scene_request('/v1/step', request))
        presentation = model.strict_json(self.nodes['a'].scene_request('/v1/presentation'), 4 << 20)
        return response, presentation

    def apply_source(self, request, response, presentation):
        model.require(response == dict(sequence=self.source_sequence+1, **request), 'source step differs; no retry')
        model.require(presentation['simulation_time_ns'] == request['simulation_time_ns'], 'source presentation differs')
        self.source_sequence += 1
        self.simulation_ns, self.enabled = request['simulation_time_ns'], request['sensors']
        self.save('live/source-%04d.json' % self.source_sequence, dict(request=request, response=response))
        self.presentation = presentation
        self.presentations[self.simulation_ns] = presentation
        model.require(len(self.presentations) <= 32, 'live presentation budget exceeded')

    def begin_source(self, request, *, command=False):
        model.require(self.pending_step is None, 'one owned source step at a time')
        self.pending_step = dict(request=request, command=command,
                                 future=self.source_worker.submit(self.source_io, request))

    def finish_source(self, *, wait=False):
        pending = self.pending_step
        if pending is None or not wait and not pending['future'].done():
            return
        self.pending_step = None
        try:
            response, presentation = pending['future'].result(timeout=13)
            self.apply_source(pending['request'], response, presentation)
        except Exception as exc:
            if pending['command']:
                self.bridge.finish_source_step(error=exc)
            raise
        if pending['command']:
            receipt = self.bridge.finish_source_step(response)
            model.require(receipt['accepted'], 'source command outcome unknown; no retry')

    def live(self):
        self.step('live-native-twin')
        session = self.out / 'session'
        session.mkdir(mode=0o700)
        self.files = SessionFiles(session, self.inputs['run_id'])
        b = self.nodes['b']
        self.pipe = docker_reader(b.target(), b.service_volume('reader', 'proofs') + '/stdout.json',
                                  self.out / 'reader-pipe.stderr')
        cursor = ReaderCursor(self.inputs['run_id'], self.read_cursor, monotonic_ns=self.clock)
        # The original reader file is created by the already deployed app.
        cursor.attach()
        self.open_additional_readers()
        step_started = time.monotonic()
        first_request = dict(simulation_time_ns=1_000_000_000, sensors=self.enabled)
        self.apply_source(first_request, *self.source_io(first_request))
        self.source_worker = ThreadPoolExecutor(max_workers=1, thread_name_prefix='native-twin-source')
        started = time.monotonic()
        next_step = step_started + 2
        self.media = EvidenceCache(self.nodes['a'].target(),
            self.nodes['a'].service_volume('fusion', 'proofs') + '/stdout.json', self.save)
        states, rejected = [], []
        ready_count = hazard_count = 0
        while time.monotonic()-started < self.duration and not self.files.stop_requested():
            self.finish_source()
            self.poll_source_presentation()
            self.poll_additional_readers()
            matched = None
            if not self.hazard_failed:
                try:
                    updates = cursor.poll()
                    matched, self.pending_reader_update = self.select_reader_update(updates, self.pending_reader_update)
                    if cursor.closed:
                        raise RuntimeError('ordinary reader ended before the live session')
                except Exception as exc:
                    self.primary_reader_failure(exc)
            selected, presentation = matched if matched is not None else (None, None)
            if selected is not None:
                if self.bridge is None:
                    config = self.live_bridge_config(self.presentation, selected['reader']['clock'])
                    if config is None:
                        self.pending_reader_update, selected = selected, None
                    else:
                        self.save('live/bridge-config.json', config)
                        self.bridge = NativeReplicaBridge(config, self.presentation, self.files, monotonic_ns=self.clock)
                        if self.executable:
                            self.window = self.native_window(session)
            if selected is not None:
                digests = [h['envelope_sha256'] for h in selected['reader']['hazards']]
                selected.update(presentation=presentation, media=self.media.available(digests))
                try:
                    if self.ingest_hazard_reader(selected):
                        ready_count += selected['reader']['status'] == 'ready'
                        self.media_digest = digests[0] if digests else None
                except ValueError as exc:
                    self.media_digest = None
                    rejected.append(dict(sequence=selected['sequence'], reason=str(exc.__cause__ or exc)))
            self.ingest_additional_readers()
            if self.bridge is not None:
                if self.media_digest is not None:
                    self.bridge.attach_media(self.media.available([self.media_digest]),
                                             envelope_sha256=self.media_digest)
                state = self.bridge.export()
                if state['snapshot']['fusion']['state'] == 'hazard':
                    hazard_count += 1
                states.append(dict(elapsed_s=time.monotonic()-started, sequence=state['sequence'],
                    state=state['snapshot']['fusion']['state'], controls=state['snapshot']['controls_available'],
                    time_s=state['snapshot']['time_s'], **self.observe_additional_layers(state)))
                if self.window:
                    self.window.poll(state)
                # Preserve the original receipt after publishing the current view.
                if selected is not None:
                    self.save('live/update-%04d.json' % selected['sequence'], selected)
                command = self.files.command(self.bridge.command_sequence+1)
                if command is not None and state['snapshot']['controls_available'] and self.pending_step is None:
                    step_started = time.monotonic()
                    request = self.bridge.begin_source_step(command, self.source_command_minimum())
                    self.begin_source(request, command=True)
                    next_step = step_started+2
            if time.monotonic() >= next_step and self.pending_step is None:
                step_started = time.monotonic()
                request = self.periodic_source_request()
                if request is not None:
                    self.begin_source(request)
                next_step = step_started+2
            time.sleep(.01)
        self.finish_source(wait=True)
        self.save('live/display-states.json', states)
        self.save('live/rejected-updates.json', rejected)
        model.require(ready_count > 0 and hazard_count > 0, 'no current native hazard reached the twin')
        if self.walkthrough:
            accepted = {row['id'] for row in self.window.actions if row.get('accepted') is True}
            model.require(accepted == {row[1] for row in self.window.plan} and self.bridge.command_sequence == 2,
                          'native walkthrough did not complete all view and sensor controls')
            model.require(any(frame.get('current_native_hazard_captured') is True for frame in self.window.frames),
                          'no current native hazard was visually captured')
        additional = self.validate_additional_layers()
        return dict(ready_updates=ready_count, hazard_display_frames=hazard_count,
                    rejected_updates=len(rejected), source_steps=self.source_sequence,
                    sensor_commands=0 if self.bridge is None else self.bridge.command_sequence,
                    replay=False, physical_devices_used=False, **additional)

    def execute(self):
        result = dict(passed=False, simulation_only=True)
        prior = {}
        def interrupted(signum, _):
            raise InterruptedError('owned live run interrupted')
        for signum in (signal.SIGINT, signal.SIGTERM):
            prior[signum] = signal.signal(signum, interrupted)
        try:
            try:
                self.setup(); self.enroll(); self.projects(); self.source_scene(); self.commission(); self.deploy()
                result['observations'] = self.live()
                result['gate_completed'] = True
            except BaseException as exc:
                result.update(gate_completed=False, error=type(exc).__name__, failed_phase=self.runtime_state,
                              reason=str(exc)[:300])
            finally:
                try:
                    try:
                        if self.bridge:
                            self.bridge.export(closed=True)
                    finally:
                        if self.window:
                            self.save('live/native-window.json', self.window.close())
                except Exception as exc:
                    result.update(gate_completed=False, native_close_error=type(exc).__name__)
                finally:
                    try:
                        try:
                            try:
                                try:
                                    if self.source_worker:
                                        self.source_worker.shutdown(wait=True, cancel_futures=True)
                                finally:
                                    self.close_source_presentation()
                            finally:
                                if self.media:
                                    self.media.close()
                        finally:
                            try:
                                try:
                                    self.close_additional_readers()
                                finally:
                                    if self.pipe:
                                        self.pipe.close()
                            finally:
                                if self.files:
                                    self.files.close()
                    except Exception as exc:
                        result.update(gate_completed=False, session_close_error=type(exc).__name__)
                    finally:
                        result['cleanup'] = self.cleanup()
                result['passed'] = result.get('gate_completed', False) and result['cleanup']['complete']
                self.save('result.json', result)
            model.require(result['passed'], 'live twin failed; original receipts retained')
            return result
        finally:
            for signum, handler in prior.items():
                signal.signal(signum, handler)


class Runner(LiveSession, RemoteRunner):
    """The original single-hazard live workflow and command-line API."""


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--stage', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--executable', type=Path)
    parser.add_argument('--duration', type=int, default=42)
    parser.add_argument('--walkthrough', action='store_true')
    parser.add_argument('--trace-rpc', action='store_true')
    args = parser.parse_args()
    Runner(args.stage, args.output, executable=args.executable, duration=args.duration,
           walkthrough=args.walkthrough, trace_rpc=args.trace_rpc).execute()


if __name__ == '__main__':
    main()
