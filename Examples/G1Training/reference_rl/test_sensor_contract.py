import pytest
import torch
from sensor_contract import camera_packet,joint_features,freeze_contract,RealSensorAdapter,visible_can_mask


def test_camera_contract_and_real_sim_parity():
    rgb=torch.randint(0,256,(2,240,320,3),dtype=torch.uint8)
    depth=torch.rand(2,240,320)*7;mask=torch.zeros(2,240,320,dtype=torch.bool);mask[0,80:150,100:170]=True
    sim=camera_packet(rgb.float()/255,depth,mask,10.)
    real=RealSensorAdapter(freeze_contract({'joint_mean':[0.]*131,'joint_std':[1.]*131},[f'j{i}' for i in range(43)])).pack_camera(rgb,depth,mask,10.,10.025,[True,False])
    torch.testing.assert_close(sim.image,real.image,rtol=0,atol=0)
    assert sim.image.shape==(2,5,240,320) and sim.detection_valid.tolist()==[True,False]
    torch.testing.assert_close(sim.age_at(10.025),torch.full((2,),.025,dtype=torch.float64))
    assert sim.image[:,3].max()<=1 and sim.image[1,4].count_nonzero()==0
    invalid=camera_packet(rgb.float()/255,depth,mask,10.,[False,False]);assert not invalid.image[:,4].any()


def test_joint_layout_validity_clock_and_normalization_identity():
    names=[f'joint_{i}' for i in range(43)];normalization={'joint_mean':[0.]*131,'joint_std':[1.]*131}
    c=freeze_contract(normalization,names);assert c==freeze_contract(normalization,names)
    adapter=RealSensorAdapter(c);rgb=torch.zeros(1,240,320,3,dtype=torch.uint8);d=torch.zeros(1,240,320);mask=torch.zeros_like(d,dtype=torch.bool)
    packet=adapter.pack_camera(rgb,d,mask,0.,0.,[False]);q=torch.arange(43).float()[None];dq=torch.zeros_like(q)
    actual=adapter.pack_joints(names,q,dq,packet,0.);expected=joint_features(q,dq,dq,False,0.,0.)
    torch.testing.assert_close(actual,expected);assert actual.shape==(1,131)
    for frame in range(1,10):actual=adapter.pack_joints(names,q,dq,packet,frame*.025)
    assert actual[0,129]==1 and actual[0,130]==pytest.approx(.225)
    assert not packet.detection_valid[0]  # not the acceleration-valid scalar
    with pytest.raises(ValueError,match='Joint order'):adapter.pack_joints(names[::-1],q,dq,packet,.25)
    with pytest.raises(ValueError,match='Future camera'):adapter.pack_camera(rgb,d,mask,2.,1.,[True])


def test_only_visible_can_geom_hits_become_mask_pixels():
    # Includes a matching ID of a different object type and hidden/background pixels.
    seg=torch.tensor([[[[4,5],[8,5],[4,9],[-1,-1],[9,5]]]])
    mask=visible_can_mask(seg,torch.tensor([4,9]),5)
    assert mask.tolist()==[[[True,False,False,False,True]]]


def test_adapter_rejects_modified_contract():
    c=freeze_contract({'joint_mean':[0.]*131,'joint_std':[1.]*131},[f'j{i}' for i in range(43)])
    c['depth_quantization_m']=1
    with pytest.raises(ValueError,match='frozen specification'):RealSensorAdapter(c)
