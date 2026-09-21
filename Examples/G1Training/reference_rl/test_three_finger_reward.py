import unittest

import torch

from grip_reward import three_finger_grip_score


class ThreeFingerRewardTest(unittest.TestCase):
    def test_thumb_index_and_middle_are_independently_required(self):
        forces = torch.tensor([
            [2.0, 2.0, 0.0],
            [2.0, 0.0, 2.0],
            [0.0, 2.0, 2.0],
            [1.0, 2.0, 2.0],
            [2.0, 2.0, 2.0],
        ])
        enabled = torch.ones(5, dtype=torch.bool)
        torch.testing.assert_close(
            three_finger_grip_score(forces, enabled),
            torch.tensor([0.0, 0.0, 0.0, 0.5, 1.0]),
        )

    def test_high_force_has_no_reward_ceiling(self):
        score = three_finger_grip_score(
            torch.tensor([[1_000_000.0, 1_000_000.0, 1_000_000.0]]),
            torch.tensor([True]),
        )
        self.assertEqual(float(score[0]), 1.0)

    def test_reward_stops_during_reference_release(self):
        score = three_finger_grip_score(
            torch.tensor([[2.0, 2.0, 2.0]]),
            torch.tensor([False]),
        )
        self.assertEqual(float(score[0]), 0.0)


if __name__ == "__main__":
    unittest.main()
