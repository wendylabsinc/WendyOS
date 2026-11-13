"""Real HTTP controls; the production CLI never accepts an alternate origin."""
import importlib.util
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
import tempfile
import threading
import time
import unittest
from urllib.request import build_opener, ProxyHandler, Request
from urllib.parse import urlsplit

spec = importlib.util.spec_from_file_location("hosted", Path(__file__).with_name("probe-hosted-export.py"))
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)


class PublicURLResponse:
    def __init__(self, response, public_url):
        self.response, self.public_url = response, public_url
        self.headers, self.status = response.headers, response.status
    def __enter__(self): return self
    def __exit__(self, *_): self.response.close()
    def geturl(self): return self.public_url
    def read(self, size): return self.response.read(size)


class LocalTransport:
    def __init__(self, origin):
        self.origin = origin
        self.opener = build_opener(ProxyHandler({}), m.NoRedirect())
    def open(self, request, timeout):
        # URL acceptance is still exercised by the real Fetcher. Only socket
        # transport is redirected to this test's owned loopback HTTP server.
        m.allowed_url(request.full_url)
        local = self.origin + urlsplit(request.full_url).path
        response = self.opener.open(Request(local, headers=dict(request.headers)), timeout=timeout)
        return PublicURLResponse(response, request.full_url)


class HostedProbeTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.base = "/branch-fixture-" + "a" * 40
        self.contract = {"redirects": {"old": "/guide/"}}
        self.routes = {}
        self.hits = []
        self.slow_paths = {"/slow"}
        owner = self
        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                owner.hits.append(self.path)
                if self.path == "/redirect":
                    self.send_response(302)
                    self.send_header("Location", "https://example.invalid/exfiltrate")
                    self.end_headers()
                    return
                if self.path in owner.slow_paths:
                    self.send_response(200)
                    self.end_headers()
                    try:
                        for _ in range(100):
                            self.wfile.write(b"x")
                            self.wfile.flush()
                            time.sleep(.02)
                    except (BrokenPipeError, ConnectionResetError): pass
                    return
                body = owner.routes.get(self.path)
                self.send_response(200 if body is not None else 404)
                self.end_headers()
                if body is not None: self.wfile.write(body)
            def log_message(self, *_): pass
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        worker = threading.Thread(target=self.server.serve_forever, daemon=True)
        worker.start()
        def stop():
            self.server.shutdown()
            self.server.server_close()
            worker.join(timeout=2)
        self.addCleanup(stop)
        self.fetcher = m.Fetcher(request_seconds=1)
        self.fetcher.opener = LocalTransport(f"http://127.0.0.1:{self.server.server_port}")
        for base in (self.base, "/latest"):
            preview = base == self.base
            for route in ("/", "/guide/", "/old/"):
                target = "/guide/" if route == "/old/" else route
                robots = "noindex,nofollow" if preview else "index,follow"
                body = f'<link rel="canonical" href="https://docs.wendy.dev{base}{target}"><meta name="robots" content="{robots}">'
                if route == "/old/":
                    body += f'<meta http-equiv="refresh" content="0; url={base}/guide/"><a href="{base}/guide/">moved</a>'
                else:
                    body += '<h1 id="section">Guide</h1><a href="#section">section</a>'
                if not preview: body += "G-FIXTURE"
                self.routes[base + route] = body.encode()
                if preview:
                    path = self.root / route.strip("/") / "index.html"
                    path.parent.mkdir(parents=True, exist_ok=True)
                    path.write_bytes(body.encode())
            self.routes[base + "/robots.txt"] = b"User-agent: *\nDisallow: /branch-\n"
        (self.root / "robots.txt").write_bytes(self.routes[self.base + "/robots.txt"])

    def probe(self):
        return m.probe(self.root, self.base, self.contract, "G-FIXTURE", self.fetcher, total_seconds=5)

    def test_exact_preview_and_independent_production(self):
        self.routes["/latest/guide/"] += b"different public revision"
        report = self.probe()
        self.assertEqual(report["failures"], [])
        self.assertEqual(len(report["requests"]), 8)
        self.assertEqual(report["preview_source_commit"], "a" * 40)

    def test_preview_artifact_mismatch_fails(self):
        self.routes[self.base + "/guide/"] += b"stale CDN bytes"
        self.assertTrue(any("exact build artifact" in f["reason"] for f in self.probe()["failures"]))

    def test_missing_alias_is_not_hidden_by_valid_destination(self):
        del self.routes["/latest/old/"]
        self.assertTrue(any(f["route"] == "/old/" and "404" in f["reason"] for f in self.probe()["failures"]))

    def test_latest_contract_drift_is_reported_but_only_the_preview_blocks(self):
        # /latest is the last stable release: a redirect added or moved by
        # this revision cannot exist there until the next release ships.
        self.contract["redirects"]["added"] = "/guide/"
        added = self.routes[self.base + "/old/"]
        self.routes[self.base + "/added/"] = added
        (self.root / "added").mkdir()
        (self.root / "added/index.html").write_bytes(added)
        failures = self.probe()["failures"]
        self.assertEqual([(f["base"], f["route"]) for f in failures], [("/latest", "/added/")])
        self.assertEqual(m.blocking(failures), [])
        del self.routes[self.base + "/added/"]
        self.fetcher.cache.clear()
        self.assertEqual([f["base"] for f in m.blocking(self.probe()["failures"])], [self.base])
        self.assertEqual(m.blocking([{"reason": "hosted probe deadline expired"}]), [{"reason": "hosted probe deadline expired"}])

    def test_production_redirect_and_target_anchor_mutations_fail(self):
        self.routes["/latest/old/"] = self.routes["/latest/old/"].replace(b"0; url=/latest/guide/", b"0; url=https://example.invalid/")
        self.routes["/latest/guide/"] = self.routes["/latest/guide/"].replace(b'id="section"', b'id="removed"')
        reasons = [f["reason"] for f in self.probe()["failures"]]
        self.assertTrue(any("static redirect" in r for r in reasons))
        self.assertTrue(any("missing anchor" in r for r in reasons))

    def test_bad_preview_policy_fails_even_if_artifact_matches(self):
        path = self.root / "guide/index.html"
        body = path.read_bytes().replace(b"noindex,nofollow", b"index,follow") + b"G-FIXTURE"
        path.write_bytes(body)
        self.routes[self.base + "/guide/"] = body
        reasons = [f["reason"] for f in self.probe()["failures"]]
        self.assertIn("preview is indexable", reasons)
        self.assertIn("preview includes analytics", reasons)

    def test_redirect_is_rejected_before_external_request(self):
        with self.assertRaisesRegex(ValueError, "HTTP redirect rejected"):
            self.fetcher.fetch(m.ORIGIN + "/redirect")
        self.assertEqual(self.hits, ["/redirect"])

    def test_fixed_origin_rejects_ssrf_inputs_before_transport(self):
        for url in ("http://docs.wendy.dev/", "https://docs.wendy.dev.evil/", "https://docs.wendy.dev@127.0.0.1/", "https://docs.wendy.dev:443/", "https://127.0.0.1/", m.ORIGIN + "/%2e%2e/secret", m.ORIGIN + "//evil"):
            with self.subTest(url=url), self.assertRaises(ValueError):
                self.fetcher.fetch(url)
        self.assertEqual(self.hits, [])

    def test_progressing_body_cannot_extend_request_deadline(self):
        self.fetcher.request_seconds = .12
        started = time.monotonic()
        with self.assertRaises(TimeoutError): self.fetcher.fetch(m.ORIGIN + "/slow")
        self.assertLess(time.monotonic() - started, .6)

    def test_total_probe_deadline_reaches_slow_read(self):
        self.slow_paths.add(self.base + "/")
        started = time.monotonic()
        with self.assertRaises(TimeoutError):
            m.probe(self.root, self.base, self.contract, "G-FIXTURE", self.fetcher, total_seconds=.12)
        self.assertLess(time.monotonic() - started, .6)

    def test_commented_or_wrong_agent_robots_rules_fail(self):
        for body in (b"User-agent: *\n# Disallow: /branch-\n", b"User-agent: special-bot\nDisallow: /branch-\n", b"User-agent: *\nDisallow: /branch-\nAllow: /branch-\n"):
            self.routes["/latest/robots.txt"] = body
            self.fetcher.cache.clear()
            reasons = [f["reason"] for f in self.probe()["failures"]]
            self.assertIn("robots does not exclude transient previews", reasons)

    def test_body_and_request_budgets(self):
        self.routes["/large"] = b"x" * 128
        old = m.MAX_BODY
        try:
            m.MAX_BODY = 64
            with self.assertRaisesRegex(ValueError, "body exceeds"):
                self.fetcher.fetch(m.ORIGIN + "/large")
        finally: m.MAX_BODY = old
        self.fetcher.observations = [{}] * m.MAX_REQUESTS
        with self.assertRaisesRegex(ValueError, "request budget"):
            self.fetcher.fetch(m.ORIGIN + "/unrequested")
        self.assertNotIn("/unrequested", self.hits)


if __name__ == "__main__": unittest.main()
