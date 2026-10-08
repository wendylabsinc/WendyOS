"""A deterministic wind field whose pattern changes over the show.

The field is the sum of a mean wind, travelling gust fronts, a drifting vortex
and spatially coherent turbulence, all scaled by a surface boundary layer. It
is evaluated at each drone every control tick and turned into aerodynamic drag
(see ``drag_forces``); the browser draws the same field from a sampled grid.
"""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np


AIR_DENSITY = 1.225  # kg/m^3
# Drag area per body axis, from the box with the upstream model's inertia.
DRAG_AREA = np.array([0.0050, 0.0050, 0.0072])  # m^2, Cd folded in
# Rotor (induced) drag grows with thrust: newtons per newton of thrust per m/s.
ROTOR_DRAG = 0.026
SURFACE_ROUGHNESS = 0.03  # m
REFERENCE_HEIGHT = 1.5  # m, where the boundary-layer factor is 1


def _smooth(x: float) -> float:
    x = min(max(x, 0.0), 1.0)
    return x * x * x * (x * (6.0 * x - 15.0) + 10.0)


def _heading(degrees: float) -> np.ndarray:
    radians = np.deg2rad(degrees)
    return np.array([np.cos(radians), np.sin(radians), 0.0])


@dataclass(frozen=True)
class WindPhase:
    start: float
    name: str
    speed: float  # mean wind speed, m/s
    heading: float  # direction the air moves toward, degrees from +x
    gusts: float = 0.0  # gust-front amplitude, m/s
    turbulence: float = 0.0  # turbulence amplitude, m/s
    vortex: float = 0.0  # peak swirl speed, m/s


# Headings give the direction the air moves; 0 deg blows toward +x (a west wind).
PHASES = (
    WindPhase(0.0, "Calm", 0.0, 0.0),
    WindPhase(7.0, "Steady breeze", 2.6, 0.0, turbulence=0.25),
    WindPhase(18.0, "Wind shift", 2.8, 90.0, turbulence=0.35),
    WindPhase(28.0, "Gust fronts", 2.2, 90.0, gusts=3.2, turbulence=0.3),
    WindPhase(42.0, "Crosswind", 3.2, 200.0, turbulence=0.6),
    WindPhase(52.0, "Vortex", 1.2, 200.0, turbulence=0.3, vortex=3.4),
    WindPhase(64.0, "Easing", 1.4, 250.0, turbulence=0.25),
    WindPhase(78.0, "Calm", 0.0, 250.0, turbulence=0.05),
)
BLEND_SECONDS = 4.0

GUST_PERIOD = 3.4  # s between fronts
GUST_WIDTH = 0.9  # m, Gaussian half-width of a front
GUST_SPEED = 4.2  # m/s, how fast each front crosses the field
FIELD_RADIUS = 7.0  # fronts start and end this far from the centre

VORTEX_CORE = 0.9  # m
VORTEX_START = np.array([-5.5, 2.0])
VORTEX_END = np.array([5.5, -2.5])
VORTEX_WINDOW = (52.0, 68.0)  # s, the vortex drifts across the field in this window

_TURBULENCE_MODES = 9


