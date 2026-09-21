import unittest

import torch

from grip_reward import opposed_grip_score
from palm_reward import eligible_palm_contact


class NoForceCeilingTest(unittest.TestCase):
    def test_opposed_grip_reward_does_not_disappear_at_high_force(self):
        forces = torch.tensor([[2.0, 2.0, 1_000_000.0]])
        score = opposed_grip_score(forces, torch.tensor([True]))
        self.assertEqual(float(score[0]), 1.0)

    def test_palm_contact_does_not_disappear_at_high_force(self):
        self.assertTrue(eligible_palm_contact(1_000_000.0, 0.0))


if __name__ == "__main__":
    unittest.main()
