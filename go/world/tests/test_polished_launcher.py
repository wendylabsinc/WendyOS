"""Check frozen-twin contents and isolation without opening the user's apps."""
import hashlib
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
import zipfile

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location('polished_launcher', ROOT / 'scripts/polished-mac-demo.py')
launcher = importlib.util.module_from_spec(spec)
spec.loader.exec_module(launcher)


class LauncherTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name).resolve()
        self.patch = patch.object(launcher, 'ROOT', self.root)
        self.patch.start()
        self.addCleanup(self.patch.stop)
        self.source = self.root / 'examples/collaborative-office'
        self.source.mkdir(parents=True)
        self.xml = b'<mujoco model="frozen test"><worldbody/></mujoco>\n'
        (self.source / 'office.xml').write_bytes(self.xml)
        self.scene = {'model': {'path': 'office.xml', 'sha256': hashlib.sha256(self.xml).hexdigest()}}
        (self.source / 'scene.json').write_text(json.dumps(self.scene))

    def test_archive_is_the_same_frozen_model_used_by_the_run(self):
        out = launcher.fresh_output(self.root / '.local/one')
        scene_path, inventory = launcher.freeze_twin(out)
        (self.source / 'office.xml').write_bytes(b'later source edits')
        self.assertEqual((scene_path.parent / 'office.xml').read_bytes(), self.xml)
        with zipfile.ZipFile(out / 'office-digital-twin.zip') as archive:
            names = set(archive.namelist())
            self.assertEqual(names, {'office-digital-twin/' + name for name in (*inventory, 'manifest.json')})
            for name, row in inventory.items():
                raw = archive.read('office-digital-twin/' + name)
                self.assertEqual(raw, (scene_path.parent / name).read_bytes())
                self.assertEqual(hashlib.sha256(raw).hexdigest(), row['sha256'])
                self.assertEqual(len(raw), row['bytes'])

    def test_wrong_model_digest_refuses_before_twin_creation(self):
        out = launcher.fresh_output(self.root / '.local/tampered')
        (self.source / 'office.xml').write_bytes(b'changed')
        with self.assertRaisesRegex(ValueError, 'digest'):
            launcher.freeze_twin(out)
        self.assertFalse((out / 'digital-twin').exists())
        self.assertFalse((out / 'office-digital-twin.zip').exists())

    def test_output_reuse_and_escaping_paths_are_refused(self):
        existing = launcher.fresh_output(self.root / '.local/existing')
        (existing / 'keep').write_text('existing run')
        for candidate in (self.root / 'outside', self.root / '.local/../outside', self.root / '.local'):
            with self.assertRaises(ValueError):
                launcher.fresh_output(candidate)
        with self.assertRaises(FileExistsError):
            launcher.fresh_output(existing)
        self.assertEqual((existing / 'keep').read_text(), 'existing run')
        (self.root / '.local/alias').symlink_to(existing, target_is_directory=True)
        with self.assertRaisesRegex(ValueError, 'symbolic'):
            launcher.fresh_output(self.root / '.local/alias/new')


if __name__ == '__main__':
    unittest.main()
