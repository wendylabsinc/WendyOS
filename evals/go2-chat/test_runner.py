import argparse
import importlib.util
from pathlib import Path
import unittest
import json
import tempfile

spec = importlib.util.spec_from_file_location("go2_eval_runner", Path(__file__).with_name("run.py"))
runner = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runner)


class RunnerBoundaryTests(unittest.TestCase):
    def test_only_literal_loopback_model_endpoints(self):
        for endpoint in ["http://127.0.0.1:11434/v1", "http://[::1]:11434/v1/"]:
            self.assertEqual(runner.loopback_url(endpoint), endpoint.rstrip("/"))
        for endpoint in ["http://192.0.2.2:11434/v1", "http://localhost:11434/v1",
                         "https://127.0.0.1:11434/v1", "http://127.0.0.1/v1",
                         "http://127.0.0.1:11434/v1?key=secret", "http://user@127.0.0.1:11434/v1",
                         "http://[::ffff:192.0.2.2]:11434/v1", "file:///tmp/model", "http://127.0.0.1:11434"]:
            with self.subTest(endpoint=endpoint), self.assertRaises(argparse.ArgumentTypeError):
                runner.loopback_url(endpoint)

    def test_redirect_cannot_escape_loopback(self):
        with self.assertRaisesRegex(RuntimeError, "redirects are forbidden"):
            runner.NoRedirect().redirect_request(None, None, 302, "Found", {}, "http://192.0.2.2/")

    def test_skipped_or_incomplete_runs_cannot_pass(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)
            with self.assertRaisesRegex(RuntimeError, "missing or unexpected"):
                runner.summarize_reports(path, "model", {"stop"})
            (path / "real-model-stop.json").write_text(json.dumps({"case": {"id": "stop"},
                "passed": False, "violations": ["robot still moving"]}))
            result = runner.summarize_reports(path, "model", {"stop"})
            self.assertEqual((result["passed"], result["failed"]), (0, 1))


if __name__ == "__main__":
    unittest.main()
