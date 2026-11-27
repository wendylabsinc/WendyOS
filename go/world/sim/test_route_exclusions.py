"""Operator route configuration, UI input routing and opt-in actual MuJoCo checks."""
from collections import deque
import copy
import json
import math
import os
from pathlib import Path
import time
from types import SimpleNamespace
import unittest
from unittest.mock import Mock

from sim.collaborative_runtime import RobotLink, SceneRuntime
from sim.collaborative_viewer import OfficeView

ROOT=Path(__file__).resolve().parents[1]
REGION={'id':'maintenance','min':[-3.5,-2.7],'max':[-3.,-2.3]}

def runtime_fixture():
    r=SceneRuntime.__new__(SceneRuntime)
    r.config=json.loads((ROOT/'examples/collaborative-office/scene.json').read_text())
    r.links={v['id']:RobotLink(v) for v in r.config['robots']}
    r._pose=lambda rid:(r.links[rid].config['start']+[.24],[0,0])
    r.authorization_started_s=None;r.authorized=False;r.stopped=False;r.fault=None
    r.data=SimpleNamespace(time=10.);r.events=deque();r.exclusions=[];r.exclusions_confirmed=[]
    r.route_preview=None;r.route_config_changed=False;r.last_request_s=0
    r.preview_request={'robot_id':'atlas','job_id':'lounge'}
    return r

class RouteExclusionTests(unittest.TestCase):
    def test_review_pending_freeze_and_input_ownership(self):
        r=runtime_fixture();regions=[copy.deepcopy(REGION)]
        self.assertTrue(r.set_exclusions(regions));regions[0]['min'][0]=-7
        self.assertEqual(r.exclusions,[REGION])
        self.assertFalse(r.start_mission())
        self.assertFalse(r.authorized)
        r.exclusions_confirmed=copy.deepcopy(r.exclusions);r.route_preview={'reachable':True}
        self.assertTrue(r.start_mission());deadline_start=r.authorization_started_s
        self.assertFalse(r.set_exclusions([]));self.assertEqual(r.exclusions,[REGION])
        self.assertEqual(r.authorization_started_s,deadline_start)
        self.assertFalse(r.select_preview('atlas','workspace'))

    def test_invalid_rectangles_do_not_change_draft(self):
        r=runtime_fixture();self.assertTrue(r.set_exclusions([REGION]))
        for rectangle in [dict(id='bad',min=[-4.4,-2.0],max=[-4.,-1.6]),
                          dict(id='bad',min=[-20,0],max=[0,1]),
                          dict(id='bad',min=[0,0],max=[0,1]),
                          dict(id='bad',min=[float('nan'),0],max=[1,1])]:
            self.assertFalse(r.set_exclusions([rectangle]));self.assertEqual(r.exclusions,[REGION])
        self.assertFalse(r.set_exclusions([REGION,REGION]))
        self.assertTrue(r.set_exclusions([]));self.assertIsNone(r.route_preview)

    def test_two_floor_clicks_create_rectangle_and_frozen_clicks_do_not_edit(self):
        view=OfficeView.__new__(OfficeView);view.runtime=runtime_fixture()
        view.floor_point=Mock(side_effect=[[-3.,-2.3],[-3.5,-2.7]])
        view.exclusion_corner=None;view.drawing_exclusion=True
        view.exclusion_click(10,20);self.assertEqual(view.runtime.exclusions,[])
        view.exclusion_click(30,40)
        self.assertEqual(view.runtime.exclusions,[dict(REGION,id='area-1')]);self.assertFalse(view.drawing_exclusion)
        view.glfw=Mock(MOUSE_BUTTON_LEFT=0,PRESS=1,RELEASE=0)
        view.glfw.get_cursor_pos.return_value=(10,20)
        view.framebuffer_cursor=lambda x,y:(x,y)
        view.snapshot={'route_exclusions_frozen':True};view.buttons=[];view.flags={'links':False}
        view.dragged=False;view.drawing_exclusion=True;view.exclusion_click=Mock()
        view.mouse(None,0,0,0);view.exclusion_click.assert_not_called()

@unittest.skipUnless(os.environ.get('ROUTE_EXCLUSION_COORDINATOR'),'set ROUTE_EXCLUSION_COORDINATOR for actual local MuJoCo')
class RouteExclusionPhysicsTests(unittest.TestCase):
    def test_actual_physics_completes_around_exclusion(self):
        r=SceneRuntime(ROOT/'examples/collaborative-office/scene.json',Path(os.environ['ROUTE_EXCLUSION_COORDINATOR']),'route-exclusion-physics')
        self.addCleanup(r.close)
        def wait_for(predicate,steps=300):
            for _ in range(steps):
                r.tick(.02);time.sleep(.003)
                self.assertIsNone(r.fault)
                if predicate():return
            self.fail('bounded simulator wait expired')
        self.assertTrue(r.select_preview('atlas','lounge'))
        wait_for(lambda:r.route_preview is not None)
        base=copy.deepcopy(r.route_preview)
        self.assertTrue(r.set_exclusions([REGION]))
        self.assertFalse(r.start_mission())
        wait_for(lambda:r.route_preview is not None)
        detour=copy.deepcopy(r.route_preview)
        self.assertTrue(detour['reachable']);self.assertGreater(detour['length_m'],base['length_m'])
        self.assertEqual(detour['baseline_path'],base['path'])
        # Preview and edit operations have no motion side effect.
        for robot in r.config['robots']:
            self.assertLess(math.dist(r._pose(robot['id'])[0][:2],robot['start']),1e-9)
        self.assertTrue(r.start_mission())
        positions=[];closest=math.inf
        for _ in range(7500):
            r.tick(.02);time.sleep(.002)
            self.assertIsNone(r.fault)
            for rid in r.links:
                p=r._pose(rid)[0][:2]
                distance=math.hypot(max(REGION['min'][0]-p[0],0,p[0]-REGION['max'][0]),max(REGION['min'][1]-p[1],0,p[1]-REGION['max'][1]))
                closest=min(closest,distance)
                self.assertGreater(distance,.32,'robot center violated the configured exclusion clearance')
            positions.append(r._pose('atlas')[0][:2])
            if r.completed==len(r.config['jobs']):break
        self.assertEqual(r.completed,6)
        self.assertFalse(r.set_exclusions([]))
        report={'scope':'Actual MuJoCo 4 planar office physics, simulation only','base':base,'detour':detour,
                'completed':r.completed,'simulation_seconds':float(r.data.time),'minimum_center_distance_to_exclusion_m':closest,
                'atlas_samples':len(positions),'preview_motion_m':0,'frozen_after_start':r.exclusions==[REGION]}
        if os.environ.get('ROUTE_EXCLUSION_REPORT'):
            Path(os.environ['ROUTE_EXCLUSION_REPORT']).write_text(json.dumps(report,indent=2)+'\n')

if __name__=='__main__':unittest.main()
