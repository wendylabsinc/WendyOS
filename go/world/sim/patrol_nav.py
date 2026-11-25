"""Private, simulated Go2 Nav2 adapter. Control only reads /sensors, never /state.

HTTP calls are for the fixture mission executor on an isolated container network.
A goal requires continuing heartbeats. Expiry cancels it, so a restarted executor
must submit a new goal; delayed heartbeats cannot resume the previous motion.
"""
import argparse
from copy import deepcopy
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import math
import os
from pathlib import Path
import subprocess
import threading
import time
import urllib.request
from sim import patrol_exclusions as restrictions
from sim import patrol_localization as localization

ROOMS = [
    ('workspace', 'Workspace', (-7.8, 1.65, -2.8, 5.8)),
    ('meeting', 'Meeting room', (-2.5, 1.65, 2.5, 5.8)),
    ('private-office', 'Private office', (2.8, 1.65, 7.8, 5.8)),
    ('lounge', 'Lounge', (-7.8, -5.8, -1.65, -1.4)),
    ('reception', 'Reception', (-1.3, -4.8, 3.6, -1.4)),
    ('kitchenette', 'Kitchenette', (3.8, -5.8, 7.8, -1.4)),
]


def request(url, payload=None, timeout=0.5):
    data = None if payload is None else json.dumps(payload, allow_nan=False).encode()
    req = urllib.request.Request(url, data=data, headers={'Content-Type': 'application/json'})
    with urllib.request.urlopen(req, timeout=timeout) as response:
        return json.load(response)


def validated_sensor_transforms(mounts, scan_frame='laser'):
    """Validate this adapter's fixed laser mount before accepting its scan.

    A static TF has no per-capture expiry. Reject malformed input here so a
    cached valid TF cannot hide a bad producer packet. Keep valid values intact.
    """
    if scan_frame != 'laser' or not isinstance(mounts, list) or len(mounts) != 1:
        raise ValueError('expected one laser mount in the laser scan frame')
    mount = mounts[0]
    if not isinstance(mount, dict) or mount.get('parent') != 'base_link' or mount.get('child') != 'laser':
        raise ValueError('invalid laser mount frames')
    for key, length in (('translation', 3), ('quaternion', 4)):
        values = mount.get(key)
        if (not isinstance(values, (list, tuple)) or len(values) != length
                or not all(type(v) in (int, float) and math.isfinite(v) for v in values)):
            raise ValueError('invalid laser mount ' + key)
    if abs(sum(v*v for v in mount['quaternion']) - 1.0) > 1e-6:
        raise ValueError('laser mount quaternion must have unit norm')
    return mounts


def supported_command(vx, vy, wz):
    """Never increase a requested velocity to overcome a policy dead band."""
    if not all(math.isfinite(v) for v in (vx, vy, wz)) or abs(vy) > 1e-6 or vx < 0:
        return {'vx': 0.0, 'vy': 0.0, 'wz': 0.0}
    return {'vx': min(vx, 0.55) if vx >= 0.5 and abs(wz) <= 0.7 else 0.0, 'vy': 0.0,
            'wz': max(-0.7, min(0.7, wz))}


class MotionLease:
    def __init__(self):
        self.goal = None
        self.deadline = 0.0
        self.expired = False

    def begin(self, goal_id):
        self.goal, self.deadline, self.expired = goal_id, 0.0, False

    def heartbeat(self, now, ttl=1.0):
        if self.deadline and now > self.deadline:
            self.expired = True
        if self.goal is not None and not self.expired:
            self.deadline = now + max(0.0, min(1.0, ttl))
            if ttl <= 0:
                self.expired = True

    def allowed(self, now):
        if self.goal is None or self.expired or self.deadline == 0:
            return False
        if now > self.deadline:
            self.expired = True
            return False
        return True

    def cancel(self):
        self.goal = None
        self.deadline = 0.0


def free_at(grid, x, y, radius=0.75):
    """Turn clearance plus arrival margin; Smac checks the actual footprint.

    A free endpoint alone can strand a forward-only robot beside furniture.
    The 0.57m padded footprint diagonal needs room to rotate after the 0.15m
    arrival tolerance and a grid-cell margin. This is an observation-position
    requirement, not a claim that every excluded room is physically unreachable.
    """
    resolution = grid['resolution']
    ox, oy = grid['origin'][:2]
    cx, cy = int(math.floor((x-ox)/resolution)), int(math.floor((y-oy)/resolution))
    extent = math.ceil(radius/resolution)
    for dy in range(-extent, extent+1):
        for dx in range(-extent, extent+1):
            if math.hypot(dx*resolution, dy*resolution) > radius:
                continue
            ix, iy = cx+dx, cy+dy
            if not (0 <= ix < grid['width'] and 0 <= iy < grid['height']):
                return False
            if grid['data'][iy*grid['width']+ix] < 0 or grid['data'][iy*grid['width']+ix] >= 100:
                return False
    return True


