"""Regression cases discovered during independent simulation review."""

import io
import json
from pathlib import Path
import random
import tempfile
import unittest

from sim.run import PackageError, gas_value, load_package, number, read_bounded


FIXTURE = Path(__file__).resolve().parents[1] / "examples" / "office"


class ReviewTests(unittest.TestCase):
    def test_large_finite_sigma_does_not_overflow(self):
        package = load_package(FIXTURE / "world.json")
        gas = package["gas_model"]
        gas.update(sigma_m=1e308, noise_stddev_ppm=0)
        self.assertEqual(gas_value(gas, [0, 0, 0], random.Random(1)), 31)

    def test_derived_concentration_overflow_is_explicit(self):
        gas = load_package(FIXTURE / "world.json")["gas_model"]
        gas.update(background_ppm=1e308, peak_ppm=1e308, noise_stddev_ppm=0)
        with self.assertRaisesRegex(PackageError, "non-finite concentration"):
            gas_value(gas, gas["source_local_m"], random.Random(1))

    def test_large_json_integer_has_controlled_validation_error(self):
        with self.assertRaises(PackageError):
            number(10**400, "value")

    def test_reader_requests_only_limit_plus_one(self):
        class ObservedStream(io.BytesIO):
            def read(self, size=-1):
                self.requested = size
                return super().read(size)

        class TestPath:
            def __init__(self):
                self.stream = ObservedStream(b"x" * 100)

            def open(self, mode):
                return self.stream

        path = TestPath()
        with self.assertRaisesRegex(PackageError, "exceeds"):
            read_bounded(path, 10, "fixture")
        self.assertEqual(path.stream.requested, 11)

    def test_oversized_manifest_and_model_are_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            manifest_path = root / "world.json"
            manifest_path.write_bytes(b" " * 1_000_001)
            with self.assertRaisesRegex(PackageError, "manifest exceeds"):
                load_package(manifest_path)
            manifest = json.loads((FIXTURE / "world.json").read_text())
            manifest_path.write_text(json.dumps(manifest))
            (root / "office.xml").write_bytes(b" " * 8_000_001)
            with self.assertRaisesRegex(PackageError, "model exceeds"):
                load_package(manifest_path)


if __name__ == "__main__":
    unittest.main()
