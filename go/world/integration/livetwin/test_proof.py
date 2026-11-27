"""Offline proof correspondence with unsigned structural fixtures, no runtime."""
from copy import deepcopy
import hashlib
import json
from pathlib import Path
import tempfile
import unittest

from integration.livetwin.proof import audit, PLAN
from sim.native_replica_bridge import NativeReplicaBridge
from sim.test_native_replica_bridge import live_fixture, restored
from integration.remotemediafusion.test_proof import instant


def write(root, name, value):
    path = root / name; path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value))


def read(root, name):
    return json.loads((root / name).read_bytes())


def fixture(root):
    config, presentation, first = live_fixture()
    config['display_clock'] = 'mach_absolute_time_ns'
    write(root, 'live/bridge-config.json', config)
    write(root, 'result.json', {'passed': True, 'gate_completed': True, 'simulation_only': True,
        'observations': {'replay': False, 'physical_devices_used': False},
        'cleanup': {'complete': True, 'resources': [{'after': 'absent'}]}})
    empty = deepcopy(first)
    empty['reader']['hazards'] = []; empty['media'] = []
    empty['reader']['sample_time'] = empty['clock']['sample_time'] = instant(2_000_000_000)
    empty['presentation']['simulation_time_ns'] = 2_000_000_000
    for source in empty['presentation']['sensor_sources']:
        if source['id'] == 'camera-b': source['enabled'] = False
    fresh = deepcopy(first); restored(fresh, 3_000_000_000)
    frames = []
    for index, update in enumerate((first, empty, fresh), 1):
        started = index * 10_000_000_000
        update['sequence'] = update['reader']['tick'] = index
        update['collection_started_monotonic_ns'] = started
        update['reader']['clock'] = deepcopy(update.pop('clock'))
        write(root, 'live/update-%04d.json' % index, update)
        now = [started]
        bridge = NativeReplicaBridge(config, update['presentation'], monotonic_ns=lambda: now[0])
        bridge.ingest(update)
        now[0] += 100_000_000
        state = bridge.export()
        hazard = bool(update['reader']['hazards'])
        png = b'\x89PNG\r\n\x1a\nunsigned-structural-test-' + bytes([index])
        image = 'session/app-capture-%d.png' % index
        (root / 'session').mkdir(exist_ok=True); (root / image).write_bytes(png)
        receipt = {'schema': 'wendy.macos-demo.app-capture.v1', 'run_id': config['run_id'],
            'mode': 'live-native-replica', 'state_sequence': index, 'renderer_sequence': index,
            'display_clock': 'mach_absolute_time_ns', 'capture_started_monotonic_ns': now[0],
            'capture_finished_monotonic_ns': now[0] + 100_000_000,
            'display_deadline_monotonic_ns': state['display_deadline_monotonic_ns'],
            'observation_deadline_monotonic_ns': state['observation_deadline_monotonic_ns'],
            'fresh_at_start': hazard, 'fresh_at_end': hazard,
            'native_observation_fresh_at_start': True, 'native_observation_fresh_at_end': True,
            'fusion_state': 'hazard' if hazard else 'no_live_result', 'localized_hazard': hazard,
            'hazard_visible': hazard, 'current_native_hazard_captured': hazard,
            'sample_time_s': state['snapshot']['time_s'],
            'layers': [{'name': 'Hazards', 'enabled': True, 'hidden': False, 'child_nodes': int(hazard)}],
            'sensor_sources': update['presentation']['sensor_sources'],
            'png_sha256': hashlib.sha256(png).hexdigest(), 'png_bytes': len(png)}
        if hazard:
            receipt['original_envelope_sha256'] = update['reader']['hazards'][0]['envelope_sha256']
        write(root, image + '.json', receipt)
        frames.append({'file': image, 'receipt': image + '.json', 'elapsed_s': index, **receipt})
    actions = []
    for index, (name, action) in enumerate(PLAN):
        value = {'schema': 'wendy.macos-demo.app-qa.v1', 'id': name, 'action': action,
                 'kind': 'application_model_check_not_OS_input', 'run_id': config['run_id'],
                 'accepted': True, 'worker_command_sequence': min(index, 2),
                 'camera': 'Floor plan' if name == 'floor-plan' else 'Overview',
                 'layers': [{'name': 'Observations', 'enabled': name != 'observations-off'}]}
        write(root, 'session/qa-result-' + name + '.json', value); actions.append(value)
    for index, ns in ((1, 2_000_000_000), (2, 4_000_000_000)):
        command = {'schema': 'wendy.macos-demo.command.v1', 'run_id': config['run_id'], 'sequence': index,
                   'action': 'toggle_sensor', 'target': 'camera-b'}
        sensors = [s for s in config['input_sources'] if s != 'camera-b' or index == 2]
        request = {'simulation_time_ns': ns, 'sensors': sensors}
        response = {'sequence': index + 1, **request}
        receipt = {'schema': 'wendy.macos-demo.command-result.v1', 'run_id': config['run_id'],
                   'sequence': index, 'action': 'toggle_sensor', 'accepted': True, 'no_mutation_retry': True,
                   'source_request': request, 'source_response': response}
        write(root, 'session/commands/%06d.json' % index, command)
        write(root, 'session/receipts/%06d.json' % index, receipt)
        write(root, 'live/source-%04d.json' % (index + 1), {'request': request, 'response': response})
    write(root, 'live/native-window.json', {'frames': frames, 'actions': actions, 'process_exit': 0})


class ProofTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(); self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name); fixture(self.root)

    def change_capture(self, index, change):
        window = read(self.root, 'live/native-window.json')
        frame = window['frames'][index]
        receipt = read(self.root, frame['receipt']); change(receipt)
        write(self.root, frame['receipt'], receipt)
        frame.update(receipt); write(self.root, 'live/native-window.json', window)

    def test_complete_exact_walkthrough_and_original_envelopes(self):
        result = audit(self.root)
        self.assertTrue(result['passed'], result['issues'])
        self.assertEqual(result['status'], 'complete')
        self.assertEqual((result['actions'], result['source_commands'], result['native_frames']), (6, 2, 3))
        self.assertFalse(result['independent_crypto_verification'])
        self.assertFalse(result['new_time_authority'])
        self.assertFalse(result['absence_is_clearance'])
        selected = result['selected']
        self.assertNotEqual(selected['before_camera_off']['envelope_sha256'], selected['after_camera_on']['envelope_sha256'])

    def test_failed_source_run_stays_incomplete_even_with_complete_footage(self):
        result = read(self.root, 'result.json'); result.update(passed=False, gate_completed=False)
        write(self.root, 'result.json', result)
        report = audit(self.root)
        self.assertEqual(report['status'], 'incomplete'); self.assertFalse(report['source_run_passed'])

    def test_all_six_actions_and_both_exact_toggle_targets_required(self):
        window = read(self.root, 'live/native-window.json'); window['actions'].pop()
        write(self.root, 'live/native-window.json', window)
        self.assertFalse(audit(self.root)['passed'])
        fixture(self.root)
        command = read(self.root, 'session/commands/000001.json'); command['target'] = 'camera-a'
        write(self.root, 'session/commands/000001.json', command)
        self.assertFalse(audit(self.root)['passed'])

    def test_png_or_sidecar_ledger_mismatch_refused(self):
        (self.root / 'session/app-capture-1.png').write_bytes(b'changed')
        self.assertFalse(audit(self.root)['passed'])
        fixture(self.root)
        receipt = read(self.root, 'session/app-capture-1.png.json'); receipt['state_sequence'] = 999
        write(self.root, 'session/app-capture-1.png.json', receipt)
        self.assertFalse(audit(self.root)['passed'])

    def test_unknown_or_expired_display_cannot_establish_observed_absence(self):
        for state in ('unknown', 'expired'):
            with self.subTest(state=state):
                fixture(self.root)
                self.change_capture(1, lambda r: r.update(fusion_state=state))
                self.assertFalse(audit(self.root)['passed'])

    def test_extra_honest_stale_empty_capture_is_preserved_but_not_absence_evidence(self):
        window = read(self.root, 'live/native-window.json')
        original_empty = window['frames'][1]
        receipt = read(self.root, original_empty['receipt'])
        receipt.update(native_observation_fresh_at_start=False, native_observation_fresh_at_end=False,
                       capture_started_monotonic_ns=receipt['observation_deadline_monotonic_ns'],
                       capture_finished_monotonic_ns=receipt['observation_deadline_monotonic_ns']+100_000_000)
        image = 'session/app-capture-stale-empty.png'
        (self.root / image).write_bytes((self.root / original_empty['file']).read_bytes())
        write(self.root, image + '.json', receipt)
        window['frames'].insert(2, {'file': image, 'receipt': image + '.json', 'elapsed_s': 2.5, **receipt})
        write(self.root, 'live/native-window.json', window)
        report = audit(self.root)
        self.assertTrue(report['passed'], report['issues'])
        self.assertEqual(report['native_frames'], 4)
        self.assertEqual(report['excluded_stale_empty_frames'], 1)
        self.assertEqual(report['observed_empty_frames'], 1)
        self.assertEqual(report['selected']['camera_off_empty']['frame'], original_empty['file'])
        self.assertEqual(report['artifacts'][image]['sha256'], receipt['png_sha256'])
        self.assertIn(image + '.json', report['artifacts'])
        # Exclusion does not bypass original file correspondence.
        (self.root / image).write_bytes(b'changed stale PNG')
        self.assertFalse(audit(self.root)['passed'])

    def test_only_stale_empty_frames_cannot_prove_source_loss_absence(self):
        for fresh_start, fresh_end in ((False, False), (True, False), (False, True)):
            with self.subTest(fresh_start=fresh_start, fresh_end=fresh_end):
                fixture(self.root)
                self.change_capture(1, lambda r: r.update(native_observation_fresh_at_start=fresh_start,
                                                          native_observation_fresh_at_end=fresh_end))
                report = audit(self.root)
                self.assertFalse(report['passed'])
                self.assertEqual(report['excluded_stale_empty_frames'], 1)
                self.assertIn('current hazard, observed source-loss absence and restored hazard frames required', report['issues'])

    def test_claimed_current_hazard_with_stale_observation_still_fails(self):
        for state in ('hazard', 'no_live_result'):
            with self.subTest(state=state):
                fixture(self.root)
                self.change_capture(0, lambda r: r.update(fusion_state=state, native_observation_fresh_at_end=False))
                report = audit(self.root)
                self.assertFalse(report['passed'])
                self.assertIn('claimed native observation was stale during capture', report['issues'])

    def test_stale_native_capture_flag_or_extended_clock_deadline_refused(self):
        self.change_capture(0, lambda r: r.update(fresh_at_end=False))
        self.assertFalse(audit(self.root)['passed'])
        fixture(self.root)
        self.change_capture(0, lambda r: r.update(display_deadline_monotonic_ns=r['display_deadline_monotonic_ns']+1))
        self.assertFalse(audit(self.root)['passed'])

    def test_reader_original_bytes_and_source_scope_stay_bound(self):
        update = read(self.root, 'live/update-0001.json')
        update['reader']['hazards'][0]['envelope_sha256'] = '0' * 64
        write(self.root, 'live/update-0001.json', update)
        self.assertFalse(audit(self.root)['passed'])
        fixture(self.root)
        config = read(self.root, 'live/bridge-config.json'); config['source']['device'] = 'different-device'
        write(self.root, 'live/bridge-config.json', config)
        self.assertFalse(audit(self.root)['passed'])

    def test_saved_source_ack_and_capture_sensor_state_must_match(self):
        source = read(self.root, 'live/source-0002.json'); source['response']['simulation_time_ns'] += 1
        write(self.root, 'live/source-0002.json', source)
        self.assertFalse(audit(self.root)['passed'])
        fixture(self.root)
        def wrong_switch(receipt):
            for row in receipt['sensor_sources']:
                if row['id'] == 'camera-b': row['enabled'] = True
        self.change_capture(1, wrong_switch)
        self.assertFalse(audit(self.root)['passed'])


if __name__ == '__main__':
    unittest.main()
