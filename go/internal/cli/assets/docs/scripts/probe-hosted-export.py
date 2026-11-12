#!/usr/bin/env python3
"""Read-only hosted docs compatibility probe. Production origin is fixed."""
import argparse
from contextlib import contextmanager
import hashlib
import importlib.util
import json
from pathlib import Path
import re
import signal
import time
from urllib.parse import unquote, urljoin, urlsplit
from urllib.request import build_opener, HTTPRedirectHandler, ProxyHandler, Request

spec = importlib.util.spec_from_file_location("export_crawler", Path(__file__).with_name("crawl-export.py"))
crawler = importlib.util.module_from_spec(spec)
spec.loader.exec_module(crawler)
ORIGIN = "https://docs.wendy.dev"
MAX_BODY = 8 * 1024 * 1024
MAX_REQUESTS = 250


@contextmanager
def wall_timeout(seconds):
    """A total deadline, including a peer that keeps slowly sending bytes."""
    if seconds <= 0:
        raise TimeoutError("hosted probe deadline expired")
    started = time.monotonic()
    previous_handler = signal.getsignal(signal.SIGALRM)
    previous_timer = signal.getitimer(signal.ITIMER_REAL)
    def expired(*_):
        raise TimeoutError("hosted probe wall deadline expired")
    signal.signal(signal.SIGALRM, expired)
    duration = min(seconds, previous_timer[0]) if previous_timer[0] else seconds
    signal.setitimer(signal.ITIMER_REAL, duration)
    try:
        yield
    finally:
        signal.setitimer(signal.ITIMER_REAL, 0)
        signal.signal(signal.SIGALRM, previous_handler)
        remaining = max(0.000001, previous_timer[0] - (time.monotonic() - started)) if previous_timer[0] else 0
        signal.setitimer(signal.ITIMER_REAL, remaining, previous_timer[1])


class NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, request, fp, code, message, headers, target):
        raise ValueError(f"HTTP redirect rejected: {code}")


def allowed_url(url):
    parsed = urlsplit(url)
    if parsed.scheme != "https" or parsed.netloc != "docs.wendy.dev" or parsed.query or parsed.fragment:
        raise ValueError("hosted probe URL must stay on exact HTTPS docs.wendy.dev origin")
    decoded = unquote(parsed.path)
    if not decoded.startswith("/") or "\\" in decoded or "//" in decoded or any(p in (".", "..") for p in decoded.split("/")):
        raise ValueError("invalid hosted probe path")
    return url


class Fetcher:
    def __init__(self, request_seconds=10):
        self.opener = build_opener(ProxyHandler({}), NoRedirect())
        self.request_seconds = request_seconds
        self.cache = {}
        self.observations = []

    def fetch(self, url):
        allowed_url(url)
        if url in self.cache:
            return self.cache[url]
        if len(self.observations) >= MAX_REQUESTS:
            raise ValueError("hosted probe request budget exceeded")
        observation = {"url": url}
        self.observations.append(observation)
        with wall_timeout(self.request_seconds):
            request = Request(url, headers={"User-Agent": "WendyDocsRegression/1", "Accept-Encoding": "identity"})
            with self.opener.open(request, timeout=self.request_seconds) as response:
                if response.status != 200 or response.geturl() != url:
                    raise ValueError("hosted response changed URL or did not return HTTP 200")
                if response.headers.get("Content-Encoding", "identity") != "identity":
                    raise ValueError("unexpected content encoding prevents exact artifact comparison")
                raw = response.read(MAX_BODY + 1)
                if len(raw) > MAX_BODY:
                    raise ValueError("hosted response body exceeds limit")
                observation.update(status=response.status, bytes=len(raw), sha256=hashlib.sha256(raw).hexdigest())
        self.cache[url] = raw
        return raw


def artifact_file(root, route):
    root = root.resolve(strict=True)
    relative = route.strip("/")
    path = root / relative / "index.html" if route.endswith("/") else root / relative
    resolved = path.resolve(strict=True)
    if root not in resolved.parents or not resolved.is_file():
        raise ValueError("artifact path escaped export root")
    return resolved


def artifact_bytes(root, route):
    with artifact_file(root, route).open("rb") as stream:
        raw = stream.read(MAX_BODY + 1)
    if len(raw) > MAX_BODY:
        raise ValueError("local export body exceeds probe limit")
    return raw


