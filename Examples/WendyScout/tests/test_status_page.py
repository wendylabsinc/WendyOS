import http.client, json, pathlib, sys, tempfile, threading, time, unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1] / "control"))
import status_page


class PageTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.work = pathlib.Path(self.tmp.name)
        self.now = 1700000010
        self.run = self.work / "runs" / "1700000000-0"
        self.run.mkdir(parents=True)
        self.path = self.run / "events.jsonl"
        self.state = {"ready": True, "worker_age": 1, "stopping": False}

    def tearDown(self):
        self.tmp.cleanup()

    def append(self, e):
        with self.path.open("ab", buffering=0) as f:
            f.write((json.dumps(e) + "\n").encode())

    def test_live_append_and_escape_only_visible_assistant_text(self):
        self.append(
            {"type": "session_started", "timestamp_utc": "2023-11-14T22:13:20+00:00"}
        )
        self.append(
            {
                "type": "message_end",
                "message": {
                    "role": "toolResult",
                    "content": [{"type": "text", "text": "PRIVATE_SCRIPT_STDOUT"}],
                },
            }
        )
        self.append(
            {
                "type": "message_end",
                "message": {
                    "role": "assistant",
                    "content": [
                        {"type": "thinking", "text": "HIDDEN_REASONING"},
                        {
                            "type": "text",
                            "text": "<script>alert(1)</script> github_pat_fixture",
                        },
                    ],
                },
            }
        )
        page = status_page.render(self.work, self.state, self.now).decode()
        self.assertIn("running", page)
        self.assertIn('name="viewport"', page)
        self.assertIn("<style>" + status_page.STYLE + "</style>", page)
        self.assertIn("10s", page)
        self.assertIn("&lt;script&gt;", page)
        self.assertNotIn("<script>", page)
        self.assertNotIn("PRIVATE_SCRIPT_STDOUT", page)
        self.assertNotIn("HIDDEN_REASONING", page)
        self.assertNotIn("github_pat_fixture", page)
        self.append(
            {
                "type": "tool_execution_start",
                "toolName": "scout_probe",
                "args": {"kind": "info"},
            }
        )
        self.assertIn(
            "scout_probe", status_page.render(self.work, self.state, self.now).decode()
        )

    def test_submitted_code_and_worker_output_are_readable_but_redacted(self):
        self.append(
            {
                "type": "tool_execution_start",
                "toolName": "script_write",
                "args": {"name": "probe.py", "text": 'print("<tag>")'},
            }
        )
        self.append(
            {
                "type": "tool_execution_end",
                "toolName": "script_run",
                "result": {
                    "content": [
                        {
                            "type": "text",
                            "text": json.dumps(
                                {
                                    "evidence_kind": "offline_sandbox",
                                    "exit": 0,
                                    "output": "result <script> github_pat_fixture",
                                    "file_output": "done",
                                }
                            ),
                        }
                    ]
                },
            }
        )
        page = status_page.render(self.work, self.state, self.now).decode()
        self.assertIn("Submitted script", page)
        self.assertIn("&lt;tag&gt;", page)
        self.assertIn("Worker stdout", page)
        self.assertIn("done", page)
        self.assertNotIn("<script>", page)
        self.assertNotIn("github_pat_fixture", page)

    def test_tail_without_start_and_state(self):
        self.path.write_text(
            "x" * 150000
            + "\n"
            + json.dumps(
                {
                    "type": "tool_execution_start",
                    "toolName": "script_run",
                    "args": {"name": "probe.py"},
                }
            )
            + "\n"
        )
        page = status_page.render(self.work, self.state, self.now).decode()
        self.assertIn("10s", page)
        self.assertIn(
            "stale",
            status_page.render(
                self.work, {**self.state, "worker_age": 11}, self.now
            ).decode(),
        )
        (self.work / "held.json").write_text('{"reason":"setup failed"}')
        self.assertIn(
            "blocked", status_page.render(self.work, self.state, self.now).decode()
        )

    def test_fixed_read_only_route(self):
        server = status_page.start(self.work, lambda: self.state, port=0)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        try:
            c = http.client.HTTPConnection("127.0.0.1", server.server_port)
            c.request("GET", "/")
            r = c.getresponse()
            self.assertEqual(r.status, 200)
            r.read()
            self.assertIn("default-src 'none'", r.getheader("Content-Security-Policy"))
            for method, path in [
                ("GET", "/state/secrets/github-issues-token"),
                ("GET", "/../../proc"),
                ("POST", "/"),
            ]:
                c = http.client.HTTPConnection("127.0.0.1", server.server_port)
                c.request(method, path)
                r = c.getresponse()
                self.assertIn(r.status, [404, 405])
                r.read()
                c.close()
        finally:
            server.shutdown()
            server.server_close()
