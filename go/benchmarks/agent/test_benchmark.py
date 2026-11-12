from contextlib import contextmanager
import http.server
import json
import os
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
from metrics import Process, host_sample, summarize
import workloads
from pair import merge


HERE = Path(__file__).resolve().parent


@contextmanager
def accepting_otlp_server():
    class Handler(http.server.BaseHTTPRequestHandler):
        def do_POST(self):
            self.rfile.read(int(self.headers["Content-Length"]))
            self.send_response(200)
            self.end_headers()
        def log_message(self, *args):
            pass
    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    thread = threading.Thread(target=server.serve_forever)
    thread.start()
    try:
        yield f"http://127.0.0.1:{server.server_port}/v1/logs"
    finally:
        server.shutdown()
        server.server_close()
        thread.join()


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

    def test_cgroup_io_device_without_io_counts_as_zero(self):
        # io.stat omits a device until the cgroup's first I/O to it.
        key = "agent.service.io.8:0.wbytes.total"
        candidate = report((10, 10, 10))
        for run in candidate["runs"][1:]:
            run["metrics"][key] = 8 * 1024**2
        result = compare(report((10, 10, 10)), candidate, {key: {"relative": .1, "absolute": 1024**2}})
        row = result["rows"][0]
        self.assertEqual((row["baseline"], row["candidate"], row["status"]), (0, 8 * 1024**2, "regression"))

    def test_change_within_one_counter_step_is_noise(self):
        # One 10 ms tick over 0.9 s is 1.11 CPU percentage points.
        def ticks(values):
            result = report(values)
            for run in result["runs"]:
                run["resolution"] = {"agent.process.cpu_seconds.percent_mean": 1.11}
            return result
        self.assertEqual(compare(ticks((0, 0)), ticks((0, 1.11)))["rows"][0]["status"], "within tolerance")
        self.assertEqual(compare(ticks((0, 0)), ticks((2.22, 2.22)))["rows"][0]["status"], "regression")

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

    def test_malformed_input_exits_2_not_regression(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "a.json").write_text(json.dumps(report()))
            (root / "list.json").write_text("[]")
            (root / "policy.json").write_text(json.dumps({"agent.process.rss_bytes.peak": .1}))
            cmd = [sys.executable, str(HERE / "benchmark.py"), "compare", "--fail-on-regression"]
            for args in ([str(root / "a.json"), str(root / "a.json"), "--policy", str(root / "policy.json")],
                         [str(root / "list.json"), str(root / "a.json")]):
                result = subprocess.run(cmd + args + ["--output", str(root / "out")], capture_output=True, text=True)
                self.assertEqual(result.returncode, 2, result.stderr)
                self.assertNotIn("Traceback", result.stderr)

    def pair_without_docker(self, root, *args):
        (root / "agent").write_bytes(b"")
        result = subprocess.run(
            [sys.executable, str(HERE / "pair.py"), "--baseline", str(root / "agent"),
             "--candidate", str(root / "agent"), "--output", str(root / "out"), *args],
            capture_output=True, text=True, env={"PATH": str(root)})  # no docker on PATH
        self.assertEqual(result.returncode, 2, result.stderr)
        self.assertFalse((root / "out").exists())
        return result.stderr

    def test_pair_rejects_policy_before_running_docker(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "policy.json").write_text(json.dumps({"agent.process.rss_bytes.peak": .1}))
            self.assertIn("policy", self.pair_without_docker(root, "--policy", str(root / "policy.json")))

    def test_pair_rejects_hooks_it_cannot_run(self):
        # Hook paths name device files; the Docker fixture has none of them.
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            profile = json.loads((HERE / "profiles/smoke.json").read_text())
            profile["scenarios"][1].update(verify=["/opt/agent-bench/check"], timeout_seconds=5)
            (root / "profile.json").write_text(json.dumps(profile))
            self.assertIn("hook", self.pair_without_docker(root, "--profile", str(root / "profile.json")))


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

    def test_cgroup_io_device_counts_from_zero_when_it_first_appears(self):
        key = "agent.service.io.8:0.wbytes"
        samples = [{"time": 0, "values": {"agent.process.rss_bytes": 1}},
                   {"time": 1, "values": {"agent.process.rss_bytes": 1, key: 4096}},
                   {"time": 2, "values": {"agent.process.rss_bytes": 1, key: 8192}}]
        result = summarize(samples)
        self.assertEqual(result[key + ".total"], 8192)
        self.assertEqual(result[key + ".per_second"], 4096)
        del samples[2]["values"][key]
        with self.assertRaisesRegex(ValueError, "reset"):
            summarize(samples)

    @unittest.skipUnless(sys.platform == "linux", "reads /proc")
    def test_process_cpu_includes_reaped_children(self):
        # /proc/PID/io already includes reaped children; CPU must match it.
        process = Process(os.getpid())
        before = process.sample()
        subprocess.run([sys.executable, "-c", "import time\nt = time.process_time()\nwhile time.process_time() - t < .3: pass"],
                       check=True)
        after = process.sample()
        self.assertGreaterEqual(after["process.cpu_seconds"] - before["process.cpu_seconds"], .25)
        self.assertGreaterEqual(after["process.children_cpu_seconds"] - before["process.children_cpu_seconds"], .25)

    @unittest.skipUnless(sys.platform == "linux", "reads /proc")
    def test_service_mode_without_cgroup_v2_path_fails_clearly(self):
        # cgroup v1 or hybrid host: /proc/PID/cgroup has no usable 0:: path.
        with patch("metrics.cgroup_path", return_value=None):
            with self.assertRaisesRegex(ValueError, "--pid"):
                Process(os.getpid(), include_cgroup=True)

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

    def test_unreadable_sensors_are_skipped(self):
        # A thermal zone can fail with EIO/ENODATA (e.g. iwlwifi with the radio
        # off); reading a directory raises a real OSError the same way.
        with tempfile.TemporaryDirectory() as directory:
            sysfs = Path(directory)
            (sysfs / "class/thermal/thermal_zone0/temp").mkdir(parents=True)
            (sysfs / "class/thermal/thermal_zone1").mkdir()
            (sysfs / "class/thermal/thermal_zone1/temp").write_text("42500\n")
            (sysfs / "devices/system/cpu/cpu0/cpufreq").mkdir(parents=True)
            (sysfs / "devices/system/cpu/cpu0/cpufreq/scaling_cur_freq").write_text("<unsupported>\n")
            data = host_sample(sysfs)
        self.assertEqual(data["temperature.thermal_zone1_c"], 42.5)
        self.assertNotIn("temperature.thermal_zone0_c", data)
        self.assertFalse(any(key.startswith("frequency.") for key in data))

    def test_gauge_mean_weights_irregular_samples(self):
        result = summarize([{"time": t, "values": {"rss_bytes": v}} for t, v in ((0, 0), (1, 10), (10, 10))])
        self.assertEqual(result["rss_bytes.mean"], 9.5)


