"""Submission rejects supervisor log collisions before creating job storage."""

import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


class SubmitTests(unittest.TestCase):
    def test_runner_step_is_rejected_before_mutation(self):
        with tempfile.TemporaryDirectory() as directory:
            spec = {
                "name": "reserved-step",
                "working_directory": directory,
                "steps": [{"name": "runner", "argv": [sys.executable, "-c", "pass"], "timeout_seconds": 2}],
            }
            result = subprocess.run(
                [sys.executable, str(Path(__file__).with_name("submit.py")),
                 json.dumps({"spec": spec, "worker_source": ""})],
                capture_output=True, text=True, timeout=5,
            )
            self.assertNotEqual(result.returncode, 0, result.stdout)
            self.assertIn("reserved", result.stdout)
            self.assertFalse((Path(directory) / ".wendy").exists())


if __name__ == "__main__":
    unittest.main()
