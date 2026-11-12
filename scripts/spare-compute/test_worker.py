import io
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tarfile
import tempfile
import threading
import time
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer
from unittest.mock import patch

import review
import worker


class WorkerTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name).resolve()
        worker.STOP = False

    def tearDown(self):
        worker.STOP = False
        self.temp.cleanup()

    def test_partition_covers_every_job_exactly_once(self):
        keys = [str(i) for i in range(200)]
        shards = [{k for k in keys if worker.shard(k, i, 4)} for i in range(4)]
        self.assertEqual(set.union(*shards), set(keys))
        self.assertEqual(sum(map(len, shards)), len(keys))

    def test_extraction_rejects_escape_and_omits_links(self):
        archive = self.root / "source.tar"
        with tarfile.open(archive, "w") as out:
            member = tarfile.TarInfo("link")
            member.type = tarfile.SYMTYPE
            member.linkname = "/etc/passwd"
            out.addfile(member)
        worker.extract(archive, self.root / "work")
        self.assertFalse((self.root / "work/link").exists())
        with tarfile.open(archive, "w") as out:
            member = tarfile.TarInfo("../escape")
            member.size = 1
            out.addfile(member, io.BytesIO(b"x"))
        with self.assertRaises(ValueError):
            worker.extract(archive, self.root / "work")
        self.assertFalse((self.root / "escape").exists())

    def test_timeout_terminates_descendant(self):
        pidfile = self.root / "pid"
        script = ("import subprocess,time; p=subprocess.Popen(['sleep','60']); "
                  f"open({str(pidfile)!r},'w').write(str(p.pid)); time.sleep(60)")
        outcome = worker.run_process([sys.executable, "-c", script], self.root,
                                     os.environ.copy(), self.root, 0.5, 0)
        self.assertEqual(outcome["status"], "timeout")
        pid = int(pidfile.read_text())
        # On Linux an orphan can briefly remain as a zombie until init reaps it.
        procstat = Path(f"/proc/{pid}/stat")
        if procstat.exists():
            self.assertEqual(procstat.read_text().split()[2], "Z")
        else:
            with self.assertRaises(ProcessLookupError):
                os.kill(pid, 0)

    def test_interrupt_recorded_separately_from_failure(self):
        worker.STOP = True
        outcome = worker.run_process(["sleep", "60"], self.root, os.environ.copy(), self.root, 30, 0)
        self.assertEqual(outcome["status"], "interrupted")
        self.assertLess(outcome["seconds"], 5)

    def test_disk_pressure_stops_job(self):
        outcome = worker.run_process(["sleep", "60"], self.root, os.environ.copy(),
                                     self.root, 30, 10**30)
        self.assertEqual(outcome["status"], "disk_pressure")

    def test_path_validation(self):
        with self.assertRaises(ValueError):
            worker.contained(self.root, "../outside")
        self.assertEqual(worker.contained(self.root, "inside"), self.root / "inside")

    def test_failed_command_is_not_completed(self):
        outcome = worker.run_process([sys.executable, "-c", "raise SystemExit(7)"],
                                     self.root, os.environ.copy(), self.root, 5, 0)
        self.assertEqual(outcome["status"], "failed")
        self.assertEqual(outcome["exit_code"], 7)

    def test_review_schema_and_provenance(self):
        finding = {"file": "file.go", "line": 2, "title": "Bug", "evidence": "Evidence",
                   "reproduction": "Test"}
        response = {"choices": [{"finish_reason": "stop", "message": {
            "content": json.dumps({"findings": [finding]})}}]}
        result = review.validate_response(response, "file.go", 1, 5)
        self.assertEqual(result["findings"][0]["verification"], "unverified")
        with self.assertRaises(ValueError):
            review.validate_response(response, "other.go", 1, 5)
        with self.assertRaises(ValueError):
            review.validate_response(response, "file.go", 3, 5)
        response["choices"][0]["finish_reason"] = "length"
        with self.assertRaises(ValueError):
            review.validate_response(response, "file.go", 1, 5)

    def test_review_http_integration(self):
        requests = []

        class Handler(BaseHTTPRequestHandler):
            def do_POST(self):
                requests.append(json.loads(self.rfile.read(int(self.headers["Content-Length"]))))
                self.send_response(200)
                self.end_headers()
                self.wfile.write(json.dumps({"choices": [{"finish_reason": "stop", "message": {
                    "content": '{"findings": []}'}}]}).encode())

            def log_message(self, *_args):
                pass

        server = HTTPServer(("127.0.0.1", 0), Handler)
        thread = threading.Thread(target=server.serve_forever)
        thread.start()
        try:
            (self.root / "file.go").write_text("package sample\nfunc Example() {}\n")
            output = self.root / "review.json"
            subprocess.run([sys.executable, review.__file__, "--file", "file.go", "--start", "1",
                            "--end", "2", "--output", str(output)], cwd=self.root, check=True,
                           env={**os.environ, "MODEL_BASE_URL": f"http://127.0.0.1:{server.server_port}/v1",
                                "SOURCE_REVISION": "abc"}, stdout=subprocess.DEVNULL)
            self.assertEqual(json.loads(output.read_text())["revision"], "abc")
            self.assertIn("1: package sample", requests[0]["messages"][1]["content"])
            self.assertNotIn("tools", requests[0])
        finally:
            server.shutdown()
            thread.join()
            server.server_close()

    def test_unavailable_model_keeps_excerpt_pending(self):
        archive = self.root / "source.tar"
        data = b"package sample\nfunc Example() {}\n"
        with tarfile.open(archive, "w", format=tarfile.PAX_FORMAT,
                          pax_headers={"comment": "b" * 40}) as out:
            member = tarfile.TarInfo("sample/file.go")
            member.size = len(data)
            out.addfile(member, io.BytesIO(data))
        config = self.root / "config.json"
        config.write_text(json.dumps({"fuzz_seconds": 1, "job_seconds": 10,
                                      "review_roots": ["sample"]}))
        # An HTTP error exercises review.py's transient-error exit code.
        class Unavailable(BaseHTTPRequestHandler):
            def do_POST(self):
                self.send_error(503)
            def log_message(self, *_args):
                pass
        server = HTTPServer(("127.0.0.1", 0), Unavailable)
        thread = threading.Thread(target=server.serve_forever)
        thread.start()
        try:
            subprocess.run([sys.executable, worker.__file__, "--archive", str(archive),
                            "--state", str(self.root / "state"), "--config", str(config), "--once"],
                           check=True, stdout=subprocess.DEVNULL,
                           env={**os.environ, "MODES": "review", "MIN_FREE_GIB": "0",
                                "MODEL_BASE_URL": f"http://127.0.0.1:{server.server_port}/v1"})
            checkpoint = json.loads((self.root / "state/checkpoint.json").read_text())
            self.assertEqual(checkpoint["cursors"].get("review", 0), 0)
            result = next((self.root / "state/reports").glob("*/result.json"))
            self.assertEqual(json.loads(result.read_text())["exit_code"], 75)
        finally:
            server.shutdown()
            thread.join()
            server.server_close()

    def test_worker_resume_and_retention(self):
        source = self.root / "source"
        source.mkdir()
        (source / "pkg").mkdir()
        archive = self.root / "source.tar"
        with tarfile.open(archive, "w", format=tarfile.PAX_FORMAT,
                          pax_headers={"comment": "a" * 40}) as out:
            out.add(source / "pkg", arcname="pkg")
        config = self.root / "config.json"
        config.write_text(json.dumps({"fuzz_seconds": 1, "job_seconds": 10, "mutation": ["pkg"]}))
        binary = self.root / "gremlins"
        binary.write_text("#!/bin/sh\necho mutation-finished\n")
        binary.chmod(0o755)
        command = [sys.executable, worker.__file__, "--archive", str(archive), "--state",
                   str(self.root / "state"), "--config", str(config), "--once"]
        env = {**os.environ, "PATH": str(self.root) + os.pathsep + os.environ["PATH"],
               "MODES": "mutation", "MIN_FREE_GIB": "0", "MAX_REPORTS": "1"}
        subprocess.run(command, env=env, check=True, stdout=subprocess.DEVNULL)
        subprocess.run(command, env=env, check=True, stdout=subprocess.DEVNULL)
        reports = list((self.root / "state/reports").iterdir())
        self.assertEqual(len(reports), 1)  # Finished mutation does not rerun on restart.
        self.assertEqual(json.loads((reports[0] / "result.json").read_text())["status"], "completed")
        (self.root / "state/checkpoint.json").unlink()
        subprocess.run(command, env=env, check=True, stdout=subprocess.DEVNULL)
        self.assertEqual(len(list((self.root / "state/reports").iterdir())), 1)
        self.assertFalse(reports[0].exists())


if __name__ == "__main__":
    unittest.main()
