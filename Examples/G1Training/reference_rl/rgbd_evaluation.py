"""Privileged simulator-only contact audit; never imported by the visual actor."""
from __future__ import annotations

import mujoco
import numpy as np


class ContactAudit:
    """Inspect every physics step for unintended robot/environment contacts.

    Intended contacts are right-hand/can, same-hand finger contacts, foot/floor,
    and can/tabletop support. Other contacts are rejected once penetration is
    >1 mm or resolved force is >2 N. These are simulation admission tolerances,
    not commissioned physical robot safety limits.
    """
    def __init__(self, model):
        self.model = model
        self.body_names = [model.body(i).name or f"body_{i}" for i in range(model.nbody)]
        self.geom_names = [model.geom(i).name or f"geom_{i}" for i in range(model.ngeom)]
        pelvis = model.body("pelvis").id
        self.robot = set()
        for body in range(model.nbody):
            ancestor = body
            while ancestor:
                if ancestor == pelvis:
                    self.robot.add(body)
                    break
                ancestor = int(model.body_parentid[ancestor])
        self.samples = 0
        self.violating_steps = 0
        self.max_penetration_m = 0.0
        self.max_force_n = 0.0
        self.violations = {}
        self.allowed_pairs = np.array([[self.allowed(i,j) for j in range(model.ngeom)] for i in range(model.ngeom)],dtype=bool)

    @staticmethod
    def hand(body, side):
        return body.startswith(f"{side}_hand_") or body == f"{side}_wrist_yaw_link"

    def allowed(self, g1, g2):
        m = self.model
        b1, b2 = int(m.geom_bodyid[g1]), int(m.geom_bodyid[g2])
        n1, n2 = self.body_names[b1], self.body_names[b2]
        a, b = self.geom_names[g1], self.geom_names[g2]
        if b1 in self.robot and b2 in self.robot:
            return any(self.hand(n1, side) and self.hand(n2, side)
                       for side in ("left", "right"))
        if b1 not in self.robot and b2 in self.robot:
            return self.allowed(g2, g1)
        if b1 in self.robot:
            return ((b == "bottle" and self.hand(n1, "right"))
                    or (b == "floor" and "ankle" in n1))
        if a == "bottle" or b == "bottle":
            other = b if a == "bottle" else a
            return other in ("table", "left_table", "right_table")
        return True

    def observe(self, data):
        self.samples += 1
        failed = False
        pairs=data.contact.geom
        for index in np.flatnonzero(~self.allowed_pairs[pairs[:,0],pairs[:,1]]):
            g1,g2=map(int,pairs[index])
            force = np.zeros(6)
            mujoco.mj_contactForce(self.model, data, index, force)
            force_n = float(np.linalg.norm(force[:3]))
            penetration = max(0.0, -float(data.contact.dist[index]))
            self.max_penetration_m = max(self.max_penetration_m, penetration)
            self.max_force_n = max(self.max_force_n, force_n)
            if penetration <= .001 and force_n <= 2:
                continue
            failed = True
            names = tuple(sorted((self.body_names[int(self.model.geom_bodyid[g1])] + "/" + self.geom_names[g1],
                                  self.body_names[int(self.model.geom_bodyid[g2])] + "/" + self.geom_names[g2])))
            key = " :: ".join(names)
            item = self.violations.setdefault(key, dict(pair=list(names), first_t=float(data.time),
                                                       samples=0, max_penetration_m=0., max_force_n=0.))
            item["samples"] += 1
            item["max_penetration_m"] = max(item["max_penetration_m"], penetration)
            item["max_force_n"] = max(item["max_force_n"], force_n)
        self.violating_steps += int(failed)

    def report(self):
        return dict(success=self.samples > 0 and not self.violations,
                    sampled_states=self.samples, violating_steps=self.violating_steps,
                    penetration_tolerance_m=.001, force_tolerance_n=2.,
                    max_unintended_penetration_m=self.max_penetration_m,
                    max_unintended_contact_force_n=self.max_force_n,
                    violations=list(self.violations.values()))
