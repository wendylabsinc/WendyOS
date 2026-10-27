import base64
import hashlib
import json
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import publication
from versioning import derive

SHA = "a" * 40
REPOSITORY = "wendylabsinc/wendy-core"


class PublicationTests(unittest.TestCase):
    def setUp(self):
        # Integrity/context tests are independent of package-format validation,
        # which has real archive fixtures in PackageVersionTests below.
        guard = patch.object(publication, "verify_versions")
        guard.start()
        self.addCleanup(guard.stop)

    def run_info(self, event="push", branch="main"):
        return {"id": 123, "run_attempt": 2, "event": event, "head_branch": branch,
                "head_sha": SHA, "head_repository": {"full_name": REPOSITORY},
                "conclusion": "success", "path": ".github/workflows/ci.yml",
                "pull_requests": [{"number": 42}]}

    def api(self, run, *, fork=False, current_sha=SHA):
        def response(path):
            if "/actions/runs/" in path:
                return run
            if "/contents/VERSION" in path:
                return {"content": base64.b64encode(b"0.1.0\n").decode()}
            if "/pulls/" in path:
                return {"head": {"repo": {"full_name": "other/fork" if fork else REPOSITORY},
                                 "sha": current_sha, "ref": "feature"}}
            if "/git/ref/heads/" in path:
                return {"object": {"sha": current_sha}}
            return {"sha": current_sha}
        return response

    def test_authenticated_redirects_do_not_leak_credentials(self):
        from urllib.request import Request
        request = Request("https://maven.pkg.github.com/source", headers={"Authorization": "Basic secret"})
        handler = publication.SafeRedirect()
        same = handler.redirect_request(request, None, 302, "Found", {}, "https://maven.pkg.github.com/other")
        self.assertEqual(same.get_header("Authorization"), "Basic secret")
        other = handler.redirect_request(request, None, 302, "Found", {}, "https://artifacts.example/download")
        self.assertIsNone(other.get_header("Authorization"))
        with self.assertRaises(ValueError):
            handler.redirect_request(request, None, 302, "Found", {}, "http://artifacts.example/download")

    def test_rederive_preview_from_trusted_run_metadata(self):
        run = self.run_info("pull_request", "feature")
        with patch.object(publication, "api", side_effect=self.api(run)):
            info = publication.context(REPOSITORY, 123, 1)
        self.assertEqual(info, derive("0.1.0", "pr", source_sha=SHA, branch="feature", pr=42, run_id=123))

    def test_seed_context_and_go_tag_are_exact(self):
        run = self.run_info("push", "v0.1.0-seed.1")
        original = self.api(run)
        def response(path, data=None):
            if "/contents/VERSION" in path:
                return {"content": base64.b64encode(b"0.1.0-seed.1\n").decode()}
            return original(path)
        with patch.object(publication, "api", side_effect=response), \
                patch.object(publication, "tag_commit", return_value=SHA) as tag:
            info = publication.context(REPOSITORY, 123, 1)
            self.assertEqual(info["version"], "0.1.0-seed.1")
            self.assertEqual(info["npmTag"], "seed")
            publication.go_tag(REPOSITORY, info)
            tag.assert_called_with(REPOSITORY, "v0.1.0-seed.1")
        for branch in ("v0.1.0", "v0.1.0-rc.1", "v0.1.0-seed.01"):
            run["head_branch"] = branch
            with self.subTest(branch=branch), patch.object(publication, "api", side_effect=response), \
                    self.assertRaises(ValueError):
                publication.context(REPOSITORY, 123, 1)

    def test_retries_keep_originating_attempt(self):
        run = self.run_info()
        with patch.object(publication, "api", side_effect=self.api(run)):
            self.assertEqual(publication.context(REPOSITORY, 123, 1)["attempt"], 1)
            with self.assertRaises(ValueError):
                publication.context(REPOSITORY, 123, 3)

    def test_forks_and_stale_previews_fail_closed(self):
        run = self.run_info("pull_request", "feature")
        for options in ({"fork": True}, {"current_sha": "b" * 40}):
            with self.subTest(options=options), patch.object(publication, "api", side_effect=self.api(run, **options)), \
                    self.assertRaises(ValueError):
                publication.context(REPOSITORY, 123)
        with patch.object(publication, "api", side_effect=self.api(self.run_info(), current_sha="b" * 40)), \
                self.assertRaises(ValueError):
            publication.context(REPOSITORY, 123)

    def test_manual_branch_need_not_have_pr_or_special_name(self):
        run = self.run_info("workflow_dispatch", "very-good")
        with patch.object(publication, "api", side_effect=self.api(run)):
            self.assertIsNone(publication.context(REPOSITORY, 123)["pr"])

    def seal(self, directory, info):
        (directory / "release.json").write_text(json.dumps({"identity": info, "version": info["version"]}))
        (directory / "sample.tgz").write_bytes(b"packaged bytes")
        entries = [f"{hashlib.sha256(file.read_bytes()).hexdigest()}  {file.name}\n" for file in sorted(directory.iterdir())]
        (directory / "SHA256SUMS").write_text("".join(entries))

    def test_integrity_and_run_identity_are_both_required(self):
        info = derive("0.1.0", "nightly", source_sha=SHA, run_id=123)
        with tempfile.TemporaryDirectory() as temp:
            directory = Path(temp)
            self.seal(directory, info)
            publication.verify(directory, info)
            with self.assertRaises(ValueError):
                publication.verify(directory, derive("0.1.0", "stable", source_sha=SHA))
            (directory / "sample.tgz").write_bytes(b"modified")
            with self.assertRaises(ValueError):
                publication.verify(directory, info)

    def test_unsealed_files_and_path_traversal_rejected(self):
        info = derive("0.1.0", "nightly", source_sha=SHA, run_id=123)
        with tempfile.TemporaryDirectory() as temp:
            directory = Path(temp)
            self.seal(directory, info)
            (directory / "unexpected").write_text("extra")
            with self.assertRaises(ValueError):
                publication.verify(directory, info)
            (directory / "unexpected").unlink()
            (directory / "SHA256SUMS").write_text("0" * 64 + "  ../outside\n")
            with self.assertRaises(ValueError):
                publication.verify(directory, info)


