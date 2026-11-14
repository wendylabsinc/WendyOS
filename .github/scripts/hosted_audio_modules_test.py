#!/usr/bin/env python3
"""Offline source/configuration checks. Never load or install kernel modules."""
import hashlib
import importlib.util
import io
import os
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import unittest
from unittest.mock import patch

SCRIPT = Path(__file__).with_name("prepare-hosted-audio-modules.py")
spec = importlib.util.spec_from_file_location("audio_modules", SCRIPT)
modules = importlib.util.module_from_spec(spec)
spec.loader.exec_module(modules)
AZURE_RELEASE = "7.0.0-1017-azure"
AZURE = modules.PINS[AZURE_RELEASE]
AWS_RELEASE = "7.0.0-1014-aws"
AWS = modules.PINS[AWS_RELEASE]


class AudioModulesTests(unittest.TestCase):
    def test_local_invocation_refuses_before_any_setup(self):
        result = subprocess.run([sys.executable, str(SCRIPT)],
                                env=dict(os.environ, GITHUB_ACTIONS=""),
                                capture_output=True, text=True, timeout=5)
        self.assertEqual(result.returncode, 2)
        self.assertIn("requires the disposable GitHub-hosted", result.stderr)
        with patch.dict(os.environ, {"GITHUB_ACTIONS": ""}), patch.object(modules.subprocess, "run") as run:
            with self.assertRaises(ValueError):
                modules.main()
            run.assert_not_called()

    def test_headers_require_exact_release_and_disabled_sound(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "include/config").mkdir(parents=True)
            (root / "include/config/kernel.release").write_text(AZURE_RELEASE)
            (root / "Module.symvers").write_text("kernel symbol CRCs")
            base = "# CONFIG_SOUND is not set\nCONFIG_MODULES=y\nCONFIG_MODVERSIONS=y\n"
            for config, release, arch, version, passes in [
                (base, AZURE_RELEASE, "x86_64", AZURE["version"], True),
                (base.replace("# CONFIG_SOUND is not set", "CONFIG_SOUND=m"), AZURE_RELEASE, "x86_64", AZURE["version"], False),
                (base + "CONFIG_MODULE_SIG_FORCE=y\n", AZURE_RELEASE, "x86_64", AZURE["version"], False),
                (base.replace("CONFIG_MODVERSIONS=y\n", ""), AZURE_RELEASE, "x86_64", AZURE["version"], False),
                (base, "7.0.0-1018-azure", "x86_64", AZURE["version"], False),
                (base, AZURE_RELEASE, "aarch64", AZURE["version"], False),
                (base, AZURE_RELEASE, "x86_64", "other-source-version", False),
            ]:
                with self.subTest(config=config, release=release, arch=arch, version=version):
                    (root / ".config").write_text(config)
                    if passes:
                        modules.validate_headers(root, release, arch, version)
                    else:
                        with self.assertRaises(ValueError):
                            modules.validate_headers(root, release, arch, version)

    def test_unpinned_kernel_skips_audio_gates_without_building(self):
        with tempfile.TemporaryDirectory() as directory:
            env_file = Path(directory) / "env"
            missing = subprocess.CompletedProcess([], 1)
            version = subprocess.CompletedProcess([], 0, stdout=AZURE["version"])
            for release, skipped in [("7.0.0-1018-azure", True), (AZURE_RELEASE, False)]:
                env_file.write_text("")
                with self.subTest(release=release), \
                        patch.dict(os.environ, {"GITHUB_ENV": str(env_file)}), \
                        patch.object(modules, "host_guard"), \
                        patch.object(modules.platform, "release", return_value=release), \
                        patch.object(modules.subprocess, "run", return_value=missing), \
                        patch.object(modules, "run", return_value=version) as run, \
                        patch.object(modules, "validate_headers", side_effect=ValueError("reached")) as validate:
                    if skipped:
                        modules.main()
                        validate.assert_not_called()
                        self.assertEqual(run.call_count, 1)  # Only the header version query.
                    else:
                        with self.assertRaisesRegex(ValueError, "reached"):
                            modules.main()
                    self.assertEqual(env_file.read_text(), "WENDY_HOSTED_AUDIO=unavailable\n" if skipped else "")

    def test_existing_or_loaded_sound_stack_is_never_shadowed(self):
        for loaded, installed in [(True, False), (False, True), (False, False)]:
            with self.subTest(loaded=loaded, installed=installed), \
                    patch.object(modules.Path, "exists", return_value=loaded), \
                    patch.object(modules.subprocess, "run", return_value=subprocess.CompletedProcess([], 0 if installed else 1)) as run:
                if loaded or installed:
                    with self.assertRaises(ValueError):
                        modules.require_empty_stack(AZURE)
                else:
                    modules.require_empty_stack(AZURE)
                    self.assertEqual(run.call_count, 5)
                if loaded:
                    run.assert_not_called()

    def test_aws_keeps_packaged_soundcore_and_requires_absent_alsa(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "include/config").mkdir(parents=True)
            (root / "include/config/kernel.release").write_text(AWS_RELEASE)
            (root / "Module.symvers").write_text("kernel and soundcore CRCs")
            base = "CONFIG_SOUND=m\n# CONFIG_SND is not set\nCONFIG_MODULES=y\nCONFIG_MODVERSIONS=y\n"
            (root / ".config").write_text(base)
            self.assertIs(modules.validate_headers(root, AWS_RELEASE, "x86_64", AWS["version"]), AWS)
            (root / ".config").write_text(base.replace("# CONFIG_SND is not set", "CONFIG_SND=m"))
            with self.assertRaises(ValueError):
                modules.validate_headers(root, AWS_RELEASE, "x86_64", AWS["version"])
        command = modules.build_command(Path("/aws-headers"), Path("/owned/sound"), AWS)
        self.assertIn("CONFIG_SOUND=", command)
        self.assertNotIn("CONFIG_SOUND=m", command)
        self.assertNotIn("soundcore", modules.required_modules(AWS))
        flags = next(arg for arg in command if arg.startswith("KCFLAGS="))
        self.assertNotIn("-DCONFIG_SOUND_MODULE", flags)
        with patch.object(modules.Path, "exists", return_value=False), \
                patch.object(modules.subprocess, "run", return_value=subprocess.CompletedProcess([], 1)) as run:
            modules.require_empty_stack(AWS)
            self.assertEqual([call.args[0][-1] for call in run.call_args_list], list(modules.MODULES[1:]))
        with patch.object(modules.Path, "exists", return_value=True), patch.object(modules.subprocess, "run") as run:
            with self.assertRaisesRegex(ValueError, "loaded module soundcore"):
                modules.require_empty_stack(AWS)
            run.assert_not_called()
        patch_text = ("--- linux-aws-7.0-7.0.0.orig/sound/core/init.c\n"
                      "+++ linux-aws-7.0-7.0.0/sound/core/init.c\n@@ -1 +1 @@\n-old\n+new\n")
        self.assertEqual(modules.sound_patch(patch_text, AWS), patch_text)
        with self.assertRaises(ValueError):
            modules.sound_patch(patch_text, AZURE)
        self.assertEqual(len(modules.archives(AWS)), 2)
        self.assertTrue(all(name.startswith("linux-aws-7.0_") for name in modules.archives(AWS)))

    def test_aws_soundcore_requires_exact_distribution_owner_path_and_abi(self):
        expected = "/lib/modules/" + AWS_RELEASE + "/kernel/sound/soundcore.ko.zst"
        package = "linux-modules-" + AWS_RELEASE
        for path, owner, version, magic, good in [
            (expected, package + ": " + expected, AWS["version"], AWS_RELEASE, True),
            ("/tmp/soundcore.ko", package + ": " + expected, AWS["version"], AWS_RELEASE, False),
            (expected, "another-package: " + expected, AWS["version"], AWS_RELEASE, False),
            (expected, package + ": " + expected, "wrong-version", AWS_RELEASE, False),
            (expected, package + ": " + expected, AWS["version"], AZURE_RELEASE, False),
        ]:
            results = [subprocess.CompletedProcess([], 0, value) for value in [path, owner, version, magic + " SMP modversions"]]
            with self.subTest(path=path, owner=owner, version=version, magic=magic), \
                    patch.object(modules, "run", side_effect=results), patch.object(modules.Path, "is_file", return_value=True):
                if good:
                    modules.validate_packaged_soundcore(AWS_RELEASE, AWS["version"])
                else:
                    with self.assertRaises(ValueError):
                        modules.validate_packaged_soundcore(AWS_RELEASE, AWS["version"])

    def test_source_size_and_digest_both_required(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "source.tar.gz"
            path.write_bytes(b"exact source")
            digest = hashlib.sha256(path.read_bytes()).hexdigest()
            modules.verify_archive(path, (12, digest))
            for expected in [(13, digest), (12, "0" * 64)]:
                with self.assertRaises(ValueError):
                    modules.verify_archive(path, expected)

    def test_superseded_archive_recovers_exact_bytes_from_launchpad(self):
        payload = b"exact audited source"
        expected = (len(payload), hashlib.sha256(payload).hexdigest())
        with tempfile.TemporaryDirectory() as directory:
            destination = Path(directory) / "source.diff.gz"
            calls = []
            def download(args):
                calls.append(args)
                if len(calls) == 1:
                    destination.write_bytes(b"partial transport")
                    raise subprocess.CalledProcessError(22, args)
                self.assertFalse(destination.exists())
                destination.write_bytes(payload)
            with patch.object(modules, "run", side_effect=download):
                modules.download_archive(Path(directory), destination.name, expected, AWS)
            self.assertEqual(destination.read_bytes(), payload)
            self.assertEqual(len(calls), 2)
            self.assertEqual(calls[1][-1], "https://launchpad.net/ubuntu/+archive/primary/+sourcefiles/"
                             + AWS["source"] + "/" + AWS["version"] + "/source.diff.gz")
            for args in calls:
                self.assertIn("--max-time", args)
                self.assertEqual(args[args.index("--proto-redir") + 1], "=https")

    def test_wrong_hash_never_falls_back_and_existing_download_is_preserved(self):
        with tempfile.TemporaryDirectory() as directory:
            destination = Path(directory) / "source.diff.gz"
            def download(args):
                destination.write_bytes(b"wrong")
            with patch.object(modules, "run", side_effect=download) as run:
                with self.assertRaisesRegex(ValueError, "checksum/size mismatch"):
                    modules.download_archive(Path(directory), destination.name, (5, "0" * 64), AWS)
                self.assertEqual(run.call_count, 1)
                with self.assertRaisesRegex(ValueError, "overwrite"):
                    modules.download_archive(Path(directory), destination.name, (5, "0" * 64), AWS)
                self.assertEqual(run.call_count, 1)
            self.assertEqual(destination.read_bytes(), b"wrong")

    def test_all_source_locations_unavailable_fails_without_partial_file(self):
        with tempfile.TemporaryDirectory() as directory:
            destination = Path(directory) / "source.diff.gz"
            def download(args):
                destination.write_bytes(b"partial")
                raise subprocess.CalledProcessError(22, args)
            with patch.object(modules, "run", side_effect=download) as run:
                with self.assertRaises(subprocess.CalledProcessError):
                    modules.download_archive(Path(directory), destination.name, (1, "0" * 64), AWS)
                self.assertEqual(run.call_count, 2)
            self.assertFalse(destination.exists())

    def test_only_regular_sound_source_is_extracted(self):
        for bad in (None, "linux-7.0/sound/../../escape", "linux-7.0/sound/link"):
            with self.subTest(bad=bad), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                archive = root / "source.tar"
                with tarfile.open(archive, "w") as out:
                    for name in ("linux-7.0/sound/Makefile", "linux-7.0/kernel/unused"):
                        member = tarfile.TarInfo(name)
                        member.size = 1
                        out.addfile(member, io.BytesIO(b"x"))
                    if bad:
                        member = tarfile.TarInfo(bad)
                        if bad.endswith("link"):
                            member.type = tarfile.SYMTYPE
                            member.linkname = "/tmp/unrelated"
                        out.addfile(member)
                if bad:
                    with self.assertRaises(ValueError):
                        modules.extract_sound(archive, root / "unpacked")
                else:
                    sound = modules.extract_sound(archive, root / "unpacked")
                    self.assertEqual((sound / "Makefile").read_text(), "x")
                    self.assertFalse((sound.parent / "kernel").exists())

    def test_ubuntu_sound_patches_and_config_reach_build(self):
        def diff(path):
            return (f"--- linux-azure-7.0-7.0.0.orig/{path}\n"
                    f"+++ linux-azure-7.0-7.0.0/{path}\n@@ -1 +1 @@\n-old\n+new\n")
        selected = modules.sound_patch(diff("sound/core/init.c") + diff("kernel/exit.c") + diff("sound/drivers/aloop.c"), AZURE)
        self.assertIn("sound/core/init.c", selected)
        self.assertIn("sound/drivers/aloop.c", selected)
        self.assertNotIn("kernel/exit.c", selected)
        command = modules.build_command(Path("/exact-headers"), Path("/owned/sound"), AZURE)
        self.assertIn("M=/owned/sound", command)
        self.assertEqual(command[-1], "modules")
        flags = next(arg for arg in command if arg.startswith("KCFLAGS="))
        for key, value in modules.CONFIG.items():
            self.assertIn(f"CONFIG_{key}={value}", command)
            self.assertIn(f"-DCONFIG_{key}{'_MODULE' if value == 'm' else ''}=1", flags)
        self.assertNotIn("KBUILD_MODPOST_WARN=1", command)


if __name__ == "__main__":
    unittest.main()
