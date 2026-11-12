import http.server
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from unittest.mock import patch

import benchmark
from compare import compare, markdown
from metrics import Process, summarize
import workloads
from pair import merge


HERE = Path(__file__).resolve().parent


def report(values=(10, 12, 11), metric="agent.process.cpu_seconds.percent_mean"):
    profile = {"scenarios": [{"name": "idle"}], "repetitions": len(values)}
    return {"schema": 1, "status": "ok", "revision": "test", "compatibility": {"profile": profile},
            "runs": [{"scenario": "idle", "repeat": i, "status": "ok", "metrics": {metric: v}}
                     for i, v in enumerate(values, 1)]}


class CompareTests(unittest.TestCase):
    def test_regression_improvement_and_noise(self):
        base = report()
        for values, status in (((15, 16, 17), "regression"), ((5, 6, 7), "improvement"), ((11, 12, 13), "within tolerance")):
            result = compare(base, report(values))
            self.assertEqual(result["rows"][0]["status"], status)
            self.assertIn("Baseline [range]", markdown(result))

    def test_zero_baseline_absolute_floor(self):
        result = compare(report((0, 0, 0)), report((.1, .2, .3)))
        self.assertEqual(result["rows"][0]["status"], "within tolerance")
        self.assertIsNone(result["rows"][0]["percent"])
        self.assertEqual(compare(report((0, 0, 0)), report((1, 1, 1)))["regressions"], 1)

    def test_higher_throughput_is_better(self):
        key = "workload.operations_per_second"
        result = compare(report((10, 10, 10), key), report((20, 20, 20), key))
        self.assertEqual(next(r for r in result["rows"] if r["metric"] == key)["status"], "improvement")

    def test_invalid_results_cannot_look_like_improvements(self):
        changes = [lambda r: r.update(status="failed"),
                   lambda r: r["runs"].pop(),
                   lambda r: r["runs"][0].update(status="failed"),
                   lambda r: r["runs"][0]["metrics"].clear(),
                   lambda r: r["compatibility"].update(environment="other-device"),
                   lambda r: r["runs"][0]["metrics"].update({"extra": 0}),
                   lambda r: r["runs"][0]["metrics"].update({"nan": float("nan")})]
        for change in changes:
            candidate = report()
            change(candidate)
            with self.assertRaises(ValueError):
                compare(report(), candidate)

    def test_invalid_policy(self):
        for policy in ({}, {"missing": {"relative": .1, "absolute": 1}},
                       {"cpu": {"relative": -1, "absolute": 1}}):
            with self.assertRaises(ValueError):
                compare(report(), report(), policy)

    def test_merge_repetitions_preserves_binary_and_environment(self):
        first = report((10,))
        first["binary_sha256"] = "first"
        second = report((20,))
        second["binary_sha256"] = "first"
        profile = {"repetitions": 2, "scenarios": [{"name": "idle"}]}
        result = merge([first, second], profile)
        self.assertEqual([r["repeat"] for r in result["runs"]], [1, 2])
        self.assertEqual(first["compatibility"]["profile"]["repetitions"], 1)
        second["binary_sha256"] = "other"
        with self.assertRaises(ValueError):
            merge([first, second], profile)

    def test_cli_exit_codes(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "a.json").write_text(json.dumps(report()))
            (root / "b.json").write_text(json.dumps(report((30, 30, 30))))
            cmd = [sys.executable, str(HERE / "benchmark.py"), "compare", str(root / "a.json"), str(root / "b.json")]
            result = subprocess.run(cmd + ["--output", str(root / "info")], capture_output=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            result = subprocess.run(cmd + ["--output", str(root / "gate"), "--fail-on-regression"], capture_output=True)
            self.assertEqual(result.returncode, 1, result.stderr)
            self.assertTrue((root / "gate/comparison.md").is_file())


class MetricsTests(unittest.TestCase):
    def test_delta_rates_and_memory_growth(self):
        samples = [{"time": t, "values": {"agent.process.cpu_seconds": cpu,
                                         "agent.process.rss_bytes": rss,
                                         "agent.process.write_bytes": io}}
                   for t, cpu, rss, io in ((10, 5, 100, 1000), (12, 6, 120, 1200), (14, 9, 140, 1600))]
        result = summarize(samples)
        self.assertEqual(result["agent.process.cpu_seconds.total"], 4)
        self.assertEqual(result["agent.process.cpu_seconds.percent_mean"], 100)
        self.assertEqual(result["agent.process.cpu_seconds.percent_p95"], 150)
        self.assertEqual(result["agent.process.rss_bytes.growth_per_second"], 10)
        self.assertEqual(result["agent.process.write_bytes.total"], 600)

    def test_missing_and_reset_counters(self):
        samples = [{"time": 1, "values": {"cpu_seconds": 10}}, {"time": 2, "values": {"cpu_seconds": 9}}]
        with self.assertRaisesRegex(ValueError, "reset"):
            summarize(samples)
        samples[1]["values"] = {}
        with self.assertRaisesRegex(ValueError, "available"):
            summarize(samples)

    def test_proc_stat_with_spaces_parentheses_and_pid_reuse(self):
        with tempfile.TemporaryDirectory() as directory:
            process = Process.__new__(Process)
            process.root = Path(directory)
            process.pid, process.start, process.ticks, process.cgroup = 123, None, 100, None
            fields = ["0"] * 22
            fields[0], fields[11], fields[12], fields[19] = "S", "150", "50", "500"
            stat = process.root / "stat"
            stat.write_text("123 (a tricky ) name) " + " ".join(fields))
            (process.root / "status").write_text("VmRSS:\t100 kB\nVmHWM:\t200 kB\n")
            data = process.sample()
            self.assertEqual(data["process.cpu_seconds"], 2)
            self.assertEqual(data["process.rss_bytes"], 102400)
            self.assertNotIn("process.read_bytes", data)
            fields[19] = "501"
            stat.write_text("123 (new) " + " ".join(fields))
            with self.assertRaisesRegex(ValueError, "reused"):
                process.sample()

    def test_cgroup_accounting_keeps_devices_and_memory_separate(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            process = Process.__new__(Process)
            process.root = root / "proc"
            process.root.mkdir()
            process.pid, process.start, process.ticks = 123, None, 100
            process.cgroup = root / "service"
            process.cgroup.mkdir()
            fields = ["0"] * 22
            fields[0], fields[19] = "S", "100"
            (process.root / "stat").write_text("123 (agent) " + " ".join(fields))
            (process.root / "status").write_text("VmRSS: 100 kB\n")
            for name, text in {
                "cpu.stat": "usage_usec 1500000\n",
                "memory.current": "2000000\n",
                "memory.stat": "anon 500000\nfile 1500000\n",
                "memory.events": "oom 0\noom_kill 0\n",
                "io.stat": "8:0 rbytes=1024 wbytes=2048 rios=1 wios=2\n8:16 rbytes=4096 wbytes=8192 rios=3 wios=4\n",
            }.items():
                (process.cgroup / name).write_text(text)
            with patch("metrics.cgroup_path", return_value=process.cgroup):
                data = process.sample()
            self.assertEqual(data["service.cpu_seconds"], 1.5)
            self.assertEqual(data["service.memory_bytes"], 2000000)
            self.assertEqual(data["process.rss_bytes"], 102400)
            self.assertEqual(data["service.io.8:0.wbytes"], 2048)
            self.assertEqual(data["service.io.8:16.wbytes"], 8192)
            self.assertNotIn("service.swap_bytes", data)

    def test_gauge_mean_weights_irregular_samples(self):
        result = summarize([{"time": t, "values": {"rss_bytes": v}} for t, v in ((0, 0), (1, 10), (10, 10))])
        self.assertEqual(result["rss_bytes.mean"], 9.5)


class WorkloadTests(unittest.TestCase):
    def test_profiles_and_required_verification(self):
        for path in (HERE / "profiles").glob("*.json"):
            benchmark.validate_profile(json.loads(path.read_text()))
        profile = json.loads((HERE / "profiles/smoke.json").read_text())
        profile["scenarios"] = [{"name": "deploy", "kind": "command", "command": ["true"], "operations": 1}]
        with self.assertRaisesRegex(ValueError, "verify"):
            benchmark.validate_profile(profile)
        profile["scenarios"] = [{"name": "../escape", "kind": "idle", "seconds": 1}]
        with self.assertRaises(ValueError):
            benchmark.validate_profile(profile)

    def test_command_failure_and_timeout(self):
        with tempfile.TemporaryDirectory() as directory:
            log = Path(directory) / "output.log"
            with self.assertRaisesRegex(ValueError, "exited 7"):
                workloads.command([sys.executable, "-c", "raise SystemExit(7)"], 2, log, threading.Event())
            before = time.monotonic()
            with self.assertRaisesRegex(ValueError, "timed out"):
                workloads.command([sys.executable, "-c", "import time; time.sleep(60)"], .1, log, threading.Event())
            self.assertLess(time.monotonic() - before, 4)

    def test_failed_verify_invalidates_custom_workload(self):
        scenario = {"kind": "command", "command": [sys.executable, "-c", "pass"],
                    "verify": [sys.executable, "-c", "raise SystemExit(1)"], "timeout_seconds": 2, "operations": 1}
        with tempfile.TemporaryDirectory() as directory:
            with self.assertRaises(ValueError):
                workloads.run(scenario, Path(directory) / "log", threading.Event())

    def test_telemetry_success_and_rejection(self):
        status = [200]
        bodies = []
        class Handler(http.server.BaseHTTPRequestHandler):
            def do_POST(self):
                bodies.append(self.rfile.read(int(self.headers["Content-Length"])))
                self.send_response(status[0])
                self.end_headers()
            def log_message(self, *args):
                pass
        server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        thread = threading.Thread(target=server.serve_forever)
        thread.start()
        scenario = {"endpoint": f"http://127.0.0.1:{server.server_port}/v1/logs", "batch": 2,
                    "payload_bytes": 10, "requests": 2, "requests_per_second": 100, "max_lag_seconds": 1}
        try:
            result = workloads.telemetry(scenario, threading.Event())
            self.assertEqual(result["workload.accepted_records"], 4)
            self.assertIn(b"wendy-agent-benchmark", bodies[0])
            status[0] = 500
            with self.assertRaisesRegex(ValueError, "HTTP 500"):
                workloads.telemetry(scenario, threading.Event())
        finally:
            server.shutdown()
            server.server_close()
            thread.join()


if __name__ == "__main__":
    unittest.main()
