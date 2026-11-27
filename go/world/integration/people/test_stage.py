import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import libraries
import stage


class StageTests(unittest.TestCase):
    def test_every_context_is_offline_gated_and_separate(self):
        for service in ('scene', 'camera-a', 'camera-b', 'people', 'reader'):
            text = stage.dockerfile(service, 'people-test', 1767225600000000000).decode()
            self.assertTrue(text.startswith('FROM scratch\n'))
            self.assertNotIn('RUN ', text)
            self.assertIn('USER 0:0', text)
            argv = json.loads(text.split('ENTRYPOINT ', 1)[1])
            self.assertEqual('--start-file' in argv, service != 'scene')
            self.assertIn('/proofs', argv)
            if service == 'people':
                self.assertIn('/model/movenet-multipose-lightning.onnx', argv)

    def test_stage_exact_membership_and_hashes(self):
        with tempfile.TemporaryDirectory(dir=stage.ROOT / '.local') as name:
            root = Path(name)
            source = root / 'elf'
            elf = bytearray(64); elf[:6] = b'\x7fELF\x02\x01'; elf[18:20] = (183).to_bytes(2, 'little')
            source.write_bytes(elf); source.chmod(0o600)
            tar = root / 'rootfs.tar'; tar.write_bytes(b'test-only-rootfs'); tar.chmod(0o600)
            value = {'version': 1, 'run_id': 'people-test', 'runtime_image': stage.RUNTIME, 'anchor_unix_ns': 1767225600000000000,
                     'model': {'model_id': 'fixture', 'model_version': '1', 'model_sha256': 'a'*64},
                     'binaries': {k: {'path': str(source), 'sha256': libraries.pair.digest(elf)} for k in stage.TOOLS},
                     'rootfs': {k: {'image': image, 'path': str(tar), 'bytes': tar.stat().st_size, 'sha256': libraries.pair.digest(tar.read_bytes())} for k, image in stage.ROOTFS.items()}}
            target = root / 'prepared'
            stage.prepare(libraries.pair.json_bytes(value), target)
            self.assertEqual(stage.validate(target)['run_id'], 'people-test')
            with self.assertRaises(ValueError):
                stage.prepare(libraries.pair.json_bytes(value), target)
            (target / 'a' / 'extra').write_text('unlisted')
            with self.assertRaises(ValueError): stage.validate(target)
            (target / 'a' / 'extra').unlink()
            path = target / 'b' / 'project/services/reader/Dockerfile'
            path.write_text(path.read_text() + '# altered\n')
            with self.assertRaises(ValueError): stage.validate(target)

    def test_old_runtime_and_anchor_refused_before_binary_reads(self):
        value = {'version': 1, 'run_id': 'people-test', 'runtime_image': stage.RUNTIME, 'anchor_unix_ns': 1, 'binaries': {}, 'rootfs': {}, 'model': {}}
        with self.assertRaises(ValueError): stage.inputs(libraries.pair.json_bytes(value))
        value['anchor_unix_ns'] = 1767225600000000000
        value['runtime_image'] = 'latest'
        with self.assertRaises(ValueError): stage.inputs(libraries.pair.json_bytes(value))


if __name__ == '__main__': unittest.main()
