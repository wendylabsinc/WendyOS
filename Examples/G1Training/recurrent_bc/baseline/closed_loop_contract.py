"""Causal online observation and event-only release audit. No expert clock."""
import math
import numpy as np
DT=.025
UPPER=np.r_[12:22,29:36]
HAND=np.arange(36,43)

class JointObservation:
 def __init__(self):self.previous=None;self.acc=np.zeros(43);self.index=0
 def observe(self,q,dq):
  if self.previous is not None:
   alpha=1-math.exp(-2*math.pi*5*DT)
   self.acc=(1-alpha)*self.acc+alpha*(dq-self.previous)/DT
  feature=np.r_[q,dq,self.acc,self.index>=8,(self.index%2)*DT].astype(np.float32)
  self.previous=np.array(dq,copy=True);self.index+=1
  return feature

class ReleaseAudit:
 """Privileged audit only: observe commanded opening after a real opposed lift.

 Opening is movement back towards the reset open hand along the grasp-to-open
 vector, at least .02 rad and 10% of that vector. It only permits a release in
 the original 4.5 cm XY / 2 cm Z placement corridor. Never supplied to actor.
 """
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
   if corridor and length>.02 and opening>=max(.02,.1*length):
    self.commanded=True
    if self.first_release is None:self.first_release=float(t)
  return bool(self.commanded and corridor)
