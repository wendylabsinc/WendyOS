import copy, http.server, json, pathlib, sys, tempfile, threading, unittest, urllib.request, types, stat, os, io
from unittest.mock import patch

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1] / "control"))
import common, inference, probes, publisher


class PinTests(unittest.TestCase):
    def test_service_model_and_tool_pins_match(self):
        root = pathlib.Path(__file__).resolve().parents[1]
        self.assertEqual(
            json.loads((root / "control/pins.json").read_text()),
            json.loads((root / "worker/pins.json").read_text()),
        )


class InferenceTests(unittest.TestCase):
    def test_text_and_tool_results(self):
        b = {
            "model": "local",
            "messages": [
                {
                    "role": "user",
                    "content": "A URL in inert text: https://example.invalid/",
                },
                {
                    "role": "assistant",
                    "content": None,
                    "tool_calls": [
                        {
                            "id": "one",
                            "type": "function",
                            "function": {
                                "name": "script_run",
                                "arguments": '{"name":"test.py"}',
                            },
                        }
                    ],
                },
                {
                    "role": "tool",
                    "tool_call_id": "one",
                    "content": [{"type": "text", "text": "offline result"}],
                },
            ],
            "stream": True,
            "max_tokens": 9999,
        }
        self.assertEqual(inference.validate(b, "local")["max_tokens"], 1536)

    def test_flash_sampling_is_trusted_and_output_bounded(self):
        body = {
            "model": "qwen3.8-flash-next",
            "messages": [{"role": "user", "content": "test"}],
            "temperature": 9,
            "top_p": 0.1,
            "max_tokens": 99999,
        }
        result = inference.validate(body, body["model"])
        self.assertEqual(result["max_tokens"], 4096)
        self.assertEqual(
            (result["temperature"], result["top_p"], result["presence_penalty"]),
            (0.7, 0.8, 1.5),
        )
        with self.assertRaises(ValueError):
            inference.validate(
                {**body, "chat_template_kwargs": {"enable_thinking": True}},
                body["model"],
            )

    def test_fetch_and_override_features_rejected(self):
        base = {"model": "local", "messages": [{"role": "user", "content": "hello"}]}
        for key, value in [
            ("image_url", "https://example.invalid"),
            ("chat_template_kwargs", {}),
            ("lora", "/state/secrets"),
            ("grammar", "x"),
            ("response_format", {}),
        ]:
            with self.subTest(key=key), self.assertRaises(ValueError):
                inference.validate({**base, key: value}, "local")
        for item in [
            {"type": "image_url", "image_url": {"url": "https://example.invalid"}},
            {"type": "input_audio", "input_audio": {"data": "x"}},
        ]:
            with self.assertRaises(ValueError):
                inference.validate(
                    {**base, "messages": [{"role": "user", "content": [item]}]}, "local"
                )
        with self.assertRaises(ValueError):
            inference.validate(
                {
                    **base,
                    "tools": [
                        {
                            "type": "function",
                            "function": {"name": "bash", "parameters": {}},
                        }
                    ],
                },
                "local",
            )


class ProjectionTests(unittest.TestCase):
    def test_private_values_excluded_and_versions_retained(self):
        raw = {
            "version": "2026.10.29-210857",
            "osVersion": "WendyOS-0.17.0",
            "cpuArchitecture": "arm64",
            "stable_id": "by-id:usb-Test_TESTSERIAL-video-index0",
            "ip_addresses": ["2001:db8::1"],
            "primary_mac": "02:00:00:00:00:01",
            "token": "github" + "_pat_" + "A" * 40,
        }
        out = common.public_observation("info", raw, 0)
        self.assertEqual(
            out,
            {
                "version": raw["version"],
                "osVersion": raw["osVersion"],
                "cpuArchitecture": "arm64",
            },
        )
        self.assertEqual(common.public_observation("cameras", [raw], 0), {"count": 1})

    def test_raw_error_not_published(self):
        text = (
            "by-id:usb-Test_TESTSERIAL-video-index0 2001:db8::1 02:00:00:00:00:01 https://example.invalid/ @someone "
            + ("github" + "_pat_" + "A" * 40)
        )
        cleaned = common.scrub(text)
        for sensitive in [
            "TESTSERIAL",
            "2001:db8",
            "02:00:00",
            "https://",
            "@someone",
            "A" * 40,
        ]:
            self.assertNotIn(sensitive, cleaned)
        out = common.public_observation(
            "audio", {"error": {"code": "error", "message": text}}, 1
        )
        self.assertNotIn("TESTSERIAL", json.dumps(out))

    def test_null_issue_body(self):
        with patch.object(
            common,
            "api",
            side_effect=[
                [{"sha": "abc", "commit": {"message": "subject"}}],
                {
                    "items": [
                        {"number": 1, "title": "title", "state": "closed", "body": None}
                    ]
                },
            ],
        ):
            self.assertEqual(common.context()["issues"][0]["body"], "")


