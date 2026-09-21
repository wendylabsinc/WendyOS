import numpy as np
import pytest
import torch
from grip_reward import opposed_grip_score,reference_grip_schedule


def test_opposition_force_budget_and_release():
    # No contact, one side only, weak opposed contact, secure contact,
    # exactly 40 N, excess force, and intentional release.
    forces=torch.tensor([[0.,0.,0.],[10.,0.,10.],[0.,10.,10.],
                         [1.,10.,11.],[2.,2.,4.],[20.,20.,40.],
                         [20.,21.,41.],[20.,20.,40.]])
    enabled=torch.tensor([True]*7+[False])
    torch.testing.assert_close(opposed_grip_score(forces,enabled),torch.tensor([0.,0.,0.,.5,1.,1.,1.,0.]))


def test_reference_release_does_not_require_policy_to_open():
    hand=np.array([[0.],[0.],[0.],[.01],[.04],[1.]])
    can=np.array([[0.,0.,0.],[0.,0.,.1],[.5,0.,.1],
                  [1.,0.,0.],[1.,0.,0.],[1.,0.,0.]])
    schedule=reference_grip_schedule(hand,can,np.zeros(3),np.array([1.,0.,0.]))
    np.testing.assert_array_equal(schedule,[True,True,True,True,False,False])


def test_opening_away_from_placement_does_not_end_grip():
    hand=np.array([[0.],[0.],[.3],[.04],[1.]])
    can=np.array([[0.,0.,0.],[0.,0.,.1],[.5,0.,.1],[1.,0.,0.],[1.,0.,0.]])
    np.testing.assert_array_equal(reference_grip_schedule(hand,can,np.zeros(3),np.array([1.,0.,0.])),[True,True,True,False,False])


def test_ambiguous_reference_is_reported():
    with pytest.raises(ValueError,match='never lifts'):
        reference_grip_schedule(np.zeros((4,7)),np.zeros((4,3)),np.zeros(3),np.ones(3))
