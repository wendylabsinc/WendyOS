import json
from pathlib import Path
import tempfile
import unittest

from integration.hazard import libraries, model, stage


class StageTests(unittest.TestCase):
    def test_offline_gated_contexts_and_actual_source_module(self):
        for service in (*model.SERVICES['a'], *model.SERVICES['b']):
            raw = stage.dockerfile(service, 'hazard-test', model.ANCHOR).decode()
            self.assertTrue(raw.startswith('FROM scratch\n'))
            self.assertNotIn('RUN ', raw)
            argv = json.loads(raw.split('ENTRYPOINT ', 1)[1])
            self.assertEqual('--start-file' in argv, service != 'scene')
            if service == 'scene':
                self.assertIn('integration.hazard.scene', argv)
                self.assertIn('ADD rootfs.tar /', raw)
            else:
                self.assertNotIn('rootfs.tar', raw)
                self.assertIn('COPY ' + ('capture' if service in model.SENSORS else service) + ' /service', raw)
        with self.assertRaises(ValueError):
            stage.dockerfile('unknown', 'hazard-test', model.ANCHOR)

    def test_pin_set_excludes_pose_and_requires_all_lifecycle_tools(self):
        self.assertEqual(set(stage.TOOLS), {'agent', 'cli', 'fixture', 'pair-fixture', 'initializer', 'app-runner', 'capture', 'fusion', 'reader'})
        self.assertEqual(set(stage.ROOTFS), {'scene'})
        value = {'version': 1, 'run_id': 'hazard-test', 'runtime_image': stage.RUNTIME,
                 'anchor_unix_ns': 1, 'binaries': {}, 'rootfs': {}}
        with self.assertRaises(ValueError): stage.inputs(model.json_bytes(value))
        value['anchor_unix_ns'] = model.ANCHOR; value['model'] = {}
        with self.assertRaises(ValueError): stage.inputs(model.json_bytes(value))

    def test_bound_helper_module_does_not_replace_people_globals(self):
        self.assertEqual(libraries.people_stage.TOOLS[-3:], ('camera', 'people', 'reader'))
        self.assertEqual(stage.TOOLS[-3:], ('capture', 'fusion', 'reader'))
        self.assertEqual(libraries.people_stage.REMOTE, '/var/lib/people-stage')
        self.assertEqual(stage.REMOTE, '/var/lib/hazard-stage')

    def test_exact_offline_stage_rejects_changed_and_extra_files(self):
        # Structural ELF/rootfs fixtures exercise packaging only, not execution.
        with tempfile.TemporaryDirectory(dir=stage.ROOT / '.local') as name:
            root = Path(name)
            elf = bytearray(64); elf[:6] = b'\x7fELF\x02\x01'; elf[18:20] = (183).to_bytes(2, 'little')
            binary = root / 'tool'; binary.write_bytes(elf); binary.chmod(0o600)
            rootfs = root / 'rootfs.tar'; rootfs.write_bytes(b'fixture rootfs'); rootfs.chmod(0o600)
            plan = {'version': 1, 'run_id': 'hazard-test', 'runtime_image': stage.RUNTIME,
                    'anchor_unix_ns': model.ANCHOR,
                    'binaries': {k: {'path': str(binary), 'sha256': model.digest(elf)} for k in stage.TOOLS},
                    'rootfs': {'scene': {'image': stage.ROOTFS['scene'], 'path': str(rootfs),
                                        'bytes': rootfs.stat().st_size, 'sha256': model.digest(rootfs.read_bytes())}}}
            prepared = root / 'prepared'
            stage.prepare(model.json_bytes(plan), prepared)
            self.assertEqual(stage.validate(prepared), plan)
            self.assertTrue((prepared / 'a/project/services/scene/source/examples/collaborative-office/scene.json').is_file())
            self.assertTrue((prepared / 'a/project/services/lidar-atlas/capture').is_file())
            self.assertEqual(set(p.name for p in (prepared / 'b/project/services').iterdir()), {'reader'})
            extra = prepared / 'a/extra'; extra.write_text('not pinned')
            with self.assertRaises(ValueError): stage.validate(prepared)
            extra.unlink()
            changed = prepared / 'b/project/services/reader/Dockerfile'
            changed.write_text(changed.read_text() + '# changed\n')
            with self.assertRaises(ValueError): stage.validate(prepared)


if __name__ == '__main__':
    unittest.main()
