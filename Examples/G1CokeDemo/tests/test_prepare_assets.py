"""Importer rejects corrupt sources and preserves previously prepared files."""
import hashlib
import importlib.util
from pathlib import Path
import tempfile
import unittest


module_spec = importlib.util.spec_from_file_location("prepare_assets", Path(__file__).resolve().parents[1] / "prepare-assets.py")
prepare_assets = importlib.util.module_from_spec(module_spec)
module_spec.loader.exec_module(prepare_assets)


class AssetPreparationTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.source = self.root / "source"
        self.source.mkdir()
        self.target = self.root / "target"
        self.target.mkdir()
        self.lock = {"schema": "wendy.g1.coke.example-assets.v1", "files": {}}
        for name, payload in (("first", b"model bytes"), ("second", b"weights bytes")):
            (self.source / name).write_bytes(payload)
            self.lock["files"]["bundle/" + name] = {"source": "runtime", "path": name,
                "bytes": len(payload), "sha256": hashlib.sha256(payload).hexdigest()}

    def run_prepare(self):
        return prepare_assets.prepare(self.target, self.lock, sources={"runtime": self.source})

    def test_prepare_recheck_and_repeat(self):
        self.assertEqual(self.run_prepare(), 2)
        self.assertEqual(self.run_prepare(), 0)
        self.assertEqual(prepare_assets.prepare(self.target, self.lock, check=True), 0)
        self.assertEqual((self.target / "bundle/first").read_bytes(), b"model bytes")

    def test_corrupt_source_does_not_publish_any_files(self):
        (self.source / "second").write_bytes(b"broken!")
        with self.assertRaisesRegex(ValueError, "size mismatch"):
            self.run_prepare()
        self.assertEqual(list(self.target.iterdir()), [])

    def test_corrupt_existing_asset_is_not_replaced(self):
        self.run_prepare()
        asset = self.target / "bundle/first"
        asset.write_bytes(b"wrong bytes")
        with self.assertRaisesRegex(ValueError, "SHA-256 mismatch"):
            self.run_prepare()
        self.assertEqual(asset.read_bytes(), b"wrong bytes")

    def test_escape_and_symlink_outside_root_are_rejected(self):
        with self.assertRaises(ValueError):
            prepare_assets.safe_path(self.target, "../source/first")
        (self.target / "outside").symlink_to(self.source, target_is_directory=True)
        with self.assertRaises(ValueError):
            prepare_assets.safe_path(self.target, "outside/first")


if __name__ == "__main__":
    unittest.main()
