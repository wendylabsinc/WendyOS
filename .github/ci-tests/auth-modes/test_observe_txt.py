import unittest

from observe_txt import observation


class TargetTXTTests(unittest.TestCase):
    def test_accepts_only_the_expected_host_and_shape(self):
        for shape in ("open", "closed", "enrolled"):
            output = f"service can be reached at dedicated-board.local.:50051\n access={shape} tls=true"
            self.assertEqual(
                observation(output, "dedicated-board.local", shape),
                {"resolvedHost": "dedicated-board.local", "access": shape},
            )
            self.assertIsNone(observation(output, "other-board.local", shape))
            other = "closed" if shape == "open" else "open"
            self.assertIsNone(observation(output, "dedicated-board.local", other))

    def test_refuses_missing_or_conflicting_record_data(self):
        for output in (
            "access=closed",  # no target binding
            "can be reached at dedicated-board.local.:50051",  # no shape
            "can be reached at dedicated-board.local.:50051 access=open access=closed",
            "can be reached at dedicated-board.local.attacker.invalid.:50051 access=closed",
        ):
            self.assertIsNone(observation(output, "dedicated-board.local", "closed"))


if __name__ == "__main__":
    unittest.main()
