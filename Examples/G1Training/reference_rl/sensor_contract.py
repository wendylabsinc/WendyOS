"""Shared camera/joint packing for simulator visibility and real YOLO masks.

Providers differ only in mask production. Actor dimensions remain 5x240x320
and 131 joint features. Detection validity is packet metadata enforced on the
mask; joint slot 129 remains acceleration validity, never detection validity.
"""
from dataclasses import dataclass
import hashlib,json
import torch

WIDTH=320
HEIGHT=240
SCHEMA='rgbd-visible-mask-reference-gru-v1'

def visible_can_mask(segmentation,can_geom_ids,geom_type):
    return (segmentation[...,1]==geom_type) & torch.isin(segmentation[...,0],can_geom_ids)



@dataclass
class CameraPacket:
    image: torch.Tensor
    detection_valid: torch.Tensor
    captured_at_s: torch.Tensor

    def age_at(self, control_at_s):
        return torch.as_tensor(control_at_s,device=self.image.device,dtype=torch.float64)-self.captured_at_s


def camera_packet(rgb, depth_m, mask, captured_at_s, detection_valid=None):
    """RGB float [0,1], aligned metric depth, binary mask; all in one frame."""
    batch=len(rgb)
    if rgb.shape!=(batch,HEIGHT,WIDTH,3) or depth_m.shape!=(batch,HEIGHT,WIDTH) or mask.shape!=depth_m.shape:
        raise ValueError('Expected aligned RGB/depth/mask at 320x240')
    if rgb.device!=depth_m.device or rgb.device!=mask.device:raise ValueError('Camera tensors must share a device')
    visible=mask.bool()
    valid=visible.flatten(1).any(-1)
    if detection_valid is not None:valid=valid & torch.as_tensor(detection_valid,device=rgb.device,dtype=torch.bool).reshape(batch)
    visible=visible & valid[:,None,None]
    depth=((depth_m*1000).round()*.001).clamp(0,5)/5
    image=torch.cat((rgb.float().clamp(0,1).permute(0,3,1,2),depth.float()[:,None],visible.float()[:,None]),dim=1)
    captured=torch.as_tensor(captured_at_s,device=rgb.device,dtype=torch.float64).expand(batch).clone()
    return CameraPacket(image,valid,captured)


def joint_features(q,dq,ddq,acceleration_valid,captured_at_s,control_at_s):
    batch=len(q)
    if q.shape!=(batch,43) or dq.shape!=q.shape or ddq.shape!=q.shape:raise ValueError('Expected 43 ordered joints')
    valid=torch.as_tensor(acceleration_valid,device=q.device,dtype=torch.float32).expand(batch)
    age=(torch.as_tensor(control_at_s,device=q.device,dtype=torch.float64)-torch.as_tensor(captured_at_s,device=q.device,dtype=torch.float64)).expand(batch).float()
    return torch.cat((q.float(),dq.float(),ddq.float(),valid[:,None],age[:,None]),-1)


def freeze_contract(normalization,joint_names):
    if len(joint_names)!=43 or len(set(joint_names))!=43:raise ValueError('Expected 43 unique joint names')
    spec=dict(schema=SCHEMA,image_shape=[5,HEIGHT,WIDTH],channels=['red','green','blue','depth','can_mask'],rgb='RGB float32 in [0,1]',
              depth_input_units='metres along the camera optical axis (Z depth)',depth_quantization_m=.001,depth_normalization='clip(depth_m,0,5)/5',invalid_depth_m=0,
              mask='binary visible pixels only; invalid detection gives all zeros',detection_valid='packet bool: provider accepted detection AND any mask pixel; represented to unchanged actor by the mask',
              captured_at_s='capture/exposure timestamp in the control clock domain; mask retains source RGB timestamp, never inference-completion time',
              camera_age_s='control timestamp minus capture timestamp; never fabricate a fresh timestamp for a cached frame',
              joint_names=list(joint_names),joint_feature_order=['q[43]','dq[43]','filtered_ddq[43]','acceleration_valid','camera_age_s'],joint_feature_dim=131,
              joint_units=['radians','radians/second','radians/second^2'],acceleration_filter='causal 5 Hz exponential lowpass of backward velocity difference; invalid for initial 0.2 s',
              normalization=normalization,feature_normalization='(joint_features-mean)/std clipped to [-10,10]',
              reference_inputs='15 nominal targets /3; 15 nominal velocities /.25; 15 previous normalized offsets; 15 offset velocities /.025',
              recurrent_memory='GRU 128, per environment; reset on episode start',actor_input_dim=316)
    encoded=json.dumps(spec,sort_keys=True,separators=(',',':'),allow_nan=False).encode()
    return dict(**spec,sha256=hashlib.sha256(encoded).hexdigest())


class RealSensorAdapter:
    """Non-actuating adapter for aligned real RGB-D and YOLO's visible mask.

    The camera driver must align depth into RGB pixels, resize/calibrate both
    to this grid, and map capture timestamps to the controller's clock first.
    This adapter never starts a camera, detector, policy, or motor writer.
    """
    def __init__(self,contract):
        if contract.get('schema')!=SCHEMA:raise ValueError('Unsupported sensor input contract')
        expected=freeze_contract(contract['normalization'],contract['joint_names'])
        if contract!=expected:raise ValueError('Sensor input contract does not match its frozen specification')
        self.contract=contract;self.previous_dq=None;self.ddq=None;self.started_at=None;self.previous_at=None

    def pack_camera(self,rgb_u8,depth_m,yolo_mask,captured_at_s,control_at_s,detection_valid):
        if yolo_mask.dtype!=torch.bool:raise ValueError('Pass the thresholded binary YOLO mask')
        if rgb_u8.dtype!=torch.uint8:raise ValueError('Real adapter requires RGB uint8; BGR is not accepted')
        packet=camera_packet(rgb_u8.float()/255,depth_m,yolo_mask,captured_at_s,detection_valid)
        if not bool(torch.isfinite(packet.image).all()):raise ValueError('Nonfinite camera input')
        if bool((packet.age_at(control_at_s)<0).any()):raise ValueError('Future camera timestamp; clocks must be aligned')
        return packet

    def pack_joints(self,names,q,dq,packet,control_at_s):
        if list(names)!=self.contract['joint_names']:raise ValueError('Joint order differs from frozen policy contract')
        q=q.float();dq=dq.float()
        now=float(control_at_s)
        if self.previous_at is None:
            self.started_at=now;self.ddq=torch.zeros_like(dq)
        else:
            dt=now-self.previous_at
            if dt<=0:raise ValueError('Joint samples must have increasing timestamps')
            import math
            self.ddq+=(1-math.exp(-2*math.pi*5*dt))*((dq-self.previous_dq)/dt-self.ddq)
        features=joint_features(q,dq,self.ddq,now-self.started_at>=.2-1e-9,packet.captured_at_s,now)
        if not bool(torch.isfinite(features).all()) or bool((features[:,-1]<0).any()):raise ValueError('Invalid joint/camera timing')
        self.previous_dq=dq.clone();self.previous_at=now
        return features
