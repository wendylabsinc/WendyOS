#!/usr/bin/env python3
"""Crawl actual static HTML over loopback HTTP. Never fetch external sites."""

import argparse
from contextlib import contextmanager
from html.parser import HTMLParser
from html import unescape
from http.server import SimpleHTTPRequestHandler, ThreadingHTTPServer
import json
from pathlib import Path
import re
import threading
from urllib.error import HTTPError
from urllib.parse import unquote, urljoin, urlsplit, urlunsplit
from urllib.request import build_opener, ProxyHandler
import xml.etree.ElementTree as ET


PUBLIC_ORIGIN = "https://docs.wendy.dev"


class Page(HTMLParser):
    def __init__(self, raw):
        super().__init__(convert_charrefs=True)
        self.ids = set()
        self.links = set()
        self.canonical = []
        self.robots = set()
        self.refresh = []
        self.raw = raw
        self.feed(raw)

    def handle_starttag(self, tag, attributes):
        attrs = dict(attributes)
        if attrs.get("id"):
            self.ids.add(attrs["id"])
        if tag == "a":
            if attrs.get("name"):
                self.ids.add(attrs["name"])
            if "href" in attrs:
                self.links.add(attrs["href"] or "")
        if tag == "link" and "canonical" in (attrs.get("rel") or "").split():
            self.canonical.append(attrs.get("href"))
        if tag == "meta":
            if (attrs.get("name") or "").lower() == "robots":
                self.robots.update(token.strip().lower() for token in (attrs.get("content") or "").split(","))
            if (attrs.get("http-equiv") or "").lower() == "refresh":
                self.refresh.append(attrs.get("content") or "")


@contextmanager
def serve_export(root, base):
    root = root.resolve()

    class Handler(SimpleHTTPRequestHandler):
        def translate_path(self, path):
            route = unquote(urlsplit(path).path)
            if not route.startswith(base + "/"):
                return str(root / ".invalid-deploy-prefix")
            candidate = (root / route[len(base) + 1:]).resolve()
            if candidate != root and root not in candidate.parents:
                return str(root / ".invalid-deploy-prefix")
            return str(candidate)

        def list_directory(self, path):
            # A static host must not turn a missing index into a successful
            # auto-generated directory listing.
            self.send_error(404, "No exported index.html")
            return None

        def log_message(self, *args):
            pass

    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    worker = threading.Thread(target=server.serve_forever, daemon=True)
    worker.start()
    try:
        yield f"http://127.0.0.1:{server.server_port}"
    finally:
        server.shutdown()
        server.server_close()
        worker.join(timeout=3)


def wildcard_robots_disallows(raw, path):
    """Evaluate wildcard-group allow/disallow rules for one preview path.

    The most specific matching rule wins; Allow wins a specificity tie.
    Comments and unrelated user-agent groups cannot establish exclusion.
    """
    groups, agents, rules = [], [], []
    for line in raw.splitlines():
        line = line.split("#", 1)[0].strip()
        if ":" not in line:
            continue
        key, value = (part.strip() for part in line.split(":", 1))
        key = key.lower()
        if key == "user-agent":
            if rules:
                groups.append((agents, rules))
                agents, rules = [], []
            agents.append(value.lower())
        elif key in ("allow", "disallow") and agents:
            rules.append((key, value))
    groups.append((agents, rules))
    matches = []
    for agents, rules in groups:
        if "*" not in agents:
            continue
        for key, value in rules:
            if not value:
                continue
            end = value.endswith("$")
            pattern = value[:-1] if end else value
            regex = "^" + ".*".join(re.escape(part) for part in pattern.split("*")) + ("$" if end else "")
            if re.search(regex, path):
                matches.append((len(pattern.replace("*", "")), key == "allow"))
    return bool(matches) and not max(matches)[1]


