#!/usr/bin/env python3
"""Portable artifact tests. They do not claim an apt/dnf/systemd guest was run."""
import importlib.util
import json
import os
import platform
from pathlib import Path
import shutil
import struct
import subprocess
import tempfile
import sys
import types
import unittest
from unittest import mock

SCRIPT = Path(__file__).with_name("test-installed-linux-packages.py")
SPEC = importlib.util.spec_from_file_location("installed_packages", SCRIPT)
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)
ROOT = SCRIPT.parents[2]


def elf(machine=62):
    result = bytearray(64)
    result[:6] = b"\x7fELF\x02\x01"
    struct.pack_into("<H", result, 18, machine)
    return bytes(result)


@unittest.skipUnless(shutil.which("dpkg-deb"), "artifact tests require dpkg-deb, never install packages")
class DebianArtifactTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="wendy-package-artifacts-")
        self.addCleanup(self.temp.cleanup)
        self.directory = Path(self.temp.name)

    def package(self, name, *, arch="amd64", machine=62, restart=True):
        root = self.directory / (name + "-root")
        control = root / "DEBIAN"
        control.mkdir(parents=True)
        (control / "control").write_text(
            f"Package: {name}\nVersion: 2026.11.12-120000\nArchitecture: {arch}\n"
            "Maintainer: Wendy regression fixture <ci@wendy.dev>\nDescription: Artifact-only fixture\n")
        binary = root / "usr/bin" / name
        binary.parent.mkdir(parents=True)
        binary.write_bytes(elf(machine))
        binary.chmod(0o755)
        if name == "wendy-agent":
            for source, target in (("wendy-agent-postinstall.sh", "postinst"),
                                   ("wendy-agent-preremove.sh", "prerm")):
                text = (ROOT / "packaging/linux/fpm" / source).read_text()
                if not restart:
                    text = text.replace("systemctl restart wendy-agent", "systemctl try-restart wendy-agent")
                (control / target).write_text(text)
                (control / target).chmod(0o755)
            for path, data in (("etc/default/wendy-agent", b"# config\n"),
                               ("etc/wendy-agent/config.json", b"{}\n"),
                               ("usr/lib/systemd/system/wendy-agent.service", b"[Service]\nExecStart=/usr/bin/wendy-agent\n"),
                               ("usr/lib/wendy-agent/setup-mdns.sh", b"#!/bin/sh\nexit 0\n")):
                file = root / path
                file.parent.mkdir(parents=True, exist_ok=True)
                file.write_bytes(data)
            (control / "conffiles").write_text("/etc/default/wendy-agent\n/etc/wendy-agent/config.json\n")
        dest = self.directory / (name + ".deb")
        subprocess.run(["dpkg-deb", "-Zgzip", "--root-owner-group", "-b", str(root), str(dest)], check=True, capture_output=True)
        return dest

    def prepare(self, **kwargs):
        agent = self.package("wendy-agent", **kwargs)
        cli = self.package("wendy")
        bundle = self.directory / "bundle"
        args = types.SimpleNamespace(format="deb", agent=str(agent), cli=str(cli), output=str(bundle),
                                     expected_version="2026.11.12-120000")
        with mock.patch("builtins.print"):
            MODULE.prepare(args)
        return bundle, agent, cli

    def test_real_deb_repack_preserves_release_bytes_and_exposes_both_hooks(self):
        bundle, agent, cli = self.prepare()
        manifest = MODULE.load_bundle(bundle)
        self.assertEqual((bundle / "candidate.deb").read_bytes(), agent.read_bytes())
        self.assertEqual((bundle / "cli.deb").read_bytes(), cli.read_bytes())
        self.assertEqual(manifest["packages"]["legacy"]["version"], "0.0.0")
        self.assertEqual(manifest["packages"]["candidate"]["version"], "2026.11.12-120000")
        with tempfile.TemporaryDirectory(dir=self.directory) as extracted:
            legacy = Path(extracted) / "legacy"
            control = Path(extracted) / "control"
            subprocess.run(["dpkg-deb", "-R", str(bundle / "legacy.deb"), str(legacy)], check=True, capture_output=True)
            subprocess.run(["dpkg-deb", "-R", str(bundle / "negative-control.deb"), str(control)], check=True, capture_output=True)
            for hook, old in (("postinst", "postinstall.sh"), ("prerm", "preremove.sh")):
                historical = SCRIPT.parent / "fixtures/package-upgrade-2352" / old
                self.assertEqual((legacy / "DEBIAN" / hook).read_bytes(), historical.read_bytes())
                self.assertTrue((legacy / "DEBIAN" / hook).stat().st_mode & 0o111)
            original = self.directory / "wendy-agent-root/DEBIAN/postinst"
            self.assertEqual((control / "DEBIAN/postinst").read_text(), original.read_text().replace(
                "systemctl restart wendy-agent", "systemctl try-restart wendy-agent"))
            self.assertEqual((control / "DEBIAN/prerm").read_bytes(),
                             (self.directory / "wendy-agent-root/DEBIAN/prerm").read_bytes())
        for role in ("legacy", "negative-control"):
            self.assertEqual(manifest["packages"][role]["binary_sha256"], manifest["packages"]["candidate"]["binary_sha256"])
        self.assertTrue(manifest["artifact_registry_acceptance"].startswith("pending"))

    def test_checksum_and_path_tampering_are_rejected_before_install(self):
        bundle, _, _ = self.prepare()
        manifest_path = bundle / "manifest.json"
        manifest = json.loads(manifest_path.read_text())
        manifest["packages"]["candidate"]["file"] = "../wendy-agent.deb"
        manifest_path.write_text(json.dumps(manifest))
        with self.assertRaisesRegex(RuntimeError, "escapes bundle"):
            MODULE.load_bundle(bundle)
        manifest["packages"]["candidate"]["file"] = "candidate.deb"
        manifest_path.write_text(json.dumps(manifest))
        (bundle / "candidate.deb").write_bytes(b"corrupt")
        with self.assertRaisesRegex(RuntimeError, "checksum"):
            MODULE.load_bundle(bundle)

    def test_control_cannot_silently_accept_already_broken_candidate(self):
        with self.assertRaisesRegex(RuntimeError, "expected restart fix"):
            self.prepare(restart=False)

    def test_package_architecture_must_match_actual_elf(self):
        with self.assertRaisesRegex(RuntimeError, "metadata disagrees"):
            self.prepare(arch="arm64")

    def test_bundle_output_is_never_overwritten(self):
        bundle, agent, cli = self.prepare()
        args = types.SimpleNamespace(format="deb", agent=str(agent), cli=str(cli), output=str(bundle), expected_version="v")
        before = (bundle / "manifest.json").read_bytes()
        with self.assertRaisesRegex(RuntimeError, "new directory"):
            MODULE.prepare(args)
        self.assertEqual((bundle / "manifest.json").read_bytes(), before)


