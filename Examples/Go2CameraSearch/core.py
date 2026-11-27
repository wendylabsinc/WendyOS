"""Geometry and bounded search policy. No hardware or web dependencies."""
from dataclasses import dataclass
import math
import re
import numpy as np

class RobotSessionExpired(RuntimeError):
    """The twin explicitly rejected a handle; this is not a transport timeout."""


ITEMS = ('watering can', 'bottle', 'backpack', 'red cup', 'blue watering can')

def requested_item(query):
    if not isinstance(query,str):
        raise ValueError('Describe the object you want to find.')
    value = re.sub(r'^(?:find me|find|look for)\s+(?:the\s+|a\s+)?', '', query.strip().lower())
    value = value.rstrip('.!?').strip().replace('waternig can','watering can')
    if not value or len(value)>120 or any(ord(c)<32 for c in value):
        raise ValueError('Describe one object in 120 characters or fewer.')
    return value


def finite(values):
    if not all(math.isfinite(float(v)) for v in values):
        raise ValueError('Coordinates must be finite.')


class Calibration:
    """Four ordered floor correspondences, restricted to their convex footprint."""
    def __init__(self, data):
        self.camera = data['camera']
        self.frame = data['frame']
        self.width, self.height = int(data['width']), int(data['height'])
        if not self.camera.startswith('sensorlink:') or not self.frame or not (16 <= self.width <= 4096 and 16 <= self.height <= 4096):
            raise ValueError('Select a SensorLink camera, room revision and valid image size.')
        self.pixels = np.asarray(data['pixels'], dtype=float)
        self.world = np.asarray(data['world'], dtype=float)
        if self.pixels.shape != (4, 2) or self.world.shape != (4, 2):
            raise ValueError('Provide exactly four pixel and room X/Z points, in perimeter order.')
        finite(self.pixels.flat)
        finite(self.world.flat)
        if np.any(self.pixels < 0) or np.any(self.pixels[:, 0] >= self.width) or np.any(self.pixels[:, 1] >= self.height) or np.any(abs(self.world) > 1000):
            raise ValueError('Calibration coordinates are outside bounds.')
        for points in (self.pixels, self.world):
            edges = np.roll(points, -1, axis=0) - points
            cross = edges[:, 0] * np.roll(edges, -1, axis=0)[:, 1] - edges[:, 1] * np.roll(edges, -1, axis=0)[:, 0]
            if not (np.all(cross > 1e-5) or np.all(cross < -1e-5)):
                raise ValueError('Points must form a convex quadrilateral in perimeter order.')
        if abs(np.linalg.det(np.stack([self.world[1] - self.world[0], self.world[3] - self.world[0]]))) < .1:
            raise ValueError('Calibration must cover at least 0.1 square metres.')
        a, b = [], []
        # Normalized pixels improve conditioning on high-resolution images.
        for (u, v), (x, z) in zip(self.pixels / [self.width, self.height], self.world):
            a.extend([[u, v, 1, 0, 0, 0, -x*u, -x*v], [0, 0, 0, u, v, 1, -z*u, -z*v]])
            b.extend([x, z])
        if np.linalg.cond(a) > 1e8:
            raise ValueError('Calibration is ill-conditioned. Spread the floor points farther apart.')
        self.matrix = np.append(np.linalg.solve(a, b), 1).reshape(3, 3)
        denominators = [float((self.matrix @ [u/self.width, v/self.height, 1])[2]) for u, v in self.pixels]
        if min(denominators) <= 0 <= max(denominators):
            raise ValueError('Calibration crosses the camera horizon.')
        self.data = data

    def project(self, u, v, width, height):
        if (width, height) != (self.width, self.height):
            raise ValueError('Camera resolution changed. Recalibrate.')
        p = np.asarray([u, v], dtype=float)
        finite(p)
        edges = np.roll(self.pixels, -1, axis=0) - self.pixels
        offsets = p - self.pixels
        cross = edges[:, 0] * offsets[:, 1] - edges[:, 1] * offsets[:, 0]
        if not (np.all(cross >= -1e-6) or np.all(cross <= 1e-6)):
            raise ValueError('Object is outside the calibrated floor area.')
        point = self.matrix @ [u/self.width, v/self.height, 1]
        if abs(point[2]) < 1e-8:
            raise ValueError('Floor projection is near the horizon.')
        return tuple(float(v) for v in point[:2] / point[2])


def approach(robot, target, stand_off=.8):
    finite([*robot, *target])
    distance = math.dist(robot, target)
    if distance <= stand_off + .2:
        raise ValueError('The object is already within the stopping distance.')
    if distance > 5:
        raise ValueError('Target is more than 5 metres away. Reposition and start a new search.')
    return tuple(t - (t-r)*stand_off/distance for r, t in zip(robot, target))


@dataclass
class RunGuard:
    started: float
    origin: tuple
    target: tuple
    previous: tuple
    travelled: float = 0

    def check(self, now, heartbeat, detected_at, observed, snapshot):
        if now - heartbeat > .8:
            return 'Browser supervision lost. Start again explicitly.'
        if now - detected_at > 1.5:
            return 'Camera or target detection is stale.'
        if observed is None or math.dist(observed, self.target) > .4:
            return 'Target moved or is ambiguous. Search again.'
        if now - self.started > 30:
            return 'The 30-second movement limit was reached.'
        if snapshot.get('Disconnected') or not snapshot.get('Pose') or not snapshot.get('Ready') or snapshot.get('ControlWarning'):
            return snapshot.get('ControlWarning') or snapshot.get('Message') or 'Robot observations are unavailable.'
        point = (snapshot['X'], snapshot['Z'])
        finite(point)
        self.travelled += math.dist(self.previous, point)
        self.previous = point
        if self.travelled > 5:
            return 'The 5-metre travel limit was reached.'
        return None
