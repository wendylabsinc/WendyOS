from pathlib import Path
import tempfile
import unittest

from integration.sharedperception import model, stage
from integration.remotemediafusion import stage as remote


def input_fixture(root):
    elf = bytearray(64); elf[:6] = b'\x7fELF\x02\x01'; elf[18:20] = (183).to_bytes(2, 'little')
    binary = root / 'tool'; binary.write_bytes(elf); binary.chmod(0o600)
    tar = root / 'rootfs'; tar.write_bytes(b'structural-test-rootfs'); tar.chmod(0o600)
    return {'version': 1, 'run_id': 'shared-test', 'runtime_image': stage.RUNTIME, 'anchor_unix_ns': model.ANCHOR,
            'binaries': {key: {'path': str(binary), 'sha256': model.digest(elf)} for key in stage.TOOLS},
            'rootfs': {key: {'image': image, 'path': str(tar), 'sha256': model.digest(tar.read_bytes()),
                             'bytes': tar.stat().st_size} for key, image in stage.ROOTFS.items()},
            'model': {'model_id': 'movenet', 'model_version': '1', 'model_sha256': 'f' * 64}}


class StageTests(unittest.TestCase):
    def test_actual_stage_uses_one_shared_source_generic_captures_and_pose_context(self):
        with tempfile.TemporaryDirectory(dir=stage.ROOT / '.local') as name:
            root = Path(name); plan = input_fixture(root); out = root / 'stage'
            self.assertEqual(stage.prepare(model.json_bytes(plan), out), plan)
            for node, services in model.SERVICES.items():
                contexts = out / node / 'project/services'
                self.assertEqual({p.name for p in contexts.iterdir()}, set(services))
                for sensor in set(services) & set(model.SENSORS):
                    self.assertTrue((contexts / sensor / 'capture').is_file())
            scene = out / 'a/project/services/scene'
            self.assertIn(b'integration.sharedperception.scene', (scene / 'Dockerfile').read_bytes())
            self.assertTrue((scene / 'source/integration/sharedperception/scene.py').is_file())
            self.assertTrue((scene / 'source/integration/hazard/scene.py').is_file())
            self.assertTrue((scene / 'source/sim/shared_perception_scene.py').is_file())
            people = out / 'a/project/services/people'
            self.assertEqual((people / 'rootfs.tar').read_bytes(), (root / 'rootfs').read_bytes())
            self.assertIn(b'/model/movenet-multipose-lightning.onnx', (people / 'Dockerfile').read_bytes())
            self.assertTrue((out / 'b/project/services/people-reader/people-reader').is_file())
            source = scene / 'source/integration/sharedperception/scene.py'
            source.write_bytes(source.read_bytes() + b'changed')
            with self.assertRaises(ValueError): stage.validate(out)
        self.assertEqual(len(remote.TOOLS), 9)
        self.assertEqual(set(remote.ROOTFS), {'scene'})

    def test_missing_model_or_extra_tool_refused(self):
        with tempfile.TemporaryDirectory(dir=stage.ROOT / '.local') as name:
            plan = input_fixture(Path(name)); del plan['model']
            with self.assertRaises(ValueError): stage.inputs(model.json_bytes(plan))
            plan = input_fixture(Path(name)); plan['binaries']['camera'] = plan['binaries']['capture']
            with self.assertRaises(ValueError): stage.inputs(model.json_bytes(plan))


if __name__ == '__main__': unittest.main()
