"""Walking, picking and placing, written as generators that yield once per physics step.

The simulation loop advances a skill one step at a time, so the robot keeps
running in real time while it walks, reaches, or waits for turbopuffer.
"""

from __future__ import annotations

from typing import Iterator

import mujoco
import numpy as np

from .robot import G1, STAND_HEIGHT, yaw_of
from .world import Slot

Steps = Iterator[None]


class RobotFell(RuntimeError):
    pass


class GraspMissed(RuntimeError):
    pass


class Skills:
    STANDOFF = 0.34   # pelvis to box centre when grasping (m)
    PICK_CLOSER = 0.04  # bending and reaching makes the balance policy step back about this much
    LEAN = 0.30       # torso pitch while reaching down to pick (rad)
    PLACE_LEAN = 0.10 # torso pitch while setting a box down low; more makes the balance policy step back
    BACK_OFF = 0.28   # extra distance for the stop-short before placing (m)
    CARRY = np.array([0.31, 0.0, 0.93])  # held box centre while walking: robot frame, height above floor
    SHARE = 0.006     # the following right palm aims this far above its grip to take some weight (m)
    # Palm site to box side when gripping: the curled fingertips and folded thumb reach ~4.2 cm past
    # the palm site, so they meet the box and the palm stops just short of it.
    GRIP_GAP = 0.039
    WALK_SPEED = 0.85  # m/s between stations; the gait policy strides out properly at this pace
    TURN_RATE = 0.9   # rad/s, at most, while steering

    def __init__(self, robot: G1, boxes: dict[str, dict]):
        self.g = robot
        self.m, self.d = robot.m, robot.d
        self.boxes = boxes      # name -> {"body", "weld", "connect", "half"}
        self.palms = [np.zeros(3), np.zeros(3)]   # IK targets, robot frame (while the hands work)
        self.relaxed = True     # empty hands: the arms hang and swing instead of following targets
        self.relax_blend, self.relax_from = 1.0, None
        self.turn_from = None   # palm orientations to turn from on the next reach
        self.held: str | None = None
        self.grip_half = np.zeros(3)    # while holding: half the right-to-left palm vector, robot frame
        self.box_offset = np.zeros(3)   # while holding: box centre minus palm midpoint, robot frame
        self.palm_turn = [np.eye(3), np.eye(3)]   # palm orientations relative to the heading
        self.right_in_box = (np.zeros(3), np.eye(3))   # while holding: right palm pose in the box frame
        self.hand_geoms = [
            {i for i in range(self.m.ngeom)
             if self.m.body(self.m.geom_bodyid[i]).name.startswith(f"{side}_hand")
             or self.m.body(self.m.geom_bodyid[i]).name == f"{side}_wrist_yaw_link"}
            for side in ("left", "right")]

    def reset(self) -> None:
        """Empty hands, arms relaxed (after the robot is put back at its start)."""
        self.palm_turn = [np.eye(3), np.eye(3)]
        self.relaxed, self.relax_blend, self.relax_from, self.turn_from = True, 1.0, None, None
        self.held = None

    # --- basics ---------------------------------------------------------------------------------
    def to_robot(self, world) -> np.ndarray:
        """World point to the robot frame: forward, left, and height above the floor."""
        origin, rotation = self.g.base_frame()
        return rotation.T @ (np.asarray(world, dtype=float) - origin)

    def to_world(self, local) -> np.ndarray:
        origin, rotation = self.g.base_frame()
        return origin + rotation @ np.asarray(local, dtype=float)

    def _palm_targets(self):
        rotation = self.g.base_frame()[1]
        targets = [(self.to_world(p), rotation @ turn) for p, turn in zip(self.palms, self.palm_turn)]
        if self.held is not None:
            # The left hand steers the box. The right palm follows the box wherever it really is:
            # two arms each steering one rigid box would fight each other.
            body = self.boxes[self.held]["body"]
            box_rotation = self.d.xmat[body].reshape(3, 3)
            point, turn = self.right_in_box
            targets[1] = (self.d.xpos[body] + box_rotation @ point + np.array([0.0, 0.0, self.SHARE]),
                          box_rotation @ turn)
        return targets

    def tick(self) -> Steps:
        if self.g.steps % self.g.decimation == 0:
            if self.relaxed:
                self.g.relax_arms(self.relax_blend, self.relax_from)
            else:
                self.g.solve_arms(self._palm_targets())
        self.g.step()
        if self.g.fallen():
            raise RobotFell(f"G1 fell at t={self.d.time:.2f}")
        yield

    def wait(self, seconds: float) -> Steps:
        for _ in range(int(seconds / self.m.opt.timestep)):
            yield from self.tick()

    def relax(self, seconds: float) -> Steps:
        """Let the arms down: ease from wherever they are into hanging (and swinging) by the sides."""
        self.relax_from = self.d.qpos[self.g.arm_qadr].copy()
        self.relaxed = True
        n = max(1, int(seconds / self.m.opt.timestep))
        for i in range(n):
            s = (i + 1) / n
            self.relax_blend = s * s * (3 - 2 * s)
            yield from self.tick()
        self.relax_blend = 1.0

    def take_hands(self) -> None:
        """Hand the arms from hanging to palm targets, starting exactly where the palms are."""
        if not self.relaxed:
            return
        rotation = self.g.base_frame()[1]
        self.palms = [self.to_robot(self.d.site_xpos[site]) for site in self.g.palm]
        self.palm_turn = [rotation.T @ self.d.site_xmat[site].reshape(3, 3) for site in self.g.palm]
        self.turn_from = [turn.copy() for turn in self.palm_turn]   # the next reach turns them to the grip
        self.relaxed = False

    def move_palms(self, goal, seconds: float) -> Steps:
        start = [p.copy() for p in self.palms]
        n = max(1, int(seconds / self.m.opt.timestep))
        for i in range(n):
            s = (i + 1) / n
            s = s * s * (3 - 2 * s)
            self.palms = [a + (b - a) * s for a, b in zip(start, goal)]
            if self.turn_from is not None:   # hands just taken from hanging: turn them to face forward
                self.palm_turn = [_slerp(turn, np.eye(3), s) for turn in self.turn_from]
            yield from self.tick()
        self.turn_from = None

    def hand_contacts(self, body: int) -> list[int]:
        geoms = {i for i in range(self.m.ngeom) if self.m.geom_bodyid[i] == body}
        counts = [0, 0]
        for contact in self.d.contact[:self.d.ncon]:
            for hand in (0, 1):
                if ((contact.geom1 in geoms and contact.geom2 in self.hand_geoms[hand])
                        or (contact.geom2 in geoms and contact.geom1 in self.hand_geoms[hand])):
                    counts[hand] += 1
        return counts

    # --- walking ---------------------------------------------------------------------------------
    def goto(self, x: float, y: float, yaw: float, speed: float | None = None, tol: float = 0.06,
             yaw_tol: float = 0.07, timeout: float = 60.0, settle: float = 0.8) -> Steps:
        """Walk to a stance: steer toward it, then align position and heading in the body frame."""
        g, d = self.g, self.d
        speed = self.WALK_SPEED if speed is None else speed
        deadline = d.time + timeout
        while True:
            delta = np.array([x, y]) - d.qpos[:2]
            dist = float(np.hypot(*delta))
            yaw_err = (yaw - g.heading() + np.pi) % (2 * np.pi) - np.pi
            if dist > 0.5:
                bearing = (np.arctan2(delta[1], delta[0]) - g.heading() + np.pi) % (2 * np.pi) - np.pi
                # full pace until the last metre, then ease off so the approach doesn't overshoot
                pace = speed * min(1.0, 0.45 + 0.55 * (dist - 0.5) / 0.6)
                g.cmd_goal = np.array([pace * max(0.0, np.cos(bearing)) ** 2, 0.0,
                                       np.clip(1.8 * bearing, -self.TURN_RATE, self.TURN_RATE)])
            else:
                c, s = np.cos(g.heading()), np.sin(g.heading())
                local = np.array([c * delta[0] + s * delta[1], -s * delta[0] + c * delta[1]])
                if dist < tol and abs(yaw_err) < yaw_tol:
                    g.cmd_goal = np.zeros(3)
                    break
                command = np.array([np.clip(1.2 * local[0], -0.2, 0.3), np.clip(1.2 * local[1], -0.18, 0.18),
                                    np.clip(1.5 * yaw_err, -0.5, 0.5)])
                # stay above the walk threshold, or the balance policy takes over mid-step
                norm = float(np.linalg.norm(command))
                if norm < 0.12:
                    command = command / (norm + 1e-9) * 0.12
                g.cmd_goal = command
            yield from self.tick()
            if d.time > deadline:
                raise RuntimeError(f"could not reach stance ({x:.2f}, {y:.2f})")
        yield from self.wait(settle)   # let the balance policy square its feet

    def stance(self, slot: Slot, extra: float = 0.0) -> tuple[float, float]:
        reach = self.STANDOFF + extra
        return slot.x - reach * np.cos(slot.facing), slot.y - reach * np.sin(slot.facing)

    @staticmethod
    def height_for(z_centre: float) -> float:
        """Pelvis height so the box centre sits at a comfortable hand height."""
        return float(np.clip(STAND_HEIGHT - max(0.0, 0.86 - z_centre), 0.50, STAND_HEIGHT))

    def retreat(self, distance: float = 0.45, speed: float = 0.32) -> Steps:
        """Walk straight back from a shelf before turning, so hands and boxes clear it."""
        start = self.d.qpos[:2].copy()
        self.g.cmd_goal = np.array([-speed, 0.0, 0.0])
        deadline = self.d.time + 3 * distance / speed
        while np.hypot(*(self.d.qpos[:2] - start)) < distance and self.d.time < deadline:
            yield from self.tick()
        self.g.cmd_goal = np.zeros(3)
        yield from self.wait(0.5)

    # --- manipulation ----------------------------------------------------------------------------
    def pick(self, name: str, slot: Slot, attempts: int = 3) -> Steps:
        """Pick a box up; if the robot drifted out of reach or missed, stand, step back and retry."""
        for attempt in range(attempts):
            try:
                yield from self._pick_once(name, slot)
                return
            except GraspMissed:
                if attempt == attempts - 1:
                    raise
                self.g.height_goal = STAND_HEIGHT
                self.g.rpy_goal = np.zeros(3)
                yield from self.retreat()      # clear of the shelf first, then let the arms down
                yield from self.relax(0.8)

    def _pick_once(self, name: str, slot: Slot) -> Steps:
        box = self.boxes[name]
        body = box["body"]
        _, hy, _ = box["half"]
        g, d = self.g, self.d
        # square up to the box where it actually is, not where it was meant to be
        centre = d.xpos[body].copy()
        facing = self._box_facing(body, slot.facing)
        stand = centre[:2] - (self.STANDOFF - self.PICK_CLOSER) * np.array([np.cos(facing), np.sin(facing)])
        normal = np.array([np.cos(slot.facing), np.sin(slot.facing)])
        too_close = (stand - np.array(self.stance(slot))) @ normal - 0.05   # keep clear of the shelf
        if too_close > 0:
            stand -= too_close * normal
        yield from self.goto(stand[0], stand[1], facing, tol=0.04)
        g.height_goal = self.height_for(centre[2])
        g.rpy_goal = np.array([0.0, self.LEAN, 0.0])
        # The hands come up and open toward the box while the body bends, then drop beside it and close in.
        self.take_hands()
        surface = centre[2] - box["half"][2]
        if surface > 0.55:
            # hanging hands start below a shelf: raise them in front of the chest, behind its edge, first
            yield from self.move_palms([np.array([0.13, side * (hy + 0.12), surface + 0.22]) for side in (1, -1)], 0.8)
        yield from self.reach_for(body, hy + 0.10, 0.10, 1.4 if surface <= 0.55 else 1.0)
        rel = self.to_robot(d.xpos[body])
        if not (0.24 < rel[0] < 0.42 and abs(rel[1]) < 0.09):
            # the balance policy sometimes steps while crouching: re-approach instead of overreaching
            raise GraspMissed(f"{name} out of reach after crouching: {rel[0]:.2f} ahead, {rel[1]:.2f} to the side")
        yield from self.reach_for(body, hy + 0.10, 0.0, 0.8)
        yield from self.reach_for(body, hy + self.GRIP_GAP, 0.0, 0.7)
        touching = self.hand_contacts(body)
        if min(touching) == 0:
            raise GraspMissed(f"no two-handed contact on {name}: {touching}")
        self._grip(name)
        lifted = self.to_robot(d.xpos[body]) + np.array([-0.03, 0.0, 0.10])
        yield from self.carry_box(lifted, 0.9)
        g.height_goal = STAND_HEIGHT
        g.rpy_goal = np.zeros(3)
        yield from self.carry_box(self.CARRY, 1.2)
        yield from self.retreat()

    def reach_for(self, body: int, half_gap: float, above: float, seconds: float) -> Steps:
        """Move the palms to either side of a box (half_gap from its centre, `above` over it),
        following the box as the robot moves."""
        start = [p.copy() for p in self.palms]
        n = max(1, int(seconds / self.m.opt.timestep))
        for i in range(n):
            s = (i + 1) / n
            s = s * s * (3 - 2 * s)
            c = self.to_robot(self.d.xpos[body])
            goal = [np.array([c[0] - 0.02, c[1] + half_gap, c[2] + above]),
                    np.array([c[0] - 0.02, c[1] - half_gap, c[2] + above])]
            self.palms = [a + (b - a) * s for a, b in zip(start, goal)]
            if self.turn_from is not None:   # turn the palms from hanging to facing the box
                self.palm_turn = [_slerp(turn, np.eye(3), s) for turn in self.turn_from]
            yield from self.tick()
        self.turn_from = None

    def _box_facing(self, body: int, facing: float) -> float:
        """The box's heading nearest the slot's (a box looks the same turned around), within 20°."""
        turn = (yaw_of(self.d.xquat[body]) - facing + np.pi / 2) % np.pi - np.pi / 2
        return facing + float(np.clip(turn, -0.35, 0.35))

    def _grip(self, name: str) -> None:
        """Hold the box where the hands are now; from here on, targets move the box."""
        box = self.boxes[name]
        self.g.attach(box["weld"], box["connect"])
        self.held = name
        # hold the hands exactly as they are: the constraints close the loop through the box, so
        # any target the arms cannot reach together would make them fight each other
        rotation = self.g.base_frame()[1]
        self.palms = [self.to_robot(self.d.site_xpos[site]) for site in self.g.palm]
        self.palm_turn = [rotation.T @ self.d.site_xmat[site].reshape(3, 3) for site in self.g.palm]
        self.grip_half = 0.5 * (self.palms[0] - self.palms[1])
        self.box_offset = self.to_robot(self.d.xpos[box["body"]]) - 0.5 * (self.palms[0] + self.palms[1])
        box_rotation = self.d.xmat[box["body"]].reshape(3, 3)
        right = self.g.palm[1]
        self.right_in_box = (box_rotation.T @ (self.d.site_xpos[right] - self.d.xpos[box["body"]]),
                             box_rotation.T @ self.d.site_xmat[right].reshape(3, 3))

    def _release(self) -> None:
        box = self.boxes[self.held]
        self.g.detach(box["weld"], box["connect"])
        self.held = None
        # the right hand was following the box; hold it where it is now
        right = self.g.palm[1]
        self.palms[1] = self.to_robot(self.d.site_xpos[right])
        self.palm_turn[1] = self.g.base_frame()[1].T @ self.d.site_xmat[right].reshape(3, 3)

    def _palms_for(self, centre: np.ndarray) -> list[np.ndarray]:
        """Palm targets that put the held box's centre at `centre` (robot frame)."""
        middle = centre - self.box_offset
        return [middle + self.grip_half, middle - self.grip_half]

    def carry_box(self, centre, seconds: float) -> Steps:
        yield from self.move_palms(self._palms_for(np.asarray(centre, dtype=float)), seconds)

    def set_box_down(self, goal_world, seconds: float) -> Steps:
        """Move the held box to a fixed point in the world, correcting as the robot sways."""
        start = self.to_world(0.5 * (self.palms[0] + self.palms[1]) + self.box_offset)
        goal_world = np.asarray(goal_world, dtype=float)
        n = max(1, int(seconds / self.m.opt.timestep))
        for i in range(n):
            s = (i + 1) / n
            s = s * s * (3 - 2 * s)
            local = self.to_robot(start + (goal_world - start) * s)
            local[0] = np.clip(local[0], 0.18, 0.44)
            local[1] = np.clip(local[1], -0.12, 0.12)
            self.palms = self._palms_for(local)
            yield from self.tick()

    def place(self, slot: Slot) -> Steps:
        """Stop short, step in standing (the gait policy cannot walk while squatting), then
        crouch if the surface is low, set the box down and let go."""
        _, _, hz = self.boxes[self.held]["half"]
        g = self.g
        rest = slot.z + hz + 0.012            # box centre just above the surface
        approach = max(rest + 0.08, self.CARRY[2])
        sx, sy = self.stance(slot, self.BACK_OFF)
        yield from self.goto(sx, sy, slot.facing)
        yield from self.carry_box(np.array([self.CARRY[0], 0.0, approach]), 1.0)
        sx, sy = self.stance(slot)
        yield from self.goto(sx, sy, slot.facing, speed=0.25, tol=0.03, settle=1.5)
        g.height_goal = self.height_for(rest)
        if g.height_goal < STAND_HEIGHT:   # lean in only when crouching to a low surface
            g.rpy_goal = np.array([0.0, self.PLACE_LEAN, 0.0])
        yield from self.set_box_down([slot.x, slot.y, rest + 0.03], 2.0)
        yield from self.set_box_down([slot.x, slot.y, rest - 0.01], 0.8)
        self._release()
        # open the hands along the grip, lift them clear, then let the arms down
        apart = self.grip_half * np.array([1.0, 1.0, 0.0])
        apart = 0.09 * apart / max(float(np.linalg.norm(apart)), 1e-6)
        up = np.array([0.0, 0.0, 0.02])
        yield from self.move_palms([self.palms[0] + apart + up, self.palms[1] - apart + up], 0.7)
        g.height_goal = STAND_HEIGHT
        g.rpy_goal = np.zeros(3)
        yield from self.retreat()          # clear of the shelf first, then let the arms down
        yield from self.relax(1.0)


def _slerp(a: np.ndarray, b: np.ndarray, s: float) -> np.ndarray:
    """Rotation part way from matrix a to matrix b."""
    qa, qb = np.zeros(4), np.zeros(4)
    mujoco.mju_mat2Quat(qa, a.reshape(9))
    mujoco.mju_mat2Quat(qb, b.reshape(9))
    if qa @ qb < 0:
        qb = -qb
    dot = min(1.0, float(qa @ qb))
    if dot > 0.9995:
        q = qa + s * (qb - qa)
    else:
        angle = np.arccos(dot)
        q = (np.sin((1 - s) * angle) * qa + np.sin(s * angle) * qb) / np.sin(angle)
    q /= np.linalg.norm(q)
    out = np.zeros(9)
    mujoco.mju_quat2Mat(out, q)
    return out.reshape(3, 3)
