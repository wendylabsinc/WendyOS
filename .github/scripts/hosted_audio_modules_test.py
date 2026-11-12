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
            (root / "include/config/kernel.release").write_text(modules.RELEASE)
            (root / "Module.symvers").write_text("kernel symbol CRCs")
            base = "# CONFIG_SOUND is not set\nCONFIG_MODULES=y\nCONFIG_MODVERSIONS=y\n"
            for config, release, arch, version, passes in [
                (base, modules.RELEASE, "x86_64", modules.VERSION, True),
                (base.replace("# CONFIG_SOUND is not set", "CONFIG_SOUND=m"), modules.RELEASE, "x86_64", modules.VERSION, False),
                (base + "CONFIG_MODULE_SIG_FORCE=y\n", modules.RELEASE, "x86_64", modules.VERSION, False),
                (base.replace("CONFIG_MODVERSIONS=y\n", ""), modules.RELEASE, "x86_64", modules.VERSION, False),
                (base, "7.0.0-1018-azure", "x86_64", modules.VERSION, False),
                (base, modules.RELEASE, "aarch64", modules.VERSION, False),
                (base, modules.RELEASE, "x86_64", "other-source-version", False),
            ]:
                with self.subTest(config=config, release=release, arch=arch, version=version):
                    (root / ".config").write_text(config)
                    if passes:
                        modules.validate_headers(root, release, arch, version)
                    else:
                        with self.assertRaises(ValueError):
                            modules.validate_headers(root, release, arch, version)

    def test_existing_or_loaded_sound_stack_is_never_shadowed(self):
        for loaded, installed in [(True, False), (False, True), (False, False)]:
            with self.subTest(loaded=loaded, installed=installed), \
                    patch.object(modules.Path, "exists", return_value=loaded), \
                    patch.object(modules.subprocess, "run", return_value=subprocess.CompletedProcess([], 0 if installed else 1)) as run:
                if loaded or installed:
                    with self.assertRaises(ValueError):
                        modules.require_empty_stack()
                else:
                    modules.require_empty_stack()
                    self.assertEqual(run.call_count, 5)
                if loaded:
                    run.assert_not_called()

    def test_source_size_and_digest_both_required(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "source.tar.gz"
            path.write_bytes(b"exact source")
            digest = hashlib.sha256(path.read_bytes()).hexdigest()
            modules.verify_archive(path, (12, digest))
            for expected in [(13, digest), (12, "0" * 64)]:
                with self.assertRaises(ValueError):
                    modules.verify_archive(path, expected)

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
        selected = modules.sound_patch(diff("sound/core/init.c") + diff("kernel/exit.c") + diff("sound/drivers/aloop.c"))
        self.assertIn("sound/core/init.c", selected)
        self.assertIn("sound/drivers/aloop.c", selected)
        self.assertNotIn("kernel/exit.c", selected)
        command = modules.build_command(Path("/exact-headers"), Path("/owned/sound"))
        self.assertIn("M=/owned/sound", command)
        self.assertEqual(command[-1], "modules")
        flags = next(arg for arg in command if arg.startswith("KCFLAGS="))
        for key, value in modules.CONFIG.items():
            self.assertIn(f"CONFIG_{key}={value}", command)
            self.assertIn(f"-DCONFIG_{key}{'_MODULE' if value == 'm' else ''}=1", flags)
        self.assertNotIn("KBUILD_MODPOST_WARN=1", command)


if __name__ == "__main__":
    unittest.main()
