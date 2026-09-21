"""Actual release-command evidence for a controller with a supplied reference."""
import numpy as np

class ReferenceReleaseAudit:
    def __init__(self,open_hand,origin,destination):
        self.open=np.array(open_hand);self.origin=np.array(origin);self.destination=np.array(destination)
        self.grasp=None;self.commanded=False;self.first_release=None;self.opposed=False

    def observe(self,t,hand,position,upright,contacts):
        opposed=any('right_hand_thumb' in n for n in contacts) and any('right_hand_index' in n or 'right_hand_middle' in n for n in contacts)
        self.opposed |= opposed
        if self.grasp is None and opposed and position[2]>self.origin[2]+.03:self.grasp=np.array(hand)
        corridor=np.linalg.norm(position[:2]-self.destination[:2])<.045 and abs(position[2]-self.destination[2])<.020 and upright>.95
        if self.grasp is not None:
            direction=self.open-self.grasp;length=np.linalg.norm(direction)
            opening=float(np.dot(hand-self.grasp,direction)/max(length,1e-12))
            # Absolute 0.02 rad projected command motion establishes that opening
            # was commanded. A fraction of the full seven-joint opening vector
            # depends on the final pose, and can delay detection until AFTER the
            # fingers have physically released. Reference 000001 loses thumb
            # contact after 0.0976 rad opening; the old 10% test required 0.261 rad.
            # No expert timestamp permits release: actual opening, prior opposed
            # lift, and the unchanged geometric corridor are all still required.
            if corridor and length>.02 and opening>=.02:
                self.commanded=True
                if self.first_release is None:self.first_release=float(t)
        return bool(self.commanded and corridor)
