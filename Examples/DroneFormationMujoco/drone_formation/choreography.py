"""Formations and the looping flight plan.

Every formation is a pure function of time that returns one slot per drone.
A morph blends two formations with a minimum-jerk weight, so reference
positions and velocities stay continuous even while a formation rotates.
Slots are assigned to drones with the Hungarian algorithm to keep paths short
and uncrossed.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Callable

import numpy as np

from .model import N_DRONES


PAD_SPACING = 0.6
PAD_HEIGHT = 0.0135  # body origin height when a drone rests on the floor
CRUISE_HEIGHT = 1.3


def _smooth(x: np.ndarray | float) -> np.ndarray | float:
    x = np.clip(x, 0.0, 1.0)
    return x * x * x * (x * (6.0 * x - 15.0) + 10.0)


def _smooth_integral(x: float) -> float:
    x = min(max(x, 0.0), 1.0)
    return x ** 6 - 3.0 * x ** 5 + 2.5 * x ** 4


def pads() -> np.ndarray:
    xs = (np.arange(4) - 1.5) * PAD_SPACING
    ys = (np.arange(3) - 1.0) * PAD_SPACING
    return np.array([[x, y, PAD_HEIGHT] for y in ys for x in xs])


PADS = pads()
Formation = Callable[[float], np.ndarray]


def _with_edges(formation: Formation, edges: list[tuple[int, int]]) -> Formation:
    formation.edges = edges  # slot pairs the viewer links to show the shape
    return formation


def _chain(count: int, closed: bool = False) -> list[tuple[int, int]]:
    edges = [(i, i + 1) for i in range(count - 1)]
    return edges + [(count - 1, 0)] if closed else edges


def grid(height: float) -> Formation:
    slots = PADS.copy()
    slots[:, 2] = height
    rows = [(r * 4 + c, r * 4 + c + 1) for r in range(3) for c in range(3)]
    columns = [(r * 4 + c, (r + 1) * 4 + c) for r in range(2) for c in range(4)]

    def at(t: float) -> np.ndarray:
        return slots

    return _with_edges(at, rows + columns)


def ring(radius: float, height: float, spin: float, spin_start: float, ramp: float = 4.0) -> Formation:
    base = np.arange(N_DRONES) * 2.0 * np.pi / N_DRONES

    def at(t: float) -> np.ndarray:
        x = (t - spin_start) / ramp
        if x <= 1.0:
            angle = spin * ramp * _smooth_integral(x)
        else:
            angle = spin * (0.5 * ramp + (t - spin_start - ramp))
        theta = base + angle
        # A gentle wave in height makes the ring read in depth.
        z = height + 0.12 * np.sin(2.0 * theta)
        return np.stack([radius * np.cos(theta), radius * np.sin(theta), z], axis=1)

    return _with_edges(at, _chain(N_DRONES, closed=True))


def chevron(apex_heading: float, height: float, spacing: float = 0.62, sweep: float = 38.0) -> Formation:
    forward = np.array([np.cos(np.deg2rad(apex_heading)), np.sin(np.deg2rad(apex_heading)), 0.0])
    left = np.array([-forward[1], forward[0], 0.0])
    slots = []
    for side in (1.0, -1.0):
        for k in range(N_DRONES // 2):
            back = (k + 0.65) * spacing * np.cos(np.deg2rad(sweep))
            out = (k + 0.65) * spacing * np.sin(np.deg2rad(sweep))
            slots.append(-back * forward + side * out * left + np.array([0.0, 0.0, height + 0.05 * k]))
    slots = np.array(slots)
    slots[:, :2] -= slots[:, :2].mean(axis=0)
    half = N_DRONES // 2
    edges = _chain(half) + [(half + a, half + b) for a, b in _chain(half)] + [(0, half)]

    def at(t: float) -> np.ndarray:
        return slots

    return _with_edges(at, edges)


def letter_w(facing: float = -90.0, centre_height: float = 1.62) -> Formation:
    """A W in the vertical plane, readable from the side it faces."""
    half = [(-1.65, 2.35), (-1.40, 1.88), (-1.15, 1.42), (-0.90, 0.95), (-0.55, 1.45), (-0.20, 1.95)]
    points = half + [(-u, z) for u, z in reversed(half)]
    heading = np.deg2rad(facing)
    # Lateral axis runs left to right for a viewer looking back along `facing`.
    lateral = np.array([np.cos(heading + np.pi / 2), np.sin(heading + np.pi / 2), 0.0])
    slots = np.array([u * lateral + np.array([0.0, 0.0, z - 1.65 + centre_height]) for u, z in points])

    def at(t: float) -> np.ndarray:
        return slots

    return _with_edges(at, _chain(N_DRONES))


def assign(source: np.ndarray, target: np.ndarray) -> np.ndarray:
    """Minimum total squared distance assignment (Hungarian); returns target index per drone."""
    cost = ((source[:, None, :] - target[None, :, :]) ** 2).sum(axis=2)
    n = cost.shape[0]
    u = np.zeros(n + 1)
    v = np.zeros(n + 1)
    p = np.zeros(n + 1, dtype=int)
    way = np.zeros(n + 1, dtype=int)
    for i in range(1, n + 1):
        p[0] = i
        j0 = 0
        minv = np.full(n + 1, np.inf)
        used = np.zeros(n + 1, dtype=bool)
        while True:
            used[j0] = True
            i0 = p[j0]
            delta = np.inf
            j1 = 0
            for j in range(1, n + 1):
                if not used[j]:
                    current = cost[i0 - 1, j - 1] - u[i0] - v[j]
                    if current < minv[j]:
                        minv[j] = current
                        way[j] = j0
                    if minv[j] < delta:
                        delta = minv[j]
                        j1 = j
            for j in range(n + 1):
                if used[j]:
                    u[p[j]] += delta
                    v[j] -= delta
                else:
                    minv[j] -= delta
            j0 = j1
            if p[j0] == 0:
                break
        while True:
            j1 = way[j0]
            p[j0] = p[j1]
            j0 = j1
            if j0 == 0:
                break
    result = np.zeros(n, dtype=int)
    for j in range(1, n + 1):
        result[p[j] - 1] = j - 1
    return result


def _clearance(points: np.ndarray) -> float:
    distance = np.linalg.norm(points[:, None, :] - points[None, :, :], axis=2)
    return float((distance + np.eye(len(points)) * 1e3).min())


@dataclass(frozen=True)
class Segment:
    start: float
    name: str
    formation: Formation
    morph: float  # seconds to blend in from the previous segment
    airborne: bool = True
    motors: str = "flight"  # "off", "idle" or "flight"


def _plan() -> tuple[list[Segment], float]:
    segments = [
        Segment(0.0, "Parked", grid(PAD_HEIGHT), 0.0, airborne=False, motors="off"),
        Segment(2.0, "Arming", grid(PAD_HEIGHT), 0.0, airborne=False, motors="idle"),
        Segment(3.5, "Grid", grid(CRUISE_HEIGHT), 3.5),
        Segment(18.0, "Ring", ring(1.3, 1.6, spin=0.22, spin_start=19.0), 5.0),
        Segment(42.0, "Chevron", chevron(apex_heading=20.0, height=1.45), 5.0),
        Segment(64.0, "Letter W", letter_w(), 6.0),
        Segment(79.0, "Grid", grid(CRUISE_HEIGHT), 5.0),
        Segment(85.0, "Landing", grid(PAD_HEIGHT), 4.5, airborne=False),
        Segment(90.0, "Parked", grid(PAD_HEIGHT), 0.0, airborne=False, motors="idle"),
        Segment(91.0, "Parked", grid(PAD_HEIGHT), 0.0, airborne=False, motors="off"),
    ]
    return segments, 93.0


SEGMENTS, SHOW_SECONDS = _plan()


class FlightPlan:
    """Reference positions, velocities and accelerations for every drone."""

    def __init__(self) -> None:
        self.segments = SEGMENTS
        self.duration = SHOW_SECONDS
        # Chain slot assignments from the pads so each morph keeps paths short:
        # where the drones are when a morph starts, to where the new formation
        # will be when it ends.
        self.assignment: list[np.ndarray] = [np.arange(N_DRONES)]
        for index in range(1, len(self.segments)):
            segment = self.segments[index]
            previous = self.segments[index - 1]
            source = previous.formation(segment.start)[self.assignment[-1]]
            target = segment.formation(segment.start + segment.morph)
            self.assignment.append(assign(source, target))
        # Vertical layers used while morphing, so crossing paths pass above or
        # below each other. Pick, per morph, the layering with the most clearance.
        self.layers = [self._choose_layers(index) for index in range(len(self.segments))]

    def _blend(self, index: int, t: float, layers: np.ndarray) -> np.ndarray:
        segment = self.segments[index]
        x = (t - segment.start) / segment.morph
        s = _smooth(x)
        blended = (1.0 - s) * self._slots(index - 1, t) + s * self._slots(index, t)
        blended[:, 2] += layers * np.sin(np.pi * x) ** 2
        return blended

    def _choose_layers(self, index: int) -> np.ndarray:
        segment = self.segments[index]
        none = np.zeros(N_DRONES)
        if index == 0 or segment.morph <= 0.0 or not (segment.airborne and self.segments[index - 1].airborne):
            return none
        times = segment.start + np.linspace(0.0, 1.0, 41) * segment.morph
        base = np.repeat([-1.0, 0.0, 1.0], N_DRONES // 3) * 0.3
        rng = np.random.default_rng(index)
        best, best_clearance = none, -1.0
        for candidate in [none] + [rng.permutation(base) for _ in range(200)]:
            clearance = min(_clearance(self._blend(index, t, candidate)) for t in times)
            if clearance > best_clearance + 1e-6:
                best, best_clearance = candidate, clearance
        return best

    def segment_index(self, t: float) -> int:
        t = t % self.duration
        index = 0
        for i, segment in enumerate(self.segments):
            if t >= segment.start:
                index = i
        return index

    def _slots(self, index: int, t: float) -> np.ndarray:
        return self.segments[index].formation(t)[self.assignment[index]]

    def position(self, t: float) -> np.ndarray:
        t = t % self.duration
        index = self.segment_index(t)
        segment = self.segments[index]
        here = self._slots(index, t)
        if index == 0 or segment.morph <= 0.0 or t >= segment.start + segment.morph:
            return here
        return self._blend(index, t, self.layers[index])

    def reference(self, t: float, h: float = 0.004) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
        """Position, velocity and acceleration by central differences."""
        # Differences never straddle the loop boundary, where drones are parked.
        t = t % self.duration
        t = min(max(t, h), self.duration - h)
        p0 = self.position(t - h)
        p1 = self.position(t)
        p2 = self.position(t + h)
        return p1, (p2 - p0) / (2 * h), (p2 - 2 * p1 + p0) / (h * h)

    def state(self, t: float) -> Segment:
        return self.segments[self.segment_index(t)]

    def _links(self, index: int) -> list[tuple[int, int]]:
        segment = self.segments[index]
        if not segment.airborne:
            return []
        drone_for_slot = np.argsort(self.assignment[index])
        return [(int(drone_for_slot[a]), int(drone_for_slot[b])) for a, b in segment.formation.edges]

    def links(self, t: float) -> list[tuple[int, int, float]]:
        """Drone pairs that outline the formation, with a weight that fades across morphs."""
        t = t % self.duration
        index = self.segment_index(t)
        segment = self.segments[index]
        weight = 1.0
        if index > 0 and segment.morph > 0.0 and t < segment.start + segment.morph:
            weight = float(_smooth((t - segment.start) / segment.morph))
        result = [(a, b, weight) for a, b in self._links(index)]
        if weight < 1.0 and index > 0:
            result += [(a, b, 1.0 - weight) for a, b in self._links(index - 1)]
        return [(a, b, round(w, 3)) for a, b, w in result if w > 0.01]
