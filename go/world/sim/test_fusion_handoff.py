"""Protocol checks without starting MuJoCo or native processes."""
import unittest
from sim.fusion_handoff import timestamp

class ClockTests(unittest.TestCase):
    def test_report_time_preserves_capture_nanoseconds(self):
        # Python datetime normally truncates nanoseconds to microseconds. A
        # truncated report time can make its own capture appear in the future.
        for seconds,expected in [(0.250000123,'2026-01-01T00:00:00.250000123Z'),
                                 (1.999999999,'2026-01-01T00:00:01.999999999Z'),
                                 (60.000000001,'2026-01-01T00:01:00.000000001Z')]:
            self.assertEqual(timestamp(seconds),expected)

if __name__=='__main__':unittest.main()
