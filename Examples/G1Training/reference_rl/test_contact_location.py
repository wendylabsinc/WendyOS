import unittest

import numpy as np

from contact_location import (
    TOP_CONTACT_PENALTY_RATE,
    classify_can_contact,
    top_contact_penalty,
)


class ContactLocationTest(unittest.TestCase):
    def test_top_side_and_bottom_use_can_local_z(self):
        half_height = 0.061
        self.assertEqual(classify_can_contact([0, 0, 0.061], half_height), 0)
        self.assertEqual(classify_can_contact([0.033, 0, 0.0529], half_height), 1)
        self.assertEqual(classify_can_contact([0, 0, -0.061], half_height), 2)

    def test_top_penalty_is_time_fraction_not_force_scaled(self):
        penalty = top_contact_penalty(np.array([0, 5, 25]), 25, 0.025)
        np.testing.assert_allclose(penalty, [0, -0.01, -0.025 * TOP_CONTACT_PENALTY_RATE])

    def test_invalid_counts_are_rejected(self):
        with self.assertRaises(ValueError):
            top_contact_penalty([26], 25, 0.025)


if __name__ == "__main__":
    unittest.main()
