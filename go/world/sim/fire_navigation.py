"""Prepare one simulated Go2 inspection from a verified camera fire cue.

This adapter reads Nav2 candidates/routes and creates the existing finite-tour
schedule. It never sends a goal, heartbeat or velocity command and supplies no
authority. The normal world-patrol simulation fixture signs and admits the
immutable schedule when the caller explicitly launches the demonstration.
"""
from copy import deepcopy
import hashlib
import io
import math
from pathlib import Path
import re
import time


DETECTION_SCHEMA = 'wendy.xiao-fire-detection.v1'
DETECTOR = 'synthetic-flame-cue-v1'
TOUR_METHOD = 'inspect-selected-rooms-once-and-return-v1'
INSPECTION_MODE = 'fixed-position-yaw-v1'
SHA256 = re.compile(r'^[0-9a-f]{64}$')


class NoObservationRoute(ValueError):
    """Read-only preparation failed; retain bounded route rejection evidence."""

    def __init__(self, attempts):
        super().__init__('no reachable commissioned stand-off observation point')
        self.attempts = deepcopy(attempts)


def finite(value):
    return type(value) in (int, float) and math.isfinite(value)


def point(value, dimensions):
    if not isinstance(value, (list, tuple)) or len(value) != dimensions or not all(finite(v) for v in value):
        raise ValueError('finite position required')
    return [float(v) for v in value]


def inside(polygon, location):
    if not isinstance(polygon, list) or not 3 <= len(polygon) <= 64:
        raise ValueError('bounded commissioned room polygon required')
    vertices = [point(p, 2) for p in polygon]
    x, y = location
    contained = False
    for a, b in zip(vertices, vertices[1:] + vertices[:1]):
        if (a[1] > y) != (b[1] > y) and x < (b[0]-a[0])*(y-a[1])/(b[1]-a[1])+a[0]:
            contained = not contained
    return contained


def verified_detection(detection, jpeg_path, scope, calibration_sha256, now):
    if scope.get('simulation') is not True or not scope.get('run_id') or not isinstance(scope.get('frame'), dict):
        raise ValueError('actual simulated physics run and frame required')
    if (detection.get('schema') != DETECTION_SCHEMA or detection.get('model') != DETECTOR
            or detection.get('origin') != 'inferred' or detection.get('detected') is not True
            or detection.get('candidate') is not True
            or detection.get('score_kind') != 'fixture_rule_not_probability'):
        raise ValueError('confirmed simulated fire-cue inference required')
    if detection.get('simulation_run') != scope['run_id'] or detection.get('frame') != scope['frame']:
        raise ValueError('camera inference belongs to another run or coordinate frame')
    if not SHA256.fullmatch(calibration_sha256) or detection.get('calibration_sha256') != calibration_sha256:
        raise ValueError('camera calibration differs from the commissioned projection')
    deadline = detection.get('expires_monotonic_s')
    if not finite(now) or not finite(deadline) or now >= deadline:
        raise ValueError('original camera evidence deadline expired')
    source = detection.get('source') or {}
    inputs = detection.get('confirming_inputs')
    if (not isinstance(inputs, list) or len(inputs) != 3
            or detection.get('confirmation_frames') != 3 or inputs[-1] != source):
        raise ValueError('three original confirming frames and their latest source required')
    previous = None
    deadlines = []
    for row in inputs:
        if (row.get('origin') != 'simulated' or row.get('simulation_run') != scope['run_id']
                or row.get('timestamp_clock') != 'firmware-boot-microseconds'
                or not isinstance(row.get('session_id'), str) or not row['session_id']
                or not SHA256.fullmatch(row.get('sha256', ''))
                or type(row.get('channel_id')) is not int or row['channel_id'] != 0
                or type(row.get('frame_seq')) is not int or not 0 <= row['frame_seq'] <= 0xffffffff
                or type(row.get('timestamp_us')) is not int or row['timestamp_us'] <= 0):
            raise ValueError('original simulated SensorLink frame metadata required')
        received = row.get('received_monotonic_s')
        original_deadline = row.get('expires_monotonic_s')
        if (not finite(received) or received > now or not finite(original_deadline)
                or original_deadline <= received or now >= original_deadline):
            raise ValueError('invalid original camera receipt timing')
        deadlines.append(original_deadline)
        if previous:
            delta = (row['frame_seq']-previous['frame_seq']) & 0xffffffff
            if (row['session_id'] != previous['session_id'] or not 0 < delta < 0x80000000
                    or row['timestamp_us'] <= previous['timestamp_us']):
                raise ValueError('confirming camera frames must advance in one firmware session')
        previous = row
    if deadline != min(deadlines):
        raise ValueError('inference extended an original camera evidence deadline')
    path = Path(jpeg_path)
    if path.is_symlink() or not path.is_file() or not 4 <= path.stat().st_size <= 8 << 20:
        raise ValueError('bounded original JPEG file required')
    raw = path.read_bytes()
    if hashlib.sha256(raw).hexdigest() != source['sha256']:
        raise ValueError('received JPEG differs from the inference evidence')
    from PIL import Image
    with Image.open(io.BytesIO(raw)) as image:
        if image.format != 'JPEG' or image.size != (source.get('width'), source.get('height')) or image.width*image.height > 16 << 20:
            raise ValueError('camera dimensions or codec differ from original metadata')
        image.load()
    position = point(detection.get('position_m'), 3)
    if abs(position[1]) > 1e-6:
        raise ValueError('this demo requires the declared floor-plane projection')
    position[1] = 0.
    return position