def candidates(grid, rooms=ROOMS):
    result = []
    for room_id, name, (xmin, ymin, xmax, ymax) in rooms:
        positions = []
        for ix in range(math.ceil((xmax-xmin)/0.2)):
            for iy in range(math.ceil((ymax-ymin)/0.2)):
                x, y = xmin+0.5+ix*0.2, ymin+0.5+iy*0.2
                if x > xmax-0.3 or y > ymax-0.3 or not free_at(grid, x, y):
                    continue
                positions.append({'x': x, 'y': y, 'yaw': math.atan2((ymin+ymax)/2-y, (xmin+xmax)/2-x)})
        # Spread observations across room geometry, while bounding route queries.
        positions.sort(key=lambda p: (min(p['x']-xmin, xmax-p['x'], p['y']-ymin, ymax-p['y']),
                                     (p['x']-(xmin+xmax)/2)**2 + (p['y']-(ymin+ymax)/2)**2))
        spread = []
        for point in positions:
            if all(math.hypot(point['x']-p['x'], point['y']-p['y']) >= 0.45 for p in spread):
                spread.append(point)
            if len(spread) >= 16:
                break
        result.append({'id': room_id, 'name': name,
                      'polygon': [[xmin,ymin], [xmax,ymin], [xmax,ymax], [xmin,ymax]],
                      'candidates': spread,
                      'candidate_clearance_m': 0.75,
                      'blocked_reason': None if spread else 'no observation position with turn and arrival clearance',
                      'candidate_blocked_reason': '' if spread else 'no observation position with turn and arrival clearance'})
    return {'rooms': result}


INSPECTION_MODE = 'fixed-position-yaw-v1'

def inspection_behavior(body):
    """Select only the installed finite-inspection tree; never accept a path."""
    if 'behavior_tree' in body or 'goal_checker' in body:
        raise ValueError('arbitrary behavior tree or goal checker is not permitted')
    if 'observation_mode' not in body:
        return ''
    if body['observation_mode'] != INSPECTION_MODE:
        raise ValueError('unknown observation mode')
    return '/world/deploy/patrol-nav/inspect.xml'


def nav_parameters(base, map_path, primitives, bt_path, initial_pose):
    config = deepcopy(base)
    def p(name):
        return config.setdefault(name, {}).setdefault('ros__parameters', {})
    for section in config.values():
        if isinstance(section, dict) and 'ros__parameters' in section:
            section['ros__parameters']['use_sim_time'] = True
    p('map_server')['yaml_filename'] = str(map_path)
    # Stance-foot odometry includes lateral gait/slip; keep the existing
    # noise coefficients while representing translation without spurious turns.
    p('amcl').update(base_frame_id='base_link', odom_frame_id='odom', global_frame_id='map',
        scan_topic='/scan', robot_model_type='nav2_amcl::OmniMotionModel',
        set_initial_pose=False, initial_pose=dict(zip(('x','y','yaw'), initial_pose), z=0.0),
        update_min_d=0.025, update_min_a=0.025, transform_tolerance=0.4,
        min_particles=600, max_particles=2000, max_beams=120, laser_max_range=12.0,
        laser_min_range=0.08, alpha1=0.05, alpha2=0.05, alpha3=0.05, alpha4=0.05,
        do_beamskip=False, laser_model_type='likelihood_field', sigma_hit=0.12)
    p('planner_server').update(expected_planner_frequency=1.0, planner_plugins=['GridBased'])
    p('planner_server')['GridBased'] = dict(plugin='wendy_nav::ExactGoalSmacPlannerLattice',
        tolerance=0.2, allow_unknown=False, max_iterations=1000000,
        max_on_approach_iterations=1000, max_planning_time=2.0,
        lattice_filepath=str(primitives), smooth_path=True, allow_reverse_expansion=False,
        rotation_penalty=5.0, change_penalty=0.0, non_straight_penalty=1.05,
        cost_penalty=2.0, retrospective_penalty=0.015, analytic_expansion_ratio=3.5,
        analytic_expansion_max_length=2.0, cache_obstacle_heuristic=False,
        smoother={'max_iterations':1000, 'w_smooth':0.3, 'w_data':0.2, 'tolerance':1e-10})
    p('controller_server').update(controller_frequency=15.0, odom_topic='/odom',
        min_x_velocity_threshold=0.001, min_y_velocity_threshold=0.001,
        min_theta_velocity_threshold=0.001, failure_tolerance=0.5,
        controller_plugins=['FollowPath'], goal_checker_plugins=['general_goal_checker','inspection_goal_checker'],
        progress_checker_plugin='progress_checker')
    p('controller_server')['progress_checker'] = dict(plugin='nav2_controller::SimpleProgressChecker',
        required_movement_radius=0.08, movement_time_allowance=15.0)
    p('controller_server')['general_goal_checker'] = dict(plugin='nav2_controller::SimpleGoalChecker',
        xy_goal_tolerance=0.15, yaw_goal_tolerance=math.pi, stateful=True)
    p('controller_server')['inspection_goal_checker'] = dict(plugin='nav2_controller::SimpleGoalChecker',
        xy_goal_tolerance=0.15, yaw_goal_tolerance=0.10, stateful=True)
    p('controller_server')['FollowPath'] = dict(plugin='nav2_regulated_pure_pursuit_controller::RegulatedPurePursuitController',
        desired_linear_vel=0.55, lookahead_dist=0.4, min_lookahead_dist=0.3, max_lookahead_dist=0.5,
        lookahead_time=1.0, use_velocity_scaled_lookahead_dist=False,
        rotate_to_heading_angular_vel=0.6, use_rotate_to_heading=True,
        rotate_to_heading_min_angle=0.2, max_angular_accel=12.0, allow_reversing=False,
        use_regulated_linear_velocity_scaling=True, regulated_linear_scaling_min_radius=1.0,
        regulated_linear_scaling_min_speed=0.55, use_cost_regulated_linear_velocity_scaling=False,
        min_approach_linear_velocity=0.55, approach_velocity_scaling_dist=0.35,
        use_collision_detection=True, max_allowed_time_to_collision_up_to_carrot=1.2,
        transform_tolerance=0.3)
    footprint = '[[0.45,0.29],[0.45,-0.29],[-0.45,-0.29],[-0.45,0.29]]'
    for name, frame in [('local_costmap','odom'), ('global_costmap','map')]:
        costmap = config[name][name]['ros__parameters']
        costmap.update(use_sim_time=True, global_frame=frame, robot_base_frame='base_link',
            resolution=0.05, footprint=footprint, footprint_padding=0.025,
            transform_tolerance=0.4, update_frequency=5.0, publish_frequency=2.0,
            track_unknown_space=True, always_send_full_costmap=True,
            plugins=['static_layer','obstacle_layer','inflation_layer'] if name=='global_costmap'
            else ['obstacle_layer','inflation_layer'])
        costmap.pop('robot_radius', None)
        costmap['filters']=['patrol_keepout']
        costmap['patrol_keepout']=dict(plugin='nav2_costmap_2d::KeepoutFilter', enabled=True, filter_info_topic='/patrol_policy/'+('global' if name=='global_costmap' else 'local')+'/info', transform_tolerance=0.4)
        costmap['obstacle_layer'] = dict(plugin='nav2_costmap_2d::ObstacleLayer', enabled=True,
            observation_sources='scan', scan=dict(topic='/scan', data_type='LaserScan',
                clearing=True, marking=True, inf_is_valid=True, expected_update_rate=0.5,
                obstacle_max_range=11.8, raytrace_max_range=12.0,
                obstacle_min_range=0.08, raytrace_min_range=0.08, max_obstacle_height=2.0))
        costmap['inflation_layer'] = dict(plugin='nav2_costmap_2d::InflationLayer',
            inflation_radius=0.7, cost_scaling_factor=5.0)
    config['local_costmap']['local_costmap']['ros__parameters'].update(width=5, height=5, rolling_window=True)
    p('bt_navigator').update(global_frame='map', robot_base_frame='base_link', odom_topic='/odom',
        default_nav_to_pose_bt_xml=str(bt_path), default_nav_through_poses_bt_xml=str(Path(bt_path).with_name('navigate_through.xml')), default_server_timeout=1000,
        bt_loop_duration=20, wait_for_service_timeout=1000)
    return config


