#!/usr/bin/env python3
"""Artifact and bootstrap controls; full build runs in the existing Linux job."""
import copy
import gzip
import importlib.util
import json
from pathlib import Path
import shutil
import struct
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

import yaml

SCRIPT = Path(__file__).with_name("test-runtime-guest-bootstrap.py")
SPEC = importlib.util.spec_from_file_location("runtime_bootstrap", SCRIPT)
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)


class BootstrapContractTests(unittest.TestCase):
    def test_actual_go_setup_references_existing_root_module_and_original_paths_fail(self):
        workflow = yaml.safe_load((MODULE.ROOT / ".github/workflows/build.yml").read_text())
        self.assertEqual(MODULE.setup_files(workflow), ["go.mod", "go.sum"])
        for key, original in (("go-version-file", "go/go.mod"), ("cache-dependency-path", "go/go.sum")):
            bad = copy.deepcopy(workflow)
            step = next(s for s in bad["jobs"]["build-macos-runtime-guest"]["steps"]
                        if s.get("uses", "").startswith("actions/setup-go@"))
            step["with"][key] = original
            with self.subTest(key=key), self.assertRaisesRegex(RuntimeError, "setup file does not exist"):
                MODULE.setup_files(bad)

    def test_isolated_context_contains_real_module_sources_but_no_build_outputs(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            files = MODULE.isolated_sources(root)
            self.assertEqual((root / "go.mod").read_bytes(), (MODULE.ROOT / "go.mod").read_bytes())
            self.assertEqual((root / "go.sum").read_bytes(), (MODULE.ROOT / "go.sum").read_bytes())
            self.assertEqual((root / MODULE.GUEST / "build.sh").read_bytes(), (MODULE.ROOT / MODULE.GUEST / "build.sh").read_bytes())
            self.assertFalse((root / MODULE.GUEST / "wendy-runtime-guest-proxy").exists())
            self.assertFalse((root / "swift/WendyAgentMac/Resources/runtime").exists())
            self.assertFalse((root / "go/go.mod").exists())
            self.assertTrue(any(path.startswith(str(MODULE.PROXY)) for path in files))

    def test_real_docker_wrapper_selects_existing_driver_and_rejects_builder_mutations(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            tools = root / "tools"
            tools.mkdir()
            receipt = root / "receipt.json"
            docker = root / "real-docker"
            docker.write_text(f"#!{sys.executable}\nimport json,sys\nopen({str(receipt)!r},'w').write(json.dumps(sys.argv[1:]))\n")
            docker.chmod(0o755)
            MODULE.wrappers(tools, str(docker), "existing-docker", "arm64", root / "calls.jsonl")
            subprocess.run([str(tools / "docker"), "buildx", "build", "--platform", "linux/arm64", "."], check=True)
            self.assertEqual(json.loads(receipt.read_text()),
                             ["buildx", "build", "--builder", "existing-docker", "--no-cache", "--platform", "linux/arm64", "."])
            receipt.unlink()
            for args in (["buildx", "create"], ["buildx", "use", "other"], ["system", "prune"],
                         ["buildx", "build", "--platform", "linux/amd64", "."],
                         ["buildx", "build", "--platform", "linux/arm64", "--builder", "other", "."]):
                with self.subTest(args=args):
                    result = subprocess.run([str(tools / "docker"), *args], capture_output=True)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertFalse(receipt.exists())

    def test_kernel_headers_and_release_must_match_target_architecture(self):
        arm = bytearray(128)
        arm[56:60] = b"ARMd"
        arm.extend(b"Linux version 6.12.99-0-virt (fixture)\x00")
        self.assertEqual(MODULE.kernel_release(arm, "arm64"), "6.12.99-0-virt")
        with self.assertRaisesRegex(RuntimeError, "x86 bzImage"):
            MODULE.kernel_release(arm, "amd64")
        x86 = bytearray(2048)
        x86[0x1fe:0x200], x86[0x202:0x206] = b"\x55\xaa", b"HdrS"
        struct.pack_into("<H", x86, 0x236, 1)
        struct.pack_into("<H", x86, 0x20e, 0x200)
        x86[0x400:0x400 + 20] = b"6.12.99-0-virt test\x00\x00"
        self.assertEqual(MODULE.kernel_release(x86, "amd64"), "6.12.99-0-virt")
        struct.pack_into("<H", x86, 0x236, 0)
        with self.assertRaisesRegex(RuntimeError, "not 64-bit"):
            MODULE.kernel_release(x86, "amd64")

    @unittest.skipUnless(shutil.which("bsdtar"), "requires libarchive bsdtar")
    def test_real_newc_archive_rejects_duplicated_kernel_and_wrong_modules(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            tree, artifacts, extracted = [root / name for name in ("tree", "artifacts", "extracted")]
            for path in (tree, artifacts, extracted):
                path.mkdir()
            (tree / "boot").mkdir()
            (tree / "boot/vmlinuz").write_bytes(b"duplicate")
            archive = artifacts / "initramfs-arm64.img"
            subprocess.run(["bsdtar", "--format=newc", "-cf", str(root / "root.cpio"), "-C", str(tree), "."], check=True)
            archive.write_bytes(gzip.compress((root / "root.cpio").read_bytes()))
            with self.assertRaisesRegex(RuntimeError, "duplicates boot"):
                MODULE.archive_names(archive, "bsdtar")
            shutil.rmtree(tree / "boot")
            (tree / "lib/modules/wrong").mkdir(parents=True)
            (tree / "lib/modules/wrong/modules.dep").write_text("wrong.ko:\n")
            subprocess.run(["bsdtar", "--format=newc", "-cf", str(root / "root.cpio"), "-C", str(tree), "."], check=True)
            archive.write_bytes(gzip.compress((root / "root.cpio").read_bytes()))
            image = bytearray(128)
            image[56:60] = b"ARMd"
            image.extend(b"Linux version 6.12.99-0-virt (fixture)\x00")
            (artifacts / "vmlinuz-arm64").write_bytes(image)
            with self.assertRaisesRegex(RuntimeError, "modules do not match"):
                MODULE.inspect_artifacts(artifacts, extracted, "arm64", "bsdtar", root)
            archive.write_bytes(gzip.compress(b"not-newc"))
            with self.assertRaisesRegex(RuntimeError, "newc CPIO"):
                MODULE.archive_names(archive, "bsdtar")

    def test_cleanup_removes_only_its_read_only_module_cache(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            sibling = root / "unrelated"
            sibling.write_text("retain")
            owned = root / "work"
            readonly = owned / "modules/pkg"
            readonly.mkdir(parents=True)
            (readonly / "source.go").write_text("package fixture")
            readonly.chmod(0o555)
            MODULE.remove_work(owned)
            self.assertFalse(owned.exists())
            self.assertEqual(sibling.read_text(), "retain")

    def test_cleanup_collects_errors_and_still_checks_other_owned_containers(self):
        with tempfile.TemporaryDirectory() as temporary:
            work = Path(temporary) / "work"
            work.mkdir()
            responses = [subprocess.TimeoutExpired("docker inspect", 15),
                         subprocess.CompletedProcess([], 1, stdout="", stderr="error: no such object: second")]
            with mock.patch.object(MODULE.subprocess, "run", side_effect=responses) as run:
                errors = MODULE.cleanup_resources("docker", ["first", "second"], "nonce", work)
                self.assertEqual(run.call_count, 2)
            self.assertEqual(len(errors), 1)
            self.assertIn("first:", errors[0])
            self.assertTrue(work.exists())
            with mock.patch.object(MODULE, "remove_work", side_effect=PermissionError("owned cache")):
                errors = MODULE.cleanup_resources("docker", [], "nonce", work)
            self.assertEqual(len(errors), 1)
            self.assertIn("owned cache", errors[0])


if __name__ == "__main__":
    unittest.main()
