"""Combined reader/display tests with no Docker, app process or source calls."""
from copy import deepcopy
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

from integration.sharedperception import live, model
from integration.livetwin import run as base
from sim.native_replica_bridge import NativeReplicaBridge, MAX_DISPLAY_AGE_NS
from sim.test_native_people_layer import Clock, people_fixture, instant


class Cursor:
    closed = False
    def __init__(self, rows): self.rows = rows
    def poll(self):
        rows, self.rows = self.rows, []
        return rows


def runner_fixture():
    config, presentation, hazard, people = people_fixture()
    r = live.Runner.__new__(live.Runner)
    r.clock = Clock()
    r.clock.display_clock = 'mach_absolute_time_ns'
    r.inputs = {'run_id': config['run_id']}
    r.pair = {'tenant': config['tenant'], 'nodes': [
        {'public': {'principal': device}} for device in config['device_labels']]}
    r.scene = {key: config[key] for key in ('site', 'frame', 'anchor_unix_ns', 'clock_domain')}
    r.tasks = {'b/reader': {'generation': 9}, 'b/people-reader': {'generation': 12}}
    r.people_clock = r.pending_people_update = r.selected_people = r.pending_step = None
    r.pending_reader_update = {'original_hazard_queue': True}
    r.people_pipe = None
    r.people_failed = r.hazard_failed = False
    r.people_ready_updates = r.people_display_iterations = r.combined_display_iterations = 0
    r.people_originals, r.hazard_originals, r.people_rejected = set(), set(), []
    r.presentations = {presentation['simulation_time_ns']: presentation}
    r.bridge = NativeReplicaBridge(config, presentation, monotonic_ns=r.clock)
    r.bridge.ingest(hazard)
    r.saved = {}
    r.save = lambda name, value: r.saved.setdefault(name, deepcopy(value))
    r.people_cursor = Cursor([people])
    r.executable = r.window = None
    r.media_digest = hazard['reader']['hazards'][0]['envelope_sha256']
    return r, presentation, hazard, people


