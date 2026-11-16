"""Keep local coverage evidence paths tied to files in this checkout."""
import json
from pathlib import Path
import unittest

ROOT = Path(__file__).resolve().parents[2]
# These citations belong to the named external repository, not this checkout.
EXTERNAL = {
    "wendy-auth/Tests/AuthServerTests/ChatGPTCIMDExchangeTests.swift",
    "wendy-auth/scripts/ci/test-cimd-companion.py",
    "cloud/swift/Tests/GRPCServicesTests/RelayCIMDTransportTests.swift",
    "scripts/test-orin-seed-producer.py",
    "scripts/orin_seed_producer_test.py",
    ".github/workflows/scripts-ci.yml",
}

class CoveragePaths(unittest.TestCase):
    def test_local_test_citations_exist(self):
        def inspect(value):
            if isinstance(value, dict):
                for key, child in value.items():
                    if key == 'tests':
                        for path in child:
                            if path not in EXTERNAL:
                                with self.subTest(path=path):
                                    self.assertTrue((ROOT / path).is_file(), 'missing local evidence file')
                    else:
                        inspect(child)
            elif isinstance(value, list):
                for child in value:
                    inspect(child)
        inspect(json.loads((ROOT / '.github/regression-coverage/manifest.json').read_text()))
