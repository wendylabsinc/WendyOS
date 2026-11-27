"""Unsigned receipt fixtures test excerpt consistency, not live verification."""
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest

from integration.distributedfusion import excerpt, model, playback
from integration.distributedfusion.test_playback import fixture, write_run


def partial_run(directory):
    pair, scene, steps = fixture()
    write_run(directory, pair, scene, steps)
    manifest = json.loads((directory / 'playback-steps.json').read_bytes())
    for index, row in enumerate(manifest['steps'][:2], 1):
        (directory / f'steps/{index:04d}.json').write_bytes(model.json_bytes(row))
    (directory / 'playback-steps.json').unlink()
    cleanup = {'complete': True, 'resources': [{'after': 'absent'} for _ in range(7)]}
    result = {'passed': False, 'gate_completed': False, 'failed_phase': 'sample-3',
              'simulation_only': True, 'cleanup': cleanup}
    (directory / 'cleanup.json').write_bytes(model.json_bytes(cleanup))
    (directory / 'result.json').write_bytes(model.json_bytes(result))
    # Deliberately malformed unfinished third-step data must never be used.
    (directory / 'steps/0003.json').write_bytes(b'not a completed receipt')
    return pair, scene, steps, result


class ExcerptTests(unittest.TestCase):
    def test_only_two_completed_steps_preserve_failure_and_exact_originals(self):
        with tempfile.TemporaryDirectory() as tmp:
            source, out = Path(tmp) / 'run', Path(tmp) / 'excerpt'
            source.mkdir()
            _, _, steps, _ = partial_run(source)
            terminal_before = (source / 'result.json').read_bytes()
            report = excerpt.assemble(source, out)
            self.assertEqual(report['status'], 'excerpt')
            self.assertFalse(report['source_run_passed'])
            self.assertTrue(report['full_scenario_incomplete'])
            self.assertFalse(report['restoration_included'])
            self.assertEqual(report['completed_steps'], 2)
            self.assertEqual(report['duration_s'], 30)
            self.assertFalse(any('0003' in name for name in report['inputs']))
            self.assertEqual((source / 'result.json').read_bytes(), terminal_before)
            frames = [json.loads(line) for line in (out / 'timeline/frames.jsonl').read_bytes().splitlines()]
            self.assertEqual({f['source_step']['name'] for f in frames}, set(playback.NAMES[:2]))
            self.assertEqual(frames[-1]['at_s'], 20)
            self.assertEqual(frames[-1]['actions'][0]['target'], 'Floor plan')
            self.assertEqual(frames[0]['state']['snapshot']['fusion']['valid_until_s'], 3)
            self.assertFalse(frames[-1]['state']['snapshot']['fusion']['observations'])
            self.assertTrue(all(f['chapter']['series'] == 'RECORDED SIMULATION · TWO-STEP EXCERPT' for f in frames))
            self.assertEqual(frames[0]['chapter']['title'], 'A shared hazard across gateways.')
            self.assertEqual(frames[-1]['chapter']['detail'], 'Only Gateway B’s gas stops in this recorded step. A’s sensors stay enabled; B’s hazard result expires.')
            for digest, metadata in report['originals'].items():
                raw = (out / 'originals' / (digest + '.json')).read_bytes()
                self.assertEqual(model.digest(raw), metadata['sha256'])
            path = Path(__file__).resolve().parents[2] / 'integration/nativeapp/render.py'
            spec = importlib.util.spec_from_file_location('excerpt_render_contract', path)
            module = importlib.util.module_from_spec(spec)
            spec.loader.exec_module(module)
            _, _, loaded, assets = module.timeline_inputs(out / 'timeline')
            self.assertEqual(len(loaded), 5)
            self.assertTrue(assets)
            self.assertIn(steps[0]['gas_origin']['proof_sha256'], report['originals'])

    def test_full_assembly_still_requires_success_and_three_steps(self):
        pair, scene, steps = fixture()
        with self.assertRaises(model.Refused):
            playback.build_timeline(pair, scene, 9, steps[:2])
        with tempfile.TemporaryDirectory() as tmp:
            source = Path(tmp) / 'run'
            source.mkdir()
            partial_run(source)
            with self.assertRaises(model.Refused):
                playback.assemble(source, Path(tmp) / 'full')
            self.assertFalse((Path(tmp) / 'full').exists())

    def test_requires_exact_failed_phase_and_complete_cleanup(self):
        for fault in ('passed', 'phase', 'cleanup'):
            with self.subTest(fault=fault), tempfile.TemporaryDirectory() as tmp:
                source, out = Path(tmp) / 'run', Path(tmp) / 'excerpt'
                source.mkdir()
                _, _, _, result = partial_run(source)
                if fault == 'passed':
                    result['passed'] = True
                elif fault == 'phase':
                    result['failed_phase'] = 'sample-2'
                else:
                    result['cleanup']['complete'] = False
                    (source / 'cleanup.json').write_bytes(model.json_bytes(result['cleanup']))
                (source / 'result.json').write_bytes(model.json_bytes(result))
                with self.assertRaises(model.Refused):
                    excerpt.assemble(source, out)
                self.assertFalse(out.exists())

    def test_missing_media_or_changed_gas_chain_refuses_without_output(self):
        for fault in ('missing', 'gas', 'control'):
            with self.subTest(fault=fault), tempfile.TemporaryDirectory() as tmp:
                source, out = Path(tmp) / 'run', Path(tmp) / 'excerpt'
                source.mkdir()
                partial_run(source)
                if fault == 'missing':
                    (source / 'steps/0002-camera-a.png').unlink()
                elif fault == 'gas':
                    path = source / 'steps/0001-gas_origin.json'
                    value = json.loads(path.read_bytes())
                    value['proof_sha256'] = 'a' * 64
                    path.write_bytes(model.json_bytes(value))
                else:
                    path = source / 'steps/0002-control.json'
                    value = json.loads(path.read_bytes())
                    value['request']['sensors'] = list(model.SENSORS)
                    path.write_bytes(model.json_bytes(value))
                with self.assertRaises(model.Refused):
                    excerpt.assemble(source, out)
                self.assertFalse(out.exists())


if __name__ == '__main__':
    unittest.main()