class CombinedLiveTests(unittest.TestCase):
    def test_configuration_waits_for_both_observed_incarnations_and_separates_source_sets(self):
        r, presentation, hazard, people = runner_fixture()
        before = deepcopy((presentation, hazard, people))
        self.assertIsNone(r.live_bridge_config(presentation, hazard['clock']))
        r.poll_additional_readers()
        cfg = r.live_bridge_config(presentation, hazard['clock'])
        self.assertEqual(cfg['reader_generation'], 9)
        self.assertEqual(cfg['people']['reader_generation'], 12)
        self.assertEqual(cfg['people']['reader_boot_id'], people['reader']['clock']['server_boot_id'])
        self.assertEqual(set(cfg['input_sources']), set(model.HAZARD_SENSORS))
        self.assertEqual(set(cfg['people']['input_sources']), set(model.PEOPLE_CAMERAS))
        for sensor, source in cfg['input_sources'].items():
            device = r.pair['nodes'][int(sensor in model.REMOTE_SENSORS)]['public']['principal']
            self.assertEqual(source['device'], device)
            self.assertEqual(source['app'], model.APP)
        self.assertEqual(cfg['people']['source']['service'], 'people')
        self.assertEqual(cfg['people']['source']['stream'], 'tracks')
        self.assertEqual((presentation, hazard, people), before)
        bad = deepcopy(people['reader']['clock']); bad['deployment_generation'] += 1
        with self.assertRaises(ValueError): live.bridge_config(r, presentation, hazard['clock'], bad)

    def test_independent_people_queue_retains_original_age_and_does_not_consume_hazard_queue(self):
        r, presentation, hazard, people = runner_fixture()
        people['reader']['clock']['sample_time'] = instant(2_000_000_000)
        people['reader']['sample_time'] = instant(2_000_000_000)
        r.pending_step = {'request': {'simulation_time_ns': 2_000_000_000}}
        original_started = people['collection_started_monotonic_ns']
        r.poll_additional_readers()
        self.assertIs(r.pending_people_update, people)
        self.assertEqual(r.pending_reader_update, {'original_hazard_queue': True})
        r.presentations[2_000_000_000] = {**presentation, 'simulation_time_ns': 2_000_000_000}
        r.clock.now += 50_000_000
        r.poll_additional_readers(); r.ingest_additional_readers()
        saved = r.saved['live/people-update-0001.json']
        self.assertEqual(saved['collection_started_monotonic_ns'], original_started)
        self.assertEqual(saved['reader'], people['reader'])
        self.assertEqual(r.bridge.people.latest['started'], original_started)
        self.assertEqual(r.people_ready_updates, 1)
        self.assertEqual(r.bridge.latest['started'], hazard['collection_started_monotonic_ns'])
        self.assertEqual(r.bridge.export()['snapshot']['time_s'], 2)

    def test_initial_second_reader_wait_cannot_refresh_an_old_positive(self):
        r, _presentation, _hazard, people = runner_fixture()
        actual_bridge, r.bridge = r.bridge, None
        r.poll_additional_readers()
        r.clock.now = people['collection_started_monotonic_ns'] + MAX_DISPLAY_AGE_NS
        r.bridge = actual_bridge
        r.ingest_additional_readers()
        self.assertEqual(r.people_ready_updates, 0)
        self.assertIsNone(r.bridge.people.latest)
        self.assertNotIn('live/people-update-0001.json', r.saved)

    def test_people_failure_clears_only_people_and_hazard_failure_only_hazard(self):
        r, _presentation, _hazard, _people = runner_fixture()
        r.poll_additional_readers(); r.ingest_additional_readers()
        self.assertTrue(r.bridge.export()['snapshot']['tracking']['tracks'])
        r.people_cursor = Cursor([{'sequence': 2, 'reader': {'status': 'failed'}}])
        r.poll_additional_readers()
        self.assertTrue(r.people_failed)
        self.assertEqual(r.bridge.export()['snapshot']['tracking']['tracks'], [])
        self.assertEqual(r.bridge.export()['snapshot']['fusion']['state'], 'hazard')
        self.assertIn('live/people-reader-failure.json', r.saved)
        fresh, *_ = runner_fixture()
        fresh.poll_additional_readers(); fresh.ingest_additional_readers()
        fresh.primary_reader_failure(RuntimeError('ended'))
        self.assertEqual(fresh.bridge.export()['snapshot']['fusion']['state'], 'unknown')
        self.assertTrue(fresh.bridge.export()['snapshot']['tracking']['tracks'])
        self.assertIsNone(fresh.media_digest)

    def test_current_typed_results_and_captured_pixels_are_separate_gates(self):
        r, _presentation, _hazard, _people = runner_fixture()
        r.poll_additional_readers(); r.ingest_additional_readers()
        for _ in range(4): r.observe_additional_layers(r.bridge.export())
        result = r.validate_additional_layers()
        self.assertEqual(result['combined_display_iterations'], 4)
        self.assertEqual(result['unique_hazard_originals'], 1)
        self.assertEqual(result['unique_people_originals'], 1)
        self.assertEqual(result['native_combined_captures'], 0)
        r.executable, r.window = 'requested-app', SimpleNamespace(frames=[])
        with self.assertRaises(ValueError): r.validate_additional_layers()
        frame = {'run_id': r.inputs['run_id'], 'mode': 'live-native-replica',
            'display_clock': 'mach_absolute_time_ns', 'state_sequence': 9, 'renderer_sequence': 9,
            'capture_started_monotonic_ns': 100, 'capture_finished_monotonic_ns': 120,
            'current_native_hazard_captured': True, 'original_envelope_sha256': next(iter(r.hazard_originals)),
            'native_tracks': [{'original_envelope_sha256': next(iter(r.people_originals)),
                               'visible': True, 'fresh_at_start': True, 'fresh_at_end': True,
                               'display_deadline_monotonic_ns': 121}]}
        r.window.frames = [frame]
        self.assertEqual(r.validate_additional_layers()['native_combined_captures'], 1)
        for change in (lambda f: f['native_tracks'][0].update(fresh_at_end=False),
                       lambda f: f['native_tracks'][0].update(visible=False),
                       lambda f: f['native_tracks'][0].update(original_envelope_sha256='f'*64),
                       lambda f: f['native_tracks'][0].update(display_deadline_monotonic_ns=120),
                       lambda f: f.update(mode='replay'), lambda f: f.update(renderer_sequence=8)):
            bad = deepcopy(frame); change(bad)
            self.assertFalse(live.captured_together(bad, r.inputs['run_id'], r.hazard_originals, r.people_originals))

    def test_native_window_waits_for_both_layers_and_uses_bounded_existing_controls(self):
        config = Path('/host/operator config.json')
        with patch.object(base.NativeWindow, '__init__', return_value=None) as initialize:
            window = live.SharedWindow(mission_config=config)
        self.assertEqual(initialize.call_args.kwargs, {'mission_config': config})
        self.assertEqual(len(window.plan), 10)
        self.assertEqual(max(row[0] for row in window.plan), 35)
        self.assertEqual([row[3] for row in window.plan if row[2] == 'command'],
                         ['toggle_sensor:camera-b', 'toggle_sensor:camera-b'])
        state = {'snapshot': {'fusion': {'state': 'hazard'}, 'tracking': {'tracks': []}}}
        self.assertFalse(window.ready(state))
        state['snapshot']['tracking']['tracks'] = [{'id': 'observed'}]
        self.assertTrue(window.ready(state))
        state['snapshot']['fusion']['state'] = 'expired'
        self.assertFalse(window.ready(state))

    def test_combined_window_passes_optional_exported_host_config(self):
        runner = live.CombinedLiveSession.__new__(live.CombinedLiveSession)
        runner.executable, runner.files, runner.out, runner.walkthrough, runner.clock = '/app', None, '/out', False, None
        for value in (None, Path('/host/operator config.json')):
            if value is not None:
                runner.mission_config = value
            with patch.object(live, 'SharedWindow') as window:
                runner.native_window('/session')
            self.assertEqual(window.call_args.kwargs, {'mission_config': value})

    def test_second_pipe_attach_failure_still_closes_both_owned_pipes(self):
        class Pipe:
            def __init__(self, fail=False): self.fail, self.closed, self.attached = fail, 0, False
            def read(self, offset, inode):
                self.attached = True
                if self.fail: raise RuntimeError('injected attachment failure')
                return dict(completeBytes=b'', newOffset=0, inode='fixed', size=0, partial=False, eof=True)
            def close(self): self.closed += 1
        r, *_ = runner_fixture()
        first, second = Pipe(), Pipe(True)
        with tempfile.TemporaryDirectory() as directory:
            r.out = Path(directory)
            r.step = lambda phase: setattr(r, 'runtime_state', phase)
            r.setup = r.enroll = r.projects = r.source_scene = r.commission = r.deploy = lambda: None
            r.cleanup = lambda: {'complete': True}
            r.files = r.window = r.media = r.pipe = r.source_worker = None
            r.bridge = None
            r.executable = None; r.duration = 15; r.walkthrough = False
            r.nodes = {'b': SimpleNamespace(target=lambda: 'a'*64,
                service_volume=lambda service, _kind: '/owned/'+service)}
            with patch.object(base, 'docker_reader', return_value=first), patch.object(live, 'docker_reader', return_value=second):
                with self.assertRaises(ValueError): r.execute()
            self.assertTrue(first.attached and second.attached)
            self.assertEqual((first.closed, second.closed), (1, 1))
            self.assertTrue(r.saved['result.json']['cleanup']['complete'])
            self.assertFalse(r.saved['result.json']['passed'])


if __name__ == '__main__': unittest.main()
