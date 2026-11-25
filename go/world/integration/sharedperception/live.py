"""Live hazard and anonymous tracks from two ordinary native replica readers.

Reuses the existing live source worker, native window, evidence cache and owned
cleanup. This has no replay/inference fallback and does not operate hardware.
"""
from __future__ import annotations

import argparse
from copy import deepcopy
from pathlib import Path

from integration.livetwin.collector import ReaderCursor
from integration.livetwin.pipe import docker_reader
from integration.livetwin.run import LiveSession, NativeWindow
from sim.native_replica_bridge import MAX_DISPLAY_AGE_NS
from . import model
from .run import Runner as SharedRunner


def bridge_config(runner, presentation, hazard_clock, people_clock, *, app=model.APP):
    """Commission the display from both actually observed reader incarnations."""
    for service, clock in (('reader', hazard_clock), ('people-reader', people_clock)):
        model.require(isinstance(clock, dict) and clock.get('deployment_generation') == runner.tasks['b/'+service]['generation']
                      and clock.get('sample_clock_domain') == runner.scene['clock_domain']
                      and isinstance(clock.get('server_boot_id'), str) and 0 < len(clock['server_boot_id']) <= 256,
                      'observed current reader incarnation required')
    devices = {node: runner.pair['nodes'][i]['public']['principal'] for i, node in enumerate(('a', 'b'))}
    def source(service, stream=None, device='a'):
        return dict(device=devices[device], app=app, service=service, stream=stream or service)
    cfg = dict(version=1, run_id=runner.inputs['run_id'], tenant=runner.pair['tenant'], site=runner.scene['site'],
        frame=deepcopy(runner.scene['frame']), model_sha256=presentation['geometry']['model_sha256'],
        anchor_unix_ns=runner.scene['anchor_unix_ns'], clock_domain=runner.scene['clock_domain'],
        reader_boot_id=hazard_clock['server_boot_id'], reader_generation=hazard_clock['deployment_generation'],
        replica_node='gateway-b', source=source('fusion', 'hazard'),
        input_sources={sensor: source(sensor, device='b' if sensor in model.REMOTE_SENSORS else 'a')
                       for sensor in model.HAZARD_SENSORS},
        people=dict(reader_boot_id=people_clock['server_boot_id'],
                    reader_generation=people_clock['deployment_generation'], source=source('people', 'tracks'),
                    input_sources={sensor: source(sensor) for sensor in model.PEOPLE_CAMERAS}),
        device_labels={devices['a']: 'Gateway A', devices['b']: 'Gateway B'})
    if runner.clock.display_clock is not None:
        cfg['display_clock'] = runner.clock.display_clock
    return cfg


class SharedWindow(NativeWindow):
    def __init__(self, *args, mission_config=None, **kwargs):
        super().__init__(*args, mission_config=mission_config, **kwargs)
        self.plan = [
            (2, 'floor-plan', 'camera', 'Floor plan', None),
            (5, 'camera-off', 'command', 'toggle_sensor:camera-b', None),
            (13, 'camera-on', 'command', 'toggle_sensor:camera-b', None),
            (18, 'people-inspector', 'inspector', 'People', None),
            (20, 'overview', 'camera', 'Overview', None),
            (23, 'tracks-off', 'toggle_layer', 'Tracks', False),
            (26, 'tracks-on', 'toggle_layer', 'Tracks', True),
            (29, 'hazard-inspector', 'inspector', 'Hazard', None),
            (32, 'observations-off', 'toggle_layer', 'Observations', False),
            (35, 'observations-on', 'toggle_layer', 'Observations', True),
        ]

    def ready(self, state):
        return super().ready(state) and bool(state['snapshot'].get('tracking', {}).get('tracks'))


def captured_together(frame, run_id, hazard_originals, people_originals):
    """Check actual native capture metadata, not merely a bridge display count."""
    start, end = frame.get('capture_started_monotonic_ns'), frame.get('capture_finished_monotonic_ns')
    if (frame.get('run_id') != run_id or frame.get('mode') != 'live-native-replica' or
            frame.get('display_clock') != 'mach_absolute_time_ns' or
            frame.get('state_sequence') != frame.get('renderer_sequence') or
            type(start) is not int or type(end) is not int or not 0 <= start <= end or
            frame.get('current_native_hazard_captured') is not True or
            frame.get('original_envelope_sha256') not in hazard_originals):
        return False
    return any(track.get('original_envelope_sha256') in people_originals and
               track.get('visible') is True and track.get('fresh_at_start') is True and
               track.get('fresh_at_end') is True and type(track.get('display_deadline_monotonic_ns')) is int and
               end < track['display_deadline_monotonic_ns'] for track in frame.get('native_tracks', []))


