#!/usr/bin/env python3
"""Check real Noble Deb822 layouts, preserved fields and rejected changes."""

import importlib.util
import tempfile
import unittest
from email.parser import Parser
from pathlib import Path

spec = importlib.util.spec_from_file_location(
    "mirrors", Path(__file__).with_name("use-canonical-ubuntu-mirrors.py")
)
mirrors = importlib.util.module_from_spec(spec)
spec.loader.exec_module(mirrors)

# Actual etc/apt/sources.list.d/ubuntu.sources, with comments omitted, from
# https://cdimage.ubuntu.com/ubuntu-base/releases/24.04/release/ubuntu-base-24.04.3-base-amd64.tar.gz
# Archive SHA256: 6bc2cde3930ad088b3bb46fa45279e96d25bc3810f209850ecbe4722711874f9
NOBLE = """Types: deb
URIs: http://archive.ubuntu.com/ubuntu/
Suites: noble noble-updates noble-backports
Components: main universe restricted multiverse
Signed-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg

Types: deb
URIs: http://security.ubuntu.com/ubuntu/
Suites: noble-security
Components: main universe restricted multiverse
Signed-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg
"""
SELECTOR = "mirror+file:/etc/apt/apt-mirrors.txt"
VENDOR = """Types: deb
URIs: https://packages.microsoft.com/repos/azure-cli/
Suites: noble
Components: main
Signed-By: /usr/share/keyrings/microsoft.gpg
"""


def pockets(text):
    result = {}
    for stanza in text.strip().split("\n\n"):
        fields = dict(Parser().parsestr(stanza).items())
        for suite in fields.pop("Suites").split():
            result[suite] = fields.copy()
    return result


class CanonicalMirrorsTests(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.sources = Path(directory.name) / "ubuntu.sources"

    def check_layout(self, text):
        self.sources.write_text(text)
        mirrors.configure(self.sources)
        actual = pockets(self.sources.read_text())
        expected = pockets(text)
        self.assertEqual(actual.keys(), expected.keys())
        for suite, fields in actual.items():
            host = "security" if suite == "noble-security" else "archive"
            self.assertEqual(fields.pop("URIs"), f"https://{host}.ubuntu.com/ubuntu/")
            expected[suite].pop("URIs")
            self.assertEqual(fields, expected[suite])
        once = self.sources.read_bytes()
        mirrors.configure(self.sources)
        self.assertEqual(self.sources.read_bytes(), once)

    def test_canonical_base_and_azure_selector(self):
        self.check_layout(NOBLE)
        # runner-images configure-apt-sources.sh at e06ccbf37708c99aa5d2db14c9e9c4a2664ed761
        # replaces Azure URIs with this selector, including the security pocket.
        self.check_layout(NOBLE.replace("http://archive.ubuntu.com/ubuntu/", SELECTOR)
                          .replace("http://security.ubuntu.com/ubuntu/", SELECTOR))

    def test_combined_pockets(self):
        stanza = NOBLE.split("\n\n")[0].replace("noble-backports", "noble-backports noble-security")
        self.check_layout(stanza.replace("http://archive.ubuntu.com/ubuntu/", SELECTOR))

    def test_third_party_sources_preserved(self):
        vendor_file = self.sources.with_name("microsoft.sources")
        vendor_file.write_text(VENDOR)
        self.sources.write_text(NOBLE + "\n" + VENDOR)
        mirrors.configure(self.sources)
        self.assertTrue(self.sources.read_text().endswith(VENDOR))
        self.assertEqual(vendor_file.read_text(), VENDOR)

    def test_unexpected_sources_fail_without_writing(self):
        for text in (
            NOBLE.replace("noble", "jammy"),
            NOBLE.replace("ubuntu-archive-keyring.gpg", "other.gpg"),
            NOBLE.replace("http://archive.ubuntu.com/ubuntu/", "https://unexpected.example/"),
            NOBLE.replace("Types: deb", "Types: deb-src"),
            NOBLE.replace("Suites: noble-security", "Suites: noble-proposed"),
            NOBLE.split("\n\n")[0],
            NOBLE.replace("URIs: ", "URIs: " + SELECTOR + " "),
            NOBLE.replace("Types: deb", "Types: deb\nTypes: deb"),
            NOBLE.replace("Types: deb", "Types: deb\ntypes: deb-src"),
            NOBLE.replace("Suites: noble-security", "Suites:\n noble-security"),
            VENDOR,
        ):
            with self.subTest(text=text):
                self.sources.write_text(text)
                with self.assertRaises(ValueError):
                    mirrors.configure(self.sources)
                self.assertEqual(self.sources.read_text(), text)

    def test_missing_source_fails(self):
        with self.assertRaises(FileNotFoundError):
            mirrors.configure(self.sources)


if __name__ == "__main__":
    unittest.main()
