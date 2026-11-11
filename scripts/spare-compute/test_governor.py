import copy
import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

import governor
sys.path.insert(0, str(Path(__file__).parent / "model"))
from lease_runner import lease_valid, admission_mode, permitted


class AlwaysOnTests(unittest.TestCase):
    def test_always_mode_runs_both_lanes_without_a_supervisor(self):
        with patch("lease_runner.lease_valid", side_effect=AssertionError("must not read permissions")):
            self.assertTrue(permitted("/missing", "cpu", "always"))
            self.assertTrue(permitted("/missing", "gpu", "always"))

    def test_default_stays_automatic_and_typo_does_not_bypass(self):
        with patch.dict("os.environ", {}, clear=True):
            self.assertEqual(admission_mode(), "auto")
            self.assertFalse(permitted("/missing", "cpu", admission_mode()))
        with patch.dict("os.environ", {"ADMISSION_MODE": "alway"}):
            with self.assertRaises(ValueError):
                admission_mode()
        with self.assertRaises(ValueError):
            permitted("/missing", "cpu", "alway")


class AdmissionTests(unittest.TestCase):
    def sample(self, second, foreign=0, own=0, **changes):
        # 4 CPUs, HZ=100. Own work is cumulative CPU-seconds across all services.
        sample = {"time": second, "boot_id": "boot", "cpu_count": 4,
                  "cpu_total": second * 400, "cpu_idle": second * 400 - (foreign + own) * 100,
                  "own_cpu_usec": own * 1e6, "groups": ["/our-worker", "/our-model"],
                  "memory_available": 200 * 1024**3, "gpu_known": True, "gpu_present": True,
                  "gpus": [{"index": 0, "uuid": "GPU-0", "foreign": False, "supported": True,
                            "utilization": 0, "free_mib": 200000, "total_mib": 288000}]}
        sample.update(changes)
        return sample

    def update(self, policy, sample):
        with patch("governor.time.monotonic", return_value=sample["time"]):
            return policy.update(sample)

    def admitted(self):
        policy = governor.Policy(idle_seconds=4)
        for tick in range(0, 8, 2):
            result = self.update(policy, self.sample(tick))
        self.assertTrue(result["cpu"] and result["gpu"])
        return policy

    def test_waits_for_sustained_idle(self):
        policy = governor.Policy(idle_seconds=4)
        for tick in (0, 2, 4):
            self.assertFalse(self.update(policy, self.sample(tick))["cpu"])
        self.assertTrue(self.update(policy, self.sample(6))["cpu"])

    def test_own_cpu_and_gpu_do_not_trigger_yield(self):
        policy = self.admitted()
        sample = self.sample(8, own=6)
        sample["gpus"][0].update(utilization=100, free_mib=1000)
        result = self.update(policy, sample)
        self.assertTrue(result["cpu"] and result["gpu"])

    def test_foreign_cpu_yields_then_requires_idle_again(self):
        policy = self.admitted()
        result = self.update(policy, self.sample(8, foreign=2))
        self.assertFalse(result["cpu"] or result["gpu"])
        self.assertFalse(self.update(policy, self.sample(10, foreign=2))["cpu"])
        self.assertFalse(self.update(policy, self.sample(12, foreign=2))["cpu"])
        self.assertTrue(self.update(policy, self.sample(14, foreign=2))["cpu"])

    def test_foreign_gpu_yields_even_at_zero_utilization(self):
        policy = self.admitted()
        sample = self.sample(8)
        sample["gpus"][0]["foreign"] = True
        result = self.update(policy, sample)
        self.assertTrue(result["cpu"])
        self.assertFalse(result["gpu"])

    def test_memory_pressure_yields(self):
        result = self.update(self.admitted(), self.sample(8, memory_available=1024**3))
        self.assertFalse(result["cpu"] or result["gpu"])

    def test_unknown_gpu_denies_inference_but_allows_cpu_work(self):
        result = self.update(self.admitted(), self.sample(8, gpu_known=False, gpus=[]))
        self.assertTrue(result["cpu"])
        self.assertFalse(result["gpu"])

    def test_cpu_host_never_grants_gpu(self):
        policy = governor.Policy(idle_seconds=0)
        self.update(policy, self.sample(0, gpu_known=False, gpu_present=False, gpus=[]))
        result = self.update(policy, self.sample(2, gpu_known=False, gpu_present=False, gpus=[]))
        self.assertTrue(result["cpu"])
        self.assertFalse(result["gpu"])

    def test_reboot_counter_reset_and_stale_probe_revoke(self):
        for change in ({"boot_id": "new"}, {"own_cpu_usec": -1}, {"groups": ["/different"]}):
            result = self.update(self.admitted(), self.sample(8, **change))
            self.assertFalse(result["cpu"] or result["gpu"])
        policy = self.admitted()
        with patch("governor.time.monotonic", return_value=100):
            result = policy.update(self.sample(8))
        self.assertFalse(result["cpu"] or result["gpu"])

    def test_unified_memory_requires_explicit_budget_and_reserve(self):
        for budget, available, granted in [(0, 100, False), (80, 83, False), (80, 100, True)]:
            policy = governor.Policy(idle_seconds=0, unified_model_gib=budget)
            for tick in (0, 2):
                sample = self.sample(tick, memory_available=available * 1024**3)
                sample["gpus"][0].update(unified=True, free_mib=None)
                result = self.update(policy, sample)
            self.assertEqual(result["gpu"], granted)
            if granted:
                sample = self.sample(4, memory_available=3 * 1024**3)
                sample["gpus"][0].update(unified=True, free_mib=None)
                self.assertFalse(self.update(policy, sample)["gpu"])

    def test_missing_expired_future_and_reboot_leases_denied(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "lease.json"
            self.assertFalse(lease_valid(path, "cpu", 10, "boot"))
            for grant in ({"cpu": True, "boot_id": "boot", "issued": 1},
                          {"cpu": True, "boot_id": "boot", "issued": 20},
                          {"cpu": True, "boot_id": "previous-boot", "issued": 9}):
                path.write_text(json.dumps(grant))
                self.assertFalse(lease_valid(path, "cpu", 10, "boot"))
            path.write_text(json.dumps({"cpu": True, "gpu": False, "boot_id": "boot", "issued": 9}))
            self.assertTrue(lease_valid(path, "cpu", 10, "boot"))
            self.assertFalse(lease_valid(path, "gpu", 10, "boot"))

class HostTransportTests(unittest.TestCase):
    def test_stats_wire_call_and_missing_stats(self):
        try:
            import grpc
            import resource_stats_pb2 as pb
        except ImportError:
            self.skipTest("run inside the built image for protobuf and gRPC")
        from concurrent.futures import ThreadPoolExecutor
        response = pb.GetResourceStatsResponse(
            host=pb.HostStats(cpu_total_jiffies=1000, cpu_idle_jiffies=900,
                             cpu_count=4, mem_total_bytes=10000, mem_available_bytes=9000),
            containers=[pb.ResourceContainerStats(app_name="our_app_worker", cpu_usage_nanos=2000000),
                        pb.ResourceContainerStats(app_name="our_app_model", cpu_usage_nanos=3000000),
                        pb.ResourceContainerStats(app_name="our_app_other_worker", cpu_usage_nanos=9000000)])
        with tempfile.TemporaryDirectory() as directory:
            socket = str(Path(directory) / "agent.sock")
            server = grpc.server(ThreadPoolExecutor(max_workers=1))
            server.add_generic_rpc_handlers((grpc.method_handlers_generic_handler(
                "wendy.agent.services.v1.WendyContainerService", {"GetResourceStats": grpc.unary_unary_rpc_method_handler(
                    lambda request, context: response,
                    request_deserializer=pb.GetResourceStatsRequest.FromString,
                    response_serializer=pb.GetResourceStatsResponse.SerializeToString)}),))
            server.add_insecure_port("unix:" + socket)
            server.start()
            try:
                stream = governor.samples(socket, "our_app")
                with patch("governor.Path.read_text", return_value="boot"):
                    sample = next(stream)
                stream.close()
                self.assertEqual(sample["own_cpu_usec"], 5000)
                self.assertEqual(sample["cpu_count"], 4)
                self.assertFalse(sample["gpu_known"])
                with self.assertRaises(ValueError):
                    governor.observation(response, "missing", 1)
                response.ClearField("host")
                with self.assertRaises(ValueError):
                    governor.observation(response, "our_app", 1)
            finally:
                server.stop(0).wait()


if __name__ == "__main__":
    unittest.main()
