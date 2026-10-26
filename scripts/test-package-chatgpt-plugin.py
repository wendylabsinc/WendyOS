#!/usr/bin/env python3
"""Regression checks for the public plugin export boundary and review metadata."""

import copy
import importlib.util
import json
from pathlib import Path
import shutil
import struct
import subprocess
import sys
import tempfile
import unittest
import zipfile
import zlib


SCRIPT = Path(__file__).with_name("package-chatgpt-plugin.py")
SPEC = importlib.util.spec_from_file_location("package_chatgpt_plugin", SCRIPT)
package = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(package)


class PackageTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.source = self.root / "plugins/wendy-chatgpt"
        shutil.copytree(package.SOURCE, self.source)
        self.manifest = json.loads((self.source / "plugin.json").read_text())
        self.mcp = json.loads((self.source / "mcp.json").read_text())

    def save(self):
        (self.source / "plugin.json").write_text(json.dumps(self.manifest))
        (self.source / "mcp.json").write_text(json.dumps(self.mcp))

    def test_archive_retains_metadata_and_excludes_private_files(self):
        (self.source / "gateway.json").write_text('{"credential":"do-not-export"}')
        self.save()
        manifest, mcp, files, gaps = package.validate_package(self.source)
        output = self.root / "plugin.zip"
        package.export_package(self.source, output, manifest, mcp, files)
        prefix = manifest["name"] + "/"
        with zipfile.ZipFile(output) as archive:
            self.assertNotIn(prefix + "gateway.json", archive.namelist())
            self.assertNotIn(prefix + "SUBMISSION.md", archive.namelist())
            exported = json.loads(archive.read(prefix + "plugin.json"))
            extension = exported["extensions"]["com.openai"]
            self.assertEqual(extension["publication"]["countries"], [])
            self.assertIs(extension["review"]["commerce"], False)
            self.assertEqual(len(extension["review"]["test_cases"]["positive"]), 5)
            self.assertEqual(len(extension["review"]["test_cases"]["negative"]), 3)
            self.assertEqual(json.loads(archive.read(prefix + "mcp.json")), mcp)
            auth = mcp["mcpServers"]["wendy-relay"]["extensions"]["com.openai"]["auth"]
            self.assertEqual(auth["baseScopes"], ["mcp:relay"])
            self.assertEqual(auth["client"], {"mode": "cimd"})
            self.assertEqual(auth["defaultScopes"], [])
            self.assertIs(auth["oidcEnabled"], False)
            onboarding = extension["onboardingSkill"]
            self.assertEqual(onboarding, "./skills/wendy/SKILL.md")
            self.assertIn(prefix + onboarding.removeprefix("./"), archive.namelist())
            self.assertEqual(
                {name for name in archive.namelist() if name.startswith(prefix + "skills/")},
                {prefix + "skills/wendy/SKILL.md"},
            )
            self.assertEqual(archive.read(prefix + "skills/wendy/SKILL.md"),
                             (self.source / "skills/wendy/SKILL.md").read_bytes())

    def test_rejects_nested_onboarding_skill(self):
        # Nested supporting files cannot be the public onboarding entrypoint.
        self.manifest["extensions"]["com.openai"]["onboardingSkill"] = "./skills/wendy/references/skills/wendy-onboarding/SKILL.md"
        self.save()
        with self.assertRaisesRegex(ValueError, "onboardingSkill"):
            package.validate_package(self.source)

    def test_rejects_missing_or_non_skill_onboarding(self):
        for reference in ("./skills/missing/SKILL.md", "./skills/wendy/references/wendy.json.md", None):
            with self.subTest(reference=reference):
                self.manifest["extensions"]["com.openai"]["onboardingSkill"] = reference
                self.save()
                with self.assertRaisesRegex(ValueError, "onboardingSkill"):
                    package.validate_package(self.source)

    def test_rejects_additional_public_skill_files(self):
        for name in ("skills/wendy/references/skills/wendy-install/SKILL.md",
                     "skills/wendy/references/setup.md", "skills/wendy/scripts/setup.sh",
                     "skills/wendy/helper.py", "skills/extra/SKILL.md"):
            with self.subTest(name=name):
                path = self.source / name
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text("Unexpected workflow or helper")
                try:
                    with self.assertRaisesRegex(ValueError, "Public skill inventory"):
                        package.validate_package(self.source)
                finally:
                    path.unlink()

    def test_sync_preserves_curated_public_skill_and_full_local_bundles(self):
        """Public ChatGPT skills stay independent of the developer skill bundle."""
        root = self.root / "sync-fixture"
        root.mkdir()

        def write(relative, content):
            destination = root / relative
            destination.parent.mkdir(parents=True, exist_ok=True)
            destination.write_bytes(content)

        def snapshot(directory):
            return {
                path.relative_to(directory).as_posix(): path.read_bytes()
                for path in directory.rglob("*") if path.is_file()
            }

        scripts = root / "scripts"
        scripts.mkdir()
        script = scripts / "sync-agent-skills.py"
        shutil.copy2(Path(__file__).with_name(script.name), script)

        developer = root / "plugins/wendy-agentic-coding"
        group = json.dumps({
            "name": "wendy-agentic-coding",
            "audience": "end-users",
            "skills": ["wendy", "wendy-install"],
        }).encode()
        write(developer / "skill-group.json", group)
        developer_files = {
            "wendy/SKILL.md": (
                b"---\nname: wendy\ndescription: Developer workflow\n---\n"
                b"Use the developer CLI to build an application.\n"
            ),
            "wendy-install/SKILL.md": (
                b"---\nname: wendy-install\ndescription: Local setup\n---\n"
                b"Read [setup](references/setup.md) for local installation.\n"
            ),
            "wendy-install/references/setup.md": (
                b"Developer-only installation instructions.\n"
            ),
        }
        for relative, content in developer_files.items():
            write(developer / "skills" / relative, content)

        public = root / "plugins/wendy-chatgpt"
        public_files = {
            relative: (self.source / relative).read_bytes()
            for relative in ("plugin.json", "mcp.json", "assets/wendy-icon.png",
                             "skills/wendy/SKILL.md")
        }
        for relative, content in public_files.items():
            write(public / relative, content)
        public_before = snapshot(public)

        # The current CLI group writer expects its assets directory to exist.
        cli_skills = root / "go/internal/cli/assets/skills"
        cli_skills.mkdir(parents=True)
        for arguments in ([], ["--check"]):
            result = subprocess.run(
                [sys.executable, str(script), *arguments],
                cwd=root, capture_output=True, text=True, timeout=15,
            )
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            # Compare both the file inventory and bytes after each command.
            self.assertEqual(snapshot(public), public_before)

        self.assertEqual((cli_skills / "end-user-group.json").read_bytes(), group)
        for relative, content in developer_files.items():
            self.assertEqual((cli_skills / relative).read_bytes(), content)

        local_bundles = (
            root / "plugins/wendy-get-started/skills/wendy",
            root / "plugins/wendyos/skills/wendy",
            root / "go/internal/cli/assets/mcp-skills/wendy",
        )
        for bundle in local_bundles:
            with self.subTest(bundle=bundle.relative_to(root).as_posix()):
                self.assertTrue((bundle / "SKILL.md").is_file())
                for relative, content in developer_files.items():
                    self.assertEqual(
                        (bundle / "references/skills" / relative).read_bytes(),
                        content,
                    )

        embedded = root / "go/internal/cli/assets/chatgpt-plugin"
        self.assertEqual(snapshot(embedded), {
            relative: public_files[relative]
            for relative in ("skills/wendy/SKILL.md",)
        })

    def test_no_recording_is_an_explicit_gap(self):
        extension = copy.deepcopy(self.manifest["extensions"]["com.openai"])
        extension["review"].pop("demo_recording_url", None)
        self.assertTrue(any("demo_recording_url" in gap for gap in package.submission_gaps(extension)))

    def test_relay_auth_accepts_complete_submission_metadata(self):
        # This fixture checks metadata compatibility, not remote playback.
        self.manifest["extensions"]["com.openai"]["review"]["demo_recording_url"] = "https://wendy.dev/review-fixture.mp4"
        self.save()
        _, _, _, gaps = package.validate_package(self.source)
        self.assertEqual(gaps, [])

    def test_strict_export_fails_before_writing_incomplete_archive(self):
        self.manifest["extensions"]["com.openai"]["review"].pop("demo_recording_url", None)
        self.save()
        scripts = self.root / "scripts"
        scripts.mkdir()
        shutil.copy2(SCRIPT, scripts / SCRIPT.name)
        output = self.root / "strict.zip"
        result = subprocess.run([sys.executable, "-O", str(scripts / SCRIPT.name), "--output", str(output), "--require-submission-metadata"], capture_output=True, text=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("demo_recording_url", result.stderr)
        self.assertFalse(output.exists())

    def test_rejects_app_bindings_even_when_empty(self):
        self.manifest["extensions"]["com.openai"]["apps"] = {}
        self.save()
        with self.assertRaisesRegex(ValueError, "cannot declare apps"):
            package.validate_package(self.source)

    def test_rejects_bearer_header(self):
        self.mcp["mcpServers"]["wendy-relay"]["headers"] = {"Authorization": "Bearer do-not-export"}
        self.save()
        with self.assertRaisesRegex(ValueError, "authentication headers"):
            package.validate_package(self.source)

    def test_rejects_standalone_gateway_scopes(self):
        auth = self.mcp["mcpServers"]["wendy-relay"]["extensions"]["com.openai"]["auth"]
        for scopes in (["robots:read", "cameras:capture", "apps:control", "preferences:write"],
                       ["mcp:relay", "robots:read"], ["mcp:relay", "mcp:relay"], [], "mcp:relay"):
            with self.subTest(scopes=scopes):
                auth["baseScopes"] = scopes
                self.save()
                with self.assertRaisesRegex(ValueError, "hosted CLI relay scope"):
                    package.validate_package(self.source)

    def test_accepts_explicit_chatgpt_public_identity_and_cimd(self):
        auth = self.mcp["mcpServers"]["wendy-relay"]["extensions"]["com.openai"]["auth"]
        for client in ({"mode": "provided", "clientId": "https://chatgpt.com/oauth/client.json",
                        "tokenEndpointAuthMethod": "none"}, {"mode": "cimd"}):
            with self.subTest(client=client):
                auth["client"] = client
                auth["defaultScopes"] = []
                auth["oidcEnabled"] = False
                self.save()
                _, checked_mcp, _, _ = package.validate_package(self.source)
                checked_auth = checked_mcp["mcpServers"]["wendy-relay"]["extensions"]["com.openai"]["auth"]
                self.assertEqual(checked_auth, auth)

    def test_discovered_oauth_client_stays_omitted_in_archive(self):
        auth = self.mcp["mcpServers"]["wendy-relay"]["extensions"]["com.openai"]["auth"]
        del auth["client"]
        self.save()
        manifest, mcp, files, gaps = package.validate_package(self.source)
        self.assertEqual(gaps, [])
        output = self.root / "discovery.zip"
        package.export_package(self.source, output, manifest, mcp, files)
        with zipfile.ZipFile(output) as archive:
            exported = json.loads(archive.read(manifest["name"] + "/mcp.json"))
        exported_auth = exported["mcpServers"]["wendy-relay"]["extensions"]["com.openai"]["auth"]
        self.assertNotIn("client", exported_auth)
        self.assertEqual(exported_auth, auth)
        auth["clientSecret"] = "do-not-export"
        self.save()
        with self.assertRaisesRegex(ValueError, "Never package an OAuth client secret"):
            package.validate_package(self.source)

    def test_rejects_other_oauth_client_registrations(self):
        auth = self.mcp["mcpServers"]["wendy-relay"]["extensions"]["com.openai"]["auth"]
        clients = (
            {},
            {"mode": "provided", "clientId": "wendy-chatgpt", "tokenEndpointAuthMethod": "none"},
            {"mode": "provided", "clientId": "https://example.org/client.json", "tokenEndpointAuthMethod": "none"},
            {"mode": "provided", "clientId": "https://chatgpt.com/oauth/client.json"},
            {"mode": "provided", "clientId": "https://chatgpt.com/oauth/client.json", "tokenEndpointAuthMethod": "client_secret_post"},
            {"mode": "provided", "clientId": "https://chatgpt.com/oauth/client.json", "tokenEndpointAuthMethod": "private_key_jwt"},
            {"mode": "dcr"}, {"mode": "cimd", "clientId": "wendy-chatgpt"},
        )
        for client in clients:
            with self.subTest(client=client):
                auth["client"] = client
                self.save()
                with self.assertRaisesRegex(ValueError, r'Use the CIMD client \{"mode": "cimd"\}'):
                    package.validate_package(self.source)

    def test_rejects_non_object_oauth_client(self):
        auth = self.mcp["mcpServers"]["wendy-relay"]["extensions"]["com.openai"]["auth"]
        for client in (None, [], "cimd"):
            with self.subTest(client=client):
                auth["client"] = client
                self.save()
                with self.assertRaisesRegex(ValueError, "OAuth client must be an object"):
                    package.validate_package(self.source)

    def test_rejects_inherited_or_nonempty_default_scopes(self):
        auth = self.mcp["mcpServers"]["wendy-relay"]["extensions"]["com.openai"]["auth"]
        auth.pop("defaultScopes", None)
        self.save()
        with self.assertRaisesRegex(ValueError, "defaultScopes"):
            package.validate_package(self.source)
        for scopes in (None, ["robots:read"], ["mcp:relay"], "", {}):
            with self.subTest(scopes=scopes):
                auth["defaultScopes"] = scopes
                self.save()
                with self.assertRaisesRegex(ValueError, "defaultScopes"):
                    package.validate_package(self.source)

    def test_rejects_oidc_enablement_or_scopes(self):
        auth = self.mcp["mcpServers"]["wendy-relay"]["extensions"]["com.openai"]["auth"]
        auth.pop("oidcEnabled", None)
        self.save()
        with self.assertRaisesRegex(ValueError, "oidcEnabled"):
            package.validate_package(self.source)
        for enabled in (None, True, 0, "false"):
            with self.subTest(enabled=enabled):
                auth["oidcEnabled"] = enabled
                self.save()
                with self.assertRaisesRegex(ValueError, "oidcEnabled"):
                    package.validate_package(self.source)
        auth["oidcEnabled"] = False
        for scopes in (["openid"], ["email", "profile"], None, "", {}):
            with self.subTest(scopes=scopes):
                auth["oidcScopesSupported"] = scopes
                self.save()
                with self.assertRaisesRegex(ValueError, "Do not declare OIDC scopes"):
                    package.validate_package(self.source)
        auth["oidcScopesSupported"] = []
        self.save()
        package.validate_package(self.source)

    def test_rejects_old_or_additional_mcp_server_keys(self):
        server = self.mcp["mcpServers"]["wendy-relay"]
        for servers in ({"wendy-robots": server}, {"wendy-relay": server, "other": server},
                        ["wendy-relay"], "wendy-relay", None):
            with self.subTest(servers=servers):
                self.mcp["mcpServers"] = servers
                self.save()
                with self.assertRaisesRegex(ValueError, "exactly its wendy-relay server"):
                    package.validate_package(self.source)

    def test_rejects_oauth_client_secret(self):
        auth = self.mcp["mcpServers"]["wendy-relay"]["extensions"]["com.openai"]["auth"]
        for location in (auth, auth["client"]):
            with self.subTest(location="auth" if location is auth else "client"):
                location["clientSecret"] = "do-not-export"
                self.save()
                with self.assertRaisesRegex(ValueError, "Never package an OAuth client secret"):
                    package.validate_package(self.source)
                del location["clientSecret"]

    def test_rejects_array_in_expected_behavior(self):
        self.manifest["extensions"]["com.openai"]["review"]["test_cases"]["positive"][0]["expected_behavior"] = ["not a string"]
        self.save()
        with self.assertRaisesRegex(ValueError, "expected_behavior"):
            package.validate_package(self.source)

    def test_rejects_symlinked_asset(self):
        (self.source / "assets/private.txt").symlink_to(self.source / "plugin.json")
        with self.assertRaisesRegex(ValueError, "Symlinks"):
            package.validate_package(self.source)

    def test_rejects_hidden_and_generated_component_files(self):
        for name in ("skills/wendy/.env", "assets/.DS_Store", "skills/wendy/.cache/data.json",
                     "skills/wendy/__pycache__/skill.pyc", "assets/node_modules/package/index.js",
                     "assets/__MACOSX/icon.png", "skills/wendy/notes.md~", "assets/icon.png.bak"):
            with self.subTest(name=name):
                source = self.root / "case"
                shutil.copytree(self.source, source)
                try:
                    path = source / name
                    path.parent.mkdir(parents=True, exist_ok=True)
                    path.write_text("do-not-export")
                    with self.assertRaisesRegex(ValueError, "Hidden or generated"):
                        package.validate_package(source)
                finally:
                    shutil.rmtree(source)

    def test_rejects_truncated_png(self):
        icon = self.source / "assets/wendy-icon.png"
        original = icon.read_bytes()
        for data in (original[:24], original[:-1], original[:-12]):
            with self.subTest(length=len(data)):
                icon.write_bytes(data)
                with self.assertRaisesRegex(ValueError, "PNG"):
                    package.validate_package(self.source)

    def test_rejects_png_crc_corruption(self):
        icon = self.source / "assets/wendy-icon.png"
        data = bytearray(icon.read_bytes())
        data[29] ^= 1  # Corrupt IHDR's CRC without changing its dimensions.
        icon.write_bytes(data)
        with self.assertRaisesRegex(ValueError, "PNG chunk CRC"):
            package.validate_package(self.source)

    def test_rejects_png_without_image_data_and_with_trailing_data(self):
        icon = self.source / "assets/wendy-icon.png"
        original = icon.read_bytes()
        cases = ((original[:33] + original[-12:], "missing PNG image data"),
                 (original + b"unrelated", "end with an empty PNG IEND"))
        for data, error in cases:
            with self.subTest(error=error):
                icon.write_bytes(data)
                with self.assertRaisesRegex(ValueError, error):
                    package.validate_package(self.source)

    def test_accepts_png_with_consecutive_image_chunks(self):
        def chunk(kind, payload):
            return struct.pack(">I", len(payload)) + kind + payload + struct.pack(">I", zlib.crc32(kind + payload))

        # A real 256-square RGB image with compressed scanlines, split over IDATs.
        compressed = zlib.compress((b"\x00" + b"\xff\x00\x00" * 256) * 256)
        midpoint = len(compressed) // 2
        data = (b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", struct.pack(">IIBBBBB", 256, 256, 8, 2, 0, 0, 0))
                + chunk(b"IDAT", compressed[:midpoint]) + chunk(b"IDAT", compressed[midpoint:]) + chunk(b"IEND", b""))
        (self.source / "assets/wendy-icon.png").write_bytes(data)
        package.validate_package(self.source)

    def test_rejects_credential_url(self):
        self.manifest["extensions"]["com.openai"]["interface"]["supportURL"] = "https://user:secret@wendy.dev/support"
        self.save()
        with self.assertRaisesRegex(ValueError, "supportURL"):
            package.validate_package(self.source)


if __name__ == "__main__":
    unittest.main()
