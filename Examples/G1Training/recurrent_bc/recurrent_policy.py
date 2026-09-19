"""Causal full-episode memory with unchanged sensor inputs and action semantics."""
import pathlib,sys
sys.path.insert(0,str(pathlib.Path(__file__).with_name('baseline')))
import torch
from torch import nn
from visual_policy import VisualPolicy,OWNED,HORIZON

class RecurrentPolicy(nn.Module):
    def __init__(self,normalization,hidden_size=128):
        super().__init__()
        self.base=VisualPolicy(**{k:normalization[k] for k in ['joint_mean','joint_std','delta_mean','delta_std']})
        self.memory=nn.GRU(256,hidden_size,batch_first=True)
        self.memory_projection=nn.Linear(hidden_size,256)
        nn.init.zeros_(self.memory_projection.weight);nn.init.zeros_(self.memory_projection.bias)
        # Frozen encoders make a reusable sensor-only feature cache possible.
        for module in [self.base.vision,self.base.joints]:
            for p in module.parameters():p.requires_grad_(False)

    def encode(self,image,joints):
        normalized=((joints-self.base.joint_mean)/self.base.joint_std).clamp(-10,10)
        return torch.cat([self.base.vision(image),self.base.joints(normalized)],dim=-1)

    def sequence(self,features,positions,hidden=None):
        """B,T features; hidden persists across ordered chunks, never episodes."""
        memory,hidden=self.memory(features,hidden)
        fused=features+self.memory_projection(memory)
        delta=self.base.head(fused).reshape(*features.shape[:2],HORIZON,15)
        actions=positions.unsqueeze(-2)+delta*self.base.delta_std+self.base.delta_mean
        return actions,hidden

    def step(self,image,joints,hidden=None):
        features=self.encode(image,joints)
        output,hidden=self.sequence(features[:,None],joints[:,OWNED,None].transpose(1,2),hidden)
        return output[:,0],hidden

class StatefulActor(nn.Module):
    """One runtime instance per environment. Explicit reset at episode start."""
    def __init__(self,model):super().__init__();self.model=model;self.hidden=None;self.steps=0
    def reset(self):self.hidden=None;self.steps=0
    def forward(self,image,joints):
        actions,self.hidden=self.model.step(image,joints,self.hidden)
        self.hidden=self.hidden.detach();self.steps+=1
        return actions
