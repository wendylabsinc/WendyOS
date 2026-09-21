"""Sensor-conditioned residual RL around recorded, controller-owned references."""
from pathlib import Path
import sys
sys.path.insert(0,str(Path(__file__).resolve().parents[1]/'recurrent_bc'))
from recurrent_policy import RecurrentPolicy
from visual_policy import OWNED
import numpy as np
import torch
from torch import nn

DT=.025
RESIDUAL_FRACTION=.15

class ResidualPolicy(nn.Module):
    def __init__(self, normalization):
        super().__init__()
        self.encoder=RecurrentPolicy(normalization).base
        for p in self.encoder.parameters():p.requires_grad_(False)
        self.trunk=nn.Sequential(nn.Linear(316,256),nn.Tanh(),nn.Linear(256,128),nn.Tanh())
        self.actor=nn.Linear(128,15)
        self.value=nn.Linear(128,1)
        self.log_std=nn.Parameter(torch.full((15,),-3.))
        nn.init.zeros_(self.actor.weight);nn.init.zeros_(self.actor.bias)

    @torch.no_grad()
    def encode(self,image,joints):
        q=((joints-self.encoder.joint_mean)/self.encoder.joint_std).clamp(-10,10)
        return torch.cat((self.encoder.vision(image),self.encoder.joints(q)),dim=-1)

    def forward(self,features):
        x=self.trunk(features)
        return torch.distributions.Normal(self.actor(x),self.log_std.clamp(-5,-1).exp()),self.value(x).squeeze(-1)

    @staticmethod
    def inputs(sensor_features,reference,velocity,previous_residual,correction_velocity):
        return torch.cat((sensor_features,reference/3.,velocity/.25,previous_residual,correction_velocity/.025),dim=-1)


def compose(reference,raw_residual,bounds):
    """Offset from this frame's reference, never an accumulated position delta."""
    reference=np.asarray(reference,dtype=np.float64)
    raw=np.asarray(raw_residual,dtype=np.float64)
    bounds=np.asarray(bounds,dtype=np.float64)
    if reference.shape!=(43,) or raw.shape!=(15,) or bounds.shape!=(43,2):
        raise ValueError('Expected 43 reference joints, 15 residuals, 43 bounds')
    if not all(np.isfinite(x).all() for x in (reference,raw,bounds)):
        raise ValueError('Nonfinite controller input')
    if np.any(bounds[:,1]<=bounds[:,0]) or np.any(reference<bounds[:,0]-1e-8) or np.any(reference>bounds[:,1]+1e-8):
        raise ValueError('Invalid reference joint bounds')
    target=reference.copy()
    cap=RESIDUAL_FRACTION*(bounds[OWNED,1]-bounds[OWNED,0])/2.
    target[OWNED]=np.clip(reference[OWNED]+cap*np.tanh(raw),bounds[OWNED,0],bounds[OWNED,1])
    return target


def reference_at(actions,index):
    """A controller frame counter; no simulator object pose or source-time field."""
    if index<0:raise ValueError('Negative reference index')
    i=min(index,len(actions)-1)
    reference=actions[i]
    velocity=(actions[min(i+1,len(actions)-1),OWNED]-reference[OWNED])/DT
    return reference,velocity


class ResidualController:
    """Smooth only the learned offset; zero corrections leave the source intact.

    These are simulation exploration settings, not hardware qualification limits.
    The final combined motion is independently audited during evaluation.
    """
    def __init__(self,bounds):
        self.bounds=np.asarray(bounds,dtype=np.float64)
        self.cap=RESIDUAL_FRACTION*np.diff(self.bounds[OWNED],axis=1)[:,0]/2
        self.offset=np.zeros(15);self.velocity=np.zeros(15);self.steps=0

    def step(self,reference,raw):
        requested=compose(reference,raw,self.bounds)[OWNED]-reference[OWNED]
        if self.steps:
            wanted_velocity=np.clip((requested-self.offset)/DT,-.025,.025)
            self.velocity+=np.clip(wanted_velocity-self.velocity,-.05*DT,.05*DT)
            self.offset=np.clip(self.offset+self.velocity*DT,-self.cap,self.cap)
        target=np.array(reference,copy=True)
        target[OWNED]=np.clip(reference[OWNED]+self.offset,self.bounds[OWNED,0],self.bounds[OWNED,1])
        self.offset=target[OWNED]-reference[OWNED];self.steps+=1
        return target

    @property
    def normalized(self):
        return (self.offset/self.cap).astype(np.float32)


def advantages(reward,done,value,last,gamma=.9995,lam=.95):
    adv=torch.zeros_like(reward);carry=torch.zeros((),device=reward.device)
    for t in reversed(range(len(reward))):
        next_value=last if t==len(reward)-1 else value[t+1]
        live=1-done[t]
        delta=reward[t]+gamma*next_value*live-value[t]
        carry=delta+gamma*lam*live*carry;adv[t]=carry
    return adv,adv+value


def initialize(bc_checkpoint,device='cuda'):
    payload=torch.load(bc_checkpoint,map_location='cpu',weights_only=False)
    model=ResidualPolicy(payload['contract']['normalization'])
    if payload['contract'].get('architecture')=='full-episode-gru-residual-features-v1':
        state={k[len('base.'):]:v for k,v in payload['model'].items() if k.startswith('base.')}
    else:state=payload['model']
    model.encoder.load_state_dict(state)
    return model.to(device),payload['contract']
