"""CNN + measured-joint encoder; predicts absolute commands without expert phase."""
import numpy as np
import torch
from torch import nn
OWNED=[12]+list(range(29,43))
HORIZON=8

class VisualPolicy(nn.Module):
 def __init__(self,joint_mean,joint_std,delta_mean,delta_std):
  super().__init__()
  self.register_buffer('joint_mean',torch.as_tensor(joint_mean,dtype=torch.float32))
  self.register_buffer('joint_std',torch.as_tensor(joint_std,dtype=torch.float32))
  self.register_buffer('delta_mean',torch.as_tensor(delta_mean,dtype=torch.float32))
  self.register_buffer('delta_std',torch.as_tensor(delta_std,dtype=torch.float32))
  self.vision=nn.Sequential(nn.Conv2d(5,24,5,2,2),nn.SiLU(),nn.Conv2d(24,48,3,2,1),nn.SiLU(),nn.Conv2d(48,64,3,2,1),nn.SiLU(),nn.Conv2d(64,96,3,2,1),nn.SiLU(),nn.AdaptiveAvgPool2d((6,8)),nn.Flatten(),nn.Linear(96*6*8,128),nn.SiLU())
  self.joints=nn.Sequential(nn.Linear(131,128),nn.SiLU(),nn.Linear(128,128),nn.SiLU())
  self.head=nn.Sequential(nn.Linear(256,256),nn.SiLU(),nn.Linear(256,HORIZON*15))
  nn.init.zeros_(self.head[-1].weight);nn.init.zeros_(self.head[-1].bias)
 def forward(self,image,joints):
  assert image.ndim==4 and image.shape[1]==5 and joints.shape[1]==131
  normalized=((joints-self.joint_mean)/self.joint_std).clamp(-10,10)
  feature=torch.cat((self.vision(image),self.joints(normalized)),dim=1)
  delta=self.head(feature).reshape(-1,HORIZON,15)*self.delta_std+self.delta_mean
  return joints[:,OWNED,None].transpose(1,2)+delta

def make_image(rgb,depth,mask):
 rgb=np.asarray(rgb,np.float32)/255.
 depth=np.clip(np.asarray(depth,np.float32),0,5)/5.
 return np.concatenate((rgb,depth[...,None],np.asarray(mask,np.float32)[...,None]),axis=-1).transpose(0,3,1,2).copy()

def loss(prediction,target,scale):
 scale=scale.clamp_min(.02)
 position=((prediction-target)/scale).square().mean()
 dp=prediction[:,1:]-prediction[:,:-1];dt=target[:,1:]-target[:,:-1]
 velocity=((dp-dt)/.00625).square().mean()
 acceleration=(((dp[:,1:]-dp[:,:-1])-(dt[:,1:]-dt[:,:-1]))/.0003125).square().mean()
 return position+.05*velocity+.002*acceleration,{'position':position,'velocity':velocity,'acceleration':acceleration}
