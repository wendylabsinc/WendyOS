"""Exercise the crawler against real temporary HTTP exports, never public sites."""
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest

spec = importlib.util.spec_from_file_location("crawl_export", Path(__file__).with_name("crawl-export.py"))
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class ExportContractTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.base = "/latest"
        self.contract = {"redirects": {"old": "/guide/"}}

    def page(self, route, body="", canonical=None, robots=None, analytics=None):
        path = self.root / route.strip("/") / "index.html"
        path.parent.mkdir(parents=True, exist_ok=True)
        canonical = canonical or "https://docs.wendy.dev" + self.base + route
        robots = robots or ("noindex,nofollow" if self.base.startswith("/branch-") else "index,follow")
        analytics = analytics if analytics is not None else ("" if self.base.startswith("/branch-") else "G-FIXTURE")
        path.write_text(f'<html><link rel="canonical" href="{canonical}"><meta name="robots" content="{robots}"><body>{body}{analytics}</body></html>')

    def fixture(self):
        self.page("/", f'<a href="{self.base}/old/#section">guide</a>')
        self.page("/guide/", '<h1 id="section">Guide</h1>')
        self.page("/old/", f'<meta http-equiv="refresh" content="0; url={self.base}/guide/"><a href="{self.base}/guide/">Moved</a>', canonical="https://docs.wendy.dev" + self.base + "/guide/")
        (self.root / "sitemap.xml").write_text(f'<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9"><url><loc>https://docs.wendy.dev{self.base}/guide/</loc></url></urlset>')
        (self.root / "robots.txt").write_text("User-agent: *\nDisallow: /branch-\n")

    def failures(self):
        return module.crawl(self.root, self.base, self.contract, "G-FIXTURE")["failures"]

    def test_both_real_http_exports_resolve_redirect_fragment(self):
        for base in ("/latest", "/branch-regression-test"):
            with self.subTest(base=base):
                self.base = base
                self.fixture()
                self.assertEqual(self.failures(), [])

    def test_removed_route_is_http_404(self):
        self.fixture()
        (self.root / "guide/index.html").unlink()
        self.assertIn("destination did not resolve", {f["reason"] for f in self.failures()})

    def test_removed_anchor_on_redirect_target_fails(self):
        self.fixture()
        self.page("/guide/", '<h1 id="renamed">Guide</h1>')
        self.assertIn("missing HTML anchor", {f["reason"] for f in self.failures()})

    def test_missing_legacy_alias_cannot_hide_behind_current_links(self):
        self.fixture()
        self.page("/", "no links")
        (self.root / "old/index.html").unlink()
        self.assertIn("historical redirect contract failed", {f["reason"] for f in self.failures()})

    def test_redirect_cycle_is_bounded(self):
        self.fixture()
        self.page("/guide/", f'<meta http-equiv="refresh" content="0; url={self.base}/old/"><a href="{self.base}/old/">back</a>')
        self.assertIn("static redirect cycle", {f["reason"] for f in self.failures()})

    def test_unprefixed_link_fails(self):
        self.fixture()
        self.page("/", '<a href="/guide/">wrong prefix</a>')
        self.assertIn("link escaped deployment prefix", {f["reason"] for f in self.failures()})

    def test_preview_metadata_and_analytics_mutations_fail(self):
        self.base = "/branch-regression-test"
        self.fixture()
        self.page("/guide/", '<h1 id="section">Guide</h1>', robots="index,follow", analytics="G-FIXTURE")
        reasons = {f["reason"] for f in self.failures()}
        self.assertIn("preview is indexable", reasons)
        self.assertIn("preview includes analytics measurement ID", reasons)

    def test_published_missing_analytics_fails(self):
        self.fixture()
        for path in self.root.rglob("*.html"):
            path.write_text(path.read_text().replace("G-FIXTURE", ""))
        self.assertIn("published export omitted configured analytics", {f["reason"] for f in self.failures()})

    def test_empty_or_wrong_root_sitemap_fails(self):
        self.fixture()
        for xml in ('<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9"/>', '<not-a-sitemap/>'):
            with self.subTest(xml=xml):
                (self.root / "sitemap.xml").write_text(xml)
                self.assertIn("sitemap has no URL set", {f["reason"] for f in self.failures()})

    def test_source_markdown_file_and_real_http_anchor_references(self):
        self.fixture()
        readme = self.root / "README.md"
        local = self.root / "example.py"
        local.write_text("print('example')\n")
        readme.write_text('[source](example.py)\n[guide](https://docs.wendy.dev/latest/guide/#section)\n')
        report = module.crawl(self.root, self.base, self.contract, "G-FIXTURE", [readme])
        self.assertEqual(report["failures"], [])
        self.assertEqual(report["source_references"], 2)
        local.unlink()
        readme.write_text(readme.read_text().replace("#section", "#removed"))
        reasons = {f["reason"] for f in module.crawl(self.root, self.base, self.contract, "G-FIXTURE", [readme])["failures"]}
        self.assertIn("source reference does not exist", reasons)
        self.assertIn("missing HTML anchor", reasons)

    def test_canonical_and_sitemap_wrong_base_fail(self):
        self.fixture()
        self.page("/guide/", '<h1 id="section">Guide</h1>', canonical="https://docs.wendy.dev/guide/")
        sitemap = self.root / "sitemap.xml"
        sitemap.write_text(sitemap.read_text().replace("/latest/", "/"))
        reasons = {f["reason"] for f in self.failures()}
        self.assertIn("incorrect canonical URL", reasons)
        self.assertIn("sitemap escaped deployment prefix", reasons)


if __name__ == "__main__":
    unittest.main()