@unittest.skipUnless(sys.platform == "linux", "collection requires Linux")
class CollectTests(unittest.TestCase):
    def test_run_records_cpu_counter_resolution(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            profile = {"schema": 1, "repetitions": 1, "sample_seconds": .1, "settle_seconds": 0,
                       "cooldown_seconds": 0, "scenarios": [{"name": "idle", "kind": "idle", "seconds": .3}]}
            (root / "profile.json").write_text(json.dumps(profile))
            target = subprocess.Popen(["sleep", "30"])
            try:
                result = subprocess.run([sys.executable, str(HERE / "benchmark.py"), "run", "--pid", str(target.pid),
                                         "--profile", str(root / "profile.json"), "--environment", "test",
                                         "--revision", "test", "--output", str(root / "out")],
                                        capture_output=True, text=True, timeout=30)
            finally:
                target.kill()
                target.wait()
            self.assertEqual(result.returncode, 0, result.stderr)
            run = json.loads((root / "out/report.json").read_text())["runs"][0]
            tick = 1 / os.sysconf("SC_CLK_TCK")
            self.assertAlmostEqual(run["resolution"]["agent.process.cpu_seconds.percent_mean"],
                                   100 * tick / run["metrics"]["elapsed_seconds"])


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

    def test_leftover_child_gets_sigterm_grace_period(self):
        # The wrapper exits normally, leaving a helper that needs 0.3 s to stop.
        with tempfile.TemporaryDirectory() as directory:
            marker, ready = Path(directory) / "stopped", Path(directory) / "ready"
            helper = ("import pathlib, signal, time\n"
                      "def stop(*_):\n    time.sleep(.3)\n    pathlib.Path(%r).touch()\n    raise SystemExit\n"
                      "signal.signal(signal.SIGTERM, stop)\n"
                      "pathlib.Path(%r).touch()\n"
                      "time.sleep(30)\n") % (str(marker), str(ready))
            wrapper = ("import os, subprocess, sys, time\n"
                       "subprocess.Popen([sys.executable, '-c', %r])\n"
                       "while not os.path.exists(%r): time.sleep(.02)\n") % (helper, str(ready))
            workloads.command([sys.executable, "-c", wrapper], 10, Path(directory) / "log", threading.Event())
            self.assertTrue(marker.exists(), "helper was killed before its SIGTERM handler finished")

    def test_telemetry_lasts_its_full_offered_schedule(self):
        # 4 requests at 20/s occupy 0.2 s. Stopping at the last send gave 4/0.15 s.
        with accepting_otlp_server() as endpoint, tempfile.TemporaryDirectory() as directory:
            scenario = {"kind": "telemetry", "endpoint": endpoint, "batch": 1, "payload_bytes": 10,
                        "requests": 4, "requests_per_second": 20, "max_lag_seconds": 1}
            result = workloads.run(scenario, Path(directory) / "log", threading.Event())
        self.assertGreaterEqual(result["workload.seconds"], .2)
        self.assertLessEqual(result["workload.operations_per_second"], 20)

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
