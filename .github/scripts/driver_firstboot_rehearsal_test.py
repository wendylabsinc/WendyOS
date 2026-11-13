"""Offline audit2628 plan validation, with synthetic metadata and no devices."""
import base64
import hashlib
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

SCRIPT = Path(__file__).resolve().parents[1]/"regression-coverage/manual/driver-firstboot-rehearsal.py"
SPEC = importlib.util.spec_from_file_location("driver_firstboot", SCRIPT)
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)
SOURCE = b'/* synthetic source-observation fixture, not a physical capture */\nconst char *message="wendyos_hello: driver add-on loaded (sysext pipeline OK)";\n'


def sha(value):
    return hashlib.sha256(value).hexdigest()


class DriverFirstbootTests(unittest.TestCase):
    def fixture(self, root):
        cli=root/"candidate cli"
        marker=root/"unexpected-cli-execution"
        cli.write_text("#!/bin/sh\ntouch '"+str(marker)+"'\nexit 99\n")
        cli.chmod(0o755)
        files={"agent_binary":b"synthetic candidate Agent", "release_archive":b"inert archive, not a flashpack", "driver_raw":b"inert bytes, not a driver", "signing_public_key":b"synthetic public-key record"}
        records={}
        for name,content in files.items():
            path=root/name;path.write_bytes(content);records[name]=str(path)
        note=root/"reviewed record";note.write_text("synthetic offline admission record")
        for name in ("identity_record","recovery_record","authorization_record","network_isolation_record"): records[name]=str(note)
        signature=b"s"*3309
        driver={"name":"wendyos-hello","version":"0.20.2","kernel_version":"5.15.185-tegra","path":"images/jetson-orin-nano/0.20.2/hello.raw","sha256":sha(files["driver_raw"]),"size_bytes":len(files["driver_raw"]),"signature":base64.b64encode(signature).decode(),"modules_load":["wendyos_hello"]}
        archive_path="images/jetson-orin-nano/0.20.2/orin-nvme.flashpack.tar.zst"
        release={"device":"jetson-orin-nano","storage":"nvme","version":"0.20.2","url":MODULE.GCS+archive_path+"?generation=1234","sha256":sha(files["release_archive"]),"size_bytes":len(files["release_archive"])}
        catalog={"device_id":"jetson-orin-nano","versions":{"0.20.2":{"nvme_flashpack_path":archive_path,"nvme_flashpack_checksum":release["sha256"],"nvme_flashpack_size_bytes":release["size_bytes"],"devkit":{"kernel_version":driver["kernel_version"]},"extensions":[driver]}}}
        review={"driver_sha256":driver["sha256"],"signature_sha256":sha(signature),"agent_sha256":sha(files["agent_binary"]),"public_key_sha256":sha(files["signing_public_key"]),"agent_signature_verification_enabled":True,"reviewer":"synthetic offline reviewer"}
        for name,value in (("release_descriptor",release),("device_catalog",catalog),("signature_review",review)):
            path=root/(name+".json");path.write_text(json.dumps(value));records[name]=str(path)
        data={**records,"cli_binary":str(cli),"cli_source_commit":"a"*40,"agent_source_commit":"b"*40,"builder_source":str(root),"disconnect_seconds":30,
              "board":{"device":"jetson-orin-nano","storage":"nvme","carrier":"developer-kit","serial":"synthetic-serial","ecid":"synthetic-ecid","owner":"test owner","storage_serial":"synthetic-disk","usb_path":"synthetic-port","serial_console":"synthetic-console","device_name":"driver-fixture","usb_address_before":"169.254.44.1:50051","usb_address_after":"169.254.44.2:50051"}}
        inventory=root/"inventory.json";inventory.write_text(json.dumps(data))
        return inventory,data,marker

    def test_commands_and_hashes_are_bound_without_running_any_device_command(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp);inventory,data,marker=self.fixture(root)
            with patch.object(MODULE.subprocess,"check_output",return_value=SOURCE) as git:
                evidence=MODULE.generate(inventory,root/"out")
            git.assert_called_once_with(["git","show",MODULE.BUILDER_REVISION+":"+MODULE.BUILDER_PATH],cwd=root.resolve(),timeout=10)
            self.assertFalse(marker.exists())
            self.assertEqual(evidence["hardware_execution"],"not_run")
            self.assertEqual({case["status"] for case in evidence["cases"]},{"not_run"})
            self.assertEqual(evidence["module_provenance"]["sha256"],sha(SOURCE))
            for record in [evidence["inventory_record"],*evidence["generated"],*evidence["records"].values(),*evidence["sources"]]:
                self.assertEqual(record["sha256"],sha(Path(record["path"]).read_bytes()))
            commands=json.loads((root/"out/commands.json").read_text())
            install=commands["install_with_driver"]
            self.assertEqual(install[0],"env")
            self.assertIn("WENDY_CONFIG_DIR="+str((root/"out/cli-config").resolve()),install)
            self.assertIn("WENDY_SECRET_STORE=file",install)
            for flag in ("--driver","--binary","--pre-enroll=false","--no-wifi"):self.assertIn(flag,install)
            self.assertEqual(install[install.index("--driver")+1],"wendyos-hello")
            self.assertEqual(install[install.index("--binary")+1],str(Path(data["agent_binary"]).resolve()))
            self.assertNotIn("--driver",commands["fresh_registry_baseline"])
            self.assertNotIn("--file",commands["registry_install"])
            self.assertNotIn("--carrier",install)  # Supported only by install plan.
            for command in commands.values():self.assertNotIn("--force",command)
            guide=(root/"out/procedure.txt").read_text()
            self.assertIn("OS written, but drivers were not installed",guide)
            self.assertIn("does not perform ML-DSA verification",guide)
            self.assertIn("wendyos_hello: driver add-on loaded (sysext pipeline OK)",guide)

    def test_actual_empty_key_policy_is_recorded_without_claiming_verification(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp);inventory,data,marker=self.fixture(root)
            Path(data["signing_public_key"]).write_bytes(b"")
            review_path=Path(data["signature_review"])
            review=json.loads(review_path.read_text())
            review["public_key_sha256"]=sha(b"")
            review["agent_signature_verification_enabled"]=False
            review_path.write_text(json.dumps(review))
            with patch.object(MODULE.subprocess,"check_output",return_value=SOURCE):
                evidence=MODULE.generate(inventory,root/"out")
            self.assertEqual(evidence["signature_policy"],{"candidate_verifier_enabled":False,"generation_verifies_signature":False})
            self.assertEqual(evidence["records"]["signing_public_key"]["size_bytes"],0)
            self.assertEqual({case["status"] for case in evidence["cases"]},{"not_run"})
            self.assertFalse(marker.exists())

    def test_rejects_mismatched_target_catalog_bytes_signature_review_and_bounds(self):
        for fault in ("carrier","storage","address","same-address","same-address-spelling","source","secret","bound","archive","kernel","raw","module","signature","disabled","review-key","origin"):
            with self.subTest(fault=fault), tempfile.TemporaryDirectory() as tmp:
                root=Path(tmp);inventory,data,marker=self.fixture(root)
                catalog_path=Path(data["device_catalog"]);catalog=json.loads(catalog_path.read_text());entry=catalog["versions"]["0.20.2"];driver=entry["extensions"][0]
                review_path=Path(data["signature_review"]);review=json.loads(review_path.read_text())
                if fault=="carrier":data["board"]["carrier"]="custom"
                elif fault=="storage":data["board"]["storage"]="emmc"
                elif fault=="address":data["board"]["usb_address_after"]="127.0.0.1:50051"
                elif fault=="same-address":data["board"]["usb_address_after"]=data["board"]["usb_address_before"]
                elif fault=="same-address-spelling":data["board"]["usb_address_after"]="[169.254.44.1]:50051"
                elif fault=="source":data["agent_source_commit"]="main"
                elif fault=="secret":data["password"]="never-record-this"
                elif fault=="bound":data["disconnect_seconds"]=601
                elif fault=="archive":Path(data["release_archive"]).write_bytes(b"changed")
                elif fault=="kernel":entry["devkit"]["kernel_version"]="wrong-kernel"
                elif fault=="raw":Path(data["driver_raw"]).write_bytes(b"changed")
                elif fault=="module":driver["modules_load"]=["unrelated_module"]
                elif fault=="signature":driver["signature"]="AQID"
                elif fault=="disabled":review["agent_signature_verification_enabled"]=False
                elif fault=="review-key":review["public_key_sha256"]="0"*64
                elif fault=="origin":driver["path"]="../latest/hello.raw"
                inventory.write_text(json.dumps(data));catalog_path.write_text(json.dumps(catalog));review_path.write_text(json.dumps(review))
                with patch.object(MODULE.subprocess,"check_output",side_effect=AssertionError("must reject before reading builder")):
                    with self.assertRaises(ValueError):MODULE.generate(inventory,root/"out")
                self.assertFalse(marker.exists());self.assertFalse((root/"out").exists())


if __name__=="__main__":unittest.main()