def source_references(path):
    raw = path.read_text()
    # Match inline Markdown links, including image targets, and actual HTML
    # links. Code fences are removed so shell/JSON examples cannot invent links.
    raw = re.sub(r"(?ms)^```.*?^```[^\n]*", "", raw)
    links = set(Page(raw).links)
    links.update(unescape(m[1]) for m in re.findall(r"\[([^\]\n]*)\]\(([^\s)]+)(?:\s+[^)]*)?\)", raw))
    links.update(unescape(m) for m in re.findall(r'<(?:img|source)\b[^>]*\b(?:src|srcset)=["\']([^"\']+)["\']', raw))
    return links


def crawl(root, base, contract, analytics_id, sources=()):
    if not re.fullmatch(r"/[a-z0-9][a-z0-9._-]{0,200}", base):
        raise ValueError("base must be one explicit deployment prefix")
    root = Path(root).resolve(strict=True)
    failures = {}

    def fail(reason, target, source):
        key = reason, target
        origins = failures.setdefault(key, [])
        if source not in origins and len(origins) < 3:
            origins.append(source)

    routes = sorted("/" + str(path.relative_to(root).parent).replace("\\", "/").strip(".") + "/"
                    for path in root.rglob("index.html") if "_not-found" not in path.relative_to(root).parts)
    routes = [route.replace("//", "/") for route in routes]
    if not routes:
        raise ValueError("no exported HTML routes were found")
    preview = base.startswith("/branch-")
    opener = build_opener(ProxyHandler({}))
    pages = {}
    requested = set()
    external = set()

    with serve_export(root, base) as origin:
        def local_url(href, source):
            url = urlsplit(urljoin(origin + base + source, href))
            if url.scheme not in ("http", "https"):
                return None
            if url.netloc == urlsplit(PUBLIC_ORIGIN).netloc:
                # Absolute /latest links intentionally reference published
                # docs. Validate that destination against this revision's
                # route/anchor graph without contacting the live website.
                path = re.sub(r"^/(?:latest|release-[^/]+|branch-[^/]+)(?=/|$)", "", url.path)
                return urlunsplit(("http", urlsplit(origin).netloc, base + (path or "/"), url.query, url.fragment))
            if url.netloc != urlsplit(origin).netloc:
                external.add(urlunsplit(url))
                return None
            return urlunsplit(url)

        def fetch(url, source):
            parts = urlsplit(url)
            plain = urlunsplit((parts.scheme, parts.netloc, parts.path, parts.query, ""))
            if not parts.path.startswith(base + "/"):
                fail("link escaped deployment prefix", parts.path, source)
                return None
            if plain in pages:
                return pages[plain]
            requested.add(plain)
            try:
                with opener.open(plain, timeout=5) as response:
                    raw = response.read(8 * 1024 * 1024 + 1)
                    if len(raw) > 8 * 1024 * 1024:
                        raise ValueError("response exceeds crawl limit")
                    page = Page(raw.decode("utf-8")) if response.headers.get_content_type() == "text/html" else None
                    pages[plain] = page
                    return page
            except (HTTPError, OSError, ValueError) as error:
                fail("destination did not resolve", parts.path, f"{source}: {error}")
                pages[plain] = None
                return None

        def destination(url, source):
            seen = set()
            original_fragment = urlsplit(url).fragment
            while url not in seen and len(seen) < 10:
                seen.add(url)
                page = fetch(url, source)
                if page is None:
                    return None, url
                if not page.refresh:
                    fragment = unquote(urlsplit(url).fragment or original_fragment).split(":~:text=", 1)[0]
                    if fragment and fragment not in ("top",) and fragment not in page.ids:
                        fail("missing HTML anchor", urlsplit(url).path + "#" + fragment, source)
                    return page, url
                if len(page.refresh) != 1 or not re.fullmatch(r"0;\s*url=.+", page.refresh[0], re.IGNORECASE):
                    fail("invalid static redirect", urlsplit(url).path, source)
                    return None, url
                target = page.refresh[0].split("=", 1)[1].strip()
                if target not in page.links:
                    fail("redirect has no matching accessible fallback link", urlsplit(url).path, source)
                next_url = local_url(target, urlsplit(url).path.removeprefix(base))
                if next_url is None:
                    fail("redirect leaves documentation site", target, source)
                    return None, url
                url = next_url
            fail("static redirect cycle", url, source)
            return None, url

        source_count = 0
        for source in sources:
            source = Path(source).resolve(strict=True)
            links = source_references(source)
            if not links:
                fail("source document has no references", str(source), str(source))
            for href in links:
                source_count += 1
                parts = urlsplit(href)
                if parts.scheme or parts.netloc:
                    linked = local_url(href, "/")
                    if linked is not None:
                        destination(linked, str(source))
                elif parts.path:
                    target = source.parent / unquote(parts.path)
                    if not target.exists():
                        fail("source reference does not exist", str(target), str(source))
                # Local Markdown fragments require the hosting platform's
                # Markdown renderer. They are deliberately outside this check.

        analytics_pages = 0
        for route in routes:
            url = origin + base + route
            page = fetch(url, route)
            if page is None:
                continue
            # The exported error page is intentionally non-indexable and has
            # no canonical. It is not an ordinary content route.
            if route == "/404/":
                if "noindex" not in page.robots:
                    fail("error page is indexable", route, route)
                continue
            _, final = destination(url, route)
            expected_canonical = PUBLIC_ORIGIN + urlsplit(final).path
            if page.canonical != [expected_canonical]:
                fail("incorrect canonical URL", route, str(page.canonical))
            if preview and not {"noindex", "nofollow"}.issubset(page.robots):
                fail("preview is indexable", route, route)
            if not preview and "noindex" in page.robots:
                fail("published page is noindex", route, route)
            if analytics_id in page.raw:
                analytics_pages += 1
                if preview:
                    fail("preview includes analytics measurement ID", route, route)
            for href in page.links:
                linked = local_url(href, route)
                if linked is not None:
                    destination(linked, route)
        if not preview and not analytics_pages:
            fail("published export omitted configured analytics", base, base)
        for old, target in contract["redirects"].items():
            route = "/" + old + "/"
            page = fetch(origin + base + route, "historical route contract")
            if page is None or page.refresh != ["0; url=" + base + target] or base + target not in page.links:
                fail("historical redirect contract failed", route, target)
            destination(origin + base + target, route)
        sitemap = ET.parse(root / "sitemap.xml")
        locations = sitemap.findall("{*}url/{*}loc")
        if sitemap.getroot().tag != "{http://www.sitemaps.org/schemas/sitemap/0.9}urlset" or not locations:
            fail("sitemap has no URL set", base, "sitemap.xml")
        for element in locations:
            if not element.text or not element.text.startswith(PUBLIC_ORIGIN + base + "/"):
                fail("sitemap escaped deployment prefix", element.text, "sitemap.xml")
                continue
            destination(origin + element.text.removeprefix(PUBLIC_ORIGIN), "sitemap.xml")
        if not wildcard_robots_disallows((root / "robots.txt").read_text(), (base if preview else "/branch-regression-check") + "/"):
            fail("robots.txt does not exclude transient previews", base, "robots.txt")
    return {"base": base, "html_routes": len(routes), "http_destinations": len(requested),
            "historical_redirects": len(contract["redirects"]), "source_references": source_count, "external_links": sorted(external),
            "failures": [{"reason": reason, "target": target, "sources": sources}
                         for (reason, target), sources in sorted(failures.items())]}


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, required=True)
    parser.add_argument("--base", required=True)
    parser.add_argument("--analytics-id", required=True)
    parser.add_argument("--contract", type=Path, default=Path(__file__).with_name("legacy-route-contract.json"))
    parser.add_argument("--report", type=Path, required=True)
    parser.add_argument("--source-markdown", type=Path, action="append", default=[])
    args = parser.parse_args()
    report = crawl(args.root, args.base, json.loads(args.contract.read_text()), args.analytics_id, args.source_markdown)
    args.report.write_text(json.dumps(report, indent=2) + "\n")
    print(f"Crawled {report['html_routes']} HTML routes, {report['http_destinations']} HTTP destinations, "
          f"{report['historical_redirects']} historical redirects at {args.base}; {len(report['failures'])} failures")
    for failure in report["failures"][:30]:
        print(f"FAIL: {failure['reason']}: {failure['target']} ({failure['sources']})")
    raise SystemExit(bool(report["failures"]))
