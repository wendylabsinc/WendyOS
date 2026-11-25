from pathlib import Path
import tempfile
import unittest

from integration.sharedinspection import model, stage
from integration.sharedperception import stage as original
from integration.sharedperception.test_stage import input_fixture


class StageTests(unittest.TestCase):
    def test_pinned_preparation_adds_only_explicit_inspection_services(self):
        with tempfile.TemporaryDirectory(dir=stage.ROOT / '.local') as name:
            root = Path(name); plan = input_fixture(root)
            for key in stage.TOOLS:
                plan['binaries'][key] = dict(plan['binaries']['agent'])
            out = root / 'stage'
            self.assertEqual(stage.prepare(model.json_bytes(plan), out), plan)
            contexts = out / 'a/project/services'
            scene = (contexts / 'scene/Dockerfile').read_bytes()
            self.assertIn(b'integration.sharedperception.scene', scene)
            self.assertIn(b'"--inspection"', scene)
            self.assertIn(b'"--inspection-defer-start"', scene)
            self.assertIn(b'"--inspection-fixed-capture-hz","1"', scene)
            executor = (contexts / 'atlas-executor/Dockerfile').read_bytes()
            self.assertIn(b'COPY atlas-executor /executor', executor)
            self.assertIn(b'COPY mission-commissioner /service', executor)
            self.assertIn(b'"/service","executor","--config","/config/launch.json"', executor)
            self.assertTrue((contexts / 'atlas-executor/mission-commissioner').is_file())
            self.assertIn(b'COPY mission-gateway /service', (contexts / 'mission-gateway/Dockerfile').read_bytes())
            self.assertTrue((contexts / 'scene/source/sim/shared_inspection.py').is_file())
            self.assertTrue((contexts / 'people/rootfs.tar').is_file())
            viewer = out / 'b/project/services/viewer'
            self.assertEqual((viewer / 'viewer').read_bytes(), Path(plan['binaries']['viewer']['path']).read_bytes())
            self.assertIn(b'COPY viewer /service', (viewer / 'Dockerfile').read_bytes())
            self.assertIn(b'"--start-file","/config/go"', (viewer / 'Dockerfile').read_bytes())
            self.assertIn(b'"/service","--config","/config/config.json"', (viewer / 'Dockerfile').read_bytes())
            self.assertNotIn('viewer', original.TOOLS)
            self.assertNotIn('mission-gateway', original.TOOLS)
            self.assertNotIn(b'--inspection', original.dockerfile('scene', plan['run_id'], model.ANCHOR))
            (contexts / 'atlas-executor/mission-commissioner').write_bytes(b'changed')
            with self.assertRaises(ValueError): stage.validate(out)

    def test_new_tools_are_explicit_not_assumed_from_other_builds(self):
        with tempfile.TemporaryDirectory(dir=stage.ROOT / '.local') as name:
            plan = input_fixture(Path(name))
            with self.assertRaises(ValueError): stage.inputs(model.json_bytes(plan))


if __name__ == '__main__': unittest.main()