class ReplayTests(unittest.TestCase):
    def instance(self, results, new_stable=False):
        p = probes.Probes.__new__(probes.Probes)
        p.agent = "test-agent"
        p.versions = {
            "past": {"published_at": "2026-10-01"},
            "nightly": {"published_at": "2026-11-01"},
            "stable": {"published_at": "2026-11-02" if new_stable else "2026-10-20"},
        }
        p.run = lambda *args: copy.deepcopy(results.pop(0))
        return p

    def r(self, status="passed", identity="state-mismatch"):
        return {
            "outcome": status,
            "completed": status != "blocked",
            "cleanup_verified": True,
            "version": "test",
            "failure_identity": identity,
        }

    def test_repeated_functional_regression_and_current_stable(self):
        rows = [
            self.r(),
            self.r("failed"),
            self.r(),
            self.r("failed"),
            self.r(),
            self.r(),
        ]
        self.assertTrue(
            self.instance(rows).replay("restart_persistence", "past", "nightly", None)[
                "verified"
            ]
        )

    def test_fixed_existing_blocked_and_flaky_are_unverified(self):
        for newer, contrast in [
            (True, ["passed", "passed"]),
            (False, ["failed", "failed"]),
            (False, ["blocked", "passed"]),
            (True, ["failed", "passed"]),
        ]:
            rows = [
                self.r(),
                self.r("failed"),
                self.r(),
                self.r("failed"),
                *[self.r(x) for x in contrast],
            ]
            self.assertFalse(
                self.instance(rows, newer).replay(
                    "restart_persistence", "past", "nightly", None
                )["verified"]
            )
        rows = [self.r(), self.r("failed", "a"), self.r(), self.r("failed", "b")]
        self.assertFalse(
            self.instance(rows).replay("restart_persistence", "past", "nightly", None)[
                "verified"
            ]
        )

    def test_shared_failure_missing_evidence_and_help_only_rejected(self):
        rows = [self.r("failed") for _ in range(4)]
        self.assertFalse(
            self.instance(rows).replay("logs_output", "past", "nightly", None)[
                "verified"
            ]
        )
        for kind in ["help_root", "info", "shell"]:
            with self.assertRaises(ValueError):
                self.instance([]).replay(kind, "past", "nightly", None)
        with self.assertRaises(ValueError):
            self.instance([]).replay("restart_persistence", "prod", "nightly", None)
        with self.assertRaises(ValueError):
            self.instance([]).replay(
                "restart_persistence",
                "past",
                "nightly",
                None,
                "field_changed",
                "osVersion",
            )


class FakeGitHub(http.server.BaseHTTPRequestHandler):
    calls = []
    issues = []
    label = False
    pages = {}

    def log_message(self, *_):
        pass

    def do_GET(self):
        self.__class__.calls.append(("GET", self.path))
        if "/labels/" in self.path and not self.__class__.label:
            self.send_error(404)
            return
        body = json.dumps(
            self.__class__.pages.get(
                self.path.rsplit("page=", 1)[-1], self.__class__.issues
            )
            if "/issues?" in self.path
            else {"name": "ai-found"}
        ).encode()
        self.send_response(200)
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        data = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        self.__class__.calls.append(("POST", self.path, data))
        if self.path.endswith("/labels"):
            self.__class__.label = True
            out = {"name": "ai-found"}
        elif self.path.endswith("/issues"):
            out = {**data, "html_url": "https://github.com/example/test/issues/1"}
            self.__class__.issues.append(out)
        else:
            self.send_error(403)
            return
        self.send_response(201)
        self.end_headers()
        self.wfile.write(json.dumps(out).encode())


