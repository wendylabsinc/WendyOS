"""Offline first-boot plan admission; no images or devices are operated."""
import hashlib
import importlib.util
import json
from pathlib import Path
import subprocess
import tempfile
import unittest

SCRIPT = Path(__file__).resolve().parents[1] / "regression-coverage/manual/orin-firstboot-rehearsal.py"
SPEC = importlib.util.spec_from_file_location("orin_firstboot", SCRIPT)
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)


class OrinFirstBootTests(unittest.TestCase):
    def fixture(self, root):
        marker = root / "unexpected-cli-execution"
        cli = root / "cli"
        cli.write_text("#!/bin/sh\ntouch " + str(marker) + "\nexit 99\n")
        cli.chmod(0o755)
        record = root / "record.txt"
        record.write_text("inert offline fixture record")
        archive = root / "archive"
        archive.write_bytes(b"inert test bytes, not a flashpack")
        descriptor = root / "release.json"
        descriptor.write_text(json.dumps({"device":"jetson-orin-nano", "storage":"nvme", "version":"0.20.2", "url":"https://example.invalid/0.20.2/fixture.flashpack.tar.zst", "sha256":hashlib.sha256(archive.read_bytes()).hexdigest(), "size_bytes":archive.stat().st_size}))
        data = {key:str(record) for key in ("agent_binary", "identity_record", "recovery_record", "ap_record")}
        data.update(cli_binary=str(cli), cli_source_commit="a"*40, agent_source_commit="b"*40,
                    release_archive=str(archive), release_descriptor=str(descriptor),
                    board={"device":"jetson-orin-nano", "storage":"nvme", "serial":"fixture-serial", "owner":"fixture owner", "interface":"wlan0", "station_mac":"02:00:00:00:01:01", "bssid":"02:00:00:00:02:02", "ssid":"lab's literal; SSID", "gateway":"192.0.2.1", "management_address":"192.0.2.2:50051", "device_name":"firstboot-fixture", "boot_timeout_seconds":300})
        return data, marker

    def test_generates_bounded_physical_plan_without_running_cli(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp)
            data, marker=self.fixture(root)
            inventory=root/"inventory.json";inventory.write_text(json.dumps(data))
            out=root/"out"
            evidence=MODULE.generate(inventory,out)
            self.assertFalse(marker.exists())
            self.assertEqual(evidence["hardware_execution"],"not_run")
            self.assertEqual({case["status"] for case in evidence["cases"]},{"not_run"})
            commands=json.loads((out/"commands.json").read_text())
            self.assertIn("--carrier", commands["plan"])
            self.assertNotIn("--carrier", commands["pre_enroll_fresh_install"])
            self.assertIn("--binary", commands["pre_enroll_fresh_install"])
            self.assertNotIn("--binary", commands["job_start_prefix"])
            self.assertIn("--no-wifi", commands["pre_enroll_fresh_install"])
            self.assertNotIn("--force", commands["pre_enroll_fresh_install"])
            self.assertNotIn("--password", commands["wifi_setup"])
            self.assertEqual(commands["wifi_setup"][-2],data["board"]["ssid"])
            compile((out/"start-seeded-job.py").read_text(),"start-seeded-job.py","exec")
            subprocess.run(["sh","-n",str(out/"board-snapshot.sh")],check=True,timeout=5)
            self.assertNotIn("--show-secrets",(out/"board-snapshot.sh").read_text())
            self.assertIn("ping -I",(out/"board-snapshot.sh").read_text())
            self.assertIn("no acme-first-boot-attempt.json",(out/"procedure.txt").read_text())
            with self.assertRaises(FileExistsError):MODULE.generate(inventory,out)

    def test_rejects_mismatched_artifact_or_unbounded_target(self):
        for fault in ("digest","size","storage","version","cloud","interface","source","timeout","credential"):
            with self.subTest(fault=fault),tempfile.TemporaryDirectory() as tmp:
                root=Path(tmp);data,marker=self.fixture(root)
                descriptor=Path(data["release_descriptor"])
                release=json.loads(descriptor.read_text())
                if fault=="digest":release["sha256"]="0"*64
                elif fault=="size":release["size_bytes"]=True
                elif fault=="storage":release["storage"]="emmc"
                elif fault=="version":release["version"]="latest"
                elif fault=="cloud":data["board"]["management_address"]="cloud://fixture"
                elif fault=="interface":data["board"]["interface"]="../../unrelated"
                elif fault=="source":data["agent_source_commit"]="main"
                elif fault=="credential":data["board"]["password"]="must-not-be-recorded"
                else:data["board"]["boot_timeout_seconds"]=999999
                descriptor.write_text(json.dumps(release))
                inventory=root/"inventory.json";inventory.write_text(json.dumps(data))
                with self.assertRaises(ValueError):MODULE.generate(inventory,root/"out")
                self.assertFalse(marker.exists())
                self.assertFalse((root/"out").exists())


if __name__=="__main__":
    unittest.main()
