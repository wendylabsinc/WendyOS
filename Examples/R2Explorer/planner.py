"""Small reactive explorer: sampled Ackermann arcs, lidar clearance and visit memory.

Only observed ranges and odometry enter the planner. It does not read the
simulator's scene, obstacle list or collision checker.
"""
import math
from collections import Counter

CELL = .25
WHEELBASE = .26
BODY_CENTER = .13
BODY_RADIUS = .30  # circumscribed footprint plus clearance


def cell(x, y):
    return math.floor(x / CELL), math.floor(y / CELL)


def wrap(angle):
    return math.atan2(math.sin(angle), math.cos(angle))


class Planner:
    def __init__(self):
        self.visits = Counter()
        self.free, self.occupied = set(), set()
        self.goal = None
        self.goal_age = 0.
        self.last_cell = None
        self.last_steering = 0.
        self.points = []
        self.trail = []

    def observe(self, pose, scan, dt):
        x, y = pose['x'], pose['y']
        here = cell(x, y)
        if here != self.last_cell:
            self.visits[here] += 1
            self.last_cell = here
        if not self.trail or math.hypot(x-self.trail[-1][0], y-self.trail[-1][1]) > .08:
            self.trail.append([x, y])
            self.trail = self.trail[-4000:]
        self.goal_age += dt
        ox, oy = scan['origin'][:2]
        self.points = []
        for i, distance in enumerate(scan['ranges']):
            if distance is None or not math.isfinite(distance) or distance <= 0:
                continue
            angle = scan['yaw'] + scan['angle_min'] + i*scan['angle_increment']
            c, s = math.cos(angle), math.sin(angle)
            px, py = ox+c*distance, oy+s*distance
            self.points.append((px, py))
            self.occupied.add(cell(px, py))
            if i % 6 == 0:
                for n in range(int(distance/CELL)):
                    self.free.add(cell(ox+c*n*CELL, oy+s*n*CELL))

    def novelty(self, x, y):
        gx, gy = cell(x, y)
        count = sum(self.visits[(gx+dx, gy+dy)] for dx in range(-2, 3) for dy in range(-2, 3))
        return 1. / (1. + count)

    def choose_goal(self, pose, scan):
        x, y, yaw = pose['x'], pose['y'], pose['yaw']
        candidates = []
        # Choose visible, unvisited destinations; a committed goal prevents
        # left/right oscillation when adjacent gaps have similar ranges.
        for i in range(0, len(scan['ranges']), 10):
            distance = scan['ranges'][i]
            if distance is None or not math.isfinite(distance) or distance < .8:
                continue
            angle = scan['yaw'] + scan['angle_min'] + i*scan['angle_increment']
            for reach in (.9, 1.6, 2.5):
                if reach > distance-.4:
                    continue
                gx = scan['origin'][0] + math.cos(angle)*reach
                gy = scan['origin'][1] + math.sin(angle)*reach
                clearance = min((math.hypot(gx-px, gy-py) for px, py in self.points), default=0)
                if clearance < .42:
                    continue
                score = 3*self.novelty(gx, gy) + .45*min(clearance, 1.5) + .15*reach
                score += .7*math.cos(wrap(angle-yaw))
                # Stable, slight turn preference breaks perfectly symmetric rooms.
                score += .03*math.sin(angle + .7)
                candidates.append((score, gx, gy))
        if candidates:
            _, gx, gy = max(candidates)
            self.goal = (gx, gy)
        else:
            self.goal = None
        self.goal_age = 0.

    def command(self, pose, scan, depth_front=None, dt=.1):
        self.observe(pose, scan, dt)
        x, y, yaw = pose['x'], pose['y'], pose['yaw']
        if self.goal is None or math.hypot(x-self.goal[0], y-self.goal[1]) < .45 or self.goal_age > 12:
            self.choose_goal(pose, scan)
        if self.goal is None:
            return 0., 0., 'No clear route'
        gx, gy = self.goal
        start_distance = math.hypot(gx-x, gy-y)
        nearby = [(px, py) for px, py in self.points if math.hypot(px-x, py-y) < 2.]
        choices = []
        forward = .38 if depth_front is None or depth_front > .8 else .22
        for speed in (forward, -.18):
            if speed > 0 and depth_front is not None and depth_front < .28:
                continue
            for steering in (-.52, -.39, -.26, -.13, 0., .13, .26, .39, .52):
                px, py, heading = x, y, yaw
                current_speed, current_steering = pose['speed'], pose['steering']
                clearance = 2.
                safe = True
                # Roll out steering slew and braking, rather than assuming the
                # wheels instantly reach the requested steering angle.
                for _ in range(15):
                    target = 0. if current_speed*speed < 0 else speed
                    rate = 2.4 if current_speed*speed < 0 or abs(speed) < abs(current_speed) else 1.2
                    current_speed += max(-rate*.1, min(rate*.1, target-current_speed))
                    current_steering += max(-math.pi*.05, min(math.pi*.05, steering-current_steering))
                    heading += current_speed*math.tan(current_steering)/WHEELBASE*.1
                    px += current_speed*math.cos(heading)*.1
                    py += current_speed*math.sin(heading)*.1
                    cx, cy = px+BODY_CENTER*math.cos(heading), py+BODY_CENTER*math.sin(heading)
                    clearance = min(clearance, min((math.hypot(cx-qx, cy-qy) for qx, qy in nearby), default=2.))
                    if clearance < BODY_RADIUS:
                        safe = False
                        break
                if not safe:
                    continue
                progress = start_distance-math.hypot(gx-px, gy-py)
                facing = math.cos(wrap(math.atan2(gy-py, gx-px)-heading))
                score = 5*progress + .65*facing + .45*min(clearance, .9)
                score -= .12*abs(steering-self.last_steering) + (.65 if speed < 0 else 0)
                choices.append((score, speed, steering))
        if not choices:
            self.goal = None
            return 0., 0., 'No safe arc; stopped'
        _, speed, steering = max(choices)
        self.last_steering = steering
        return speed, steering, 'Exploring' if speed > 0 else 'Backing into a clear gap'

    def map(self):
        return {'cell_size': CELL, 'free': sorted(self.free-self.occupied),
                'occupied': sorted(self.occupied), 'trail': self.trail, 'goal': self.goal,
                'visited_m2': round(len(self.visits)*CELL*CELL, 2)}
