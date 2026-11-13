"""Offline physical-plan admission; never open a camera or contact a device."""
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location('campaign_plan', ROOT/'.github/regression-coverage/manual/camera-campaign-rehearsal.py')
PLAN = importlib.util.module_from_spec(spec); spec.loader.exec_module(PLAN)


class CameraCampaignPlan(unittest.TestCase):
    def fixture(self, root):
        record = root/'inert-record'; record.write_text('synthetic offline inventory, not physical evidence')
        model = root/'model.json'; model.write_text(json.dumps({'repository': PLAN.MODEL,
            'revision': PLAN.REVISION, 'files': {'model.safetensors': 'a'*64, 'config.json': 'b'*64}}))
        raw = {'trial': 'a'*32, 'source_commit': 'b'*40, 'reserved_target': True,
               'no_existing_campaigns_or_recordings': True, 'endpoint': '192.0.2.1:50051',
               'cli_binary': str(record), 'agent_binary': str(record), 'decoder_binary': str(record),
               'target': {'serial': 'reserved-board', 'machine_id': 'fixture-id', 'os_release': 'fixture-linux',
                          'kernel': 'fixture-kernel', 'owner': 'owner', 'agent_unit': 'wendy-agent.service', 'arch': 'aarch64'},
               'cameras': [{'serial': 'camera-'+x, 'usb_path': '1-'+x, 'source_id': 'v4l2:/dev/video'+x,
                            'description': 'physical fixture identity'} for x in ('0','1')],
               'records': {name: str(record) for name in ('authorization', 'device_identity', 'camera_inventory',
                             'candidate_build', 'recovery', 'scene_protocol')}}
        raw['records']['model_manifest'] = str(model)
        path = root/'inventory.json'; path.write_text(json.dumps(raw)); return path, raw

    def test_generation_is_offline_and_all_cases_remain_not_run(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp); inventory, raw = self.fixture(root)
            with patch('subprocess.Popen') as spawn:
                evidence = PLAN.generate(inventory, root/'plan')
            spawn.assert_not_called()
            self.assertEqual(evidence['hardware_execution'], 'not_run')
            self.assertEqual({case['status'] for case in evidence['cases']}, {'not_run'})
            campaign = json.loads((root/'plan/campaign.yaml').read_text())
            self.assertEqual(campaign['inference']['revision'], PLAN.REVISION)
            self.assertEqual(campaign['upload'], {'when': 'manual'})
            self.assertNotIn('notify', campaign)
            for command in evidence['commands'].values():
                self.assertIn('WENDY_CONFIG_DIR=', command)
                self.assertIn('--device 192.0.2.1:50051', command)
                self.assertIn('--timeout 45', command)
            guide = (root/'plan/procedure.txt').read_text()
            self.assertNotIn('@PLAN@', guide)
            self.assertNotIn('@CLI@', guide)
            self.assertIn('Do not\n   redeploy the plan', guide)
            self.assertIn('V4L2 sample-ID join', guide)
            self.assertIn('input_reference_status', guide)

    def test_invalid_identity_or_model_refuses_before_output(self):
        for defect in ('busy','same-camera','model','revision','endpoint','secret','service'):
            with self.subTest(defect=defect), tempfile.TemporaryDirectory() as temp:
                root=Path(temp); inventory, raw=self.fixture(root)
                if defect=='busy': raw['no_existing_campaigns_or_recordings']=False
                if defect=='same-camera': raw['cameras'][1]['serial']=raw['cameras'][0]['serial']
                if defect=='model': Path(raw['records']['model_manifest']).write_text('{}')
                if defect=='revision': raw['source_commit']='main'
                if defect=='endpoint': raw['endpoint']='cloud://production:443'
                if defect=='secret': raw['target']['token']='must not be recorded'
                if defect=='service': raw['target']['agent_unit']='unrelated.service'
                inventory.write_text(json.dumps(raw))
                with self.assertRaises((ValueError, KeyError)): PLAN.generate(inventory, root/'plan')
                self.assertFalse((root/'plan').exists())

    def test_symlink_artifact_and_existing_output_are_refused(self):
        with tempfile.TemporaryDirectory() as temp:
            root=Path(temp); inventory, raw=self.fixture(root)
            link=root/'link'; link.symlink_to(raw['cli_binary']); raw['cli_binary']=str(link)
            inventory.write_text(json.dumps(raw))
            with self.assertRaises(ValueError): PLAN.generate(inventory, root/'plan')
            inventory, raw=self.fixture(root)
            output=root/'plan'; output.mkdir(); (output/'keep').write_text('owned elsewhere')
            with self.assertRaises(ValueError): PLAN.generate(inventory, output)
            self.assertEqual((output/'keep').read_text(), 'owned elsewhere')


if __name__ == '__main__': unittest.main()
