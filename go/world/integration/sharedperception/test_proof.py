"""Unsigned structural receipt fixtures; no process, inference or live capture."""
from copy import deepcopy
from pathlib import Path
import tempfile
import unittest

from integration.livetwin.test_proof import fixture as hazard_fixture, read, write
from integration.sharedperception.proof import audit, PLAN, SOURCES
from sim.native_replica_bridge import NativeReplicaBridge
from sim.test_native_people_layer import people_fixture, instant


def fixture(root):
    hazard_fixture(root)
    config, presentation, _, people = people_fixture()
    config['display_clock'] = 'mach_absolute_time_ns'
    write(root, 'live/bridge-config.json', config)
    window = read(root, 'live/native-window.json')
    for index, frame in enumerate(window['frames'], 1):
        update = read(root, 'live/update-%04d.json' % index)
        additions = deepcopy(presentation['sensor_sources'][-2:])
        update['presentation']['sensor_sources'] += additions
        write(root, 'live/update-%04d.json' % index, update)
        receipt = read(root, frame['receipt'])
        receipt['sensor_sources'] += additions
        receipt['native_tracks'] = []
        receipt['layers'] += [{'name': 'Tracks', 'enabled': True, 'hidden': False, 'child_nodes': int(index == 1)}]
        receipt['inspector_panel'] = 'Hazard'
        if index == 1:
            people['collection_started_monotonic_ns'] = update['collection_started_monotonic_ns']
            people['presentation'] = deepcopy(update['presentation'])
            write(root, 'live/people-update-0001.json', people)
            bridge = NativeReplicaBridge(config, people['presentation'],
                                         monotonic_ns=lambda: people['collection_started_monotonic_ns'])
            bridge.ingest_people(people)
            tracks = bridge.export()['snapshot']['tracking']['tracks']
            receipt['native_tracks'] = [{'id': row['id'], 'original_envelope_sha256': row['original_envelope_sha256'],
                'display_deadline_monotonic_ns': row['display_deadline_monotonic_ns'],
                'visible': True, 'fresh_at_start': True, 'fresh_at_end': True} for row in tracks]
        write(root, frame['receipt'], receipt)
        frame.update(receipt)
    actions = []
    for index, (name, action) in enumerate(PLAN):
        value = {'schema': 'wendy.macos-demo.app-qa.v1', 'id': name, 'action': action,
                 'kind': 'application_model_check_not_OS_input', 'run_id': config['run_id'],
                 'accepted': True, 'worker_command_sequence': min(index, 2),
                 'camera': 'Floor plan' if name == 'floor-plan' else 'Overview',
                 'inspector_panel': 'People' if name == 'people-inspector' else 'Hazard',
                 'layers': [{'name': 'Tracks', 'enabled': name != 'tracks-off'},
                            {'name': 'Observations', 'enabled': name != 'observations-off'}]}
        write(root, 'session/qa-result-' + name + '.json', value)
        actions.append(value)
    window['actions'] = actions
    write(root, 'live/native-window.json', window)
    for index in (1, 2):
        receipt = read(root, 'session/receipts/%06d.json' % index)
        sensors = sorted(SOURCES - ({'camera-b'} if index == 1 else set()))
        receipt['source_request']['sensors'] = sensors
        receipt['source_response']['sensors'] = sensors
        write(root, 'session/receipts/%06d.json' % index, receipt)
        write(root, 'live/source-%04d.json' % receipt['source_response']['sequence'],
              {'request': receipt['source_request'], 'response': receipt['source_response']})


class SharedProofTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(); self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name); fixture(self.root)

    def capture(self, index, change):
        window = read(self.root, 'live/native-window.json')
        frame = window['frames'][index]
        receipt = read(self.root, frame['receipt']); change(receipt)
        write(self.root, frame['receipt'], receipt)
        frame.update(receipt); write(self.root, 'live/native-window.json', window)

    def test_complete_combined_capture_and_unique_original_counts(self):
        result = audit(self.root)
        self.assertTrue(result['passed'], result['issues'])
        self.assertEqual((result['actions'], result['source_commands'], result['current_combined_frames']), (10, 2, 1))
        self.assertEqual((result['unique_hazard_originals'], result['unique_people_originals']), (2, 1))
        self.assertEqual(result['selected']['together']['frame'], 'session/app-capture-1.png')
        self.assertEqual(len(result['selected']['together']['tracks']), 1)
        self.assertFalse(result['independent_crypto_verification']); self.assertFalse(result['absence_is_clearance'])

    def test_newer_people_presentation_does_not_rebase_hazard_observation(self):
        update = read(self.root, 'live/people-update-0001.json')
        update['reader']['sample_time'] = update['reader']['clock']['sample_time'] = instant(1_100_000_000)
        update['presentation']['simulation_time_ns'] = 1_100_000_000
        write(self.root, 'live/people-update-0001.json', update)
        self.capture(0, lambda r: r.update(sample_time_s=1.1))
        result = audit(self.root)
        self.assertTrue(result['passed'], result['issues'])
        selected = result['selected']['together']
        self.assertEqual((selected['sample_ns'], selected['hazard_sample_ns']), (1_100_000_000, 1_000_000_000))
        self.capture(0, lambda r: r.update(observation_deadline_monotonic_ns=r['observation_deadline_monotonic_ns']+1))
        self.assertFalse(audit(self.root)['passed'])

    def test_both_layers_must_be_visible_and_fresh_in_one_actual_frame(self):
        self.capture(0, lambda r: r['native_tracks'][0].update(visible=False))
        self.assertFalse(audit(self.root)['passed'])
        fixture(self.root)
        self.capture(0, lambda r: r['layers'][1].update(enabled=False))
        self.assertFalse(audit(self.root)['passed'])

    def test_track_deadline_and_original_digest_cannot_be_extended_or_replaced(self):
        for field, value in (('display_deadline_monotonic_ns', 999_000_000_000),
                             ('original_envelope_sha256', 'e'*64), ('fresh_at_end', False)):
            with self.subTest(field=field):
                fixture(self.root)
                self.capture(0, lambda r: r['native_tracks'][0].update({field: value}))
                self.assertFalse(audit(self.root)['passed'])

    def test_new_shared_sample_must_not_display_expired_people(self):
        update = read(self.root, 'live/update-0001.json')
        update['reader']['sample_time'] = update['reader']['clock']['sample_time'] = instant(2_250_000_000)
        update['presentation']['simulation_time_ns'] = 2_250_000_000
        write(self.root, 'live/update-0001.json', update)
        self.capture(0, lambda r: r.update(sample_time_s=2.25))
        report = audit(self.root)
        self.assertFalse(report['passed'])
        self.assertIn('captured track has no matching original people observation', report['issues'])

    def test_people_reader_has_its_own_boot_generation_and_original_bytes(self):
        for key, value in (('deployment_generation', 9), ('server_boot_id', 'wrong-boot')):
            with self.subTest(key=key):
                fixture(self.root)
                update = read(self.root, 'live/people-update-0001.json'); update['reader']['clock'][key] = value
                write(self.root, 'live/people-update-0001.json', update)
                self.assertFalse(audit(self.root)['passed'])
        fixture(self.root)
        update = read(self.root, 'live/people-update-0001.json')
        update['reader']['tracks'][0]['payload']['position_xz'][0] += 1
        write(self.root, 'live/people-update-0001.json', update)
        self.assertFalse(audit(self.root)['passed'])

    def test_ten_actions_seven_source_controls_and_both_inspectors_required(self):
        window = read(self.root, 'live/native-window.json'); window['actions'].pop()
        write(self.root, 'live/native-window.json', window)
        self.assertFalse(audit(self.root)['passed'])
        fixture(self.root)
        receipt = read(self.root, 'session/receipts/000001.json')
        receipt['source_request']['sensors'].remove('people-front')
        receipt['source_response']['sensors'].remove('people-front')
        write(self.root, 'session/receipts/000001.json', receipt)
        write(self.root, 'live/source-0002.json', {'request': receipt['source_request'], 'response': receipt['source_response']})
        self.assertFalse(audit(self.root)['passed'])
        fixture(self.root)
        window = read(self.root, 'live/native-window.json'); window['actions'][3]['inspector_panel'] = 'Hazard'
        write(self.root, 'session/qa-result-people-inspector.json', window['actions'][3])
        write(self.root, 'live/native-window.json', window)
        self.assertFalse(audit(self.root)['passed'])

    def test_no_stale_empty_or_failed_run_promotion(self):
        self.capture(1, lambda r: r.update(native_observation_fresh_at_end=False))
        report = audit(self.root)
        self.assertFalse(report['passed']); self.assertEqual(report['excluded_stale_empty_frames'], 1)
        fixture(self.root)
        result = read(self.root, 'result.json'); result['passed'] = False
        write(self.root, 'result.json', result)
        self.assertFalse(audit(self.root)['passed'])

    def test_png_ledger_and_sample_presentation_must_match(self):
        (self.root / 'session/app-capture-1.png').write_bytes(b'changed')
        self.assertFalse(audit(self.root)['passed'])
        fixture(self.root)
        self.capture(0, lambda r: r.update(sample_time_s=1.01))
        self.assertFalse(audit(self.root)['passed'])

    def test_repeated_display_does_not_inflate_original_counts(self):
        window = read(self.root, 'live/native-window.json')
        duplicate = deepcopy(window['frames'][0])
        duplicate['file'] = 'session/app-capture-extra.png'; duplicate['receipt'] = duplicate['file']+'.json'
        (self.root / duplicate['file']).write_bytes((self.root / window['frames'][0]['file']).read_bytes())
        receipt = {k: v for k, v in duplicate.items() if k not in ('file', 'receipt', 'elapsed_s')}
        write(self.root, duplicate['receipt'], receipt)
        window['frames'].insert(1, duplicate); write(self.root, 'live/native-window.json', window)
        result = audit(self.root)
        self.assertTrue(result['passed'], result['issues'])
        self.assertEqual(result['current_combined_frames'], 2)
        self.assertEqual(result['unique_people_originals'], 1)


if __name__ == '__main__':
    unittest.main()