class SwiftReleaseTests(unittest.TestCase):
    info = derive("0.1.0-seed.1", "stable", source_sha=SHA)
    source = "let before = 1\n// BEGIN BINARY TARGETS\nlet binaryTargets: [Target] = local()\n// END BINARY TARGETS\nlet after = 2\n"

    def assets(self, directory):
        import swift_release
        for name in swift_release.ASSETS:
            (directory / swift_release.asset(name, self.info["version"])).write_bytes(name.encode())

    def test_release_manifest_replaces_only_the_binary_block(self):
        import swift_release
        with tempfile.TemporaryDirectory() as temp:
            self.assets(Path(temp))
            text = swift_release.release_manifest(self.source, REPOSITORY, self.info["version"], Path(temp))
        version = self.info["version"]
        url = f"https://github.com/{REPOSITORY}/releases/download/v{version}/CWendyCore-{version}.xcframework.zip"
        self.assertTrue(text.startswith("let before = 1\n// BEGIN BINARY TARGETS\n"))
        self.assertTrue(text.endswith("// END BINARY TARGETS\nlet after = 2\n"))
        self.assertIn(f'url: "{url}", checksum: "{hashlib.sha256(b"CWendyCoreApple").hexdigest()}"', text)
        self.assertNotIn("local()", text)
        for invalid in ("1.0", '1.0.0"; evil', "0.1.0-rc.1", "0.1.0-seed.0", "0.1.0-seed.01"):
            with self.subTest(version=invalid), self.assertRaises(ValueError):
                swift_release.release_manifest(self.source, REPOSITORY, invalid, Path(temp))
        with self.assertRaises(ValueError):
            swift_release.manifest("let binaryTargets: [Target] = []\n", {})

    def github(self, tags, created):
        from urllib.error import HTTPError
        manifests = {SHA: self.source}

        def response(path, data=None):
            if data is not None:
                created.append((path, data))
                if path.endswith("/git/blobs"):
                    manifests["release"] = data["content"]
                return {"sha": "release" if path.endswith("/git/commits") else "new"}
            if "/git/ref/tags/" in path:
                tag = path.rsplit("/", 1)[1]
                if tag not in tags:
                    raise HTTPError(path, 404, "Not Found", {}, None)
                return {"object": {"type": "commit", "sha": tags[tag]}}
            if "/contents/Package.swift" in path:
                return {"content": base64.b64encode(manifests[path.rsplit("=", 1)[1]].encode()).decode()}
            if path.endswith("/git/commits/" + SHA):
                return {"tree": {"sha": "tree"}}
            return {"parents": [{"sha": SHA}]}
        return response

    def test_tag_is_a_release_only_child_of_the_tested_source(self):
        created = []
        with tempfile.TemporaryDirectory() as temp, \
                patch.object(publication, "api", side_effect=self.github({}, created)):
            self.assets(Path(temp))
            publication.swift_tag(Path(temp), REPOSITORY, self.info)
            commit = next(data for path, data in created if path.endswith("/git/commits"))
            self.assertEqual(commit["parents"], [SHA])
            self.assertEqual(created[-1][1], {"ref": "refs/tags/" + self.info["version"], "sha": "release"})
            # A retry accepts the identical tag and refuses a different one.
            api = self.github({self.info["version"]: "release"}, [])
            api(f"repos/{REPOSITORY}/git/blobs", {"content": created[0][1]["content"]})
            with patch.object(publication, "api", side_effect=api):
                publication.swift_tag(Path(temp), REPOSITORY, self.info)
            api = self.github({self.info["version"]: "release"}, [])
            api(f"repos/{REPOSITORY}/git/blobs", {"content": "different"})
            with patch.object(publication, "api", side_effect=api), self.assertRaises(ValueError):
                publication.swift_tag(Path(temp), REPOSITORY, self.info)


