"""Real restricted-loader contracts; execute in an existing Torch/NumPy runtime."""
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import random
import shlex
import tempfile
import unittest
from unittest import mock
try:
    import torch
    import numpy as np
except ImportError:
    torch = None
spec=importlib.util.spec_from_file_location('fixed_checkpoint',Path(__file__).with_name('checkpoint.py'))
inspector=importlib.util.module_from_spec(spec);spec.loader.exec_module(inspector)
sha=lambda p:hashlib.sha256(Path(p).read_bytes()).hexdigest()

@unittest.skipUnless(torch is not None,'Requires actual Torch/NumPy; no mock loader substitute')
class CheckpointTests(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory();self.root=Path(self.tmp.name)
        (self.root/'rl').mkdir();(self.root/'scripts').mkdir()
        (self.root/'rl/a.py').write_text('# protected module\n')
        (self.root/'train.py').write_text('# protected trainer\n')
        (self.root/'scripts/adapter.py').write_text('# original environment adapter\n')
        self.path=self.root/'parent.pt'
        self.saved={'iteration':64,'optimizer_steps_total':16384,'transitions':100000,
                    'policy':{'secret_tensor':torch.tensor([1.,2.,3.])},
                    'optimizer':{'state':{0:{'step':torch.tensor(16384.)}}},
                    'torch_rng':torch.get_rng_state(),'cuda_rng':[],
                    'numpy_rng':np.random.get_state(),'python_rng':random.getstate(),'experiment_rng':{},
                    'manifest':{'experiment':{'seed':93113,'planned_iterations':128,'num_envs':2048,'coefficient':0,'actor_scope':'corrector','source_hashes':{name:sha(self.root/name) for name in ['train.py','rl/a.py']}},'stress_profile':{},'perturbation_module_sha256':sha(self.root/'scripts/adapter.py')}}
        torch.save(self.saved,self.path)
    def tearDown(self):self.tmp.cleanup()
    def request(self):return {'path':str(self.path),'sha256':sha(self.path),'source_directory':str(self.root)}

    def test_counters_empty_rng_marker_and_adapter_are_available_without_tensor_payload(self):
        result=inspector.inspect(self.request())
        self.assertEqual(result['iteration'],64);self.assertEqual(result['optimizer_parameter_steps'],[16384.])
        self.assertEqual(result['experiment_rng_keys'],[])
        self.assertTrue(result['available_state']['experiment_rng'])
        self.assertEqual(result['environment_adapter']['matching_project_files'],['scripts/adapter.py'])
        self.assertEqual(result['environment_adapter']['stress_profile'],{})
        self.assertTrue(result['source_check']['python_directory_inventory_matches'])
        self.assertNotIn('secret_tensor',json.dumps(result));self.assertFalse(result['training_started'])
        self.assertFalse(result['cuda_execution_verified'])

    def test_missing_adapter_profile_stays_unknown(self):
        del self.saved['manifest']['stress_profile']
        torch.save(self.saved,self.path)
        result=inspector.inspect(self.request())['environment_adapter']
        self.assertIsNone(result['stress_profile'])
        self.assertFalse(result['stress_profile_available'])

    def test_extra_module_cannot_hide_behind_matching_declared_hashes(self):
        (self.root/'rl/extra.py').write_text('# extra module\n')
        result=inspector.inspect(self.request())['source_check']
        self.assertTrue(result['declared_hashes_match']);self.assertFalse(result['python_directory_inventory_matches'])
        self.assertEqual(result['extra_python_files'],['rl/extra.py'])
        (self.root/'rl/a.py').write_text('# changed\n')
        self.assertEqual(inspector.inspect(self.request())['source_check']['changed'],['rl/a.py'])

    def test_hash_size_and_symlink_fail_before_loading(self):
        request=self.request();request['sha256']='0'*64
        with self.assertRaisesRegex(ValueError,'hash mismatch'):inspector.inspect(request)
        link=self.root/'alias.pt';link.symlink_to(self.path);request=self.request();request['path']=str(link)
        with self.assertRaises(OSError):inspector.inspect(request)
        with self.path.open('r+b') as stream:stream.truncate(inspector.MAX_CHECKPOINT+1)
        with self.assertRaisesRegex(ValueError,'bounded regular'):inspector.inspect(self.request())

    def test_replacement_during_loading_cannot_change_hash_bound_metadata(self):
        original_load=torch.load
        request=self.request()
        def replace_then_load(*args,**kwargs):
            replacement=dict(self.saved,iteration=999)
            torch.save(replacement,self.path)
            return original_load(*args,**kwargs)
        with mock.patch.object(torch,'load',side_effect=replace_then_load):
            result=inspector.inspect(request)
        self.assertEqual(result['iteration'],64)
        self.assertEqual(result['checkpoint_sha256'],request['sha256'])
        self.assertNotEqual(sha(self.path),request['sha256'])

    def test_unsafe_pickle_never_runs_and_never_falls_back(self):
        marker=self.root/'unsafe-loader-ran'
        class Unsafe:
            def __reduce__(self):return os.system,('touch '+shlex.quote(str(marker)),)
        torch.save({'unsafe':Unsafe()},self.path)
        with self.assertRaises(Exception):inspector.inspect(self.request())
        self.assertFalse(marker.exists())

    @unittest.skipUnless(os.environ.get('WENDY_TEST_REAL_PROJECT'),'Actual source-backed parent is optional')
    def test_actual_g1_parent_preserves_adapter_and_complete_state_metadata(self):
        root=Path(os.environ['WENDY_TEST_REAL_PROJECT']);path=root/'inputs/resume-iteration-0064.pt'
        result=inspector.inspect({'path':str(path),'sha256':os.environ['WENDY_TEST_PARENT_SHA256'],'source_directory':str(root)})
        self.assertEqual(result['iteration'],64);self.assertEqual(result['optimizer_steps_total'],16384)
        self.assertEqual(result['experiment_rng_keys'],[])
        self.assertEqual(result['environment_adapter']['matching_project_files'],['scripts/robustness_worker.py'])
        self.assertTrue(result['source_check']['python_directory_inventory_matches'])

if __name__=='__main__':unittest.main()
