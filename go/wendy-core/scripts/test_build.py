import json
import os
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import build
from check_release import require_compatible
from versioning import base_version, derive


def api(version, **functions):
    return {"version": version, "c": {"functions": [{"name": name, "signature": signature} for name, signature in functions.items()]}}


class ReleaseTests(unittest.TestCase):
    def test_versions_match_and_wrong_tags_fail(self):
        info = derive(base_version(), "stable", source_sha=build.identity()["sourceSha"])
        with patch.dict(os.environ, {"CORE_BUILD_IDENTITY": json.dumps(info)}):
            self.assertEqual(build.check_version("go/wendy-core/v" + info["version"]), info["version"])
            with self.assertRaises(ValueError):
                build.check_version("v-not-semver")

    def test_go_development_version_must_advance_with_version(self):
        # A future VERSION bump must fail until the source fallback is updated.
        with patch.object(build, "base_version", return_value="99.0.0"):
            with self.assertRaisesRegex(ValueError, "Go development version differs"):
                build.check_version()

    def test_removal_requires_major_bump_even_before_one(self):
        with self.assertRaises(ValueError):
            require_compatible(api("0.1.0", WendyHelloWorld="int32_t (void)"), api("0.2.0"))
        require_compatible(api("0.1.0", WendyHelloWorld="int32_t (void)"), api("1.0.0"))

    def test_addition_is_not_a_patch(self):
        with self.assertRaises(ValueError):
            require_compatible(api("1.0.0"), api("1.0.1", WendyHelloWorld="int32_t (void)"))
        require_compatible(api("1.0.0"), api("1.1.0", WendyHelloWorld="int32_t (void)"))

    def test_seed_release_order_and_compatibility(self):
        require_compatible(api("0.0.1"), api("0.1.0-seed.1", WendyCoreVersion="char *(void)"))
        require_compatible(api("0.1.0-seed.1"), api("0.1.0-seed.2"))
        require_compatible(api("0.1.0-seed.2"), api("0.1.0"))
        with self.assertRaises(ValueError):
            require_compatible(api("0.1.0"), api("0.1.0-seed.1"))

    def test_unchanged_operation_allows_patch(self):
        require_compatible(api("1.0.0", WendyHelloWorld="int32_t (void)"), api("1.0.1", WendyHelloWorld="int32_t (void)"))

    def test_distribution_license_copies_match_the_root(self):
        with tempfile.TemporaryDirectory() as temp:
            directories = {name: Path(temp) / name for name in ("JS", "PYTHON")}
            with patch.multiple(build, **directories):
                build.stage_licenses()
            for directory in directories.values():
                for name in ("LICENSE", "NOTICE"):
                    self.assertEqual((directory / name).read_bytes(), (build.ROOT / name).read_bytes())

    def test_assembly_rejects_wrong_artifact_identity(self):
        with tempfile.TemporaryDirectory() as temp:
            directory = Path(temp) / "native-linux-x64"
            directory.mkdir()
            (directory / "manifest.json").write_text(json.dumps({"version": "9.0.0", "target": "linux-x64"}))
            # Do not remove a developer's staged artifacts while testing failure.
            with patch.object(build.shutil, "rmtree"), self.assertRaises(ValueError):
                build.assemble(Path(temp))


if __name__ == "__main__":
    unittest.main()