def write_map(grid, directory):
    """Generated map images are runtime artifacts, never repository assets."""
    directory = Path(directory)
    directory.mkdir(parents=True, exist_ok=True)
    width, height = grid['width'], grid['height']
    pixels = bytes(205 if value < 0 else 0 if value >= 65 else 254
                   for y in range(height-1,-1,-1) for value in grid['data'][y*width:(y+1)*width])
    (directory/'office.pgm').write_bytes(f'P5\n{width} {height}\n255\n'.encode()+pixels)
    path = directory/'office.yaml'
    path.write_text(f'image: office.pgm\nresolution: {grid["resolution"]}\norigin: {grid["origin"]}\nnegate: 0\noccupied_thresh: 0.65\nfree_thresh: 0.2\n')
    return path


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--physics', default='http://physics:8091')
    parser.add_argument('--port', type=int, default=8092)
    parser.add_argument('--runtime', default='/tmp/patrol-nav')
    parser.add_argument('--scene', default='examples/collaborative-office/scene.json')
    parser.add_argument('--frame-id', default='office')
    parser.add_argument('--frame-revision', default='1')
    parser.add_argument('--initial-pose', default='-5.8,0.15,0.08')
    args = parser.parse_args()
    initial_pose = [float(x) for x in args.initial_pose.split(',')]
    if len(initial_pose)!=3 or not all(math.isfinite(v) for v in initial_pose):
        parser.error('initial pose must be three finite map coordinates')
    import yaml
    from ament_index_python.packages import get_package_share_directory
    deadline = time.monotonic()+120
    while True:
        try:
            grid = request(args.physics+'/map', timeout=10)
            break
        except Exception:
            if time.monotonic()>deadline:
                raise
            time.sleep(0.5)
    runtime = Path(args.runtime)
    map_path = write_map(grid, runtime)
    share = Path(get_package_share_directory('nav2_bringup'))
    primitives = Path(get_package_share_directory('nav2_smac_planner'))/'sample_primitives/5cm_resolution/1m_turning_radius/diff/output.json'
    if not primitives.is_file():
        raise RuntimeError('Installed Humble Nav2 lacks differential State Lattice primitives')
    root = Path(__file__).resolve().parents[1]
    params = nav_parameters(yaml.safe_load((share/'params/nav2_params.yaml').read_text()),
        map_path, primitives, root/'deploy/patrol-nav/navigate.xml', initial_pose)
    params_path = runtime/'params.yaml'
    params_path.write_text(yaml.safe_dump(params))
    os.environ['PATROL_NAV_PARAMS'] = str(params_path)
    stack = subprocess.Popen(['ros2','launch',str(root/'deploy/patrol-nav/stack.launch.py')])
    try:
        scene = json.loads(Path(args.scene).read_text())
        rooms = [(r['id'], r['label'], (r['center'][0]-r['size'][0]/2, r['center'][1]-r['size'][1]/2,
                  r['center'][0]+r['size'][0]/2, r['center'][1]+r['size'][1]/2)) for r in scene['presentation']['spaces']]
        run_bridge(args, grid, rooms)
    finally:
        try: request(args.physics+'/command', supported_command(0,0,0))
        except Exception: pass
        stack.terminate()
        stack.wait(timeout=10)