class GuardTests(unittest.TestCase):
    def test_health_probe_supplies_guest_account_home_to_real_subprocess(self):
        with mock.patch.dict(os.environ, {}, clear=True):
            command = (sys.executable, "-c", "import json,os; print(json.dumps([os.environ['HOME'], os.environ['WENDY_AGENT_SOCKET']]))")
            # Reproduce the transient-service environment that broke the real
            # installed CLI before it could reach the Agent in hosted CI.
            missing_home = MODULE.run(*command, env={"WENDY_AGENT_SOCKET": MODULE.SOCKET}, check=False)
            self.assertNotEqual(missing_home.returncode, 0)
            self.assertIn(b"HOME", missing_home.stderr)
            result = MODULE.run(*command, env=MODULE.health_probe_environment())
            self.assertEqual(json.loads(result.stdout), [MODULE.pwd.getpwuid(os.getuid()).pw_dir, MODULE.SOCKET])
            self.assertNotIn("HOME", os.environ)

    def test_host_is_rejected_before_package_manager_calls(self):
        with mock.patch.object(MODULE.sys, "platform", "darwin"), mock.patch.object(MODULE, "run") as run:
            with self.assertRaisesRegex(RuntimeError, "Linux root"):
                MODULE.guest_guard({}, "")
            run.assert_not_called()

    def test_non_nspawn_linux_host_and_nonce_mismatch_are_rejected(self):
        with mock.patch.object(MODULE.sys, "platform", "linux"), mock.patch.object(MODULE.os, "geteuid", return_value=0):
            with mock.patch.object(Path, "read_text", side_effect=["systemd\n", "docker\n"]):
                with self.assertRaisesRegex(RuntimeError, "non-nspawn"):
                    MODULE.guest_guard({}, "")
            with mock.patch.object(Path, "read_text", side_effect=["systemd\n", "systemd-nspawn\n"]):
                with self.assertRaisesRegex(RuntimeError, "nonce"):
                    MODULE.guest_guard({"nonce": "expected"}, "wrong")

    def test_elf_rejects_script_payload_and_wrong_machine(self):
        self.assertEqual(MODULE.elf_arch(elf(183)), "arm64")
        for payload in (b"#!/bin/sh\nexit 0\n", elf(3)):
            with self.assertRaises(RuntimeError):
                MODULE.elf_arch(payload)


