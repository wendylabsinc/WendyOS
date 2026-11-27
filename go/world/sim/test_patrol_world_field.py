import unittest
from sim.patrol_world_field import WorldField
from sim.patrol_world_site import validate_original
import copy

class Dummy:
    def __init__(self):
        self.time=0.;self.site_xpos=[[float(i%4)*2-7,float(2-4*(i//4)),.75] for i in range(8)];self.xpos=[[-6,-1.6,.5]];self.mocap_pos=[[0.,0.,0.]]
    def body(self,name):return type('ID',(),dict(id=0))()
    def site(self,name):return type('ID',(),dict(id=int(name.rsplit('-',1)[1])))()
    body_mocapid=[0]
class FieldTests(unittest.TestCase):
    def test_original_step_read_is_immutable_and_cadence_is_not_receipt_time(self):
        d=Dummy();f=WorldField(None,d,d,'run','a'*64,'boot')
        d.time=.25;f.step(1000);first=f.snapshot();self.assertEqual(first['sample']['sequence'],1)
        for _ in range(10):self.assertEqual(f.snapshot(),first)
        edited=f.snapshot();edited['sample']['readings'][0]['value_ppm']=0;self.assertEqual(f.snapshot(),first)
        d.time=.3;f.step(1050);self.assertEqual(f.snapshot(),first)
        d.time=.5;f.step(1250);last=f.snapshot();self.assertEqual(last['sample']['sequence'],2);self.assertEqual(last['sample']['captured_unix_ms'],1250);self.assertEqual(last['sample']['simulation_ns'],500000000)
        self.assertEqual(len(last['sample']['readings']),8);self.assertEqual(last['catalog'][0]['position_xz'],[-7.,-2.])
        f.step(1300);self.assertIsNone(f.snapshot()['sample'])
    def test_adapter_rejects_model_catalog_and_duplicate_identity_changes(self):
        d=Dummy();f=WorldField(None,d,d,'run','a'*64,'boot');d.time=.25;f.step(1000);original=f.snapshot();scope=dict(run_id='run',model_sha256='a'*64,frame=original['sample']['frame'])
        self.assertEqual(validate_original(original,scope,'boot',original['catalog']),original['sample'])
        for mutate in (lambda v:v['sample'].update(model_sha256='b'*64),lambda v:v['sample']['readings'].__setitem__(1,v['sample']['readings'][0]),lambda v:v['catalog'][0].update(calibration='changed')):
            changed=copy.deepcopy(original);mutate(changed)
            with self.assertRaises(ValueError):validate_original(changed,scope,'boot',original['catalog'])
        changed=copy.deepcopy(original);changed['sample']['readings'][0]['value_ppm']+=1
        with self.assertRaises(ValueError):validate_original(changed,scope,'boot',original['catalog'],original['sample'])
if __name__=='__main__':unittest.main()
