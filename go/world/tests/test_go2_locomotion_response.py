import hashlib
import importlib.util
import json
import math
from pathlib import Path
import tempfile
import struct
from types import SimpleNamespace
import unittest
from unittest import mock

spec=importlib.util.spec_from_file_location('response',Path(__file__).with_name('go2_locomotion_response.py'))
r=importlib.util.module_from_spec(spec);spec.loader.exec_module(r)


class Fake:
    def __init__(self, clock):
        self.clock=clock;self.calls=[];self.steps=0;self.command=[0.,0.,0.];self.stopped=False;self.model=SimpleNamespace(nq=19,nv=18,nu=12,ngeom=18,nbody=14,body_mass=[15.])
    def arm(self):return 'test-owner'
    def command_velocity(self,x,y,z,token):
        assert token=='test-owner'
        self.calls.append((self.clock(),x));self.command=[x,y,z]
    def step(self):self.steps+=1
    def stop(self,token):self.stopped=True
    def sample(self):
        return {'simulation_time_s':self.clock(),'position_xyz_m':[self.clock(),0.,.34],
                'quaternion_wxyz':[1.,0.,0.,0.], 'body_velocity_mps':[1.,0.,0.],
                'body_angular_velocity_rps':[0.,0.,0.], 'roll_rad':0.,'pitch_rad':0.,
                'command_target':self.command[:],'applied_target':self.command[:],
                'mode':'moving','policy_updates':self.steps//10}


class ResponseTests(unittest.TestCase):
    def test_fixed_matrix_cadence_and_no_pose_mutation_interface(self):
        sims=[];rows=[]
        def factory(clock):
            sim=Fake(clock);sims.append(sim);return sim
        out=r.run_case(.05,factory,lambda s:s.sample(),rows.append)
        self.assertEqual(len(sims),1);s=sims[0]
        self.assertEqual(s.steps,14000);self.assertTrue(s.stopped)
        self.assertEqual(out['command_refreshes'],280);self.assertEqual(len(rows),2803)
        self.assertEqual(s.calls[0],(0.,0.));self.assertEqual(s.calls[30],(3.,.05));self.assertEqual(s.calls[230],(23.,0.))
        self.assertTrue(all(abs((b[0]-a[0])-.1)<1e-12 for a,b in zip(s.calls,s.calls[1:])))
        self.assertAlmostEqual(out['phases']['command']['response']['sampled_command_forward_integral_m'],1.)
        self.assertAlmostEqual(out['phases']['command']['response']['mean_body_velocity_mps'][0],1.)
        self.assertEqual(out['phases']['command']['after_first_two_seconds']['elapsed_s'],18.)
        self.assertEqual(out['phases']['zero']['sampled_settling']['time_s'],None)
        self.assertIsNone(out['safety_pass']);self.assertFalse(out['live_commissioning'])
    def test_counterfactual_label_and_new_instance(self):
        sims=[]
        def factory(clock):s=Fake(clock);sims.append(s);return s
        with mock.patch.object(r,'PHASES',(('warmup',5),('zero',5))):
            a=r.run_case(0.,factory,lambda s:s.sample(),lambda _:None)
            b=r.run_case(.4,factory,lambda s:s.sample(),lambda _:None)
        self.assertIsNot(sims[0],sims[1]);self.assertFalse(a['counterfactual_above_live_profile']);self.assertTrue(b['counterfactual_above_live_profile'])
    def test_no_arbitrary_speed_or_nonfinite(self):
        for x in [-.05,.051,.8,math.nan,math.inf,True]:
            with self.subTest(x=x),self.assertRaises(ValueError):r.run_case(x,lambda _:None,None,None)
    def test_named_matrices_are_explicit_and_bounded(self):
        self.assertEqual(r.MATRICES,{'baseline':(0.,.05,.1,.2,.4),'commissioning-window':(.25,.30,.35)})
        with mock.patch.object(r,'PHASES',(('warmup',5),('zero',5))):
            for speed in r.MATRICES['commissioning-window']:
                out=r.run_case(speed,Fake,lambda s:s.sample(),lambda _:None,matrix='commissioning-window')
                self.assertEqual(out['matrix_name'],'commissioning-window')
                self.assertTrue(out['counterfactual_above_live_profile']);self.assertFalse(out['live_commissioning'])
        for matrix,speed in [('baseline',.25),('commissioning-window',.4),('commissioning-window',.05),('commissioning-window',.31),('unknown',.05)]:
            with self.subTest(matrix=matrix,speed=speed),self.assertRaises(ValueError):
                r.run_case(speed,lambda _:None,None,None,matrix=matrix)
    def test_capture_nonfinite_fails_without_retry(self):
        sims=[]
        def factory(clock):s=Fake(clock);sims.append(s);return s
        def sample(s):x=s.sample();x['roll_rad']=math.nan;return x
        with self.assertRaises(ValueError):r.run_case(.05,factory,sample,lambda _:None)
        self.assertEqual(len(sims),1);self.assertEqual(len(sims[0].calls),1)
    def test_departed_mode_refuses(self):
        def sample(s):x=s.sample();x['mode']='fallen';return x
        with self.assertRaises(ValueError):r.run_case(.05,Fake,sample,lambda _:None)
    def test_strict_json(self):
        for raw in ['{"a":1,"a":2}','{"a":NaN}']:
            with self.assertRaises(ValueError):r.strict(raw)
    def test_path_traversal_and_symlink(self):
        with tempfile.TemporaryDirectory() as d:
            root=Path(d)
            for name in ['../x','/x','./x','x/../y','x//y','x\\y','']:
                with self.subTest(name=name),self.assertRaises(ValueError):r.relative(root,name)
            (root/'link').symlink_to(root,target_is_directory=True)
            with self.assertRaises(ValueError):r.relative(root,'link/secret')
    def test_inventory_digest_and_special_refusal(self):
        with tempfile.TemporaryDirectory() as d:
            root=Path(d);(root/'x').write_bytes(b'abc');good={'x':{'sha256':hashlib.sha256(b'abc').hexdigest(),'bytes':3}}
            self.assertEqual(r.check_inventory(root,good),good)
            good['x']['bytes']=4
            with self.assertRaises(ValueError):r.check_inventory(root,good)
            with self.assertRaises(ValueError):r.read(root)
    def test_read_size_bound(self):
        with tempfile.TemporaryDirectory() as d:
            p=Path(d)/'x';p.write_bytes(b'abc')
            with self.assertRaises(ValueError):r.read(p,2)
    def test_no_overwrite_output(self):
        with tempfile.TemporaryDirectory() as d:
            p=Path(d)/'x';r.write_json(p,{'a':1})
            with self.assertRaises(FileExistsError):r.write_json(p,{'a':2})
            self.assertEqual(json.loads(p.read_text()),{'a':1})
    def test_virtual_clock_exact_ticks(self):
        c=r.VirtualClock()
        for _ in range(14000):c.advance()
        self.assertEqual(c.now(),28.)
    def test_settling_needs_contiguous_suffix_and_one_second(self):
        clock=r.VirtualClock();sim=Fake(clock.now);rows=[]
        for i in range(501):
            row=sim.sample();row.update(phase='zero',phase_time_s=i/100,virtual_monotonic_s=i/100)
            row['body_velocity_mps']=[0. if i>=200 else .1,0.,0.];rows.append(row)
        self.assertEqual(r.phase_summary(rows)['sampled_settling']['time_s'],2.)
        rows[-2]['body_velocity_mps']=[.1,0.,0.]
        self.assertIsNone(r.phase_summary(rows)['sampled_settling']['time_s'])

    def test_complete_source_and_locked_assets(self):
        with tempfile.TemporaryDirectory() as d:
            base=Path(d);source=base/'source';assets=base/'assets';source.mkdir();assets.mkdir()
            asset=b'fixed test asset';(assets/'x').write_bytes(asset)
            lock={'bundle':r.POLICY,'files':[{'path':'x','sha256':r.sha(asset),'size':len(asset)}]}
            (source/'assets.lock.json').write_text(json.dumps(lock));(source/'test.py').write_text('fixture only')
            inventory={p.name:{'sha256':r.sha(p.read_bytes()),'bytes':p.stat().st_size} for p in source.iterdir()}
            h=hashlib.sha256(b'wendy-go2-source-v1\0')
            for name in sorted(inventory):
                for raw in [name.encode(),(source/name).read_bytes()]:h.update(struct.pack('>Q',len(raw)));h.update(raw)
            digest='sha256:'+h.hexdigest()
            pins={'schema':r.SCHEMA,'source_digest':digest,'source_files':inventory,'asset_files':{'x':{'sha256':r.sha(asset),'bytes':len(asset)}},'engines':r.ENGINES,'policy_bundle':r.POLICY}
            path=base/'pins.json';path.write_text(json.dumps(pins))
            with mock.patch.object(r,'SOURCE_DIGEST',digest):
                # Nonloaded asset extras are allowed; source extras are not.
                (assets/'unused').write_text('unused')
                self.assertEqual(r.validate_inputs(source,assets,path)[2]['source_digest'],digest)
                pins['engines']={**r.ENGINES,'mujoco':'3.3.7'};path.write_text(json.dumps(pins))
                with self.assertRaises(ValueError):r.validate_inputs(source,assets,path)
                self.assertEqual(r.validate_inputs(source,assets,path,'3.3.7')[2]['engines']['mujoco'],'3.3.7')
                with self.assertRaises(ValueError):r.validate_inputs(source,assets,path,'4.0.1')
                pins['engines']=r.ENGINES;path.write_text(json.dumps(pins))
                (source/'extra').write_text('unexpected')
                with self.assertRaises(ValueError):r.validate_inputs(source,assets,path)
                (source/'extra').unlink();(assets/'x').write_bytes(b'tampered')
                with self.assertRaises(ValueError):r.validate_inputs(source,assets,path)
            pins['engines']={'mujoco':'different','onnxruntime':'1.23.2'};path.write_text(json.dumps(pins))
            with self.assertRaises(ValueError):r.validate_inputs(source,assets,path)
    def test_command_integral_uses_left_endpoint(self):
        rows=[{'virtual_monotonic_s':0.,'command_target':[.05]}, {'virtual_monotonic_s':1.,'command_target':[0.]}]
        self.assertEqual(r.integral(rows,'command_target',0),.05)

if __name__=='__main__':unittest.main()