def probe(preview_root, preview_base, contract, analytics_id, fetcher=None, total_seconds=180):
    if not re.fullmatch(r"/branch-[a-z0-9][a-z0-9._-]{0,193}-[0-9a-f]{40}", preview_base):
        raise ValueError("preview prefix must include the exact 40-character source commit")
    if not analytics_id or not re.fullmatch(r"G-[A-Z0-9]+", analytics_id):
        raise ValueError("a concrete configured GA measurement ID is required")
    fetcher = fetcher or Fetcher()
    failures = []
    deadline = time.monotonic() + total_seconds
    target_routes = set(contract["redirects"].values()) | {"/"}
    if not contract["redirects"]:
        raise ValueError("historical redirect contract is empty")
    expected_ids = {}
    for route in target_routes:
        expected_ids[route] = crawler.Page(artifact_bytes(preview_root, route).decode("utf-8")).ids

    def fail(base, route, reason):
        failures.append({"base": base, "route": route, "reason": reason})

    def read(base, route, preview):
        if time.monotonic() >= deadline:
            raise TimeoutError("hosted probe total deadline expired")
        try:
            raw = fetcher.fetch(allowed_url(ORIGIN + base + route))
            if preview:
                expected = artifact_bytes(preview_root, route)
                if raw != expected:
                    raise ValueError("served preview bytes differ from exact build artifact")
            return raw
        except (OSError, ValueError, TimeoutError) as error:
            if time.monotonic() >= deadline:
                raise TimeoutError("hosted probe total deadline expired") from error
            fail(base, route, str(error))
            return None

    def page_policy(page, base, route, canonical, preview):
        if page.canonical != [ORIGIN + base + canonical]:
            fail(base, route, "canonical URL differs from compatibility destination")
        if preview:
            if not {"noindex", "nofollow"}.issubset(page.robots):
                fail(base, route, "preview is indexable")
            if analytics_id in page.raw or "googletagmanager.com/gtag/js" in page.raw:
                fail(base, route, "preview includes analytics")
        elif "noindex" in page.robots:
            fail(base, route, "production route is non-indexable")

    with wall_timeout(total_seconds):
        for base, preview in ((preview_base, True), ("/latest", False)):
            pages = {}
            for route in sorted(target_routes):
                raw = read(base, route, preview)
                if raw is None:
                    continue
                page = crawler.Page(raw.decode("utf-8"))
                pages[route] = page
                page_policy(page, base, route, route, preview)
                if page.refresh:
                    fail(base, route, "compatibility destination unexpectedly redirects")
                if preview and not expected_ids[route].issubset(page.ids):
                    fail(base, route, "preview destination lost an artifact anchor")
                for href in page.links:
                    linked = urlsplit(urljoin(ORIGIN + base + route, href))
                    if linked.scheme == "https" and linked.netloc == "docs.wendy.dev" and linked.path == base + route and linked.fragment:
                        fragment = unquote(linked.fragment).split(":~:text=", 1)[0]
                        if fragment and fragment != "top" and fragment not in page.ids:
                            fail(base, route, "hosted self-link has missing anchor: " + fragment)
            for old, target in sorted(contract["redirects"].items()):
                route = "/" + old + "/"
                raw = read(base, route, preview)
                if raw is None:
                    continue
                page = crawler.Page(raw.decode("utf-8"))
                expected = base + target
                if page.refresh != ["0; url=" + expected] or expected not in page.links:
                    fail(base, route, "static redirect or accessible fallback differs from contract")
                page_policy(page, base, route, target, preview)
                if target not in pages:
                    fail(base, route, "redirect destination was not successfully verified")
            robots = read(base, "/robots.txt", preview)
            if robots is not None and not crawler.wildcard_robots_disallows(robots.decode("utf-8"), preview_base + "/"):
                fail(base, "/robots.txt", "robots does not exclude transient previews")
    return {"origin": ORIGIN, "preview_base": preview_base, "preview_source_commit": preview_base.rsplit("-", 1)[1],
            "preview_identity": "exact response bytes compared to supplied export",
            "production_identity": "current public /latest, independent revision; no artifact identity claim",
            "historical_redirects_per_prefix": len(contract["redirects"]), "target_routes_per_prefix": len(target_routes),
            "requests": fetcher.observations, "failures": failures}


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--preview-root", type=Path, required=True)
    parser.add_argument("--preview-base", required=True)
    parser.add_argument("--analytics-id", required=True)
    parser.add_argument("--report", type=Path, required=True)
    parser.add_argument("--contract", type=Path, default=Path(__file__).with_name("legacy-route-contract.json"))
    args = parser.parse_args()
    try:
        fetcher = Fetcher()
        with wall_timeout(180):
            report = probe(args.preview_root, args.preview_base, json.loads(args.contract.read_text()), args.analytics_id, fetcher)
    except (OSError, ValueError, TimeoutError) as error:
        report = {"origin": ORIGIN, "requests": fetcher.observations if "fetcher" in locals() else [], "failures": [{"reason": str(error)}]}
    args.report.write_text(json.dumps(report, indent=2) + "\n")
    print(f"Hosted docs probe: {len(report.get('requests', []))} bounded requests; {len(report['failures'])} failures")
    for failure in report["failures"][:20]:
        print(failure)
    raise SystemExit(bool(report["failures"]))