@unittest.skipUnless(all(shutil.which(command) for command in ("fpm", "rpm", "rpm2cpio", "bsdtar", "cpio", "dpkg-deb")),
                     "RPM artifact roundtrip requires FPM/rpm/bsdtar; hosted package job supplies these")
class RpmArtifactTests(unittest.TestCase):
    setUp = DebianArtifactTests.setUp
    package = DebianArtifactTests.package

    @unittest.skipUnless(sys.platform == "linux", "shared release recipe requires Linux packaging tools")
    def test_shared_release_recipe_packages_exact_input_and_service_files(self):
        architecture = {"x86_64": "amd64", "aarch64": "arm64"}[platform.machine()]
        destination = self.directory / "release-packages"
        # A real ELF supplies artifact bytes only. No Wendy or service is run.
        MODULE.run("bash", str(SCRIPT.with_name("build-linux-packages.sh")), "2099.01.04-regression-package",
                   architecture, "/bin/true", "/bin/false", str(destination), timeout=180)
        self.assertEqual(len(list(destination.iterdir())), 4)
        for fmt in ("deb", "rpm"):
            for archive in destination.glob("*." + fmt):
                name = MODULE.metadata(archive, fmt)["name"]
                source = Path("/bin/false" if name == "wendy-agent" else "/bin/true")
                self.assertEqual(MODULE.member_bytes(archive, fmt, "/usr/bin/" + name), source.read_bytes())
                if name == "wendy-agent":
                    self.assertEqual(MODULE.member_bytes(archive, fmt, "/usr/lib/systemd/system/wendy-agent.service"),
                                     (ROOT / "packaging/linux/systemd/wendy-agent.service").read_bytes())
                else:
                    for packaged, original in (
                        ("/usr/lib/sysusers.d/wendy.conf", "packaging/linux/sysusers/wendy.conf"),
                        ("/usr/lib/udev/rules.d/70-wendy-jetson.rules", "packaging/linux/udev/70-wendy-jetson.rules"),
                        ("/usr/lib/udev/rules.d/70-wendy-qualcomm.rules", "packaging/linux/udev/70-wendy-qualcomm.rules"),
                    ):
                        self.assertEqual(MODULE.member_bytes(archive, fmt, packaged), (ROOT / original).read_bytes())

    def test_real_rpm_repack_preserves_release_payload_and_changes_recovery_hook(self):
        packages = {}
        for name in ("wendy-agent", "wendy"):
            deb = self.package(name)
            rpm = self.directory / (name + ".rpm")
            command = ["fpm", "-s", "deb", "-t", "rpm", "-v", "2026.11.12_120000",
                       "--rpm-rpmbuild-define", "_rpmformat 4", "-p", str(rpm)]
            if name == "wendy-agent":
                command += ["--rpm-posttrans", str(ROOT / "packaging/linux/fpm/wendy-agent-posttrans.sh")]
            MODULE.run(*command, str(deb), timeout=120)
            packages[name] = rpm
        bundle = self.directory / "rpm-bundle"
        args = types.SimpleNamespace(format="rpm", agent=str(packages["wendy-agent"]), cli=str(packages["wendy"]),
                                     output=str(bundle), expected_version="2026.11.12-120000")
        with mock.patch("builtins.print"):
            MODULE.prepare(args)
        manifest = MODULE.load_bundle(bundle)
        self.assertEqual((bundle / "candidate.rpm").read_bytes(), packages["wendy-agent"].read_bytes())
        self.assertEqual(manifest["packages"]["legacy"]["version"], "0.0.0-1")
        self.assertIn("systemctl start wendy-agent", MODULE.output("rpm", "-qp", "--qf", "%{POSTTRANS}", str(bundle / "candidate.rpm")))
        for role in ("legacy", "negative-control"):
            self.assertNotIn("systemctl", MODULE.output("rpm", "-qp", "--qf", "%{POSTTRANS}", str(bundle / (role + ".rpm"))))
            self.assertEqual(manifest["packages"][role]["binary_sha256"], manifest["packages"]["candidate"]["binary_sha256"])


if __name__ == "__main__":
    unittest.main()
