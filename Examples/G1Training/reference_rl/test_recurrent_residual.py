import copy
import torch
from recurrent_residual import RecurrentResidualPolicy,initialize_recurrent,ordered_chunks
from recurrent_policy import RecurrentPolicy
from policy import ResidualPolicy


def normalization():
    return dict(joint_mean=[0.]*131,joint_std=[1.]*131,delta_mean=[[0.]*15]*8,delta_std=[[1.]*15]*8)


def test_migration_preserves_parent_and_reuses_memory():
    n=normalization();bc=RecurrentPolicy(n);parent=ResidualPolicy(n)
    torch.nn.init.normal_(bc.memory_projection.weight,std=.01)
    parent.encoder.load_state_dict(bc.base.state_dict())
    torch.nn.init.normal_(parent.actor.weight,std=.01)
    model=initialize_recurrent(dict(contract={'normalization':n},model=parent.state_dict()),dict(model=bc.state_dict()),'cpu')
    x=torch.randn(3,316);a,v=parent(x);b,w,_=model.step(x,model.initial_hidden(3))
    torch.testing.assert_close(a.mean,b.mean,rtol=0,atol=0);torch.testing.assert_close(v,w,rtol=0,atol=0)
    for k,val in bc.memory.state_dict().items():torch.testing.assert_close(model.memory.state_dict()[k],val)
    b.mean.square().sum().backward();assert model.memory_gate.grad.abs()>0


def test_ordered_sequence_matches_steps_and_reset_is_per_world():
    torch.manual_seed(13);m=RecurrentResidualPolicy(normalization())
    m.memory_gate.data.fill_(.5);torch.nn.init.normal_(m.actor.weight,std=.01)
    x=torch.randn(3,9,316);hidden=m.initial_hidden(3);means=[];logs=[]
    for t in range(9):
        d,_,hidden=m.step(x[:,t],hidden);means.append(d.mean);logs.append(d.log_prob(torch.zeros_like(d.mean)))
    seq,_,h=m.sequence(x,m.initial_hidden(3))
    torch.testing.assert_close(seq.mean,torch.stack(means,1));torch.testing.assert_close(h,hidden)
    torch.testing.assert_close(seq.log_prob(torch.zeros_like(seq.mean)),torch.stack(logs,1))
    left,_,h1=m.sequence(x[:,:4],m.initial_hidden(3));right,_,h2=m.sequence(x[:,4:],h1)
    torch.testing.assert_close(torch.cat([left.mean,right.mean],1),seq.mean)
    d,_,after=m.step(x[:,0],hidden,torch.tensor([True,False,False]))
    fresh,_,_=m.step(x[:,0],m.initial_hidden(3));continued,_,_=m.step(x[:,0],hidden)
    torch.testing.assert_close(d.mean[0],fresh.mean[0]);torch.testing.assert_close(d.mean[1:],continued.mean[1:])
    assert not torch.allclose(d.mean[1:],fresh.mean[1:])
    restored=RecurrentResidualPolicy(normalization());restored.load_state_dict(copy.deepcopy(m.state_dict()))
    check,_,_=restored.step(x[:,0],hidden,torch.tensor([True,False,False]));torch.testing.assert_close(check.mean,d.mean)


def test_chunks_never_cross_reset_and_cover_every_frame():
    chunks=list(ordered_chunks(128,[39,95],32))
    assert [t for a,b in chunks for t in range(a,b)]==list(range(128))
    assert all(not(a<reset<b) for a,b in chunks for reset in [39,95])
    assert all(b-a<=32 for a,b in chunks)


def test_memory_receives_gradients_and_worlds_do_not_mix():
    m=RecurrentResidualPolicy(normalization());m.memory_gate.data.fill_(.1)
    torch.nn.init.normal_(m.actor.weight,std=.01)
    x=torch.randn(3,7,316);hidden=torch.randn(1,3,128)
    d,v,_=m.sequence(x,hidden)
    (d.mean.square().mean()+v.square().mean()).backward()
    assert m.memory.weight_hh_l0.grad.abs().sum()>0
    assert m.memory_projection.weight.grad.abs().sum()>0
    assert all(p.grad is None for p in m.encoder.parameters())
    order=torch.tensor([2,0,1]);permuted,_,_=m.sequence(x[order],hidden[:,order])
    torch.testing.assert_close(permuted.mean,d.mean[order])
