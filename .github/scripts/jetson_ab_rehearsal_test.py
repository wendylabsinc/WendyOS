"""Offline OTA-plan admission only; no device, mount, flash or firmware access."""
import hashlib
import importlib.util
import io
import json
from pathlib import Path
import tarfile
import tempfile
import unittest
from unittest.mock import patch

ROOT=Path(__file__).resolve().parents[1]/"regression-coverage/manual"

def module(name,path):
    spec=importlib.util.spec_from_file_location(name,path)
    value=importlib.util.module_from_spec(spec)
    spec.loader.exec_module(value)
    return value

M=module("jetson_ab",ROOT/"jetson-ab-rehearsal.py")
S=module("jetson_ab_snapshot",ROOT/"jetson-ab-snapshot.py")

class JetsonABTests(unittest.TestCase):
    def fixture(self,root,device="jetson-orin-nano"):
        marker=root/"unexpected-cli"
        cli=root/"cli"
        cli.write_text("#!/bin/sh\ntouch "+str(marker)+"\nexit 99\n")
        cli.chmod(0o755)
        record=root/"record";record.write_text("inert local record; no physical evidence")
        updater=root/"source"
        for path in ("cmd/wendyos-update/main.go","internal/connector/tegrauefi/tegrauefi.go","internal/connector/tegrauefi/swap-slot.go","internal/engine/commit.go","docs/cli-contract.md","docs/state-schema.md"):
            file=updater/path;file.parent.mkdir(parents=True,exist_ok=True);file.write_text("inert source for offline generator validation")
        trial="1234567890abcdef"*2
        data={k:str(record) for k in ("agent_binary","updater_binary","identity_record","recovery_record","authorization_record")}
        data.update(cli_binary=str(cli),cli_revision="a"*40,agent_revision="b"*40,updater_source=str(updater),trial=trial,install_seconds=1800,boot_seconds=300,
                    board={"device_type":device,"soc":"tegra264" if device=="jetson-agx-thor" else "tegra234","storage":"nvme","serial":"lab-serial","device_tree_serial":"dt-serial","ecid":"owned-ecid","owner":"lab owner","carrier":"developer kit","root_disk_serial":"owned-disk","firmware":"recorded UEFI","l4t":"recorded L4T","address":"192.0.2.2:50052","initial_slot":"a","app_partuuid":"11111111-1111-1111-1111-111111111111","app_b_partuuid":"22222222-2222-2222-2222-222222222222","esp_partuuid":"33333333-3333-3333-3333-333333333333","redundancy":{"attributes":6,"value":0},"retries":{"attributes":6,"value":3}},artifacts={})
        hook="#!/bin/sh\nprintf '%s\\n' 'CI2341_HEALTH_REJECT "+trial+"' >&2\nexit 1\n"
        binary_hash=hashlib.sha256(record.read_bytes()).hexdigest()
        for role in ("normal","reject"):
            payload=(role+" inert payload, intentionally not a physical zstd image").encode()
            raw_hash=hashlib.sha256((role+" rootfs placeholder").encode()).hexdigest()
            manifest={"format_version":1,"artifact_name":"ci-2341-"+role,"artifact_version":"0.20.2-"+role,"compatible_devices":[device],"payload":{"name":"rootfs.ext4.zst","compression":"zstd","sha256":raw_hash,"compressed_sha256":hashlib.sha256(payload).hexdigest()}}
            archive=root/(role+".wendy")
            with tarfile.open(archive,"w",format=tarfile.USTAR_FORMAT) as tar:
                for name,content in (("manifest.json",json.dumps(manifest).encode()),("rootfs.ext4.zst",payload)):
                    info=tarfile.TarInfo(name);info.size=len(content);tar.addfile(info,io.BytesIO(content))
            archive_hash=hashlib.sha256(archive.read_bytes()).hexdigest()
            descriptor=root/(role+"-descriptor.json")
            descriptor.write_text(json.dumps({"url":"https://example.invalid/immutable/"+role+".wendy","sha256":archive_hash,"size_bytes":archive.stat().st_size,"storage":"nvme","artifact_name":manifest["artifact_name"],"artifact_version":manifest["artifact_version"]}))
            inspection=root/(role+"-inspection.json")
            inspection.write_text(json.dumps({"artifact_sha256":archive_hash,"rootfs_sha256":raw_hash,"agent_sha256":binary_hash,"updater_sha256":binary_hash,"bootloader_marker_present":False,"health_hook_path":"etc/wendyos-update/health.d/99-ci-2341-"+trial if role=="reject" else None,"health_hook_mode":"0755" if role=="reject" else None,"health_hook_sha256":hashlib.sha256(hook.encode()).hexdigest() if role=="reject" else None,"reviewer":"offline synthetic fixture only"}))
            data["artifacts"][role]={"archive":str(archive),"descriptor":str(descriptor),"payload_inspection":str(inspection)}
        return data,marker

    def generate(self,data,root):
        inventory=root/"inventory.json";inventory.write_text(json.dumps(data))
        def git(argv,**kwargs):
            self.assertEqual(argv[0],"git")
            self.assertEqual(kwargs["timeout"],10)
            return M.UPDATER_REVISION+"\n" if argv[1]=="rev-parse" else ""
        with patch.object(M.subprocess,"check_output",side_effect=git):
            return M.generate(inventory,root/"out")

    def test_both_firmware_families_have_only_unexecuted_cases(self):
        for device in ("jetson-orin-nano","jetson-agx-thor"):
            with self.subTest(device=device),tempfile.TemporaryDirectory() as tmp:
                root=Path(tmp);data,marker=self.fixture(root,device)
                evidence=self.generate(data,root)
                self.assertFalse(marker.exists())
                self.assertEqual(evidence["hardware_execution"],"not_run")
                self.assertEqual({c["status"] for c in evidence["cases"]},{"not_run"})
                commands=json.loads((root/"out/commands.json").read_text())
                for role in ("normal","reject"):
                    cmd=commands["update_"+role]
                    self.assertIn("--binary",cmd);self.assertIn("--artifact-url",cmd)
                    self.assertNotIn("--json",cmd);self.assertNotIn("--pr",cmd)
                    self.assertEqual(cmd[cmd.index("--device")+1],data["board"]["address"])
                self.assertEqual(commands["console_rollback"],["/usr/bin/wendyos-update","rollback"])
                self.assertNotIn("mark-good",str(commands))
                self.assertIn("stale-pending",(root/"out/procedure.txt").read_text())
                self.assertEqual(len(evidence["generated"]),5)
                compile((root/"out/jetson-ab-snapshot.py").read_text(),"snapshot.py","exec")
                with self.assertRaises(FileExistsError):self.generate(data,root)

    def test_rejects_wrong_artifact_identity_or_unbounded_plan(self):
        for fault in ("descriptor_hash","payload_hash","capsule","hook","board","slots","query","timeout","credential"):
            with self.subTest(fault=fault),tempfile.TemporaryDirectory() as tmp:
                root=Path(tmp);data,marker=self.fixture(root)
                descriptor=Path(data["artifacts"]["reject"]["descriptor"])
                desc=json.loads(descriptor.read_text())
                inspection=Path(data["artifacts"]["reject"]["payload_inspection"])
                ins=json.loads(inspection.read_text())
                if fault=="descriptor_hash":desc["sha256"]="0"*64
                elif fault=="payload_hash":
                    archive=Path(data["artifacts"]["reject"]["archive"])
                    raw=archive.read_bytes();raw=raw.replace(b"reject inert payload",b"wrong! inert payload")
                    archive.write_bytes(raw);desc["sha256"]=hashlib.sha256(raw).hexdigest();ins["artifact_sha256"]=desc["sha256"]
                elif fault=="capsule":ins["bootloader_marker_present"]=True
                elif fault=="hook":ins["health_hook_sha256"]="0"*64
                elif fault=="board":data["board"]["soc"]="tegra264"
                elif fault=="slots":data["board"]["app_b_partuuid"]=data["board"]["app_partuuid"]
                elif fault=="query":desc["url"]+="?credential=unsafe"
                elif fault=="timeout":data["boot_seconds"]=999999
                else:data["password"]="never-record"
                descriptor.write_text(json.dumps(desc));inspection.write_text(json.dumps(ins))
                with self.assertRaises(ValueError):self.generate(data,root)
                self.assertFalse(marker.exists());self.assertFalse((root/"out").exists())

    def test_snapshot_requires_gpt_roles_and_exact_parent_disk(self):
        board={"app_partuuid":"aa","app_b_partuuid":"bb","esp_partuuid":"cc","root_disk_serial":"owned"}
        partitions={name:{"path":"/dev/test"+str(i),"major_minor":"8:"+str(i)} for i,name in enumerate(("app_partuuid","app_b_partuuid","esp_partuuid"),1)}
        rows=[{"path":"/dev/test","type":"disk","serial":"owned"}]
        for name,label in (("app_partuuid","APP"),("app_b_partuuid","APP_b"),("esp_partuuid","esp")):
            rows.append({"path":partitions[name]["path"],"maj:min":partitions[name]["major_minor"],"partuuid":board[name],"partlabel":label,"type":"part","pkname":"/dev/test"})
        S.check_disk_layout(rows,partitions,board)
        for key,value in (("partuuid","wrong"),("partlabel","APP_b"),("pkname","/dev/other"),("maj:min","8:99")):
            broken=json.loads(json.dumps(rows));broken[1][key]=value
            with self.subTest(key=key),self.assertRaises(ValueError):S.check_disk_layout(broken,partitions,board)
        rows[0]["serial"]="unrelated"
        with self.assertRaises(ValueError):S.check_disk_layout(rows,partitions,board)

    def test_snapshot_refuses_unrelated_serial_before_commands(self):
        config={"board":{"device_tree_serial":"reserved","soc":"tegra234"}}
        with patch.object(S,"small",side_effect=[b"different\x00",b"nvidia,tegra234\x00"]),patch.object(S,"run") as run:
            with self.assertRaisesRegex(ValueError,"serial/SoC"):
                S.collect(config,"a")
            run.assert_not_called()


if __name__=="__main__":unittest.main()