def prepare_response(detection, jpeg_path, physics_scope, navigation_status,
                     candidate_inventory, route_query, *, navigation_frame,
                     calibration_sha256, now_monotonic=None, min_standoff_m=1.5,
                     max_standoff_m=4.0, min_travel_m=6.0, route_acceptor=None):
    """Return evidence-bound schedule after read-only route checks.

    ``route_query`` accepts the normal /route body and returns its JSON reply.
    ``navigation_frame`` comes from /policy/context. No World truth pose is used
    for route planning. A caller must retain expires_monotonic_s and recheck it
    before launching its explicitly authorized simulation fixture.
    ``route_acceptor``, when supplied, receives a copy of the complete Nav-space
    route and must return exactly True. It can conservatively reject paths that
    would put the robot in a pre-rendered fixed camera's field of view.
    """
    now = time.monotonic() if now_monotonic is None else now_monotonic
    fire = verified_detection(detection, jpeg_path, physics_scope, calibration_sha256, now)
    if navigation_frame != physics_scope['frame']:
        raise ValueError('navigation and camera frames differ')
    if (navigation_status.get('ready') is not True
            or navigation_status.get('provenance') != 'simulated'
            or navigation_status.get('inspection_mode') != INSPECTION_MODE):
        raise ValueError('fresh simulated fixed-yaw navigation required')
    if navigation_status.get('policy_frozen') or navigation_status.get('policy_digest'):
        raise ValueError('this preparation requires an unused navigation mission')
    pose = navigation_status.get('pose') or {}
    start = point([pose.get('x'), pose.get('y'), pose.get('yaw')], 3)
    fire_nav = [fire[0], -fire[2]]
    if not all(finite(v) for v in (min_standoff_m, max_standoff_m, min_travel_m)) or not 1 <= min_standoff_m < max_standoff_m <= 5 or not 1 <= min_travel_m <= 20:
        raise ValueError('invalid bounded stand-off or travel requirement')
    if math.dist(start[:2], fire_nav) < min_travel_m:
        raise ValueError('the robot is not distant from this incident')
    rooms = candidate_inventory.get('rooms')
    if not isinstance(rooms, list) or not 1 <= len(rooms) <= 32:
        raise ValueError('bounded commissioned room inventory required')
    options = []
    for room in rooms:
        if not inside(room.get('polygon'), fire_nav):
            continue
        points = room.get('candidates')
        if not isinstance(points, list) or len(points) > 64:
            raise ValueError('bounded navigation candidate list required')
        for candidate in points:
            p = point([candidate.get('x'), candidate.get('y')], 2)
            distance = math.dist(p, fire_nav)
            if not inside(room['polygon'], p) or not min_standoff_m <= distance <= max_standoff_m:
                continue
            yaw = math.atan2(fire_nav[1]-p[1], fire_nav[0]-p[0])
            # Prefer the near side of the incident, then the shortest approach.
            options.append((math.dist(start[:2], p), abs(distance-2.5), room['id'], p, yaw, distance))
    attempts = []
    for _, _, room_id, p, yaw, standoff in sorted(options)[:8]:
        query = {'x': p[0], 'y': p[1], 'yaw': yaw, 'policy_digest': ''}
        route = route_query(deepcopy(query))
        if not isinstance(route, dict) or route.get('reachable') is not True:
            attempts.append({'target': query, 'reason': route.get('reason', 'unreachable') if isinstance(route, dict) else 'invalid_reply'})
            continue
        vertices = route.get('route')
        if not isinstance(vertices, list) or not 2 <= len(vertices) <= 10000:
            raise ValueError('reachable route has no bounded path')
        vertices = [point(vertex, 3) for vertex in vertices]
        length = sum(math.dist(a[:2], b[:2]) for a, b in zip(vertices, vertices[1:]))
        if math.dist(vertices[0][:2], start[:2]) > 1 or math.dist(vertices[-1][:2], p) > .4:
            raise ValueError('route endpoints differ from the localized start or requested destination')
        if not min_travel_m <= length <= 200:
            attempts.append({'target': query, 'reason': 'route_outside_distance_bounds'})
            continue
        # A route through the incident is not an observation approach. Check
        # segments, not just sparse planner vertices, without issuing a goal.
        if any(segment_distance(fire_nav, a[:2], b[:2]) < min_standoff_m for a, b in zip(vertices, vertices[1:])):
            attempts.append({'target': query, 'reason': 'route_crosses_incident_standoff'})
            continue
        if route_acceptor is not None and route_acceptor(deepcopy(vertices)) is not True:
            attempts.append({'target': query, 'reason': 'cameraFOV'})
            continue
        schedule = {'tour': {'method': TOUR_METHOD, 'stops': [
            {'room_id': room_id, 'position': [p[0], .3, -p[1]], 'yaw': yaw}]},
            'return': {'position': [start[0], .3, -start[1]], 'yaw': start[2], 'before_expiry_seconds': 30},
            'revisit_seconds': {}}
        return {'schema': 'wendy.fire-inspection-preparation.v1', 'simulation': True,
                'run_id': physics_scope['run_id'], 'frame': deepcopy(physics_scope['frame']),
                'expires_monotonic_s': detection['expires_monotonic_s'],
                'detection': deepcopy(detection), 'schedule': schedule, 'route': vertices,
                'route_length_m': length, 'initial_incident_distance_m': math.dist(start[:2], fire_nav),
                'standoff_m': standoff, 'attempts': attempts,
                'motion_dispatched': False, 'authority_supplied': False}
    raise NoObservationRoute(attempts)


def segment_distance(p, a, b):
    dx, dy = b[0]-a[0], b[1]-a[1]
    length = dx*dx+dy*dy
    t = 0 if length == 0 else max(0, min(1, ((p[0]-a[0])*dx+(p[1]-a[1])*dy)/length))
    return math.hypot(p[0]-a[0]-t*dx, p[1]-a[1]-t*dy)