class CombinedLiveSession(LiveSession):
    def __init__(self, *args, **kwargs):
        self.people_pipe = self.people_cursor = self.people_clock = None
        self.pending_people_update = self.selected_people = None
        self.people_failed = False
        self.people_ready_updates = self.people_display_iterations = self.combined_display_iterations = 0
        self.people_originals, self.hazard_originals = set(), set()
        self.people_rejected = []
        super().__init__(*args, **kwargs)

    def sensor_ids(self):
        return model.SENSORS

    def live_bridge_config(self, presentation, clock):
        if self.people_clock is None:
            return None
        return bridge_config(self, presentation, clock, self.people_clock)

    def native_window(self, session):
        return SharedWindow(self.executable, session, self.files, self.out, self.walkthrough, self.clock,
                            mission_config=getattr(self, 'mission_config', None))

    def open_additional_readers(self):
        b = self.nodes['b']
        # Called after hazard EOF attachment and before the first Source.step.
        # Store the pipe immediately so an attach failure still closes its PID.
        self.people_pipe = docker_reader(b.target(), b.service_volume('people-reader', 'proofs')+'/stdout.json',
                                         self.out/'people-reader-pipe.stderr')
        self.people_cursor = ReaderCursor(self.inputs['run_id'], self.people_pipe.read,
                                          reader_kind='people', monotonic_ns=self.clock)
        self.people_cursor.attach()

    def _people_failure(self, error):
        if self.people_failed:
            return
        self.people_failed = True
        self.pending_people_update = self.selected_people = None
        if self.bridge is not None:
            self.bridge.people.latest = None
        self.save('live/people-reader-failure.json', {'status': 'failed', 'error': type(error).__name__,
                  'layer_cleared': 'people', 'reconnected': False})

    def primary_reader_failure(self, error):
        self.hazard_failed = True
        self.pending_reader_update = self.media_digest = None
        if self.bridge is not None:
            self.bridge.latest = None
            self.bridge.unknown_reason = 'The hazard reader stopped. Waiting for a new session.'
        self.save('live/hazard-reader-failure.json', {'status': 'failed', 'error': type(error).__name__,
                  'layer_cleared': 'hazard', 'reconnected': False})

    def poll_additional_readers(self):
        if self.people_failed:
            return
        try:
            updates = self.people_cursor.poll()
            for update in updates:
                self.save('live/people-event-%04d.json' % update['sequence'], update)
                reader = update['reader']
                if reader['status'] == 'failed':
                    raise RuntimeError('ordinary people reader failed')
                clock = reader.get('clock')
                if clock is not None:
                    model.require(clock['deployment_generation'] == self.tasks['b/people-reader']['generation'] and
                                  clock['sample_clock_domain'] == self.scene['clock_domain'],
                                  'people reader task/clock differs')
                    if self.people_clock is not None:
                        model.require(clock['server_boot_id'] == self.people_clock['server_boot_id'],
                                      'people reader boot changed')
                    self.people_clock = deepcopy(clock)
            queued = self.pending_people_update if self.pending_people_update is not None else (
                self.selected_people[0] if self.selected_people is not None else None)
            self.selected_people, self.pending_people_update = self.select_reader_update(updates, queued)
            if self.people_cursor.closed:
                raise RuntimeError('ordinary people reader ended before the live session')
        except Exception as exc:
            self._people_failure(exc)

    def ingest_additional_readers(self):
        if self.bridge is None or self.selected_people is None or self.people_failed:
            return
        selected, presentation = self.selected_people
        self.selected_people = None
        # Waiting for the second incarnation never turns an old row into fresh
        # input. Use its original pre-EOF collection checkpoint unchanged.
        if not 0 <= self.clock()-selected['collection_started_monotonic_ns'] < MAX_DISPLAY_AGE_NS:
            return
        update = {**selected, 'presentation': presentation}
        try:
            if self.bridge.ingest_people(update):
                self.people_ready_updates += update['reader']['status'] == 'ready'
                self.people_originals.update(row['envelope_sha256'] for row in update['reader']['tracks'])
        except ValueError as exc:
            self.people_rejected.append({'sequence': update['sequence'], 'reason': str(exc.__cause__ or exc)})
        self.save('live/people-update-%04d.json' % update['sequence'], update)

    def observe_additional_layers(self, state):
        snapshot = state['snapshot']
        tracks = snapshot.get('tracking', {}).get('tracks', [])
        hazard = snapshot['fusion']
        self.people_display_iterations += bool(tracks)
        self.combined_display_iterations += bool(tracks) and hazard['state'] == 'hazard'
        if hazard['state'] == 'hazard':
            self.hazard_originals.add(hazard['original_envelope_sha256'])
        return {'track_count': len(tracks), 'track_originals': [t['original_envelope_sha256'] for t in tracks]}

    def validate_additional_layers(self):
        self.save('live/people-rejected-updates.json', self.people_rejected)
        model.require(not self.people_failed and not self.hazard_failed and self.people_ready_updates > 0 and
                      self.people_display_iterations > 0 and self.combined_display_iterations > 0,
                      'both current ordinary native result layers must reach the twin')
        captures = [] if self.window is None else [frame for frame in self.window.frames if
            captured_together(frame, self.inputs['run_id'], self.hazard_originals, self.people_originals)]
        model.require(self.executable is None or captures,
                      'no native window capture showed current visible tracks and hazard together')
        return {'people_ready_updates': self.people_ready_updates,
                'people_display_iterations': self.people_display_iterations,
                'combined_display_iterations': self.combined_display_iterations,
                'unique_people_originals': len(self.people_originals),
                'unique_hazard_originals': len(self.hazard_originals),
                'native_combined_captures': len(captures), 'people_rejected_updates': len(self.people_rejected)}

    def close_additional_readers(self):
        if self.people_pipe is not None:
            self.people_pipe.close()


class Runner(CombinedLiveSession, SharedRunner):
    pass


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--stage', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--executable', type=Path)
    parser.add_argument('--duration', type=int, default=45)
    parser.add_argument('--walkthrough', action='store_true')
    parser.add_argument('--trace-rpc', action='store_true')
    args = parser.parse_args()
    Runner(args.stage, args.output, executable=args.executable, duration=args.duration,
           walkthrough=args.walkthrough, trace_rpc=args.trace_rpc).execute()


if __name__ == '__main__':
    main()
