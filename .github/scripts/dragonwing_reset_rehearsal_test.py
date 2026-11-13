"""Synthetic offline admission/marker tests, never EDL or physical reset evidence."""
import hashlib
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

MANUAL=Path(__file__).resolve().parents[1]/"regression-coverage/manual"


def load(name):
    spec=importlib.util.spec_from_file_location(name,MANUAL/(name+".py"))
    module=importlib.util.module_from_spec(spec);spec.loader.exec_module(module)
    return module


PLAN=load("dragonwing-reset-rehearsal")
MARKER=load("dragonwing-reset-marker")
SOURCE=b"synthetic Builder source observation, not firmware provenance\n"


def sha(content):
    return hashlib.sha256(content).hexdigest()


class DragonwingResetTests(unittest.TestCase):
    def fixture(self,root,device="dragonwing-iq-9075"):
        invoked=root/"unexpected-device-command"
        cli=root/"candidate CLI"
        cli.write_text("#!/bin/sh\ntouch '"+str(invoked)+"'\nexit 99\n");cli.chmod(0o755)
        records={"cli_binary":str(cli)}
        raw=b'<data><program label="config" filename="" SECTOR_SIZE_IN_BYTES="4096" num_partition_sectors="8192" physical_partition_number="0" start_sector="1024" size_in_KB="32768"/><program label="data" filename="" SECTOR_SIZE_IN_BYTES="4096" num_partition_sectors="0" physical_partition_number="0" start_sector="10000"/></data>'
        files={"agent_binary":b"synthetic arm64 candidate", "release_archive":b"synthetic inert archive", "rawprogram":raw,"patches":b'<patches><patch filename="DISK"/></patches>',"programmer":b"synthetic DDR ELF"}
        for key,content in files.items():
            path=root/key;path.write_bytes(content);records[key]=str(path)
        record=root/"reviewed record";record.write_text("synthetic offline record, never a hardware observation")
        for key in ("identity_record","storage_record","recovery_record","authorization_record","ap_record"):records[key]=str(record)
        archive_path="images/"+device+"/0.20.2/fixture.qcomflash.tar.gz"
        release={"device":device,"storage":"ufs","version":"0.20.2","url":"https://storage.googleapis.com/wendyos-images-public/"+archive_path+"?generation=1234","sha256":sha(files["release_archive"]),"size_bytes":len(files["release_archive"])}
        catalog={"device_id":device,"versions":{"0.20.2":{"qcomflash_path":archive_path,"qcomflash_checksum":release["sha256"],"qcomflash_size_bytes":release["size_bytes"]}}}
        review={key+"_sha256":sha(files[key]) for key in ("release_archive","rawprogram","patches","programmer")}
        review.update(builder_source_commit="c"*40,reviewer="synthetic reviewer")
        for key,value in (("release_descriptor",release),("device_catalog",catalog),("bundle_review",review)):
            path=root/(key+".json");path.write_text(json.dumps(value));records[key]=str(path)
        data={**records,"cli_source_commit":"a"*40,"agent_source_commit":"b"*40,"builder_source_commit":"c"*40,"builder_source":str(root),"trial":"1"*32,"flash_seconds":1800,"boot_seconds":300,
              "board":{"device":device,"storage":"ufs","msm_id":PLAN.BOARDS[device]["msm_id"],"serial":"fixture-board","soc_serial":"fixture-soc","soc_serial_path":"/sys/devices/soc0/serial_number","owner":"fixture owner","edl_usb_path":"fixture physical port","serial_console":"fixture console","ufs_serial":"fixture-UFS","ufs_size_bytes":32*1024**3,"carrier_revision":"fixture-rev","management_address":"192.0.2.4:50051","device_name":"edl-fixture","partitions":{"config":{"lun":0,"start_sector":1024,"sector_size":4096,"num_sectors":8192},"data":{"lun":0,"start_sector":10000,"sector_size":4096,"num_sectors":0}}},
              "wifi":{"security":"open","ssid":"Reserved fixture AP","interface":"wlan0","station_mac":"02:00:00:00:00:01","bssid":"02:00:00:00:00:02","gateway":"192.0.2.1"}}
        inventory=root/"inventory.json";inventory.write_text(json.dumps(data))
        return inventory,data,invoked

    def test_both_board_plans_bind_artifacts_without_device_execution(self):
        for device in PLAN.BOARDS:
            with self.subTest(device=device),tempfile.TemporaryDirectory() as tmp:
                root=Path(tmp);inventory,data,invoked=self.fixture(root,device)
                with patch.object(PLAN.subprocess,"check_output",return_value=SOURCE) as git:
                    evidence=PLAN.generate(inventory,root/"out")
                self.assertEqual(git.call_count,2)
                for call,path in zip(git.call_args_list,PLAN.BUILDER_FILES):
                    self.assertEqual(call.args[0],["git","show","c"*40+":"+path])
                    self.assertEqual(call.kwargs,{"cwd":root.resolve(),"timeout":10})
                self.assertFalse(invoked.exists())
                self.assertEqual(evidence["hardware_execution"],"not_run")
                self.assertEqual({case["status"] for case in evidence["cases"]},{"not_run"})
                self.assertEqual(evidence["data_blank_bytes"],1048576)
                for record in [evidence["inventory_record"],*evidence["records"].values(),*evidence["sources"],*evidence["generated"]]:
                    self.assertEqual(record["sha256"],sha(Path(record["path"]).read_bytes()))
                self.assertEqual({source["sha256"] for source in evidence["builder_sources"]},{sha(SOURCE)})
                commands=json.loads((root/"out/commands.json").read_text());install=commands["install"]
                self.assertIn("--pre-enroll=false",install)
                self.assertEqual(install[install.index("--device-type")+1],device)
                self.assertEqual(install[install.index("--binary")+1],str(Path(data["agent_binary"]).resolve()))
                self.assertEqual(install[install.index("--wifi")+1],"ssid=Reserved fixture AP,security=open")
                self.assertIn("WENDY_CONFIG_DIR="+str((root/"out/cli-config").resolve()),install)
                self.assertEqual((root/"out/cli-config").stat().st_mode&0o777,0o700)
                for flag in ("--force","--storage","--drive","--rootfs-only","--no-bmap","--yes-overwrite-internal"):self.assertNotIn(flag,install)
                guide=(root/"out/procedure.txt").read_text()
                self.assertIn(PLAN.BOARDS[device]["switch"],guide)
                self.assertIn('`wrote "config"` and `wrote "data"`',guide)
                self.assertIn("SAME new /data UUID",guide)
                self.assertIn("NOT secure erasure",guide)
                self.assertNotIn("@BOOT@",guide)

    def test_refuses_wrong_target_artifact_geometry_and_unbounded_plan(self):
        for fault in ("board","storage","msm","serial-path","cloud","source","secret","bound","archive","catalog","url","review","geometry","overlap","config-empty","filled-data","xml-entity"):
            with self.subTest(fault=fault),tempfile.TemporaryDirectory() as tmp:
                root=Path(tmp);inventory,data,invoked=self.fixture(root)
                release_path=Path(data["release_descriptor"]);release=json.loads(release_path.read_text())
                catalog_path=Path(data["device_catalog"]);catalog=json.loads(catalog_path.read_text())
                review_path=Path(data["bundle_review"]);review=json.loads(review_path.read_text())
                if fault=="board":data["board"]["device"]="dragonwing-unknown"
                elif fault=="storage":data["board"]["storage"]="spi-nor"
                elif fault=="msm":data["board"]["msm_id"]=PLAN.BOARDS["dragonwing-iq-8275"]["msm_id"]
                elif fault=="serial-path":data["board"]["soc_serial_path"]="/etc/shadow"
                elif fault=="cloud":data["board"]["management_address"]="cloud://unrelated"
                elif fault=="source":data["builder_source_commit"]="main"
                elif fault=="secret":data["wifi"]["password"]="do-not-record"
                elif fault=="bound":data["flash_seconds"]=86400
                elif fault=="archive":Path(data["release_archive"]).write_bytes(b"changed")
                elif fault=="catalog":catalog["versions"]["0.20.2"]["qcomflash_checksum"]="d"*64
                elif fault=="url":release["url"]="https://unrelated.test/image.tar.gz"
                elif fault=="review":review["programmer_sha256"]="d"*64
                elif fault=="geometry":data["board"]["partitions"]["data"]["lun"]=1
                else:
                    raw_path=Path(data["rawprogram"]);raw=raw_path.read_text()
                    if fault=="overlap":
                        raw=raw.replace('start_sector="10000"','start_sector="2048"');data["board"]["partitions"]["data"]["start_sector"]=2048
                    elif fault=="config-empty":raw=raw.replace('num_partition_sectors="8192"','num_partition_sectors="0"')
                    elif fault=="filled-data":raw=raw.replace('label="data" filename=""','label="data" filename="foreign.img"')
                    elif fault=="xml-entity":raw='<!DOCTYPE data [<!ENTITY bad "bad">]>'+raw
                    raw_path.write_text(raw);review["rawprogram_sha256"]=sha(raw.encode())
                release_path.write_text(json.dumps(release));catalog_path.write_text(json.dumps(catalog));review_path.write_text(json.dumps(review));inventory.write_text(json.dumps(data))
                with patch.object(PLAN.subprocess,"check_output") as git,self.assertRaises((ValueError,KeyError)):
                    PLAN.generate(inventory,root/"out")
                git.assert_not_called();self.assertFalse(invoked.exists());self.assertFalse((root/"out").exists())

    def marker_fixture(self,root):
        _,data,_=self.fixture(root)
        board=data["board"]
        for path,content in ((board["soc_serial_path"],board["soc_serial"]+"\n"),("/etc/wendyos/device-type","BOARD="+board["device"]+"\nMACHINE=fixture-yocto\nSTORAGE=ufs\n"),("/proc/sys/kernel/random/boot_id","fixture-boot-id\n")):
            target=root/path.lstrip("/");target.parent.mkdir(parents=True,exist_ok=True);target.write_text(content)
        (root/"data").mkdir()
        mount={"filesystems":[{"target":"/data","fstype":"ext4","uuid":"fixture-uuid","maj:min":"8:3"}]}
        rows={"blockdevices":[{"path":"/dev/sda","pkname":None,"maj:min":"8:0","serial":board["ufs_serial"],"size":board["ufs_size_bytes"],"partlabel":None,"partuuid":None,"type":"disk"},{"path":"/dev/sda2","pkname":"/dev/sda","maj:min":"8:2","type":"part","partlabel":"system_a","partuuid":"fixture-root"},{"path":"/dev/sda3","pkname":"/dev/sda","maj:min":"8:3","type":"part","partlabel":"data","partuuid":"fixture-data"}]}
        def observe(argv):
            if argv==["findmnt","--json","--target","/data","--output","TARGET,FSTYPE,UUID,MAJ:MIN"]:return json.dumps(mount)
            if argv==["lsblk","--json","--list","--bytes","--paths","--output","PATH,PKNAME,MAJ:MIN,SERIAL,SIZE,PARTLABEL,PARTUUID,TYPE"]:return json.dumps(rows)
            if argv==["findmnt","-n","-o","MAJ:MIN","--target","/"]:return "8:2\n"
            self.fail("unexpected device observation: "+repr(argv))
        return {"board":board,"trial":data["trial"]},mount,rows,observe

    def test_owned_marker_lifecycle_and_no_overwrite(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp);config,_,_,observe=self.marker_fixture(root)
            first=MARKER.marker(config,"create",root,observe);path=Path(first["marker"])
            self.assertEqual(path.read_text(),config["trial"]+"\n");self.assertEqual(path.stat().st_mode&0o777,0o600)
            self.assertEqual(MARKER.marker(config,"present",root,observe)["data"],first["data"])
            with self.assertRaises(FileExistsError):MARKER.marker(config,"create",root,observe)
            with self.assertRaisesRegex(ValueError,"survived"):MARKER.marker(config,"absent",root,observe)
            path.write_text("different trial\n")
            with self.assertRaisesRegex(ValueError,"differs"):MARKER.marker(config,"present",root,observe)
            path.unlink()  # Synthetic fixture only, not a simulated physical reset claim.
            self.assertEqual(MARKER.marker(config,"absent",root,observe)["observation"],"absent")
            unrelated=root/"unrelated";unrelated.write_text("keep")
            path.symlink_to(unrelated)
            with self.assertRaises(OSError):MARKER.marker(config,"present",root,observe)
            with self.assertRaises(FileExistsError):MARKER.marker(config,"create",root,observe)
            with self.assertRaises(ValueError):MARKER.marker(config,"absent",root,observe)
            self.assertEqual(unrelated.read_text(),"keep")

    def test_identity_and_actual_mount_refusals_precede_marker_write(self):
        for fault in ("serial","type","duplicate-type","storage","mount","fstype","uuid","major","partlabel","mapper","disk-serial","disk-size","root-parent"):
            with self.subTest(fault=fault),tempfile.TemporaryDirectory() as tmp:
                root=Path(tmp);config,mount,rows,observe=self.marker_fixture(root)
                if fault=="serial":config["board"]["soc_serial"]="another-board"
                elif fault=="type":(root/"etc/wendyos/device-type").write_text("dragonwing-iq-8275\n")
                elif fault=="duplicate-type":(root/"etc/wendyos/device-type").write_text("BOARD=dragonwing-iq-9075\nBOARD=dragonwing-iq-9075\n")
                elif fault=="storage":(root/"etc/wendyos/device-type").write_text("BOARD=dragonwing-iq-9075\nSTORAGE=emmc\n")
                elif fault=="mount":mount["filesystems"][0]["target"]="/"
                elif fault=="fstype":mount["filesystems"][0]["fstype"]="tmpfs"
                elif fault=="uuid":mount["filesystems"][0]["uuid"]=""
                elif fault=="major":mount["filesystems"][0]["maj:min"]="9:9"
                elif fault=="partlabel":rows["blockdevices"][2]["partlabel"]="other"
                elif fault=="mapper":rows["blockdevices"][2]["type"]="crypt"
                elif fault=="disk-serial":rows["blockdevices"][0]["serial"]="other-disk"
                elif fault=="disk-size":rows["blockdevices"][0]["size"]+=512
                elif fault=="root-parent":rows["blockdevices"][1]["pkname"]="/dev/sdb"
                with self.assertRaises(ValueError):MARKER.marker(config,"create",root,observe)
                self.assertEqual(list((root/"data").iterdir()),[])
        self.assertEqual(MARKER.device_type("dragonwing-iq-9075\n"),"dragonwing-iq-9075")


if __name__=="__main__":unittest.main()
