import importlib.util
from pathlib import Path
import unittest

spec=importlib.util.spec_from_file_location('camera_campaign_gate',Path(__file__).with_name('test-camera-campaign-runtime.py'))
gate=importlib.util.module_from_spec(spec);spec.loader.exec_module(gate)


class CameraCampaignRuntimeAdmission(unittest.TestCase):
    def test_normal_requires_both_real_boot_episodes_and_reconnect(self):
        log='CAMPAIGN_RUNTIME_EPISODE boot=0 x\nCAMPAIGN_RUNTIME_EPISODE boot=1 y\nCAMPAIGN_RECONNECT_VERIFIED actual new generation\nCAMPAIGN_RUNTIME_VERIFIED actual runtime\n'
        self.assertTrue(gate.accepted('normal',0,log))
        for marker in ('CAMPAIGN_RUNTIME_EPISODE boot=1','CAMPAIGN_RECONNECT_VERIFIED','CAMPAIGN_RUNTIME_VERIFIED'):
            self.assertFalse(gate.accepted('normal',0,log.replace(marker,'missing')))
        self.assertFalse(gate.accepted('normal',1,log))
        self.assertFalse(gate.accepted('normal',0,log+'--- SKIP: unrelated'))

    def test_controls_require_specific_runtime_failure_not_build_or_timeout(self):
        marker='--- FAIL: TestYAMLCampaignManagedModelRecordsAndRestores '
        for name,boundary in [('lost-presence','camera reconnection lost presence/cooldown and duplicated episode'),('lost-input-ledger','model outputs/inputs missing from real episode')]:
            self.assertTrue(gate.accepted(name,1,marker+boundary))
            for code in (0,2,124,137):self.assertFalse(gate.accepted(name,code,marker+boundary))
            self.assertFalse(gate.accepted(name,1,boundary))
            self.assertFalse(gate.accepted(name,1,marker+'could not load model'))
        self.assertFalse(gate.accepted('unknown',1,marker+'anything'))


if __name__=='__main__':unittest.main()
