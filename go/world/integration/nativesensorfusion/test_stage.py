import json
from pathlib import Path
import tempfile
import unittest
from integration.nativesensorfusion import model, stage
from integration.hazard import stage as original


class StageTests(unittest.TestCase):
    def test_native_bridge_uses_only_world_socket_and_scene_microsecond_capture(self):
        raw = stage.dockerfile('camera-b', 'native-test', model.ANCHOR).decode()
        argv = json.loads(raw.split('ENTRYPOINT ', 1)[1])
        self.assertEqual(argv[-4:], ['--mode', 'native-bridge', '--config', '/config/config.json'])
        self.assertIn('COPY sensorlink /service', raw)
        self.assertNotIn('COPY capture', raw)
        self.assertIn('--start-file', argv)
        scene = stage.dockerfile('scene', 'native-test', model.ANCHOR).decode()
        argv = json.loads(scene.split('ENTRYPOINT ', 1)[1])
        self.assertEqual(argv[-2:], ['--capture-resolution-ns', '1000'])
        self.assertNotIn('capture-resolution-ns', original.dockerfile('scene', 'native-test', model.ANCHOR).decode())

    def test_offline_exact_stage_routes_native_tools_without_credentials(self):
        with tempfile.TemporaryDirectory(dir=stage.ROOT / '.local') as name:
            root = Path(name)
            elf = bytearray(64); elf[:6] = b'\x7fELF\x02\x01'; elf[18:20] = (183).to_bytes(2, 'little')
            tool = root / 'tool'; tool.write_bytes(elf); tool.chmod(0o600)
            fs = root / 'rootfs.tar'; fs.write_bytes(b'fixture'); fs.chmod(0o600)
            plan = {'version': 1, 'run_id': 'native-test', 'runtime_image': stage.RUNTIME,
                'anchor_unix_ns': model.ANCHOR,
                'binaries': {k: {'path': str(tool), 'sha256': model.digest(elf)} for k in stage.TOOLS},
                'rootfs': {'scene': {'image': stage.ROOTFS['scene'], 'path': str(fs),
                    'sha256': model.digest(fs.read_bytes()), 'bytes': 7}}}
            output = root / 'stage'
            self.assertEqual(stage.prepare(model.json_bytes(plan), output), plan)
            self.assertTrue((output / 'a/project/services/camera-b/sensorlink').is_file())
            self.assertFalse((output / 'a/project/services/camera-b/capture').exists())
            self.assertTrue((output / 'a/tools/native-source').is_file())
            self.assertEqual(set(p.name for p in (output / 'b/project/services').iterdir()), {'reader'})
            self.assertEqual(original.TOOLS[-3:], ('capture', 'fusion', 'reader'))
            self.assertNotIn('device-key.pem', [p.name for p in output.rglob('*')])
