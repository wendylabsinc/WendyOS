"""Historical overlays/observer offline checks; never invoke mount or reboot."""
import hashlib
import importlib.util
import json
from pathlib import Path
import stat
import subprocess
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

HERE=Path(__file__).resolve().parent
MANUAL=HERE.parent/"regression-coverage/manual"

def module(name,path):
    spec=importlib.util.spec_from_file_location(name,path);value=importlib.util.module_from_spec(spec);spec.loader.exec_module(value);return value

M=module("capsule_boundary",MANUAL/"jetson-capsule-boundary.py")
O=module("capsule_boundary_observer",MANUAL/"jetson-capsule-boundary-observer.py")
C=module("capsule_boundary_preparation_tests",HERE/"jetson_capsule_rehearsal_test.py")

class CapsuleBoundaryTests(unittest.TestCase):
    def fixture(self,root):
        prepared=C.CapsulePlanTests();data,marker=prepared.fixture(root,"jetson-agx-thor");prepared.generate(data,root)
        source=root/"historical-agent";updater=root/"historical-updater"
        bodies={M.AGENT_PATH:"func update() {\n\tosVersion, _ := wendyOSVersion()\n\tif err := rebootAfterOSUpdate(newRebooter(s.logger), osVersion); err != nil { panic(err) }\n}\n",
                M.REBOOT_PATH:"sync:      unix.Sync,\nimmediate: func() error { return syscall.Reboot(syscall.LINUX_REBOOT_CMD_RESTART) },\nclean:     systemctlReboot,\ngrace:     rebootGrace,\nsleep:     time.Sleep,\n",
                "go/internal/agent/services/reboot.go":"inert source fixture", "go.mod":"inert module fixture", "go.sum":"inert sums fixture"}
        for name,body in bodies.items():
            path=source/name;path.parent.mkdir(parents=True,exist_ok=True);path.write_text(body)
        path=updater/"internal/connector/tegrauefi/swap-slot.go";path.parent.mkdir(parents=True)
        # Golden file identity is checked separately by source tests below; a
        # private minimal body avoids requiring another repository for discovery.
        path.write_text("file.Sync without filesystem sync, inert fixture")
        legacy=root/"legacy-updater";legacy.write_bytes(b"inert old updater binary")
        provenance=root/"legacy-producer.json";provenance.write_text(json.dumps({"source_revision":M.UPDATER_REV,"binary_sha256":hashlib.sha256(legacy.read_bytes()).hexdigest(),"running_os":"0.17.0","reviewer":"offline fixture only"}))
        return {"capsule_plan":str(root/"capsule-plan/evidence.json"),"agent_source":str(source),"updater_source":str(updater),"nonce":"1"*32,"running_os":"0.17.0","legacy_updater_binary":str(legacy),"legacy_producer_record":str(provenance)},marker

    def generate(self,data,root):
        path=root/"boundary-inventory.json";path.write_text(json.dumps(data))
        def git(repo,*args):
            if args[0]=="status":return ""
            return M.AGENT_REV if Path(repo)==Path(data["agent_source"]).resolve() else M.UPDATER_REV
        old_hash=M.AB.record(str(Path(data["updater_source"])/"internal/connector/tegrauefi/swap-slot.go"))["sha256"]
        with patch.object(M,"git",side_effect=git),patch.object(M,"UPDATER_SHA",old_hash),patch.object(M,"AGENT_SHA",hashlib.sha256(b"func update() {\n\tosVersion, _ := wendyOSVersion()\n\tif err := rebootAfterOSUpdate(newRebooter(s.logger), osVersion); err != nil { panic(err) }\n}\n").hexdigest()):
            return M.generate(path,root/"boundary")

    def test_overlays_preserve_actual_sync_syscall_and_watchdog(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp);data,marker=self.fixture(root);evidence=self.generate(data,root)
            self.assertFalse(marker.exists());self.assertEqual(evidence["hardware_execution"],"not_run")
            self.assertEqual({v["status"] for v in evidence["cases"]},{"not_run"})
            out=root/"boundary"
            self.assertEqual(json.loads((out/"fixed/overlay.json").read_text()),{"Replace":{}})
            commands=json.loads((out/"device-commands.json").read_text())
            self.assertNotIn("--artifact-url",commands["deploy_fixed"])
            self.assertIn("--artifact-url",commands["update_fixed"])
            self.assertNotIn("--json",commands["update_fixed"])
            self.assertEqual(set(json.loads((out/"sync-only/overlay.json").read_text())["Replace"]),{str((Path(data["agent_source"])/M.AGENT_PATH).resolve())})
            for variant,call in (("historical-instant","newRebooter(s.logger).immediate()"),("sync-only","newRebooter(s.logger).reboot()")):
                text=(out/variant/"agent_service.go").read_text()
                original=(Path(data["agent_source"])/M.AGENT_PATH).read_text()
                restored=text.replace(call,"rebootAfterOSUpdate(newRebooter(s.logger), osVersion)")
                restored=restored.replace('\tif err := rebootAfterOSUpdate','\tosVersion, _ := wendyOSVersion()\n\tif err := rebootAfterOSUpdate')
                self.assertEqual(restored,original)
            for variant in ("clean-request-failure","watchdog-no-reboot"):
                text=(out/variant/"reboot_linux.go").read_text()
                self.assertIn("sync:      unix.Sync",text);self.assertIn("syscall.Reboot(syscall.LINUX_REBOOT_CMD_RESTART)",text)
                self.assertIn("grace:     rebootGrace",text);self.assertIn("sleep:     time.Sleep",text)
            for unit in ("wendy-agent","wendyos-update-verify","wendyos-update-commit"):
                drop=(out/"image-files/etc/systemd/system"/(unit+".service.d")/"ci1682-observer.conf").read_text()
                self.assertIn("Requires=ci1682-observe.service",drop);self.assertIn("After=ci1682-observe.service",drop)
            with self.assertRaises(FileExistsError): self.generate(data,root)

    def test_changed_source_and_modern_updater_provenance_refuse(self):
        for fault in ("source","provenance","os","orin"):
            with self.subTest(fault=fault),tempfile.TemporaryDirectory() as tmp:
                root=Path(tmp);data,marker=self.fixture(root)
                if fault=="source": (Path(data["agent_source"])/M.AGENT_PATH).write_text("changed")
                elif fault=="os":data["running_os"]="0.18.1"
                elif fault=="orin":
                    path=Path(data["capsule_plan"]);value=json.loads(path.read_text());value["board"]["soc"]="tegra234";value["board"]["device_type"]="jetson-agx-orin";path.write_text(json.dumps(value))
                else:
                    path=Path(data["legacy_producer_record"]);value=json.loads(path.read_text());value["source_revision"]=M.AB.UPDATER_REVISION;path.write_text(json.dumps(value))
                with self.assertRaises(ValueError):self.generate(data,root)
                self.assertFalse((root/"boundary").exists());self.assertFalse(marker.exists())

    def test_observer_identity_refuses_before_any_command(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp);path=root/"proc/device-tree/serial-number";path.parent.mkdir(parents=True);path.write_bytes(b"wrong\0")
            with patch.object(O,"run") as run,self.assertRaisesRegex(ValueError,"serial/SoC"):
                O.collect({"board":{"device_tree_serial":"reserved","soc":"tegra234"}},root=root)
            run.assert_not_called()

    def test_observer_records_capsule_before_cleanup_without_mounting_existing_view(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp);board={"device_tree_serial":"reserved","soc":"tegra234","esp_partuuid":"a"*36,"root_disk_serial":"owned","app_partuuid":"b"*36,"app_b_partuuid":"c"*36}
            for name,data in {"proc/device-tree/serial-number":b"reserved\0","proc/device-tree/compatible":b"nvidia,tegra234\0","proc/sys/kernel/random/boot_id":b"11111111-2222-3333-4444-555555555555","esp/EFI/UpdateCapsule/TEGRA_BL.Cap":b"actual private bytes"}.items():
                path=root/name;path.parent.mkdir(parents=True,exist_ok=True);path.write_bytes(data)
            original_stat=Path.stat
            def stat_path(path,*args,**kwargs):
                if str(path).endswith(board["esp_partuuid"]):return SimpleNamespace(st_mode=stat.S_IFBLK,st_rdev=O.os.makedev(8,1))
                return original_stat(path,*args,**kwargs)
            calls=[]
            def run(*argv):
                calls.append(argv)
                if argv[0]=="lsblk":return json.dumps({"blockdevices":[{"path":"/dev/owned1","partuuid":board["esp_partuuid"],"type":"part","partlabel":"esp","maj:min":"8:1","pkname":"/dev/owned"},{"path":"/dev/owned","type":"disk","serial":"owned"},{"path":"/dev/owned2","type":"part","partuuid":"b"*36,"partlabel":"APP","pkname":"/dev/owned","maj:min":"8:2"}]})
                if argv[0]=="findmnt" and "--target" in argv:return "8:2\n"
                if argv[0]=="findmnt" and "--list" in argv:return json.dumps({"filesystems":[{"maj:min":"8:1","target":str(root/"esp"),"fstype":"vfat","options":"rw"}]})
                return "a\n"
            config={"board":board,"nonce":"f"*32}
            with patch.object(Path,"stat",stat_path),patch.object(O,"run",side_effect=run):result=O.collect(config,root=root)
            def wrong_root(*argv):
                value=run(*argv)
                if argv[0]=="lsblk":
                    parsed=json.loads(value);parsed["blockdevices"][-1].update(partuuid="unknown",partlabel=None)
                    return json.dumps(parsed)
                return value
            with patch.object(Path,"stat",stat_path),patch.object(O,"run",side_effect=wrong_root),self.assertRaisesRegex(ValueError,"reserved slot"):
                O.collect(config,root=root)
            self.assertEqual(result["capsule"]["sha256"],hashlib.sha256(b"actual private bytes").hexdigest())
            self.assertFalse(any(c[0] in ("mount","umount","sync","reboot") for c in calls))
            (root/"data").mkdir()
            with patch("builtins.print"):O.save(config,result,root=root)
            saved=root/"data/ci1682"/("f"*32)/(result["boot_id"]+".json")
            self.assertEqual(json.loads(saved.read_text()),result)
            with self.assertRaises(FileExistsError):O.save(config,result,root=root)
            outside=root/"outside";outside.mkdir()
            (root/"data/ci1682"/("e"*32)).symlink_to(outside,target_is_directory=True)
            with self.assertRaisesRegex(ValueError,"unowned or unsafe"):
                O.save({**config,"nonce":"e"*32},result,root=root)
            self.assertEqual(list(outside.iterdir()),[])
            (root/"run").mkdir()
            calls.clear()
            def unknown_mount(*argv):
                if argv[0]=="findmnt" and "--list" in argv:return '{"filesystems":[]}'
                if argv[0]=="mount":
                    calls.append(argv)
                    raise subprocess.TimeoutExpired(argv,10)
                return run(*argv)
            with patch.object(Path,"stat",stat_path),patch.object(O,"run",side_effect=unknown_mount),patch("builtins.print"),self.assertRaises(subprocess.TimeoutExpired):
                O.collect(config,root=root)
            self.assertFalse(any(c[0]=="umount" for c in calls))
            self.assertEqual(len(list((root/"run").glob("ci1682-esp-*"))),1)


if __name__=="__main__":unittest.main()
