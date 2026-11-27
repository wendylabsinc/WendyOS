import base64
import hashlib
import json
from pathlib import Path
import struct
import tempfile
import unittest
import uuid

from wendy_site import decode_archive, export_archive, import_archive, read_site


class SiteArchiveTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.site = self.root / 'site'
        self.site.mkdir()
        self.model = b'PK\x03\x04synthetic test container'
        self.meta = {'version': 1, 'id': str(uuid.uuid4()), 'name': 'Test room',
                     'modelRevision': str(uuid.uuid4()), 'metresPerModelUnit': 1, 'cameras': []}
        (self.site / 'site.json').write_text(json.dumps(self.meta))
        (self.site / 'model.usdz').write_bytes(self.model)

    def add_map(self):
        self.reference = {'version': 1, 'modelRevision': self.meta['modelRevision'],
                          'modelSHA256': hashlib.sha256(self.model).hexdigest(), 'capturedAt': 0,
                          'frameCount': 1, 'landmarks': base64.b64encode(struct.pack('<f', 1) * 131 * 120).decode()}
        (self.site / 'visual-map.json').write_text(json.dumps(self.reference))

    def test_roundtrip_both_native_formats_preserves_bytes(self):
        for version in (1, 2):
            if version == 2:
                self.add_map()
            archive = self.root / ('test%d.wendysite' % version)
            export_archive(self.site, archive)
            self.assertEqual(archive.read_bytes()[:8], ('WENDYST%d' % version).encode())
            target = import_archive(archive, self.root / ('import%d' % version))
            self.assertEqual(read_site(self.site), read_site(target))

    def test_model_or_revision_change_rejected(self):
        self.add_map()
        (self.site / 'model.usdz').write_bytes(self.model + b'changed')
        with self.assertRaises(ValueError):
            export_archive(self.site, self.root / 'bad.wendysite')
        (self.site / 'model.usdz').write_bytes(self.model)
        self.meta['modelRevision'] = str(uuid.uuid4())
        (self.site / 'site.json').write_text(json.dumps(self.meta))
        with self.assertRaises(ValueError):
            read_site(self.site)

    def test_invalid_landmarks_rejected(self):
        self.add_map()
        self.reference['landmarks'] = base64.b64encode(struct.pack('<f', float('nan')) * 131 * 120).decode()
        (self.site / 'visual-map.json').write_text(json.dumps(self.reference))
        with self.assertRaises(ValueError):
            read_site(self.site)

    def test_truncated_and_extra_archive_bytes_rejected(self):
        self.add_map()
        archive = self.root / 'test.wendysite'
        export_archive(self.site, archive)
        good = archive.read_bytes()
        for bad in (good[:19], good[:-1], good + b'extra', good[:8] + b'\xff' * 4 + good[12:]):
            archive.write_bytes(bad)
            with self.assertRaises(ValueError):
                decode_archive(archive)

    def test_no_overwrite_or_symlink_inputs(self):
        archive = self.root / 'test.wendysite'
        export_archive(self.site, archive)
        with self.assertRaises(FileExistsError):
            export_archive(self.site, archive)
        with self.assertRaises(FileExistsError):
            import_archive(archive, self.site)
        alias = self.root / 'alias.wendysite'
        alias.symlink_to(archive)
        with self.assertRaises(ValueError):
            decode_archive(alias)

    def test_invalid_scale_rejected(self):
        for scale in (0, -1, True, float('nan')):
            self.meta['metresPerModelUnit'] = scale
            (self.site / 'site.json').write_text(json.dumps(self.meta))
            with self.assertRaises(ValueError):
                read_site(self.site)


if __name__ == '__main__':
    unittest.main()