def run_bridge(args, grid, rooms):
    import rclpy
    from rclpy.node import Node
    from rclpy.clock import Clock as ROSClock, ClockType
    from rclpy.action import ActionClient
    from rclpy.callback_groups import ReentrantCallbackGroup
    from rclpy.executors import MultiThreadedExecutor
    from rclpy.qos import qos_profile_sensor_data, QoSProfile, DurabilityPolicy
    from geometry_msgs.msg import Twist, PoseStamped, PoseWithCovarianceStamped, TransformStamped
    from sensor_msgs.msg import LaserScan
    from nav_msgs.msg import Odometry, OccupancyGrid, Path as NavPath
    from nav2_msgs.action import NavigateToPose, ComputePathToPose
    from nav2_msgs.msg import CostmapFilterInfo
    from rosgraph_msgs.msg import Clock
    from std_srvs.srv import Empty
    from lifecycle_msgs.srv import GetState
    from tf2_ros import TransformBroadcaster, StaticTransformBroadcaster, Buffer, TransformListener
    from builtin_interfaces.msg import Time
    rclpy.init()

    def stamp(t):
        seconds = math.floor(t)
        return Time(sec=seconds, nanosec=int((t-seconds)*1e9))
    def quaternion(yaw, q):
        q.z, q.w = math.sin(yaw/2), math.cos(yaw/2)
    def yaw(q):
        return math.atan2(2*(q.w*q.z+q.x*q.y), 1-2*(q.y*q.y+q.z*q.z))
    def wait(future, timeout=8):
        event = threading.Event()
        future.add_done_callback(lambda _: event.set())
        if not event.wait(timeout):
            raise TimeoutError('Nav2 action did not respond')
        return future.result()

    class Bridge(Node):
        def __init__(self):
            super().__init__('patrol_bridge', parameter_overrides=[rclpy.parameter.Parameter('use_sim_time', value=True)])
            self.group = ReentrantCallbackGroup()
            self.lock = threading.RLock()
            self.lease = MotionLease()
            self.policy = None
            self.policy_frozen = False
            self.policy_mission = None
            self.policy_lock = threading.Lock()
            self.policy_time = 0.0
            self.policy_ready = True
            self.policy_mask = restrictions.mask(grid,None)
            self.policy_frame = dict(id=args.frame_id,revision=args.frame_revision)
            qos = QoSProfile(depth=1,durability=DurabilityPolicy.TRANSIENT_LOCAL)
            self.mask_pubs={kind:self.create_publisher(OccupancyGrid,'/patrol_policy/'+kind+'/mask',qos) for kind in ('global','local')}
            self.info_pubs={kind:self.create_publisher(CostmapFilterInfo,'/patrol_policy/'+kind+'/info',qos) for kind in ('global','local')}
            self.sensor_time = self.pose_time = self.command_time = 0.0
            self.last_stamp = -1.0
            self.scan_valid = False
            self.sensors_ready = False
            self.raw_command = (0.,0.,0.)
            self.applied_command = (0.,0.,0.)
            self.passive_updates = localization.PassiveUpdates()
            self.pose = None
            self.covariance = []
            self.goal = {'id':None, 'state':'idle', 'distance_remaining':None}
            self.goal_handle = None
            self.route = []
            self.speed = 0.0
            self.angular_speed = 0.0
            self.seed_sent = False
            self.seed_pending = False
            self.costmap_time = 0.0
            self.grid = grid
            self.scan_points = []
            self.last_error = None
            self.clock_pub = self.create_publisher(Clock, '/clock', 10)
            self.scan_pub = self.create_publisher(LaserScan, '/scan', qos_profile_sensor_data)
            self.odom_pub = self.create_publisher(Odometry, '/odom', 10)
            self.tf = TransformBroadcaster(self)
            self.static_tf = StaticTransformBroadcaster(self)
            self.tf_buffer = Buffer()
            self.tf_listener = TransformListener(self.tf_buffer, self)
            self.nav = ActionClient(self, NavigateToPose, '/navigate_to_pose', callback_group=self.group)
            self.planner = ActionClient(self, ComputePathToPose, '/compute_path_to_pose', callback_group=self.group)
            self.initial_pub = self.create_publisher(PoseWithCovarianceStamped, '/initialpose', 1)
            self.amcl_state = self.create_client(GetState, '/amcl/get_state', callback_group=self.group)
            self.no_motion = self.create_client(Empty, '/request_nomotion_update', callback_group=self.group)
            self.create_subscription(Twist, '/nav_cmd_vel', self.command, 10, callback_group=self.group)
            self.create_subscription(PoseWithCovarianceStamped, '/amcl_pose', self.localized,
                QoSProfile(depth=1, durability=DurabilityPolicy.TRANSIENT_LOCAL), callback_group=self.group)
            self.create_subscription(NavPath, '/plan', self.plan, 10, callback_group=self.group)
            self.create_subscription(OccupancyGrid, '/global_costmap/costmap', self.costmap,
                QoSProfile(depth=1, durability=DurabilityPolicy.TRANSIENT_LOCAL), callback_group=self.group)
            wall_clock = ROSClock(clock_type=ClockType.STEADY_TIME)
            self.create_timer(0.05, self.poll, callback_group=self.group, clock=wall_clock)
            self.create_timer(0.05, self.drive, callback_group=self.group, clock=wall_clock)
            # Poll with margin below the strict 3s pose age; the admission gate
            # still requires >=2s, advancing scans and measured/applied stop.
            self.create_timer(0.1, self.update_stationary, callback_group=self.group, clock=wall_clock)
            self.poll_lock = threading.Lock()
            self.drive_dispatch = localization.NonOverlappingCall()
            self.publish_policy()

        def publish_policy(self):
            self.policy_time=time.monotonic();self.policy_ready=False
            for kind in ('global','local'):
                msg=OccupancyGrid();msg.header.frame_id='map';msg.header.stamp=self.get_clock().now().to_msg()
                msg.info.resolution=grid['resolution'];msg.info.width=grid['width'];msg.info.height=grid['height']
                msg.info.origin.position.x,msg.info.origin.position.y=grid['origin'][:2];msg.info.origin.orientation.w=1.
                # Lattice planning uses a forbidden-centre mask. The local
                # controller checks its actual footprint, so applying the same
                # dilation there would count the robot footprint twice.
                msg.data=self.policy_mask if kind=='global' else restrictions.mask(grid,self.policy,robot_centres=False)
                info=CostmapFilterInfo();info.header=msg.header;info.type=0;info.filter_mask_topic='/patrol_policy/'+kind+'/mask';info.base=0.;info.multiplier=1.
                self.mask_pubs[kind].publish(msg);self.info_pubs[kind].publish(info)

        def apply_policy(self, policy):
            with self.lock:
                if self.policy_frozen or self.lease.goal is not None: raise ValueError('Restricted areas are frozen during a mission')
                if not self.ready(): raise ValueError('Fresh localization and sensors are required to review restricted areas')
                pose=self.status()['pose']
                if not pose or not restrictions.clear(policy,pose['x'],pose['y'],restrictions.uncertainty_radius(self.covariance)): raise ValueError('Restricted area overlaps current Go2 footprint and tracking allowance')
                self.policy=policy;self.policy_mask=restrictions.mask(grid,policy);self.publish_policy()
            deadline=time.monotonic()+8
            while time.monotonic()<deadline:
                with self.lock:
                    if self.policy_ready: return
                time.sleep(.05)
            raise ValueError('Navigation has not acknowledged the restricted-area mask')

        def policy_preview(self, body):
            with self.policy_lock:
                policy=restrictions.validate(body['policy'],self.policy_frame,require_digest=False)
                # Baseline planning is read-only and only available before any
                # mission is frozen. Neither preview creates a goal or a lease.
                self.apply_policy(None)
                baseline=self.compute(body['target'])
                self.apply_policy(policy)
                planned=self.compute(dict(body['target'],policy_digest=policy['sha256']))
                return_target=body.get('return_target') or self.status()['pose']
                return_route=self.compute(dict(return_target,start=body['target'],policy_digest=policy['sha256']))
                return dict(policy=policy,baseline=baseline,restricted=planned,return_route=return_route,clearance_m=restrictions.CLEARANCE)

        def commit_policy(self, body):
            policy=restrictions.validate(body['policy'],self.policy_frame)
            mission=body.get('mission_id')
            if not isinstance(mission,str) or not mission or len(mission)>256: raise ValueError('Mission identity required')
            with self.policy_lock:
                with self.lock:
                    if self.policy_frozen:
                        if self.policy!=policy or self.policy_mission!=mission: raise ValueError('Policy already frozen to another mission')
                        return dict(accepted=True,policy_digest=policy['sha256'],policy_frozen=True)
                self.apply_policy(policy)
                with self.lock: self.policy_frozen=True;self.policy_mission=mission
                return dict(accepted=True,policy_digest=policy['sha256'],policy_frozen=True)

        def policy_matches(self, body):
            return restrictions.matches(self.policy,body.get('policy_digest'))

        def command(self, msg):
            with self.lock:
                self.raw_command = (msg.linear.x, msg.linear.y, msg.angular.z)
                self.command_time = time.monotonic()

        def localized(self, msg):
            with self.lock:
                self.pose_time = time.monotonic()
                self.pose = dict(x=msg.pose.pose.position.x, y=msg.pose.pose.position.y, yaw=yaw(msg.pose.pose.orientation))
                self.covariance = list(msg.pose.covariance)

        def plan(self, msg):
            with self.lock:
                self.route = [[p.pose.position.x,p.pose.position.y,yaw(p.pose.orientation)] for p in msg.poses]

        def costmap(self, msg):
            with self.lock:
                self.costmap_time = time.monotonic()
                self.grid = dict(resolution=msg.info.resolution, width=msg.info.width, height=msg.info.height,
                    origin=[msg.info.origin.position.x,msg.info.origin.position.y,0.0], data=list(msg.data))
                # Receipt from the actual global costmap, not just publication.
                if (self.costmap_time>self.policy_time+.2 and len(msg.data)==len(self.policy_mask)
                    and msg.info.width==grid['width'] and msg.info.height==grid['height']
                    and abs(msg.info.resolution-grid['resolution'])<1e-6
                    and abs(msg.info.origin.position.x-grid['origin'][0])<1e-6
                    and abs(msg.info.origin.position.y-grid['origin'][1])<1e-6):
                    self.policy_ready=all(v!=100 or msg.data[i]>=100 for i,v in enumerate(self.policy_mask))

        def update_stationary(self):
            if not self.seed_sent and not self.seed_pending and self.amcl_state.service_is_ready():
                self.seed_pending = True
                def seed(future):
                    self.seed_pending = False
                    if future.result().current_state.id != 3 or self.seed_sent:
                        return
                    msg = PoseWithCovarianceStamped()
                    msg.header.frame_id = 'map'; msg.header.stamp = self.get_clock().now().to_msg()
                    values = [float(x) for x in args.initial_pose.split(',')]
                    msg.pose.pose.position.x, msg.pose.pose.position.y = values[:2]
                    quaternion(values[2], msg.pose.pose.orientation)
                    msg.pose.covariance[0] = msg.pose.covariance[7] = 0.25
                    msg.pose.covariance[35] = 0.0685
                    self.initial_pub.publish(msg)
                    self.seed_sent = True
                self.amcl_state.call_async(GetState.Request()).add_done_callback(seed)
            with self.lock:
                if self.seed_sent and self.no_motion.service_is_ready():
                    token = self.passive_updates.begin(
                        now=time.monotonic(), stamp=self.last_stamp, sensor_time=self.sensor_time,
                        sensors_ready=self.sensors_ready, scan_valid=self.scan_valid,
                        speed=self.speed, angular_speed=self.angular_speed,
                        command=self.applied_command,
                        scope=(self.policy_frame['id'], self.policy_frame['revision'], self.goal['id']))
                    if token is not None:
                        def completed(future):
                            # The service only schedules a scan update. Its completion
                            # cannot refresh pose age, clear a hold or renew a lease.
                            with self.lock:
                                self.passive_updates.finish(token)
                        try:
                            self.no_motion.call_async(Empty.Request()).add_done_callback(completed)
                        except Exception:
                            self.passive_updates.finish(token)

        def localization_health(self):
            return localization.health(now=time.monotonic(), sensor_time=self.sensor_time,
                pose_time=self.pose_time, costmap_time=self.costmap_time,
                policy_ready=self.policy_ready, sensors_ready=self.sensors_ready,
                scan_valid=self.scan_valid, pose=self.pose, covariance=self.covariance,
                navigation_ready=self.nav.server_is_ready(), planner_ready=self.planner.server_is_ready(),
                passive=self.passive_updates)

        def ready(self):
            return self.localization_health()['ready']

        def poll(self):
            if not self.poll_lock.acquire(False): return
            try:
                data = request(args.physics+'/sensors')
                t = float(data['stamp'])
                if not math.isfinite(t) or t<=self.last_stamp: return
                scan_data, odom_data = data['scan'], data['odom']
                if not all(math.isfinite(float(odom_data[k])) for k in ('x','y','yaw','vx','vy','wz')):
                    raise ValueError('invalid odometry')
                mounts = validated_sensor_transforms(data.get('transforms'), scan_data.get('frame_id', 'laser'))
                ranges = scan_data['ranges']
                valid = len(ranges)>=180 and sum(v is not None and math.isfinite(v) and v>=scan_data['range_min'] for v in ranges)>=len(ranges)*0.8
                # Null encodes no return; positive infinity is ROS LaserScan clearing semantics.
                scan = LaserScan()
                scan.header.stamp, scan.header.frame_id = stamp(t), scan_data.get('frame_id','laser')
                scan.angle_min = float(scan_data['angle_min'])
                scan.angle_increment = float(scan_data['angle_increment'])
                scan.angle_max = scan.angle_min+(len(ranges)-1)*scan.angle_increment
                scan.range_min, scan.range_max = float(scan_data['range_min']), float(scan_data['range_max'])
                scan.ranges = [float('inf') if v is None else float(v) for v in ranges]
                scan.scan_time = float(scan_data.get('scan_time',0.1))
                clock = Clock(); clock.clock = stamp(t); self.clock_pub.publish(clock)
                odom = Odometry()
                odom.header.stamp, odom.header.frame_id, odom.child_frame_id = stamp(t), 'odom', 'base_link'
                odom.pose.pose.position.x, odom.pose.pose.position.y = float(odom_data['x']), float(odom_data['y'])
                quaternion(odom_data['yaw'], odom.pose.pose.orientation)
                odom.twist.twist.linear.x, odom.twist.twist.linear.y = float(odom_data['vx']),float(odom_data['vy'])
                odom.twist.twist.angular.z = float(odom_data['wz'])
                odom.pose.covariance[0] = odom.pose.covariance[7] = 0.0025
                odom.pose.covariance[35] = 0.0025
                self.odom_pub.publish(odom)
                transform = TransformStamped(); transform.header = odom.header; transform.child_frame_id = 'base_link'
                transform.transform.translation.x, transform.transform.translation.y = odom.pose.pose.position.x, odom.pose.pose.position.y
                transform.transform.rotation = odom.pose.pose.orientation
                self.tf.sendTransform(transform)
                transforms = []
                for mount in mounts:
                    tf = TransformStamped(); tf.header.stamp=stamp(t); tf.header.frame_id=mount['parent']; tf.child_frame_id=mount['child']
                    tf.transform.translation.x,tf.transform.translation.y,tf.transform.translation.z = map(float,mount['translation'])
                    tf.transform.rotation.x,tf.transform.rotation.y,tf.transform.rotation.z,tf.transform.rotation.w = map(float,mount['quaternion'])
                    transforms.append(tf)
                self.static_tf.sendTransform(transforms)
                self.scan_pub.publish(scan)
                with self.lock:
                    self.sensor_time, self.last_stamp, self.scan_valid = time.monotonic(), t, valid
                    self.sensors_ready = bool(data.get('ready'))
                    self.speed = math.hypot(odom_data['vx'],odom_data['vy'])
                    self.angular_speed = abs(odom_data['wz'])
                    self.last_error = None
                    self.scan_points = [[r*math.cos(scan.angle_min+i*scan.angle_increment),r*math.sin(scan.angle_min+i*scan.angle_increment)]
                        for i,r in enumerate(ranges) if r is not None and math.isfinite(r) and scan.range_min<=r<scan.range_max]
            except Exception as exc:
                with self.lock:
                    self.last_error = str(exc)
                    self.scan_valid = False
            finally:
                self.poll_lock.release()

        def drive(self):
            # Reentrant ROS timers can overlap when the HTTP request is slow.
            # Serialize dispatch and acknowledgment so an older stopped command
            # cannot overwrite the recorded state of a newer moving command.
            self.drive_dispatch.run(self.drive_once)

        def drive_once(self):
            cancel = None
            with self.lock:
                now = time.monotonic()
                allowed = self.lease.allowed(now)
                if self.lease.expired and self.goal['state'] in ('accepted','navigating'):
                    self.goal['state']='executor_lost'; cancel=self.goal_handle; self.goal_handle=None
                if not self.ready() and self.goal['state'] in ('accepted','navigating'):
                    self.goal['state']='sensor_hold'; cancel=self.goal_handle; self.goal_handle=None; self.lease.cancel()
                cmd = supported_command(*self.raw_command) if allowed and self.ready() and now-self.command_time<0.35 else supported_command(0,0,0)
                if self.policy and allowed:
                    pose=self.status()['pose']
                    trajectory=[] if pose is None else [[pose['x']+cmd['vx']*.6*i/12*math.cos(pose['yaw']),pose['y']+cmd['vx']*.6*i/12*math.sin(pose['yaw'])] for i in range(13)]
                    if not self.policy_frozen or not trajectory or not restrictions.path_clear(self.policy,trajectory,restrictions.uncertainty_radius(self.covariance)):
                        self.goal['state']='policy_hold';cancel=self.goal_handle;self.goal_handle=None;self.lease.cancel();cmd=supported_command(0,0,0)
                # Mark a pending nonzero dispatch before releasing the lock so a
                # stationary update cannot race the start of movement.
                if any(cmd.values()): self.applied_command = (cmd['vx'],cmd['vy'],cmd['wz'])
            if cancel is not None: cancel.cancel_goal_async()
            try:
                request(args.physics+'/command',cmd)
                with self.lock: self.applied_command = (cmd['vx'],cmd['vy'],cmd['wz'])
            except Exception as exc:
                with self.lock: self.last_error=str(exc)

        def status(self):
            with self.lock:
                pose = self.pose
                try:
                    tf=self.tf_buffer.lookup_transform('map','base_link',rclpy.time.Time())
                    pose=dict(x=tf.transform.translation.x,y=tf.transform.translation.y,yaw=yaw(tf.transform.rotation))
                except Exception: pass
                health = self.localization_health()
                pose, covariance = localization.status_measurements(pose, self.covariance)
                return dict(schema='wendy.nav.patrol.adapter.v1', ready=health['ready'], localization_health=health, pose=pose, inspection_mode=INSPECTION_MODE,
                    covariance=covariance, route=self.route, goal=dict(self.goal), speed=self.speed,
                    speed_mps=self.speed, angular_speed_rps=self.angular_speed,
                    sensor_age_s=round(time.monotonic()-self.sensor_time,3), error=self.last_error,
                    scan_points=self.scan_points, stamp=self.last_stamp, frame='map', provenance='simulated',
                    policy_digest=self.policy['sha256'] if self.policy else '',policy_frozen=self.policy_frozen,restricted_areas=self.policy,
                    localization='AMCL', planner='SmacPlannerLattice', controller='RegulatedPurePursuit')

        def target(self, body):
            values = [float(body[k]) for k in ('x','y','yaw')]
            if not all(math.isfinite(v) for v in values): raise ValueError('finite goal required')
            msg = PoseStamped(); msg.header.frame_id='map'; msg.header.stamp=self.get_clock().now().to_msg()
            msg.pose.position.x,msg.pose.position.y=values[:2]; quaternion(values[2],msg.pose.orientation)
            return msg

        def compute(self, body):
            if not self.ready(): return dict(reachable=False, reason='not_ready', route=[])
            if not self.policy_matches(body) or not restrictions.clear(self.policy,float(body['x']),float(body['y'])): return dict(reachable=False,reason='restricted_destination_or_policy_mismatch',route=[])
            goal=ComputePathToPose.Goal(); goal.goal=self.target(body); goal.planner_id='GridBased'; goal.use_start=False
            if body.get('start') is not None:
                start=body['start']
                if not restrictions.clear(self.policy,float(start['x']),float(start['y'])): return dict(reachable=False,reason='restricted_start',route=[])
                goal.start=self.target(start);goal.use_start=True
            handle=wait(self.planner.send_goal_async(goal))
            if not handle.accepted: return dict(reachable=False,reason='rejected',route=[])
            result=wait(handle.get_result_async())
            route=[[p.pose.position.x,p.pose.position.y,yaw(p.pose.orientation)] for p in result.result.path.poses]
            if result.status != 4 or not route:
                return dict(reachable=False, route=[], reason='no_route')
            if not restrictions.path_clear(self.policy,route): return dict(reachable=False,route=[],reason='restricted_route_clearance')
            # A passive observation must not strand this forward-only walker in
            # a pocket where it cannot turn or navigate back out.
            back=ComputePathToPose.Goal(); back.start=goal.goal
            back.goal=goal.start if goal.use_start else self.target(self.status()['pose']); back.use_start=True; back.planner_id='GridBased'
            return_handle=wait(self.planner.send_goal_async(back))
            if not return_handle.accepted:
                return dict(reachable=False, route=[], reason='no_egress')
            return_result=wait(return_handle.get_result_async())
            if return_result.status != 4 or not return_result.result.path.poses or not restrictions.path_clear(self.policy,[[p.pose.position.x,p.pose.position.y] for p in return_result.result.path.poses]):
                return dict(reachable=False, route=[], reason='no_egress')
            return dict(reachable=True, route=route,
                length=sum(math.hypot(b[0]-a[0],b[1]-a[1]) for a,b in zip(route,route[1:])))

        def cancel(self):
            with self.lock:
                handle=self.goal_handle; self.goal_handle=None; self.lease.cancel(); self.goal['state']='canceled'
                self.raw_command=(0.,0.,0.)
            if handle is not None: wait(handle.cancel_goal_async())
            request(args.physics+'/command',supported_command(0,0,0))
            return dict(accepted=True)

        def navigate(self, body):
            behavior=inspection_behavior(body)
            if not self.ready(): return dict(accepted=False,reason='not_ready')
            if not self.policy_matches(body) or self.policy and not self.policy_frozen or not restrictions.clear(self.policy,float(body['x']),float(body['y'])): return dict(accepted=False,reason='restricted_destination_or_policy_mismatch')
            self.cancel()
            goal=NavigateToPose.Goal(); goal.pose=self.target(body); goal.behavior_tree=behavior
            goal_id=str(body['id'])
            with self.lock:
                self.lease.begin(goal_id); self.goal=dict(id=goal_id,state='accepted',distance_remaining=None)
            def feedback(message):
                with self.lock:
                    if self.goal['id']==goal_id and self.goal['state'] in ('accepted','navigating'):
                        self.goal.update(state='navigating',distance_remaining=message.feedback.distance_remaining)
            handle=wait(self.nav.send_goal_async(goal,feedback_callback=feedback))
            if not handle.accepted:
                with self.lock: self.lease.cancel(); self.goal['state']='rejected'
                return dict(accepted=False,reason='rejected')
            with self.lock: self.goal_handle=handle
            def finished(future):
                with self.lock:
                    if self.goal['id']!=goal_id or self.goal_handle is not handle: return
                    self.goal['state']={4:'succeeded',5:'canceled',6:'failed'}.get(future.result().status,'failed')
                    self.lease.cancel(); self.goal_handle=None; self.raw_command=(0.,0.,0.)
            handle.get_result_async().add_done_callback(finished)
            return dict(accepted=True,id=goal_id)

    bridge=Bridge()
    class Handler(BaseHTTPRequestHandler):
        def log_message(self,*_): pass
        def respond(self, value, code=200):
            data=json.dumps(value,allow_nan=False).encode(); self.send_response(code)
            self.send_header('Content-Type','application/json'); self.send_header('Content-Length',str(len(data))); self.end_headers(); self.wfile.write(data)
        def do_GET(self):
            try:
                if self.path=='/status': return self.respond(bridge.status())
                if self.path=='/policy/context': return self.respond(dict(frame=bridge.policy_frame,grid=dict(resolution=grid['resolution']*4,width=math.ceil(grid['width']/4),height=math.ceil(grid['height']/4),origin=grid['origin'],data=[grid['data'][y*grid['width']+x] for y in range(0,grid['height'],4) for x in range(0,grid['width'],4)]),clearance_m=restrictions.CLEARANCE))
                if self.path=='/candidates':
                    with bridge.lock: current=bridge.grid
                    return self.respond(candidates(current, rooms))
                self.respond({'error':'not_found'},404)
            except Exception as exc: self.respond({'error':str(exc)},500)
        def do_POST(self):
            try:
                length=int(self.headers.get('Content-Length','0'))
                if length>16384: raise ValueError('request too large')
                body=json.loads(self.rfile.read(length) or b'{}')
                if self.path=='/heartbeat':
                    expiry = body.get('expires_at_unix_ms')
                    if expiry is not None and not math.isfinite(float(expiry)):
                        raise ValueError('expiry must be finite')
                    ttl = 1.0 if expiry is None else min(1.0, float(expiry)/1000-time.time())
                    with bridge.lock:
                        bridge.lease.heartbeat(time.monotonic(), ttl)
                        accepted = not bridge.lease.expired and bridge.lease.goal is not None
                    return self.respond({'accepted':accepted})
                if self.path=='/policy/reset':
                    with bridge.policy_lock: bridge.apply_policy(None)
                    return self.respond({'accepted':True})
                if self.path=='/policy/preview': return self.respond(bridge.policy_preview(body))
                if self.path=='/policy/commit': return self.respond(bridge.commit_policy(body))
                if self.path=='/goal': return self.respond(bridge.navigate(body))
                if self.path=='/route': return self.respond(bridge.compute(body))
                if self.path=='/cancel': return self.respond(bridge.cancel())
                self.respond({'error':'not_found'},404)
            except (KeyError,ValueError) as exc: self.respond({'error':str(exc)},400)
            except Exception as exc: self.respond({'error':str(exc)},503)
    server=ThreadingHTTPServer(('0.0.0.0',args.port),Handler)
    threading.Thread(target=server.serve_forever,daemon=True).start()
    executor=MultiThreadedExecutor(num_threads=4); executor.add_node(bridge)
    try: executor.spin()
    finally:
        server.shutdown(); bridge.destroy_node(); rclpy.shutdown()


if __name__=='__main__':
    main()
