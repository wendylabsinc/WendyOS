import unittest

from palm_reward import (
    eligible_palm_contact,
    is_inside_palm_region,
    update_palm_progress,
)


class PalmRewardTest(unittest.TestCase):
    def test_inside_face_contact_and_lift_gates(self):
        self.assertTrue(is_inside_palm_region((0.09, 0.015, 0.0)))
        self.assertFalse(is_inside_palm_region((0.09, -0.015, 0.0)))
        self.assertTrue(eligible_palm_contact(2.0, 0.0))
        self.assertTrue(eligible_palm_contact(1_000_000.0, 0.0))
        self.assertFalse(eligible_palm_contact(2.0, 0.03))

    def test_one_second_streak_earns_exactly_one_bonus(self):
        streak = best = reward_total = 0.0
        complete = False
        for _ in range(40):
            streak, best, complete, reward = update_palm_progress(
                streak, best, complete, True, 0.025,
            )
            reward_total += reward
        self.assertTrue(complete)
        self.assertAlmostEqual(best, 1.0)
        self.assertAlmostEqual(reward_total, 1.0)
        _, _, _, extra = update_palm_progress(streak, best, complete, True, 0.025)
        self.assertEqual(extra, 0.0)

    def test_broken_contact_cannot_farm_repeated_partial_rewards(self):
        streak = best = reward_total = 0.0
        complete = False
        for _ in range(20):
            streak, best, complete, reward = update_palm_progress(
                streak, best, complete, True, 0.025,
            )
            reward_total += reward
        streak, best, complete, reward = update_palm_progress(
            streak, best, complete, False, 0.025,
        )
        reward_total += reward
        for _ in range(20):
            streak, best, complete, reward = update_palm_progress(
                streak, best, complete, True, 0.025,
            )
            reward_total += reward
        self.assertAlmostEqual(reward_total, 0.5)
        self.assertFalse(complete)


if __name__ == "__main__":
    unittest.main()