class FakeTransport:
    def __init__(self, url):
        self.url = url

    def request(self, method, suffix, data=None):
        # Test transport is injected only here; production base URL is immutable.
        req = urllib.request.Request(
            self.url + "/repos/example/test/" + suffix,
            method=method,
            data=json.dumps(data).encode() if data is not None else None,
            headers={"Content-Type": "application/json"},
        )
        with urllib.request.urlopen(req, timeout=2) as r:
            return json.load(r)


class PublisherTests(unittest.TestCase):
    def setUp(self):
        FakeGitHub.calls = []
        FakeGitHub.issues = []
        FakeGitHub.label = False
        FakeGitHub.pages = {}
        self.server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), FakeGitHub)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.temp = tempfile.TemporaryDirectory()
        self.pub = publisher.Publisher(
            self.temp.name,
            repo="example/test",
            transport=FakeTransport("http://127.0.0.1:" + str(self.server.server_port)),
        )

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()
        self.temp.cleanup()

    def finding(self, field="state-mismatch"):
        rows = []
        for i in range(4):
            passed = i % 2 == 0
            rows.append(
                {
                    "id": "experiment-fixture" + str(i),
                    "evidence_kind": "functional_workflow",
                    "kind": "restart_persistence",
                    "version": "2026.10.29-210857" if passed else "2026.11.04-165715",
                    "agent": "2026.10.29-210857",
                    "completed": True,
                    "cleanup_verified": True,
                    "outcome": "passed" if passed else "failed",
                    "exit": 0 if passed else 1,
                    "output": {
                        "checks": [
                            {
                                "check": "restart_state",
                                "expected": "RUNNING",
                                "actual": "RUNNING" if passed else "STOPPED",
                                "passed": passed,
                            }
                        ]
                    },
                    "steps": [
                        {
                            "kind": "scratch_start",
                            "version": "2026.10.29-210857",
                            "command": [
                                "wendy",
                                "--json",
                                "device",
                                "apps",
                                "start",
                                "<Scout-owned scratch ID>",
                            ],
                            "exit": 0,
                        }
                    ],
                    "failure_identity": field,
                }
            )
        return {
            "verified": True,
            "kind": "functional-regression",
            "field": "",
            "probe": "restart_persistence",
            "agent": "2026.10.29-210857",
            "results": rows,
        }

    def test_local_fake_publication_label_and_duplicate(self):
        d = self.pub.publish(self.finding())
        self.assertEqual(d["status"], "published")
        self.assertEqual(self.pub.publish(self.finding())["status"], "duplicate")
        posts = [x for x in FakeGitHub.calls if x[0] == "POST"]
        self.assertEqual([x[1].split("/")[-1] for x in posts], ["labels", "issues"])
        self.assertEqual(posts[-1][2]["labels"], ["ai-found"])
        self.assertIn("2026.10.29-210857", posts[-1][2]["body"])

    def test_functional_state_regression_replays_then_publishes_only_to_fake(self):
        from test_functional import Runner
        from functional import experiment

        p = probes.Probes.__new__(probes.Probes)
        p.agent = "fixture-agent"
        p.versions = {
            "past": {"published_at": "2026-10-01"},
            "stable": {"published_at": "2026-10-20"},
            "nightly": {"published_at": "2026-11-01"},
        }
        p.run = lambda kind, v, run: experiment(
            Runner("args" if v == "nightly" else ""), kind, v, None
        )
        finding = p.replay("reject_extra_args", "stable", "nightly", None)
        self.assertTrue(finding["verified"])
        self.assertEqual(self.pub.publish(finding)["status"], "published")
        body = next(
            c[2]["body"]
            for c in FakeGitHub.calls
            if c[0] == "POST" and c[1].endswith("/issues")
        )
        self.assertIn("invalid_args_no_mutation_agent", body)
        self.assertIn("STOPPED", body)

    def test_closed_duplicate(self):
        self.pub.publish(self.finding())
        FakeGitHub.issues[0]["state"] = "closed"
        self.assertEqual(self.pub.publish(self.finding())["status"], "duplicate")
        self.assertEqual(
            len(
                [
                    x
                    for x in FakeGitHub.calls
                    if x[0] == "POST" and x[1].endswith("/issues")
                ]
            ),
            1,
        )

    def test_second_page_duplicate_and_distinct_failure_identity(self):
        f = self.finding()
        fp = self.pub.fingerprint(f)
        FakeGitHub.pages = {
            "1": [
                {
                    "title": "unrelated",
                    "body": None,
                    "html_url": "https://github.com/example/test/issues/2",
                }
            ]
            * 100,
            "2": [
                {
                    "title": "known",
                    "body": "<!-- wendy-scout:" + fp + " -->",
                    "html_url": "https://github.com/example/test/issues/1",
                }
            ],
        }
        self.assertEqual(self.pub.publish(f)["status"], "duplicate")
        self.assertFalse(any(c[0] == "POST" for c in FakeGitHub.calls))
        other = copy.deepcopy(f)
        other["results"][1]["failure_identity"] = "different-failure"
        other["results"][3]["failure_identity"] = "different-failure"
        self.assertNotEqual(fp, self.pub.fingerprint(other))
        FakeGitHub.pages = {}
        FakeGitHub.issues = [
            {
                "title": "unrelated same error",
                "body": "osVersion field_changed-regression",
                "html_url": "https://github.com/example/test/issues/2",
            }
        ]
        self.assertEqual(self.pub.publish(other)["status"], "published")

    def test_help_only_and_missing_live_evidence_never_published(self):
        f = self.finding()
        f["probe"] = "help_root"
        with self.assertRaises(ValueError):
            self.pub.publish(f)
        f = self.finding()
        f["results"][0]["completed"] = False
        with self.assertRaises(ValueError):
            self.pub.publish(f)
        self.assertFalse(FakeGitHub.calls)

    def test_arbitrary_stdout_is_not_issue_content(self):
        f = self.finding()
        for r in f["results"]:
            r["raw_stdout"] = "github_pat_private https://example.invalid/ [mention]"
        self.pub.publish(f)
        body = next(
            c[2]["body"]
            for c in FakeGitHub.calls
            if c[0] == "POST" and c[1].endswith("/issues")
        )
        self.assertNotIn("github_pat_private", body)
        self.assertIn("expected", body)
        self.assertIn("actual", body)

    def test_scoped_transport_authenticates_private_duplicate_reads(self):
        token = pathlib.Path(self.temp.name) / "scoped-token"
        token.write_text("github_pat_fixture")
        token.chmod(0o600)

        class Response(io.BytesIO):
            def __enter__(self):
                return self

            def __exit__(self, *args):
                self.close()

        requests = []

        class Opener:
            def open(self, request, timeout):
                requests.append(request)
                return Response(b"[]")

        with (
            patch.object(
                publisher.os,
                "fstat",
                return_value=types.SimpleNamespace(
                    st_mode=stat.S_IFREG | 0o600, st_uid=0
                ),
            ),
            patch.object(
                publisher.urllib.request, "build_opener", return_value=Opener()
            ),
        ):
            gh = publisher.GitHub("example/private", token)
            self.assertIsNone(publisher.find_issue(gh, lambda issue: True))
        self.assertEqual(len(requests), 1)
        self.assertEqual(
            requests[0].get_header("Authorization"), "Bearer github_pat_fixture"
        )
        self.assertTrue(
            requests[0].full_url.startswith(
                "https://api.github.com/repos/example/private/issues?state=all&"
            )
        )
        with patch.dict(os.environ, {"SCOUT_ISSUE_REPOSITORY": "example/private"}):
            self.assertEqual(common.REPO, "wendylabsinc/WendyOS")

    def test_unverified_fixed_and_unapproved_routes(self):
        for d in [{}, {**self.finding(), "already_fixed_in_newer_stable": True}]:
            with self.assertRaises(ValueError):
                self.pub.publish(d)
        gh = publisher.GitHub("example/test", "/nonexistent")
        for method, path in [
            ("POST", "issues/1/comments"),
            ("PUT", "contents/file"),
            ("POST", "actions/workflows/run"),
            ("POST", "../other/issues"),
        ]:
            with self.assertRaises(ValueError):
                gh.request(method, path, {})


if __name__ == "__main__":
    unittest.main()
