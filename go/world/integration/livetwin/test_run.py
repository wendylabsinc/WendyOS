"""Pure launcher matching/configuration checks; no lifecycle or UI invocation."""
from copy import deepcopy
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

from integration.livetwin.run import LiveSession, NativeWindow, bridge_config, current_update, presented_update, retain_presented_update
from sim.native_replica_bridge import MAX_DISPLAY_AGE_NS


ANCHOR = 1767225600000000000  # 2026-01-01T00:00:00Z, the configured source anchor.
DEVICE_A = 'spiffe://wendy.sh/tenant/tenant-a/device/gateway-a'
DEVICE_B = 'spiffe://wendy.sh/tenant/tenant-a/device/gateway-b'


class MissionLaunchTests(unittest.TestCase):
    def test_optional_config_is_one_argument_and_default_argv_is_unchanged(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            config = root / 'operator config.json'
            config.write_text('{}')
            for value in (None, config):
                out = root / ('default' if value is None else 'missions')
                out.mkdir()
                with patch('integration.livetwin.run.subprocess.Popen') as launch:
                    NativeWindow('/app', root / 'session', None, out, False, lambda: 0,
                                 mission_config=value)
                args, kwargs = launch.call_args
                expected = ['/app', '--session', str(root / 'session')]
                if value is not None:
                    expected += ['--mission-config', str(config)]
                self.assertEqual(args, (expected,))
                self.assertTrue(kwargs['start_new_session'])
                kwargs['stdout'].close(); kwargs['stderr'].close()

    def test_invalid_config_refuses_before_process_or_output_files(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            empty, large = root / 'empty', root / 'large'
            empty.write_bytes(b''); large.write_bytes(b'x' * ((16 << 10) + 1))
            for value in (Path('relative.json'), root / 'missing', root, empty, large):
                with self.subTest(value=value), patch('integration.livetwin.run.subprocess.Popen') as launch:
                    with self.assertRaises(ValueError):
                        NativeWindow('/app', root, None, root, False, lambda: 0, mission_config=value)
                    launch.assert_not_called()
                    self.assertFalse((root / 'native.stdout').exists())
                    self.assertFalse((root / 'native.stderr').exists())

    def test_session_passes_optional_exported_host_path(self):
        runner = LiveSession.__new__(LiveSession)
        runner.executable, runner.files, runner.out, runner.walkthrough, runner.clock = '/app', None, '/out', False, None
        for value in (None, Path('/host/operator config.json')):
            if value is not None:
                runner.mission_config = value
            with patch('integration.livetwin.run.NativeWindow') as window:
                runner.native_window('/session')
            self.assertEqual(window.call_args.kwargs, {'mission_config': value, 'viewer_profile': None})

    def test_viewer_profile_passes_as_one_argument_without_exposing_contents(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            profile = root / 'native viewer.json'
            profile.write_text('{"credentials":{"private_key":"do-not-log"}}')
            with patch('integration.livetwin.run.subprocess.Popen') as launch:
                NativeWindow('/app', root, None, root, False, lambda: 0, viewer_profile=profile)
            args, kwargs = launch.call_args
            self.assertEqual(args[0], ['/app', '--session', str(root), '--viewer-profile', str(profile)])
            self.assertNotIn('do-not-log', str(args))
            kwargs['stdout'].close(); kwargs['stderr'].close()

    def test_invalid_viewer_profile_refuses_before_launch(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            empty, large = root / 'empty', root / 'large'
            empty.write_bytes(b''); large.write_bytes(b'x' * ((64 << 10) + 1))
            for value in (Path('relative.json'), root / 'missing', root, empty, large):
                with self.subTest(value=value), patch('integration.livetwin.run.subprocess.Popen') as launch:
                    with self.assertRaises(ValueError):
                        NativeWindow('/app', root, None, root, False, lambda: 0, viewer_profile=value)
                    launch.assert_not_called()
                    self.assertFalse((root / 'native.stdout').exists())


def update(sequence, sample, *, status='ready'):
    """An already-collected event, including its original age and result bytes."""
    result = {'version': 1, 'run_id': 'live-office', 'sequence': sequence,
              'collection_started_monotonic_ns': 100_000_000 + sequence,
              'reader': {'status': status, 'tick': sequence, 'sample_time': sample,
                         'hazards': [{'envelope_json': 'original-envelope', 'envelope_sha256': 'a' * 64}]}}
    if status != 'failed':
        result['reader']['clock'] = {
            'sample_time': sample, 'sample_clock_domain': 'simulation:live-office',
            'physical_earliest': '2026-11-12T12:00:00Z',
            'physical_latest': '2026-11-12T12:00:00.001Z',
            'server_boot_id': 'ordinary-reader-boot', 'deployment_generation': 17}
    return result


class CurrentUpdateTests(unittest.TestCase):
    def test_pending_new_instant_row_is_consumed_once_after_original_presentation_arrives(self):
        row = update(13, '2026-01-01T00:00:04Z'); before = deepcopy(row)
        old = {'simulation_time_ns': 3_000_000_000}
        cache = {3_000_000_000: old}
        start = row['collection_started_monotonic_ns']
        match, queued = retain_presented_update([row], cache, ANCHOR, None, start+100, 4_000_000_000)
        self.assertIsNone(match); self.assertIs(queued, row)
        fresh = {'simulation_time_ns': 4_000_000_000}
        cache[4_000_000_000] = fresh
        match, queued = retain_presented_update([], cache, ANCHOR, queued, start+200, None)
        self.assertIs(match[0], row); self.assertIs(match[1], fresh); self.assertIsNone(queued)
        self.assertEqual(row, before)
        self.assertEqual(retain_presented_update([], cache, ANCHOR, queued, start+300, None), (None, None))

    def test_queued_partial_presentation_wait_does_not_renew_original_collection_age(self):
        row = update(13, '2026-01-01T00:00:04Z'); start = row['collection_started_monotonic_ns']
        _, queued = retain_presented_update([row], {}, ANCHOR, None, start, 4_000_000_000)
        duplicate = deepcopy(row); duplicate['sequence'] += 1
        duplicate['collection_started_monotonic_ns'] = start + MAX_DISPLAY_AGE_NS - 1
        _, queued = retain_presented_update([duplicate], {}, ANCHOR, queued, start+MAX_DISPLAY_AGE_NS-1, 4_000_000_000)
        self.assertIs(queued, row)
        cache = {4_000_000_000: {'simulation_time_ns': 4_000_000_000}}
        self.assertEqual(retain_presented_update([], cache, ANCHOR, queued, start+MAX_DISPLAY_AGE_NS, None), (None, None))

    def test_newer_successful_empty_read_supersedes_queued_positive_without_resurrection(self):
        positive = update(13, '2026-01-01T00:00:04Z')
        empty = update(14, '2026-01-01T00:00:04Z'); empty['reader']['hazards'] = []
        now = empty['collection_started_monotonic_ns'] + 100
        _, queued = retain_presented_update([empty], {}, ANCHOR, positive, now, 4_000_000_000)
        self.assertIs(queued, empty)
        cache = {4_000_000_000: {'simulation_time_ns': 4_000_000_000}}
        match, queued = retain_presented_update([positive], cache, ANCHOR, queued, now+100, None)
        self.assertIs(match[0], empty); self.assertEqual(match[0]['reader']['hazards'], [])
        self.assertIsNone(queued)

    def test_only_pending_sample_is_retained_and_clock_regression_cannot_make_it_fresh(self):
        row = update(13, '2026-01-01T00:00:04Z'); start = row['collection_started_monotonic_ns']
        self.assertEqual(retain_presented_update([row], {}, ANCHOR, None, start, 5_000_000_000), (None, None))
        self.assertEqual(retain_presented_update([row], {}, ANCHOR, None, start-1, 4_000_000_000), (None, None))
        failed = update(14, '2026-01-01T00:00:04Z', status='failed')
        with self.assertRaisesRegex(RuntimeError, 'ordinary native reader failed'):
            retain_presented_update([failed], {}, ANCHOR, row, start+100, 4_000_000_000)

    def test_in_flight_source_step_keeps_original_presentation_available(self):
        observed = update(13, '2026-01-01T00:00:03Z')
        original = {'simulation_time_ns': 3_000_000_000, 'sensor_sources': [{'id': 'camera-b', 'enabled': True}]}
        newer = {'simulation_time_ns': 4_000_000_000, 'sensor_sources': [{'id': 'camera-b', 'enabled': False}]}
        selected, presentation = presented_update([observed], {3_000_000_000: original, 4_000_000_000: newer}, ANCHOR)
        self.assertIs(selected, observed)
        self.assertIs(presentation, original)
        self.assertEqual(selected['collection_started_monotonic_ns'], 100_000_013)

    def test_queued_pre_step_reader_cannot_acquire_new_presentation(self):
        earlier = update(3, '2026-01-01T00:00:01Z')
        presentation = {'simulation_time_ns': 2_000_000_000, 'sensor_sources': [
            {'id': 'camera-b', 'enabled': False}]}
        before = deepcopy((earlier, presentation))
        self.assertIsNone(current_update([earlier], presentation, ANCHOR))
        self.assertEqual((earlier, presentation), before)

    def test_exact_matching_row_and_its_collection_age_are_preserved(self):
        earlier = update(1, '2026-01-01T00:00:01Z')
        matching = update(2, '2026-01-01T00:00:02Z')
        later_other_scene = update(3, '2026-01-01T00:00:03Z')
        rows = [earlier, matching, later_other_scene]
        before = deepcopy(rows)
        selected = current_update(rows, {'simulation_time_ns': 2_000_000_000}, ANCHOR)
        self.assertIs(selected, matching)
        self.assertEqual(rows, before)
        self.assertEqual(selected['collection_started_monotonic_ns'], 100_000_002)
        self.assertNotIn('presentation', selected)

    def test_latest_matching_tick_wins_without_rounding_sample_nanoseconds(self):
        rounded = update(1, '2026-01-01T00:00:04.1Z')
        first = update(2, '2026-01-01T00:00:04.100000001Z')
        second = update(3, '2026-01-01T00:00:04.100000001Z')
        presentation = {'simulation_time_ns': 4_100_000_001}
        self.assertIsNone(current_update([rounded], presentation, ANCHOR))
        self.assertIs(current_update([rounded, first, second], presentation, ANCHOR), second)

    def test_waiting_without_clock_is_not_a_current_observation(self):
        waiting = update(1, '2026-01-01T00:00:01Z', status='waiting_for_source')
        waiting['reader'].pop('clock')
        waiting['reader']['hazards'] = []
        self.assertIsNone(current_update([waiting], {'simulation_time_ns': 1_000_000_000}, ANCHOR))

    def test_terminal_failure_propagates_even_after_matching_observation(self):
        matching = update(1, '2026-01-01T00:00:01Z')
        failed = update(2, '2026-01-01T00:00:02Z', status='failed')
        for rows in ([failed], [matching, failed], [failed, matching]):
            with self.subTest(order=[row['sequence'] for row in rows]):
                with self.assertRaisesRegex(RuntimeError, '^ordinary native reader failed$'):
                    current_update(rows, {'simulation_time_ns': 1_000_000_000}, ANCHOR)


class ConfigurationTests(unittest.TestCase):
    def test_live_mixin_delegates_existing_lifecycle_once_and_preserves_default_view_gate(self):
        calls = []
        class Lifecycle:
            def __init__(self, prepared, output, *, trace_rpc=False):
                calls.append((prepared, output, trace_rpc))
        class Isolated(LiveSession, Lifecycle):
            pass
        runner = Isolated('prepared', 'output', duration=42, trace_rpc=True)
        self.assertEqual(calls, [('prepared', 'output', True)])
        self.assertEqual(set(runner.enabled), {'camera-a', 'camera-b', 'audio-1', 'gas-1', 'lidar-atlas'})
        self.assertEqual(runner.duration, 42)
        self.assertFalse(runner.hazard_failed)
        window = NativeWindow.__new__(NativeWindow)
        self.assertTrue(window.ready({'snapshot': {'fusion': {'state': 'hazard'}}}))
        self.assertFalse(window.ready({'snapshot': {'fusion': {'state': 'expired'}}}))

    def test_gateway_ownership_and_observed_reader_identity(self):
        runner = SimpleNamespace(
            inputs={'run_id': 'live-office', 'expected_generation': 1},
            pair={'tenant': 'tenant-a', 'nodes': [
                {'public': {'principal': DEVICE_A}}, {'public': {'principal': DEVICE_B}}]},
            scene={'site': 'synthetic-office', 'frame': {'id': 'office', 'revision': 'office-' + 'b' * 16},
                   'anchor_unix_ns': ANCHOR, 'clock_domain': 'simulation:live-office'},
            tasks={'b/reader': {'generation': 17}, 'a/fusion': {'generation': 29}})
        presentation = {'geometry': {'model_sha256': 'b' * 64}}
        clock = update(1, '2026-01-01T00:00:01Z')['reader']['clock']
        before = deepcopy((runner, presentation, clock))
        config = bridge_config(runner, presentation, clock)
        self.assertEqual(set(config['input_sources']), {'camera-a', 'camera-b', 'audio-1', 'gas-1', 'lidar-atlas'})
        self.assertEqual(config['source'], {
            'device': DEVICE_A, 'app': 'com.wendy.world.remotemediafusion', 'service': 'fusion', 'stream': 'hazard'})
        for sensor, source in config['input_sources'].items():
            with self.subTest(sensor=sensor):
                expected_device = DEVICE_B if sensor in ('camera-b', 'audio-1') else DEVICE_A
                self.assertEqual(source, {'device': expected_device, 'app': 'com.wendy.world.remotemediafusion',
                                          'service': sensor, 'stream': sensor})
        self.assertEqual(config['reader_generation'], 17)
        self.assertEqual(config['reader_boot_id'], 'ordinary-reader-boot')
        self.assertEqual(config['replica_node'], 'gateway-b')
        self.assertEqual(config['clock_domain'], runner.scene['clock_domain'])
        self.assertEqual(config['anchor_unix_ns'], ANCHOR)
        self.assertEqual(config['frame'], runner.scene['frame'])
        self.assertEqual(config['model_sha256'], 'b' * 64)
        self.assertEqual(config['device_labels'], {DEVICE_A: 'Gateway A', DEVICE_B: 'Gateway B'})
        self.assertEqual((runner, presentation, clock), before)


if __name__ == '__main__':
    unittest.main()
