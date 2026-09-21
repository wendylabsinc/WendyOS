import numpy as np
import pytest
import torch
from policy import compose,reference_at,ResidualPolicy,OWNED

def test_zero_is_exact_and_only_owned_joints_change():
    bounds=np.tile([-2.,2.],(43,1));ref=np.linspace(-1,1,43)
    assert np.array_equal(compose(ref,np.zeros(15),bounds),ref)
    target=compose(ref,np.full(15,100),bounds)
    np.testing.assert_allclose(target[OWNED]-ref[OWNED],.3)
    other=np.setdiff1d(np.arange(43),OWNED)
    assert np.array_equal(target[other],ref[other])
    assert np.array_equal(compose(ref,np.full(15,100),bounds),target)

def test_clip_and_reject():
    bounds=np.tile([-2.,2.],(43,1));ref=np.full(43,1.99)
    assert np.all(compose(ref,np.full(15,100),bounds)<=2)
    with pytest.raises(ValueError):compose(ref,np.full(15,np.nan),bounds)
    with pytest.raises(ValueError):compose(ref+1,np.zeros(15),bounds)

def test_reference_end_holds_and_uses_only_commands():
    actions=np.zeros((3,43));actions[1:]=.001
    r,v=reference_at(actions,0);assert r.shape==(43,)
    np.testing.assert_allclose(v,.04)
    r,v=reference_at(actions,99);assert np.array_equal(r,actions[-1]);assert not v.any()

def test_actor_starts_zero_and_has_trainable_weights():
    n=dict(joint_mean=[0.]*131,joint_std=[1.]*131,delta_mean=[[0.]*15]*8,delta_std=[[1.]*15]*8)
    model=ResidualPolicy(n);features=torch.randn(8,316)
    dist,value=model(features);assert torch.equal(dist.mean,torch.zeros_like(dist.mean))
    loss=-(dist.log_prob(torch.ones_like(dist.mean)*.01).sum(-1)).mean()+value.square().mean()
    loss.backward();assert model.actor.weight.grad.abs().sum()>0
    assert all(p.grad is None for p in model.encoder.parameters())

def test_smoothed_controller_primes_and_keeps_zero_reference_exact():
    from policy import ResidualController
    bounds=np.tile([-2.,2.],(43,1));controller=ResidualController(bounds)
    references=np.linspace(-.1,.1,1000)[:,None]*np.ones((1,43))
    for ref in references:assert np.array_equal(controller.step(ref,np.zeros(15)),ref)
    controller=ResidualController(bounds);ref=np.zeros(43)
    assert np.array_equal(controller.step(ref,np.ones(15)),ref)
    offsets=[]
    for i in range(200):offsets.append(controller.step(ref,np.ones(15))[OWNED])
    speeds=np.diff(offsets,axis=0)/.025
    assert np.max(abs(speeds))<=.025+1e-12
    assert np.max(abs(np.diff(speeds,axis=0)/.025))<=.05+1e-10

def test_advantages_do_not_bootstrap_or_leak_across_episode_end():
    from policy import advantages
    r=torch.tensor([1.,2.]);done=torch.tensor([1.,0.]);value=torch.tensor([.5,.25])
    adv,returns=advantages(r,done,value,torch.tensor(3.),gamma=.9,lam=.95)
    assert adv[0]==.5 and returns[0]==1.
    torch.testing.assert_close(returns[1],torch.tensor(4.7))

def test_release_uses_demonstrated_open_hand_not_curled_reset():
    from closed_loop_contract import ReleaseAudit
    reset=np.array([-.034,-.894,-1.525,1.533,1.704,1.248,1.7])
    grasp=np.array([0.,.113,-1.3,1.1,1.3,1.1,1.3])
    opened=np.array([0.,-.1,-.1,.05,.05,.05,.05])
    contacts={'right_hand_thumb_1_link','right_hand_middle_1_link'}
    audit=ReleaseAudit(opened,np.zeros(3),np.zeros(3))
    old=ReleaseAudit(reset,np.zeros(3),np.zeros(3))
    for instance in [audit,old]:instance.observe(1.,grasp,np.array([0.,0.,.05]),1.,contacts)
    target=grasp+.3*(opened-grasp)
    assert audit.observe(2.,target,np.array([0.,0.,.01]),1.,contacts)
    assert not old.observe(2.,target,np.array([0.,0.,.01]),1.,contacts)

def test_reference_release_detects_small_real_opening_before_contact_loss():
    from release_audit import ReferenceReleaseAudit
    grasp=np.array([0.,.113,-1.3,1.1,1.3,1.1,1.3]);opened=np.array([0.,-.1,-.1,.05,.05,.05,.05])
    audit=ReferenceReleaseAudit(opened,np.zeros(3),np.zeros(3))
    contact={'right_hand_thumb_1_link','right_hand_middle_1_link'}
    assert not audit.observe(0.,opened,np.array([0.,0.,.01]),1.,contact)
    audit.observe(1.,grasp,np.array([0.,0.,.05]),1.,contact)
    small=grasp+.04*(opened-grasp)
    assert not audit.observe(2.,small,np.array([.1,0.,.01]),1.,contact)
    assert audit.observe(3.,small,np.array([0.,0.,.01]),1.,contact)
    assert not audit.observe(4.,small,np.array([.1,0.,.01]),1.,set())
