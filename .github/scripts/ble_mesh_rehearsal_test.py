import importlib.util
import json
from pathlib import Path
import tempfile
import unittest

SOURCE=Path(__file__).resolve().parents[1]/'regression-coverage/manual/ble-mesh-rehearsal.py'
spec=importlib.util.spec_from_file_location('ble_mesh_plan',SOURCE)
MODULE=importlib.util.module_from_spec(spec);spec.loader.exec_module(MODULE)

class PlanTests(unittest.TestCase):
    def fixture(self,root):
        artifact=root/'artifact';artifact.write_bytes(b'inert candidate/independent record')
        tenant='11111111-1111-4111-8111-111111111111'
        data={key:str(artifact) for key in ('cli_binary','agent_binary','lite_firmware','identity_record','authorization_record','topology_record','native_ap_record','recovery_record','refusal_qualification_record','slow_link_qualification_record','coded_phy_qualification_record','operator_profile_record','baseline_record','build_record','permission_record','traffic_ca','traffic_record')}
        data.update(trial='a'*32,source_commit='b'*40,lite_source_commit='c'*40,tenant=tenant,approved_power_dbm=0,traffic_url='https://owned.test/challenge?trial='+('a'*32),traffic_sha256='d'*64,traffic_ca_on_borrower='/tmp/owned-trial/ca.pem')
        data['operator']={'identity':'reserved-mac','owner':'fixture owner','os_version':'pinned build','controller_identity':'native controller','permission_record':'reviewed record','platform':'darwin','uid':502,'profile_directory':'/Users/reserved/profile'}
        data['nodes']={}
        for index,role in enumerate(('borrower','donor','lite'),2):
            flags=('ble','nan','infrastructure-wifi') if role=='lite' else ('ble','nan','ethernet','infrastructure-wifi','roam','share-uplink')
            data['nodes'][role]={'serial':role+'-serial','owner':'fixture owner','board':'reserved board','os_version':'pinned OS','controller_identity':'exact controller','controller_firmware':'exact firmware','management':'wendy-lite:/dev/cu.owned' if role=='lite' else '192.0.2.'+str(index)+':50052','management_interface':'usb-managed','recovery_record':'reviewed recovery','device_id':str(index)*8+'-2222-4222-8222-222222222222','tenant':tenant,'adapter':'hci0','agent_unit':'wendy-agent.service','controller_usb_id':'13d3:3549','coded_phy_supported':True,'profile':'c5-psram-native-operator-router','baseline':{'participate':False,'mode':'normal','advertising':'default','connections':'default','settings':dict.fromkeys(flags,False)}}
        return data

    def generate(self,root,data):
        path=root/'inventory.json';path.write_text(json.dumps(data))
        return MODULE.generate(path,root/'plan')

    def test_generation_is_offline_and_each_case_stays_not_run(self):
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory);evidence=self.generate(root,self.fixture(root))
            self.assertEqual(len(evidence['cases']),6)
            self.assertTrue(all(case['status']=='not_run' for case in evidence['cases']))
            self.assertEqual(evidence['hardware_execution'],'not_run')
            commands=json.loads((root/'plan/commands.json').read_text())
            lite=commands['lite-ble-only']['argv']
            self.assertNotIn('--ethernet=false',lite)
            self.assertNotIn('--share-uplink=false',lite)
            self.assertIn('--share-uplink=true',commands['donor-ble-only']['argv'])
            self.assertIn('--participate=false',commands['borrower-restore-net']['argv'])
            self.assertIn('--connections=0',commands['donor-approved-power']['argv'])
            self.assertEqual(len(evidence['generated']),2)
            text=(root/'plan/procedure.txt').read_text()
            for required in ('0x0c','more than 15 seconds','inconclusive','not proof','restoration'):
                self.assertIn(required,text)

    def test_refuses_wrong_identity_or_unsafe_baseline(self):
        for fault in ('tenant','controller','baseline','lite','power','secret'):
            with self.subTest(fault=fault),tempfile.TemporaryDirectory() as directory:
                root=Path(directory);data=self.fixture(root)
                if fault=='tenant':data['nodes']['donor']['tenant']='22222222-2222-4222-8222-222222222222'
                elif fault=='controller':data['nodes']['borrower']['controller_usb_id']='0000:0000'
                elif fault=='baseline':data['nodes']['borrower']['baseline']['participate']=True
                elif fault=='lite':data['nodes']['lite']['profile']='c6'
                elif fault=='power':data['approved_power_dbm']='maximum'
                elif fault=='secret':data['password']='never logged'
                with self.assertRaises(ValueError):self.generate(root,data)
                self.assertFalse((root/'plan').exists())

    def test_saved_carrier_settings_are_restored_exactly(self):
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory);data=self.fixture(root)
            data['nodes']['borrower']['baseline']['settings'].update(ethernet=True,roam=True)
            self.generate(root,data)
            command=json.loads((root/'plan/commands.json').read_text())['borrower-restore-net']['argv']
            self.assertIn('--ethernet=true',command);self.assertIn('--roam=true',command)
            self.assertIn('--participate=false',command)

if __name__=='__main__':unittest.main()
