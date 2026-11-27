"""Pure oracle/encoding checks. Actual mj_ray generation runs in pinned Linux."""
import copy
import hashlib
import json
import math
from pathlib import Path
import importlib.util
import subprocess
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch
import xml.etree.ElementTree as ET
from sim import go2_cloud_fixture as f
from sim.go2_cloud import decode_cloud, SOURCE_DIGEST

_runner_spec=importlib.util.spec_from_file_location('cloud_fixture_runner',Path(__file__).resolve().parents[1]/'tests/go2_cloud_grid_fixtures.py')
runner=importlib.util.module_from_spec(_runner_spec)
_runner_spec.loader.exec_module(runner)


class FixtureTests(unittest.TestCase):
    def test_unique_supported_and_explicit_counterexamples(self):
        cases=f.definitions()
        self.assertEqual(len(cases),9)
        self.assertEqual(len({c['name'] for c in cases}),9)
        self.assertEqual(sum(c['supported_model'] for c in cases),6)
        for c in cases:
            self.assertEqual(bool(c['unsupported_reasons']),not c['supported_model'])
            xml,floors=f.scene_xml(c);root=ET.fromstring(xml)
            self.assertEqual(root.tag,'mujoco')
            self.assertTrue(floors)
            self.assertFalse(root.findall('.//joint'))
            self.assertFalse(root.findall('.//actuator'))
            if c['supported_model']:
                self.assertTrue(all(g['half_size_m'][2]*2>=1 for g in c['columns']))
        before=copy.deepcopy(cases);cases[0]['columns'][0]['name']='changed'
        self.assertEqual(f.definitions(),before)

    def test_original_float32_values_and_missing_slots_round_trip(self):
        # Two independent, exactly representable source directions.
        raw=f.cloud_wire([[-1.,0.,0.],[1.,0.,0.]],f.CAPTURE_NS)
        cloud=decode_cloud(raw,source_digest=SOURCE_DIGEST)
        self.assertEqual(cloud.capture_unix_ns,f.CAPTURE_NS)
        self.assertEqual([p.ray_index for p in cloud.points],[4320,4500])
        self.assertEqual([p.xyz_m for p in cloud.points],[(-1.,0.,0.),(1.,0.,0.)])
        self.assertEqual(sum(x is None for x in cloud.ranges_m),8998)
        self.assertEqual(cloud.serialized_sha256,hashlib.sha256(raw).hexdigest())

    def test_clipping_detects_edge_crossing_without_contained_vertices(self):
        horizontal=f.rectangle(-2,2,-.1,.1)
        vertical=f.rectangle(-.1,.1,-2,2)
        intersection=f.clip_polygon(horizontal,vertical)
        self.assertAlmostEqual(f.polygon_area(intersection),.04)
        disjoint=f.clip_polygon(horizontal,f.rectangle(3,4,-.1,.1))
        self.assertEqual(disjoint,[])
        touching=f.clip_polygon(f.rectangle(0,1,0,1),f.rectangle(1,2,0,1))
        self.assertTrue(touching)
        self.assertEqual(f.polygon_area(touching),0)

    def test_rotated_corner_and_full_body_hull_are_checked(self):
        diamond=[[0,-1],[1,0],[0,1],[-1,0]]
        near=f.rectangle(.45,.55,.45,.55)
        self.assertTrue(f.clip_polygon(near,diamond))
        self.assertFalse(f.clip_polygon(f.rectangle(.9,1,.9,1),diamond))
        poly=f.hull([(0,0),(1,0),(1,1),(0,1),(.5,.5),(0,0)])
        self.assertEqual(len(poly),4)
        self.assertEqual(f.polygon_area(poly),1)

    def test_hole_not_filled_by_remote_floor_and_low_obstacle_retained(self):
        oracle={'columns':[],'floor_rectangles_xy_m':[[-2,0,-2,2],[1,2,-2,2],[0,1,-2,0],[0,1,1,2]]}
        self.assertFalse(f.ground_truth(f.rectangle(.1,.9,.1,.9),oracle)['supporting_floor'])
        self.assertTrue(f.ground_truth(f.rectangle(-.9,-.1,.1,.9),oracle)['supporting_floor'])
        oracle['columns']=[{'name':'low','polygon_xy_m':f.rectangle(-.5,-.4,.2,.3),'z_max_m':.05}]
        self.assertEqual(f.ground_truth(f.rectangle(-.9,-.1,.1,.9),oracle)['obstacles'],['low'])

    def test_comparison_keeps_unsupported_counterexample_and_no_qualification(self):
        req=b'{"only":"sensor-data"}\n'
        oracle={'columns':[{'name':'column','polygon_xy_m':f.rectangle(.02,.08,.02,.08)}],
                'floor_rectangles_xy_m':[[-2,2,-2,2]],'nominal_body_projected_polygon_xy_m':f.rectangle(-1,-.5,-1,-.5)}
        case={'name':'case','supported_model':True,'unsupported_reasons':[],'oracle':oracle}
        out={'schema':'wendy.world.go2-cloud-check-result.v1','input_sha256':f.sha(req),'operational_authority':False,
             'evidence':{'cells':[{'x':0,'y':0,'state':'free'}]},'cloud':{'current_footprint_clear':False},
             'horizontal':{'current_footprint_clear':False}}
        result=f.compare(case,out,req)
        self.assertFalse(result['supported_scene_pass'])
        self.assertEqual(result['free_cell_oracle_conflicts'][0]['obstacles'],['column'])
        case['supported_model']=False;case['unsupported_reasons']=['thin_subangular_obstacle']
        result=f.compare(case,out,req)
        self.assertTrue(result['unsupported_counterexample_observed'])
        self.assertFalse(result['qualification'])
        self.assertFalse(result['oracle_used_as_algorithm_input'])
        out['input_sha256']='0'*64
        with self.assertRaises(ValueError):f.compare(case,out,req)

    def test_all_unknown_is_not_positive_supported_geometry_evidence(self):
        req=b'{}';case={'name':'case','supported_model':True,'unsupported_reasons':[],
          'oracle':{'columns':[],'floor_rectangles_xy_m':[[-2,2,-2,2]],'nominal_body_projected_polygon_xy_m':f.rectangle(-.4,.4,-.3,.3)}}
        out={'schema':'wendy.world.go2-cloud-check-result.v1','input_sha256':f.sha(req),'operational_authority':False,
             'evidence':{'cells':[{'x':0,'y':0,'state':'unknown'}]},'cloud':{'current_footprint_clear':False},'horizontal':{'current_footprint_clear':False}}
        self.assertFalse(f.compare(case,out,req)['supported_scene_pass'])
        out['evidence']['cells'].append({'x':0,'y':0,'state':'free'})
        with self.assertRaises(ValueError):f.compare(case,out,req)

    def test_cleanup_distinguishes_absence_from_daemon_error(self):
        for message in (b'error: no such object: owned\n',b'Error: No such object: owned\n',
                        b'Error response from daemon: No such container: owned\n'):
            with patch.object(runner.subprocess,'run',return_value=SimpleNamespace(returncode=1,stdout=b'[]',stderr=message)):
                self.assertIsNone(runner.inspect_owned('owned','nonce'))
        for message in (b'daemon unavailable',b'error: no such object: owned-other',b'permission denied'):
            with patch.object(runner.subprocess,'run',return_value=SimpleNamespace(returncode=1,stdout=b'',stderr=message)):
                with self.assertRaises(ValueError):runner.inspect_owned('owned','nonce')

    def test_cleanup_removes_only_owned_immutable_id_without_volume(self):
        responses=[SimpleNamespace(returncode=0,stdout=json.dumps([{'Id':'immutable-id','Config':{'Labels':{runner.LABEL:'nonce'}}}]).encode(),stderr=b''),
                   SimpleNamespace(returncode=0,stdout=b'',stderr=b''),
                   SimpleNamespace(returncode=1,stdout=b'[]',stderr=b'error: no such object: owned')]
        with patch.object(runner.subprocess,'run',side_effect=responses) as process:
            result=runner.cleanup_owned('owned','nonce')
            self.assertTrue(result['owned_container_absent'])
            self.assertEqual(process.call_args_list[1].args[0],['docker','rm','-f','immutable-id'])

    def test_foreign_container_and_inspection_timeout_never_removed(self):
        response=SimpleNamespace(returncode=0,stdout=json.dumps([{'Id':'foreign','Config':{'Labels':{runner.LABEL:'other'}}}]).encode(),stderr=b'')
        with patch.object(runner.subprocess,'run',return_value=response) as process:
            with self.assertRaises(ValueError):runner.cleanup_owned('owned','nonce')
            self.assertEqual(process.call_count,1)
        with patch.object(runner.subprocess,'run',side_effect=subprocess.TimeoutExpired('inspect',10)) as process:
            with self.assertRaises(subprocess.TimeoutExpired):runner.cleanup_owned('owned','nonce')
            self.assertEqual(process.call_count,1)

    def test_private_exclusive_outputs_and_symlink_size_refusal(self):
        # Resolve macOS /var -> /private/var before constructing checked paths.
        with tempfile.TemporaryDirectory() as temp:
            root=Path(temp).resolve();out=runner.output_directory(root/'new')
            self.assertEqual(out.stat().st_mode&0o777,0o700)
            with self.assertRaises(ValueError):runner.output_directory(out)
            runner.write_new(out/'file',b'abc')
            self.assertEqual((out/'file').stat().st_mode&0o777,0o600)
            self.assertEqual(runner.read_regular(out/'file',3),b'abc')
            with self.assertRaises(FileExistsError):runner.write_new(out/'file',b'replaced')
            with self.assertRaises(ValueError):runner.read_regular(out/'file',2)
            (out/'link').symlink_to(out/'file')
            with self.assertRaises(ValueError):runner.read_regular(out/'link',3)
            (root/'alias').symlink_to(out,target_is_directory=True)
            with self.assertRaises(ValueError):runner.output_directory(root/'alias'/'bad')


if __name__=='__main__':unittest.main()
