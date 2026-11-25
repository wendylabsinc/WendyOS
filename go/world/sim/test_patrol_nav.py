import math
import tempfile
import xml.etree.ElementTree as ET
import unittest
from pathlib import Path

from sim.patrol_nav import MotionLease, supported_command, free_at, candidates, write_map, nav_parameters, inspection_behavior, INSPECTION_MODE


class PatrolNavTests(unittest.TestCase):
    def test_deadband_is_never_silently_raised(self):
        for speed in (0.01,0.1,0.49):
            command=supported_command(speed,0,0.4)
            self.assertEqual(command['vx'],0)
            self.assertEqual(command['wz'],0.4)
        self.assertEqual(supported_command(.55,0,0)['vx'],.55)
        self.assertEqual(supported_command(.8,0,0)['vx'],.55)
        self.assertEqual(supported_command(.55,0,1.1),dict(vx=0.,vy=0.,wz=.7))
        for command in ((-.1,0,0),(.55,.1,0),(float('nan'),0,0)):
            self.assertEqual(supported_command(*command),dict(vx=0,vy=0,wz=0))

    def test_executor_crash_latches_goal_off(self):
        lease=MotionLease(); lease.begin('a')
        self.assertFalse(lease.allowed(10))
        lease.heartbeat(10)
        self.assertTrue(lease.allowed(10.9))
        self.assertFalse(lease.allowed(11.1))
        lease.heartbeat(11.2)
        self.assertFalse(lease.allowed(11.3))
        lease.begin('b'); lease.heartbeat(12)
        self.assertTrue(lease.allowed(12.1))
        lease.begin('c'); lease.heartbeat(20); lease.heartbeat(21.1)
        self.assertFalse(lease.allowed(21.2))
        lease.begin('d'); lease.heartbeat(30, ttl=.2)
        self.assertTrue(lease.allowed(30.1))
        self.assertFalse(lease.allowed(30.3))
        lease.begin('e'); lease.heartbeat(40, ttl=-1)
        self.assertFalse(lease.allowed(40))
        lease.cancel(); lease.heartbeat(42.2)
        self.assertFalse(lease.allowed(12.3))

    def test_occupied_unknown_and_outside_candidates_rejected(self):
        grid=dict(resolution=.1,width=20,height=20,origin=[0,0,0],data=[0]*400)
        self.assertTrue(free_at(grid,1,1))
        self.assertFalse(free_at(grid,.1,.1))
        grid['data'][10*20+10]=100
        self.assertFalse(free_at(grid,1,1))
        grid['data'][10*20+10]=-1
        self.assertFalse(free_at(grid,1,1))

    def test_map_y_is_flipped_for_pgm_only(self):
        grid=dict(resolution=.05,width=2,height=2,origin=[-1,-2,0],data=[0,100,-1,0])
        with tempfile.TemporaryDirectory() as directory:
            path=write_map(grid,directory)
            self.assertEqual((Path(directory)/'office.pgm').read_bytes().split(b'255\n')[1],bytes([205,254,254,0]))
            self.assertIn('origin: [-1, -2, 0]',path.read_text())

    def test_unreachable_rooms_stay_explicit(self):
        grid=dict(resolution=.1,width=170,height=130,origin=[-8.5,-6.5,0],data=[100]*(170*130))
        rooms=candidates(grid)['rooms']
        self.assertEqual(len(rooms),6)
        self.assertTrue(all(room['candidates']==[] for room in rooms))

    def test_route_stays_fixed_while_valid(self):
        root=Path(__file__).resolve().parents[1]
        for name, action in [('navigate.xml','ComputePathToPose'), ('navigate_through.xml','ComputePathThroughPoses')]:
            tree=ET.parse(root/'deploy/patrol-nav'/name).getroot()
            rate=tree.find('.//RateController')
            self.assertEqual([node.tag for node in rate],['Fallback'])
            fallback=rate[0]
            self.assertEqual([node.tag for node in fallback],['ReactiveSequence',action])
            valid=fallback.find('ReactiveSequence')
            self.assertIsNotNone(valid.find('Inverter').find('GlobalUpdatedGoal'))
            self.assertIsNotNone(valid.find('IsPathValid'))

    def test_inspection_uses_only_installed_strict_tree(self):
        self.assertEqual(inspection_behavior({}), '')
        self.assertEqual(inspection_behavior({'observation_mode':INSPECTION_MODE}), '/world/deploy/patrol-nav/inspect.xml')
        for body in ({'observation_mode':None},{'observation_mode':'other'},{'behavior_tree':'/tmp/x.xml'},{'goal_checker':'loose'}):
            with self.assertRaises(ValueError): inspection_behavior(body)
        root=Path(__file__).resolve().parents[1]/'deploy/patrol-nav'
        strict=ET.parse(root/'inspect.xml').find('.//FollowPath')
        ordinary=ET.parse(root/'navigate.xml').find('.//FollowPath')
        self.assertEqual(strict.attrib['goal_checker_id'],'inspection_goal_checker')
        self.assertEqual(ordinary.attrib['goal_checker_id'],'general_goal_checker')
        base={name:{name:{'ros__parameters':{}}} for name in ('global_costmap','local_costmap')}
        controller=nav_parameters(base,'map','primitives','navigate',[-6,0,0])['controller_server']['ros__parameters']
        self.assertEqual(controller['inspection_goal_checker']['yaw_goal_tolerance'],.10)
        self.assertEqual(controller['general_goal_checker']['yaw_goal_tolerance'],math.pi)
        self.assertEqual(controller['inspection_goal_checker']['xy_goal_tolerance'],controller['general_goal_checker']['xy_goal_tolerance'])

    def test_nav_configuration_is_localized_lattice_pursuit(self):
        base={name:{name:{'ros__parameters':{}}} for name in ('global_costmap','local_costmap')}
        config=nav_parameters(base,'map.yaml','primitives.json','navigate.xml',[-5.8,.15,.08])
        self.assertIn('SmacPlannerLattice',config['planner_server']['ros__parameters']['GridBased']['plugin'])
        self.assertIn('RegulatedPurePursuit',config['controller_server']['ros__parameters']['FollowPath']['plugin'])
        self.assertEqual(config['amcl']['ros__parameters']['initial_pose']['x'],-5.8)
        self.assertEqual(config['controller_server']['ros__parameters']['FollowPath']['desired_linear_vel'],.55)
        self.assertEqual(base['global_costmap']['global_costmap']['ros__parameters'],{})


if __name__=='__main__': unittest.main()
