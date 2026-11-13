"""Portable admission/cleanup tests only. Real kernel proof requires the VM gate."""
import importlib.util
import configparser
import io
import json
import os
import pty
import select
import shutil
import subprocess
import tty
from pathlib import Path
import tarfile
import tempfile
import sys
import time
import unittest
from unittest import mock

PATH = Path(__file__).with_name("test-driver-lifecycle-vm.py")
SPEC = importlib.util.spec_from_file_location("driver_vm", PATH)
VM = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(VM)


class DriverVMTests(unittest.TestCase):
    def test_exported_markers_are_removed_only_from_the_owned_tree(self):
        with tempfile.TemporaryDirectory() as tmp:
            work = Path(tmp)
            root = work / "rootfs"
            (root / "fixture").mkdir(parents=True)
            (root / "fixture/nonce").write_text("a" * 32)
            for name in VM.CONTAINER_IDENTITY_MARKERS:
                path = root / name
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text("docker\n")
            retained = root / "fixture/driver.data"
            retained.write_bytes(b"actual payload stays")
            runner = VM.Runner(work, work, "a" * 32)
            runner.results["export"] = {"container_identity_markers": list(VM.CONTAINER_IDENTITY_MARKERS)}
            def remove(argv, **options):
                self.assertEqual(argv[:4], ["sudo", "-n", "rm", "--"])
                self.assertEqual(argv[4:], [str(root / name) for name in VM.CONTAINER_IDENTITY_MARKERS])
                subprocess.run(argv[2:], check=True, timeout=3)
            with mock.patch.object(runner, "command", side_effect=remove):
                runner.remove_export_container_identity(root)
            self.assertTrue(all(not (root / name).exists() for name in VM.CONTAINER_IDENTITY_MARKERS))
            self.assertEqual(retained.read_bytes(), b"actual payload stays")
            self.assertEqual(len(runner.results["removed_export_container_identity"]), 4)
            # A later accidental marker or foreign root cannot silently pass.
            (root / ".dockerenv").write_text("docker")
            runner.results["export"]["container_identity_markers"] = []
            with self.assertRaisesRegex(ValueError, "identity remains"):
                runner.remove_export_container_identity(root)
            (root / "fixture/nonce").write_text("b" * 32)
            with self.assertRaisesRegex(ValueError, "nonce mismatch"):
                runner.remove_export_container_identity(root)

    def test_export_rejects_linked_container_markers(self):
        for kind, target in ((tarfile.SYMTYPE, "fixture/nonce"), (tarfile.LNKTYPE, "fixture/nonce")):
            with self.subTest(kind=kind), tempfile.TemporaryDirectory() as tmp:
                archive = self.archive(Path(tmp), [("fixture", tarfile.DIRTYPE, ""),
                    ("fixture/nonce", tarfile.REGTYPE, ""), (".dockerenv", kind, target)])
                with self.assertRaisesRegex(ValueError, "regular archive member"):
                    VM.validate_export(archive)
        with tempfile.TemporaryDirectory() as tmp:
            archive = self.archive(Path(tmp), [(".dockerenv", tarfile.REGTYPE, "")])
            self.assertEqual(VM.validate_export(archive)["container_identity_markers"], [".dockerenv"])

    def test_native_boot_refuses_container_private_mount_and_namespace_mismatch(self):
        script = r'''
timeout() { shift 2; "$@"; }
systemd-detect-virt() {
  if [[ $1 == --container ]]; then printf '%s\n' "$CONTAINER"; return "$CONTAINER_STATUS"; fi
  printf '%s\n' "$VM_KIND"
}
findmnt() { printf '%s\n' "$PROPAGATION"; }
readlink() {
  if [[ $1 == /proc/1/ns/mnt ]]; then printf '%s\n' "$INIT_NAMESPACE"; else printf 'mnt:[1]\n'; fi
}
source "$1"
driver_vm_require_native_boot
'''
        baseline = dict(os.environ, CONTAINER="none", CONTAINER_STATUS="1", VM_KIND="qemu", PROPAGATION="shared", INIT_NAMESPACE="mnt:[1]")
        for change in ({}, {"CONTAINER": "docker", "CONTAINER_STATUS": "0"},
                       {"CONTAINER_STATUS": "124"}, {"VM_KIND": "none"},
                       {"PROPAGATION": "private"}, {"INIT_NAMESPACE": "mnt:[2]"}):
            with self.subTest(change=change):
                result = subprocess.run(["bash", "-c", script, "boot-admission", str(VM.FIXTURE / "boot.sh")],
                                        env={**baseline, **change}, capture_output=True, text=True, timeout=3)
                self.assertEqual(result.returncode == 0, not change, result.stderr)
                self.assertEqual("DRIVER_VM_NATIVE_BOOT_READY " in result.stdout, not change)

    def test_failed_guest_keeps_original_exit_even_when_diagnostics_time_out(self):
        tail = (VM.FIXTURE / "run-guest.sh").read_text().split('if "$binary" -test.run ', 1)[1]
        tail = 'if "$binary" -test.run ' + tail
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            binary = root / "binary"
            timer = root / "timeout"
            marker = root / "diagnosed"
            timer.write_text('#!/bin/bash\nprintf called > "$DIAGNOSTIC_MARKER"\nexit 124\n')
            timer.chmod(0o755)
            env = dict(os.environ, PATH=str(root) + os.pathsep + os.environ["PATH"],
                       DIAGNOSTIC_MARKER=str(marker))
            for code in (7, 0):
                marker.unlink(missing_ok=True)
                binary.write_text(f"#!/bin/bash\nexit {code}\n")
                binary.chmod(0o755)
                result = subprocess.run(["bash", "-c", 'set -euo pipefail; binary=$1; ' + tail,
                                         "guest-exit-test", str(binary)], env=env,
                                        capture_output=True, timeout=5)
                self.assertEqual(result.returncode, code, result.stderr)
                self.assertEqual(marker.exists(), code != 0)

    def test_payload_probes_are_read_only_and_reject_foreign_guest(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            calls = root / "calls.jsonl"
            stub = root / "command"
            stub.write_text(f'''#!{sys.executable}
import json, os, pathlib, subprocess, sys
name = pathlib.Path(sys.argv[0]).name
args = sys.argv[1:]
with open(os.environ["PROBE_CALLS"], "a") as out:
    out.write(json.dumps([name, *args]) + "\\n")
if name == "cat":
    if args == ["/fixture/nonce"]: print("a" * 32)
    elif args == ["/sys/class/dmi/id/product_name"]: print("WendyDriverFixture-" + "a" * 32)
elif name == "uname": print("fixture-kernel")
elif name == "timeout": sys.exit(subprocess.call(args[2:]))
''')
            stub.chmod(0o755)
            for name in ("cat", "uname", "timeout", "head", "ls", "modinfo", "modprobe", "systemd-sysext"):
                (root / name).symlink_to(stub)
            env = dict(os.environ, PATH=str(root), PROBE_CALLS=str(calls), WENDY_DRIVER_VM="a" * 32)
            bash = shutil.which("bash")
            result = subprocess.run([bash, str(VM.FIXTURE / "diagnostics.sh")], env=env,
                                    capture_output=True, timeout=10)
            self.assertEqual(result.returncode, 0, result.stderr)
            invocations = [json.loads(line) for line in calls.read_text().splitlines()]
            self.assertEqual([call for call in invocations if call[0] == "modprobe"],
                             [["modprobe", "--show-depends", "--", name] for name in
                              ("wendy_fixture_healthy", "wendy_fixture_target", "wendy_fixture_broken")])
            self.assertIn(["systemd-sysext", "--json=short", "status"], invocations)
            self.assertIn(["modinfo", "-F", "vermagic", "/usr/lib/wendyos-driver-payloads/wendy-fixture-healthy/modules/fixture-kernel/wendy_fixture_healthy.ko"], invocations)
            calls.unlink()
            env["WENDY_DRIVER_VM"] = "b" * 32
            result = subprocess.run([bash, str(VM.FIXTURE / "diagnostics.sh")], env=env,
                                    capture_output=True, timeout=5)
            self.assertEqual(result.returncode, 2)
            self.assertEqual([json.loads(line) for line in calls.read_text().splitlines()],
                             [["cat", "/fixture/nonce"]])

    def archive(self, root, entries):
        path = root / "image.tar"
        with tarfile.open(path, "w") as archive:
            for name, kind, link in entries:
                item = tarfile.TarInfo(name)
                item.type = kind
                item.linkname = link
                if kind == tarfile.REGTYPE:
                    item.size = 4
                    archive.addfile(item, io.BytesIO(b"test"))
                else:
                    archive.addfile(item)
        return path

    def test_export_preserves_inert_symlinks_and_regular_files(self):
        with tempfile.TemporaryDirectory() as tmp:
            path=self.archive(Path(tmp),[("usr",tarfile.DIRTYPE,""),("usr/bin",tarfile.DIRTYPE,""),
                ("usr/bin/tool",tarfile.REGTYPE,""),("bin",tarfile.SYMTYPE,"usr/bin")])
            result=VM.validate_export(path)
            self.assertEqual(result,{"members":4,"expanded_bytes":4,"container_identity_markers":[]})

    def test_rejects_paths_that_could_escape_privileged_extraction(self):
        cases=[[("../outside",tarfile.REGTYPE,"")],[("/outside",tarfile.REGTYPE,"")],
            [("escape",tarfile.SYMTYPE,"/etc"),("escape/shadow",tarfile.REGTYPE,"")],
            [("hard",tarfile.LNKTYPE,"../../outside")],[("node",tarfile.CHRTYPE,"")],
            [("same",tarfile.REGTYPE,""),("same",tarfile.REGTYPE,"")]]
        for entries in cases:
            with self.subTest(entries=entries),tempfile.TemporaryDirectory() as tmp:
                with self.assertRaises(ValueError):VM.validate_export(self.archive(Path(tmp),entries))

    def test_requires_real_lifecycle_and_specific_control_boundary(self):
        nonce="a"*32
        normal=f"DRIVER_VM_EVIDENCE_CHANNEL_READY\nDRIVER_VM_NATIVE_BOOT_READY virtualization=qemu usr-propagation=shared\n--- PASS: TestDriverLifecycleVM\nDRIVER_VM_LIFECYCLE_VERIFIED {nonce}\nDRIVER_VM_EXIT nonce={nonce} control=normal phase=first status=0\n"
        VM.accept_log(normal,nonce,"normal","first",0)
        for bad in (normal.replace("DRIVER_VM_NATIVE_BOOT_READY", "missing"), normal.replace("DRIVER_VM_EVIDENCE_CHANNEL_READY", "missing"), normal.replace("LIFECYCLE_VERIFIED","missing"),normal+"--- SKIP: prereq\n",normal.replace(nonce,"b"*32)):
            with self.assertRaises(ValueError):VM.accept_log(bad,nonce,"normal","first",0)
        control=f"DRIVER_VM_EVIDENCE_CHANNEL_READY\nDRIVER_VM_NATIVE_BOOT_READY virtualization=qemu usr-propagation=shared\n--- FAIL: TestDriverLifecycleVM\nlive driver version mismatch: wendy_fixture_target\nDRIVER_VM_EXIT nonce={nonce} control=omit-unload phase=first status=1\n"
        VM.accept_log(control,nonce,"omit-unload","first",0)
        with self.assertRaises(ValueError):VM.accept_log(control.replace("live driver version mismatch: wendy_fixture_target","compile failed"),nonce,"omit-unload","first",0)
        with self.assertRaises(ValueError):VM.accept_log(control,nonce,"omit-unload","first",124)

    def test_generated_guest_masks_both_console_providers(self):
        class StopBeforeDocker(Exception):
            pass
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            runner = VM.Runner(root, root, "a" * 32)
            def command(argv, **options):
                if argv[:2] == ["git", "rev-parse"]:
                    return "controlled-source\n"
                if argv[0] == "env":
                    Path(argv[argv.index("-o") + 1]).write_bytes(b"not-an-executable")
                    return ""
                if argv[:2] == ["docker", "build"]:
                    context = Path(argv[-1])
                    unit = configparser.ConfigParser(interpolation=None)
                    unit.read(context / "driver-fixture.service")
                    self.assertEqual(unit["Service"]["StandardInput"], "null")
                    self.assertEqual(unit["Service"]["ExecStart"], "/bin/bash /fixture/run-guest.sh")
                    self.assertEqual(unit["Service"]["TimeoutStartSec"], "480")
                    recipe = (context / "Dockerfile").read_text()
                    self.assertIn("systemctl mask serial-getty@ttyS0.service console-getty.service &&", recipe)
                    self.assertIn("virtio_console", (context / "build-guest.sh").read_text())
                    self.assertTrue((context / "evidence.sh").is_file())
                    raise StopBeforeDocker
                self.fail("unexpected external operation: " + repr(argv))
            with mock.patch.object(VM, "SOURCES", {}), mock.patch.object(runner, "command", side_effect=command):
                with self.assertRaises(StopBeforeDocker):
                    runner.prepare()

    def test_evidence_uses_live_character_channel_and_refuses_regular_file(self):
        helper = VM.FIXTURE / "evidence.sh"
        master, slave = pty.openpty()
        tty.setraw(slave)
        try:
            result = subprocess.run(["bash", "-c",
                'source "$1"; driver_vm_open_evidence "$2" 1 || exit; printf "owned-independent-evidence\\n" >&3',
                "test", str(helper), os.ttyname(slave)], capture_output=True, timeout=3)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertTrue(select.select([master], [], [], 2)[0], "independent evidence channel produced no bytes")
            self.assertEqual(os.read(master, 128), b"owned-independent-evidence\n")
            self.assertEqual(result.stdout, b"")
        finally:
            os.close(master)
            os.close(slave)
        with tempfile.TemporaryDirectory() as temporary:
            regular = Path(temporary) / "not-a-port"
            regular.write_text("preserved")
            result = subprocess.run(["bash", "-c",
                'source "$1"; driver_vm_open_evidence "$2" 0',
                "test", str(helper), str(regular)], capture_output=True, timeout=3)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn(b"DRIVER_VM_EVIDENCE_UNAVAILABLE", result.stderr)
            self.assertEqual(regular.read_text(), "preserved")

    def test_boot_separates_console_from_nonce_checked_evidence(self):
        nonce = "a" * 32
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            runner = VM.Runner(root, root, nonce)
            def qemu(command, timeout, **options):
                self.assertIn("virtserialport,chardev=driver-evidence,name=wendy.driver.evidence", command)
                channel = command[command.index("-chardev") + 1]
                self.assertEqual(channel, "file,id=driver-evidence,path=" + str(root / "normal-first.log"))
                options["stdout"].write("independent boot console\n")
                (root / "normal-first.log").write_text(
                    f"DRIVER_VM_EVIDENCE_CHANNEL_READY\nDRIVER_VM_NATIVE_BOOT_READY virtualization=qemu usr-propagation=shared\n--- PASS: TestDriverLifecycleVM\n"
                    f"DRIVER_VM_LIFECYCLE_VERIFIED {nonce}\n"
                    f"DRIVER_VM_EXIT nonce={nonce} control=normal phase=first status=0\n")
                return 0
            with mock.patch.object(VM.GUARDIAN, "run", side_effect=qemu):
                runner.boot(root / "normal.qcow2", "normal", "first")
            self.assertTrue(runner.results["normal/first"]["passed"])
            self.assertEqual((root / "normal-first-console.log").read_text(), "independent boot console\n")
            with self.assertRaisesRegex(ValueError, "already exist"):
                runner.boot(root / "normal.qcow2", "normal", "first")

    def test_shared_deadline_bounds_a_real_command_and_prevents_next_launch(self):
        with tempfile.TemporaryDirectory() as tmp:
            runner = VM.Runner(Path(tmp), Path(tmp), "a" * 32)
            runner.deadline = time.monotonic() + 1
            # The real command exits immediately. The guardian's mandatory group
            # drain also consumes the shared budget and cannot reset it.
            with mock.patch.object(VM.GUARDIAN, "run", wraps=VM.GUARDIAN.run) as run:
                code = runner.command([sys.executable, "-c", "print('owned command completed')"], timeout=120)
                self.assertEqual(code, 0)
                self.assertGreater(run.call_args.args[1], 0)
                self.assertLessEqual(run.call_args.args[1], 1)
            self.assertIn("owned command completed", (Path(tmp) / "command-01.log").read_text())
            with mock.patch.object(VM.GUARDIAN, "run") as run:
                with self.assertRaisesRegex(ValueError, "shared wall deadline"):
                    runner.command([sys.executable, "-c", "raise SystemExit(0)"])
                run.assert_not_called()
            # Expiration must not disable the separately bounded cleanup.
            with mock.patch.object(VM.shutil, "rmtree") as remove:
                self.assertEqual(runner.cleanup(), [])
                remove.assert_called_once_with(Path(tmp))
                self.assertGreater(runner.remaining_timeout(120), 0)

    def test_never_deletes_a_mounted_root(self):
        with tempfile.TemporaryDirectory() as tmp:
            work=Path(tmp)/"work";work.mkdir();root=work/"rootfs";root.mkdir()
            runner=VM.Runner(work,Path(tmp),"a"*32)
            with mock.patch.object(runner,"command",return_value=json.dumps({"filesystems":[{"target":str(root/"usr")}] })) as command:
                with self.assertRaises(ValueError):runner.remove_rootfs(root)
                self.assertEqual(command.call_count,1)
            self.assertTrue(root.exists())

    def test_unknown_docker_owner_blocks_removal_and_retains_work(self):
        with tempfile.TemporaryDirectory() as tmp:
            work=Path(tmp)/"work";work.mkdir()
            runner=VM.Runner(work,Path(tmp),"a"*32);runner.created_container=True
            def command(argv,**kwargs):
                if "ls" in argv:return "some-container\n"
                if "inspect" in argv:return json.dumps([{"Config":{"Labels":{VM.LABEL:"another-owner"}}}])
                self.fail("unexpected mutation: "+str(argv))
            with mock.patch.object(runner,"command",side_effect=command):
                failures=runner.cleanup()
            self.assertTrue(failures)
            self.assertTrue(work.exists())


if __name__=="__main__":unittest.main()