class WindField:
    """Wind velocity (m/s) at any point and time on a looping timeline."""

    def __init__(self, duration: float, seed: int = 7) -> None:
        self.duration = duration
        rng = np.random.default_rng(seed)
        # Random Fourier modes, each divergence-free in the horizontal plane.
        angle = rng.uniform(0.0, 2.0 * np.pi, _TURBULENCE_MODES)
        magnitude = rng.uniform(0.9, 2.6, _TURBULENCE_MODES)  # rad/m
        self._k = np.stack([np.cos(angle) * magnitude, np.sin(angle) * magnitude,
                            rng.uniform(-0.8, 0.8, _TURBULENCE_MODES)], axis=1)
        self._mode_dir = np.stack([-np.sin(angle), np.cos(angle), rng.uniform(-0.25, 0.25, _TURBULENCE_MODES)], axis=1)
        self._omega = rng.uniform(0.7, 2.2, _TURBULENCE_MODES)
        self._phase = rng.uniform(0.0, 2.0 * np.pi, _TURBULENCE_MODES)
        self._amp = 1.0 / np.sqrt(_TURBULENCE_MODES / 2.0)

    # --- schedule -----------------------------------------------------------------

    def _phase_index(self, t: float) -> int:
        index = 0
        for i, phase in enumerate(PHASES):
            if t >= phase.start:
                index = i
        return index

    def schedule(self, t: float) -> dict[str, float | str | np.ndarray]:
        """Blended mean wind and pattern strengths at time t."""
        t = t % self.duration
        index = self._phase_index(t)
        current = PHASES[index]
        previous = PHASES[index - 1] if index > 0 else PHASES[-1]
        k = _smooth((t - current.start) / BLEND_SECONDS)

        # Blend the mean wind as a vector so direction changes sweep smoothly.
        mean = (1 - k) * previous.speed * _heading(previous.heading) + k * current.speed * _heading(current.heading)
        # Heading turns the short way even when the speed passes near zero.
        delta = (current.heading - previous.heading + 180.0) % 360.0 - 180.0
        heading = previous.heading + delta * k
        speed = (1 - k) * previous.speed + k * current.speed
        if speed > 1e-6:
            mean = speed * _heading(heading)
        return {
            "name": current.name,
            "phaseStart": current.start,
            "mean": mean,
            "heading": heading % 360.0,
            "gusts": (1 - k) * previous.gusts + k * current.gusts,
            "turbulence": (1 - k) * previous.turbulence + k * current.turbulence,
            "vortex": (1 - k) * previous.vortex + k * current.vortex,
            "vortexProgress": (t - VORTEX_WINDOW[0]) / (VORTEX_WINDOW[1] - VORTEX_WINDOW[0]),
            "gustHeading": current.heading if current.gusts > 0 else previous.heading,
            "time": t,
        }

    # --- field --------------------------------------------------------------------

    def velocity(self, points: np.ndarray, t: float, state: dict | None = None) -> np.ndarray:
        """Wind velocity at each row of points (N, 3) at time t."""
        state = self.schedule(t) if state is None else state
        points = np.asarray(points, dtype=np.float64).reshape(-1, 3)
        wind = np.broadcast_to(state["mean"], points.shape).copy()

        gusts = float(state["gusts"])
        if gusts > 1e-3:
            direction = _heading(float(state["gustHeading"]))
            lateral = np.array([-direction[1], direction[0], 0.0])
            along = points @ direction
            across = points @ lateral
            spacing = GUST_SPEED * GUST_PERIOD
            travelled = (float(state["time"]) * GUST_SPEED) % spacing
            envelope = np.zeros(len(points))
            # Fronts are gently curved so they read as waves, not walls.
            front = -FIELD_RADIUS + travelled + 0.25 * np.sin(0.8 * across)
            for offset in (-spacing, 0.0, spacing):
                envelope += np.exp(-(((along - (front + offset)) / GUST_WIDTH) ** 2))
            wind += gusts * envelope[:, None] * direction

        vortex = float(state["vortex"])
        if vortex > 1e-3:
            s = _smooth(float(state["vortexProgress"]))
            centre = VORTEX_START + (VORTEX_END - VORTEX_START) * s
            dx = points[:, 0] - centre[0]
            dy = points[:, 1] - centre[1]
            r2 = dx * dx + dy * dy + 1e-9
            r = np.sqrt(r2)
            # Lamb-Oseen profile, normalised so the peak swirl equals `vortex`.
            swirl = vortex * 1.567 * (VORTEX_CORE / r) * (1.0 - np.exp(-r2 / (VORTEX_CORE ** 2)))
            wind[:, 0] += -swirl * dy / r
            wind[:, 1] += swirl * dx / r
            wind[:, 2] += 0.35 * vortex * np.exp(-r2 / (1.5 * VORTEX_CORE) ** 2)

        turbulence = float(state["turbulence"])
        if turbulence > 1e-3:
            argument = points @ self._k.T - self._omega[None, :] * float(state["time"]) + self._phase[None, :]
            wind += turbulence * self._amp * (np.cos(argument) @ self._mode_dir)

        # Log-law boundary layer: still air at the floor.
        z = np.clip(points[:, 2], SURFACE_ROUGHNESS * 1.01, None)
        layer = np.clip(np.log(z / SURFACE_ROUGHNESS) / np.log(REFERENCE_HEIGHT / SURFACE_ROUGHNESS), 0.0, 1.15)
        return wind * layer[:, None]

    def grid(self, t: float, xs: np.ndarray, ys: np.ndarray, zs: np.ndarray) -> np.ndarray:
        """Field sampled on a regular grid, shaped (len(zs), len(ys), len(xs), 3)."""
        zz, yy, xx = np.meshgrid(zs, ys, xs, indexing="ij")
        points = np.stack([xx.ravel(), yy.ravel(), zz.ravel()], axis=1)
        return self.velocity(points, t).reshape(len(zs), len(ys), len(xs), 3)


def drag_forces(velocity: np.ndarray, wind: np.ndarray, rotation: np.ndarray, thrust: np.ndarray) -> np.ndarray:
    """World-frame aerodynamic force on each drone.

    velocity, wind: (N, 3) world frame; rotation: (N, 3, 3) body-to-world;
    thrust: (N,) total rotor thrust. Quadratic drag acts per body axis, and
    rotor drag opposes the air-relative velocity in the rotor plane.
    """
    relative = velocity - wind
    body = np.einsum("nji,nj->ni", rotation, relative)
    quadratic = -0.5 * AIR_DENSITY * DRAG_AREA[None, :] * np.abs(body) * body
    in_plane = body.copy()
    in_plane[:, 2] = 0.0
    induced = -ROTOR_DRAG * thrust[:, None] * in_plane
    return np.einsum("nij,nj->ni", rotation, quadratic + induced)
