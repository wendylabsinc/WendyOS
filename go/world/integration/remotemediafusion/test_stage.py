from pathlib import Path
import tempfile
import unittest

from integration.remotemediafusion import model, stage
from integration.hazard import stage as original


class StageTests(unittest.TestCase):
    def test_new_offline_stage_places_remote_media_only_on_b_and_preserves_existing_stage(self):
        # Structural file fixtures test staging, not executable qualification.
        with tempfile.TemporaryDirectory(dir=stage.ROOT / '.local') as name:
            root = Path(name)
            elf = bytearray(64); elf[:6] = b'\x7fELF\x02\x01'; elf[18:20] = (183).to_bytes(2, 'little')
            binary = root / 'tool'; binary.write_bytes(elf); binary.chmod(0o600)
            tar = root / 'rootfs'; tar.write_bytes(b'test-rootfs'); tar.chmod(0o600)
            plan = {'version': 1, 'run_id': 'split-test', 'runtime_image': stage.RUNTIME, 'anchor_unix_ns': model.ANCHOR,
                    'binaries': {key: {'path': str(binary), 'sha256': model.digest(elf)} for key in stage.TOOLS},
                    'rootfs': {'scene': {'image': stage.ROOTFS['scene'], 'path': str(tar), 'sha256': model.digest(tar.read_bytes()), 'bytes': tar.stat().st_size}}}
            out = root / 'stage'
            self.assertEqual(stage.prepare(model.json_bytes(plan), out), plan)
            self.assertFalse((out / 'a/project/services/camera-b').exists())
            self.assertTrue((out / 'b/project/services/camera-b/capture').is_file())
            self.assertTrue((out / 'b/project/services/audio-1/capture').is_file())
            self.assertFalse((out / 'a/project/services/audio-1').exists())
            self.assertTrue((out / 'a/project/services/gas-1/capture').is_file())
            self.assertFalse((out / 'b/project/services/scene').exists())
            self.assertTrue((out / 'a/project/services/scene/source/integration/hazard/scene.py').is_file())
            path = out / 'b/project/services/camera-b/Dockerfile'
            self.assertIn(b'--start-file', path.read_bytes())
            path.write_bytes(path.read_bytes() + b'changed')
            with self.assertRaises(ValueError): stage.validate(out)
        self.assertEqual(stage.REMOTE, '/var/lib/remotemediafusion-stage')
        self.assertEqual(original.REMOTE, '/var/lib/hazard-stage')
        self.assertEqual(original.SERVICES['b'], ('reader',))


if __name__ == '__main__':
    unittest.main()
