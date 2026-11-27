import unittest

from compare import compare, parse, select_adapter


class ComparisonTests(unittest.TestCase):
    def test_adapter_requires_matching_production_api(self):
        old = "func (s *Synchronizer) ReconcileBounded("
        current = "func (s *Synchronizer) seedAnnouncements("
        batch = "func (s *Synchronizer) receiveHaveBatch("
        self.assertEqual(select_adapter(old, ""), "base")
        self.assertEqual(select_adapter(current, ""), "head")
        self.assertEqual(select_adapter(current, batch), "batching")
        with self.assertRaisesRegex(ValueError, "unsupported synchronizer"):
            select_adapter("", batch)

    def test_known_regression_and_throughput_direction(self):
        samples = {
            "base": [{"benchmark/cpu/ns/op": 100, "benchmark/send/MB/s": 100}] * 3,
            "head": [{"benchmark/cpu/ns/op": 120, "benchmark/send/MB/s": 120}] * 3,
        }
        rows = {r["metric"]: r for r in compare(samples, 10)}
        self.assertTrue(rows["benchmark/cpu/ns/op"]["regression"])
        self.assertFalse(rows["benchmark/send/MB/s"]["regression"])

    def test_missing_measurement_is_not_zero(self):
        with self.assertRaisesRegex(ValueError, "missing metric"):
            compare({"base": [{"benchmark/x/ns/op": 1}] * 3, "head": [{}] * 3}, 10)

    def test_new_frame_kind_is_not_silently_ignored(self):
        rows = compare({"base": [{}] * 3, "head": [{"simulation/x/kind/gossip/data": 1}] * 3}, 10)
        self.assertTrue(rows[0]["regression"])
        self.assertIsNone(rows[0]["change_pct"])

    def test_combined_go_output_preserves_case_and_units(self):
        output = '''=== RUN   TestVirtualMeshRegression/line-8/slow=true
    fixture.go:1: MESH_RESULT {"scenario":"cold","control_frames":3,"frames_by_kind":{"gossip/have":2}}
BenchmarkVirtualCarrierCost/small         100 85.00 ns/op 1 allocs/op
PASS
'''
        metrics = parse(output)
        self.assertEqual(metrics["simulation/line-8/slow=true/cold/control_frames"], 3)
        self.assertEqual(metrics["benchmark/BenchmarkVirtualCarrierCost/small/ns/op"], 85)
        with self.assertRaisesRegex(ValueError, "simulation results missing"):
            parse("PASS\n")


if __name__ == "__main__":
    unittest.main()
