"""Portable admission/cleanup tests only. Real kernel proof requires the VM gate."""
import importlib.util
import io
import json
from pathlib import Path
import tarfile
import tempfile
import unittest
from unittest import mock

PATH = Path(__file__).with_name("test-driver-lifecycle-vm.py")
SPEC = importlib.util.spec_from_file_location("driver_vm", PATH)
VM = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(VM)


class DriverVMTests(unittest.TestCase):
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
            self.assertEqual(result,{"members":4,"expanded_bytes":4})

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
        normal=f"--- PASS: TestDriverLifecycleVM\nDRIVER_VM_LIFECYCLE_VERIFIED {nonce}\nDRIVER_VM_EXIT nonce={nonce} control=normal phase=first status=0\n"
        VM.accept_log(normal,nonce,"normal","first",0)
        for bad in (normal.replace("LIFECYCLE_VERIFIED","missing"),normal+"--- SKIP: prereq\n",normal.replace(nonce,"b"*32)):
            with self.assertRaises(ValueError):VM.accept_log(bad,nonce,"normal","first",0)
        control=f"--- FAIL: TestDriverLifecycleVM\nlive driver version mismatch: wendy_fixture_target\nDRIVER_VM_EXIT nonce={nonce} control=omit-unload phase=first status=1\n"
        VM.accept_log(control,nonce,"omit-unload","first",0)
        with self.assertRaises(ValueError):VM.accept_log(control.replace("live driver version mismatch: wendy_fixture_target","compile failed"),nonce,"omit-unload","first",0)
        with self.assertRaises(ValueError):VM.accept_log(control,nonce,"omit-unload","first",124)

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