class PackageVersionTests(unittest.TestCase):
    def archive(self, path, name, content):
        import io
        import tarfile
        with tarfile.open(path, "w:gz") as archive:
            member = tarfile.TarInfo(name)
            member.size = len(content)
            archive.addfile(member, io.BytesIO(content))

    def fixtures(self, directory, info):
        import zipfile
        from build import TARGETS
        self.archive(directory / f"wendylabs-core-{info['version']}.tgz", "package/package.json",
                     json.dumps({"name": "@wendylabs/core", "version": info["version"]}).encode())
        for target in TARGETS:
            with zipfile.ZipFile(directory / f"wheel-{target}.whl", "w") as archive:
                archive.writestr("wendy_core.dist-info/METADATA", f"Name: wendy-core\nVersion: {info['pythonVersion']}\n")
        for suffix in TARGETS:
            self.archive(directory / f"wendy-core-{info['version']}-{suffix}.tar.gz", "VERSION", info["version"].encode())
        with zipfile.ZipFile(directory / f"CWendyCore-{info['version']}.artifactbundle.zip", "w") as archive:
            bundle = {"artifacts": {"CWendyCore": {"version": info["version"]}}}
            archive.writestr("CWendyCore.artifactbundle/info.json", json.dumps(bundle))
        with zipfile.ZipFile(directory / f"CWendyCore-{info['version']}.xcframework.zip", "w") as archive:
            archive.writestr("CWendyCore.xcframework/Info.plist", "")

    def test_inner_versions_must_match_even_if_artifacts_are_sealed(self):
        info = derive("0.1.0", "pr", source_sha=SHA, pr=42, branch="feature", run_id=123)
        with tempfile.TemporaryDirectory() as temp:
            directory = Path(temp)
            self.fixtures(directory, info)
            publication.verify_versions(directory, info)
            self.archive(directory / f"wendylabs-core-{info['version']}.tgz", "package/package.json",
                         b'{"name":"@wendylabs/core","version":"0.1.0"}')
            with self.assertRaises(ValueError):
                publication.verify_versions(directory, info)


class MonorepoPublicationTests(unittest.TestCase):
    def test_publication_cli_is_disabled_before_credentials_or_network_access(self):
        import sys
        for command in ('context', 'go-tag', 'swift-tag', 'maven', 'npm-check', 'pypi-check'):
            with self.subTest(command=command), \
                    patch.object(sys, 'argv', ['publication.py', command]), \
                    patch.object(publication, 'api') as api:
                with self.assertRaisesRegex(ValueError, 'publication is disabled'):
                    publication.main()
                api.assert_not_called()


if __name__ == "__main__":
    unittest.main()
