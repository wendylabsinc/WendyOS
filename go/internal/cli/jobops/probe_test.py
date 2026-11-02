"""Integration contracts for the same fixed program used by CLI and MCP."""

import json, os, signal, subprocess, sys, tempfile, threading, time, unittest
from pathlib import Path

SCRIPT = Path(__file__).with_name("probe.py")


class ProbeTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.root = Path(self.tmp.name).resolve()
        (self.root / "status.json").write_text(
            json.dumps(
                {
                    "state": "running",
                    "stage": "control-train",
                    "runner_pid": 999999999,
                    "robot_commands_sent": 0,
                    "stages": [{"label": "selftest", "returncode": 0}],
                }
            )
        )

    def tearDown(self):
        self.tmp.cleanup()

    def probe(self, **args):
        p = subprocess.run(
            [sys.executable, str(SCRIPT), json.dumps({"path": str(self.root), **args})],
            capture_output=True,
            text=True,
            timeout=5,
        )
        self.assertEqual(p.returncode, 0, p.stderr + p.stdout)
        return json.loads(p.stdout.removeprefix("WENDY_JOB "))

    def test_missing_process_is_not_reported_live_and_repeat_is_compact(self):
        a = self.probe()
        self.assertEqual(a["process"]["status"], "missing")
        self.assertEqual(a["reported_state"], "running")
        self.assertEqual(a["health"], "not_running")
        b = self.probe(since=a["cursor"])
        self.assertFalse(b["changed"])
        self.assertNotIn("progress", b)
        self.assertLess(len(json.dumps(b)), 360)

    def test_progress_changes_cursor_even_when_stage_does_not_change(self):
        d = self.root / "control"
        d.mkdir()
        (d / "metrics.jsonl").write_text('{"iteration":1,"optimizer_steps_total":8}\n')
        a = self.probe()
        (d / "metrics.jsonl").write_text('{"iteration":2,"optimizer_steps_total":16}\n')
        b = self.probe(since=a["cursor"])
        self.assertTrue(b["changed"])
        self.assertEqual(b["progress"]["control"]["iteration"], 2)

    def test_wait_returns_for_progress_change_without_more_client_calls(self):
        a = self.probe()

        def change():
            time.sleep(0.15)
            (self.root / "status.json").write_text(
                '{"state":"failed","error":"OOM","stage":"control-train"}'
            )

        t = threading.Thread(target=change)
        t.start()
        b = self.probe(since=a["cursor"], wait_seconds=2)
        t.join()
        self.assertTrue(b["changed"])
        self.assertEqual(b["error"], "OOM")
        self.assertEqual(b["reported_state"], "failed")

    def test_torn_last_metric_record_preserves_last_complete_sample(self):
        d = self.root / "control"
        d.mkdir()
        (d / "metrics.jsonl").write_text('{"iteration":11}\n{"iteration":')
        a = self.probe()
        self.assertEqual(a["progress"]["control"]["iteration"], 11)
        self.assertIn("warnings", a)

    def test_bounded_summary_keeps_full_report_reference(self):
        data = json.loads((self.root / "status.json").read_text())
        data["baseline"] = [{"score": n} for n in range(1500)]
        (self.root / "status.json").write_text(json.dumps(data))
        a = self.probe()
        self.assertLess(len(json.dumps(a)), 2048)
        self.assertEqual(a["report"]["path"], str(self.root / "status.json"))
        self.assertEqual(len(a["report"]["sha256"]), 64)

    def test_pid_reuse_does_not_claim_identity(self):
        data = json.loads((self.root / "status.json").read_text())
        data["runner_pid"] = os.getpid()
        (self.root / "status.json").write_text(json.dumps(data))
        a = self.probe()
        self.assertIn(a["process"]["status"], ["identity_mismatch", "unknown"])
        self.assertIn(a["health"], ["not_running", "unknown"])

    def test_progress_budget_keeps_latest_samples_and_reports_omissions(self):
        for index in range(10):
            directory = self.root / ("step-" + str(index))
            directory.mkdir()
            metrics = directory / "metrics.jsonl"
            metrics.write_text(json.dumps({"iteration": index}) + "\n")
            os.utime(metrics, (1700000000 + index, 1700000000 + index))
        result = self.probe()
        self.assertEqual(len(result["progress"]), 8)
        self.assertEqual(result["progress_omitted"], 2)
        self.assertIn("step-9", result["progress"])
        self.assertNotIn("step-0", result["progress"])

    def test_failure_diagnostic_is_bounded_and_does_not_open_escaped_paths(self):
        status = {"state": "failed", "stages": [{"label": "resume", "returncode": 1, "timed_out": False}]}
        (self.root / "status.json").write_text(json.dumps(status))
        (self.root / "resume.log").write_text("noise"*5000 + "\nAssertionError: Resume source mismatch\n")
        result = self.probe(until_terminal=True)
        diagnostic = result["diagnostic"]
        self.assertIn("Resume source mismatch", diagnostic["excerpt"])
        self.assertLessEqual(len(diagnostic["excerpt"].encode()), 2048)
        self.assertEqual(self.probe(since=result["cursor"])["changed"], False)
        status["stages"][0]["label"] = "../../private"
        (self.root / "status.json").write_text(json.dumps(status))
        result = self.probe(until_terminal=True)
        self.assertNotIn("excerpt", result["diagnostic"])
        self.assertIn("invalid step label", result["diagnostic"]["unavailable"])

    def test_failure_diagnostic_refuses_symlinks_and_fifo(self):
        status = {"state": "failed", "stages": [{"label": "resume", "returncode": 1, "timed_out": False}]}
        (self.root / "status.json").write_text(json.dumps(status))
        target = self.root / "target.txt"
        target.write_text("private content")
        log = self.root / "resume.log"
        log.symlink_to(target)
        self.assertNotIn("excerpt", self.probe(until_terminal=True)["diagnostic"])
        log.unlink()
        if hasattr(os, "mkfifo"):
            os.mkfifo(log)
            self.assertNotIn("excerpt", self.probe(until_terminal=True)["diagnostic"])

    def test_future_metrics_are_not_warnings_but_missing_started_metrics_are(self):
        status = {"state":"running", "stage":"current", "execution_seconds":2.5,
                  "stages":[{"label":"finished", "returncode":0}],
                  "progress_files":{label:label+"/output/metrics.jsonl" for label in ["finished","current","future"]}}
        (self.root / "status.json").write_text(json.dumps(status))
        result=self.probe()
        self.assertEqual(result["warnings"], ["finished: declared progress file unavailable", "current: declared progress file unavailable"])
        self.assertNotIn("execution_seconds",result)
        self.assertEqual(result["recorded_execution_seconds"],2.5)
        status["state"]="completed"
        (self.root / "status.json").write_text(json.dumps(status))
        self.assertEqual(self.probe()["execution_seconds"],2.5)

    def test_declared_nested_progress_stays_confined_and_bounded(self):
        directory = self.root / "train" / "output"
        directory.mkdir(parents=True)
        (directory / "metrics.jsonl").write_text('{"iteration":65,"optimizer_steps_total":16512}\n')
        status = {"state": "running", "progress_files": {"train":"train/output/metrics.jsonl"}}
        (self.root / "status.json").write_text(json.dumps(status))
        self.assertEqual(self.probe()["progress"]["train"]["iteration"], 65)
        status["progress_files"]["train"] = "train/../../private.jsonl"
        (self.root / "status.json").write_text(json.dumps(status))
        self.assertNotIn("train", self.probe()["progress"])
        (self.root / "train" / "alias").symlink_to(directory, target_is_directory=True)
        status["progress_files"]["train"] = "train/alias/metrics.jsonl"
        (self.root / "status.json").write_text(json.dumps(status))
        self.assertNotIn("train", self.probe()["progress"])
        status["progress_files"] = None
        (self.root / "status.json").write_text(json.dumps(status))
        self.assertEqual(self.probe()["progress"], {})

    @unittest.skipUnless(sys.platform == "linux", "Linux process identity contract")
    def test_linux_runner_pause_and_start_time_are_verified(self):
        runner = self.root / "runner.py"
        runner.write_text("import time; time.sleep(30)")
        started = time.time()
        child = subprocess.Popen([sys.executable, str(runner)])
        try:
            (self.root / "status.json").write_text(
                json.dumps(
                    {
                        "state": "running",
                        "runner_pid": child.pid,
                        "started_unix": started,
                    }
                )
            )
            time.sleep(0.05)
            a = self.probe()
            self.assertEqual(a["process"]["status"], "live")
            self.assertEqual(a["health"], "unknown")
            os.kill(child.pid, signal.SIGSTOP)
            time.sleep(0.05)
            b = self.probe()
            self.assertEqual(b["process"]["status"], "paused")
            self.assertEqual(b["health"], "not_running")
            (self.root / "status.json").write_text(
                json.dumps(
                    {
                        "state": "running",
                        "runner_pid": child.pid,
                        "started_unix": started - 100,
                    }
                )
            )
            c = self.probe()
            self.assertEqual(c["process"]["status"], "identity_mismatch")
        finally:
            os.kill(child.pid, signal.SIGCONT)
            child.terminate()
            child.wait(timeout=3)


if __name__ == "__main__":
    unittest.main()
