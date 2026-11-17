"""Offline capsule-plan admission; inert files only, no device or firmware calls."""
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

HERE = Path(__file__).resolve().parent

def module(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    value = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(value)
    return value

BASE = module("capsule_ab_tests", HERE/"jetson_ab_rehearsal_test.py")
M = module("capsule_plan", HERE.parent/"regression-coverage/manual/jetson-capsule-rehearsal.py")


class CapsulePlanTests(unittest.TestCase):
    def fixture(self, root, device="jetson-orin-nano"):
        base = BASE.JetsonABTests()
        data, marker = base.fixture(root, device)
        base.generate(data, root)
        source = Path(data["updater_source"])/"internal/connector/tegrauefi/verify.go"
        source.write_text("inert verification source, no hardware evidence")
        capsule = root/"tegra-bl.cap"
        capsule.write_bytes(b"inert capsule fixture, not NVIDIA firmware")
        role = data["artifacts"]["normal"]
        ins = json.loads(Path(role["payload_inspection"]).read_text())
        ins.update(bootloader_marker_present=True, capsule_path="/opt/nvidia/UpdateCapsule/tegra-bl.cap", capsule_sha256=M.AB.record(str(capsule))["sha256"], capsule_size_bytes=capsule.stat().st_size)
        inspection = root/"capsule-inspection.json"
        inspection.write_text(json.dumps(ins))
        result = {k:data["identity_record"] for k in ("transition_record", "recovery_record", "power_isolation_record", "authorization_record")}
        result.update(ab_plan=str(root/"out/evidence.json"), archive=role["archive"], descriptor=role["descriptor"], payload_inspection=str(inspection), capsule=str(capsule), boot_seconds=300, power_off_seconds=10,
                      transition={"from_firmware":data["board"]["firmware"], "to_firmware":"different reviewed firmware", "from_esrt_version":"1", "to_esrt_version":"2", "esrt_firmware_guid":"11111111-2222-3333-4444-555555555555", "vendor_qualification":"inert offline admission only", "antirollback_compatible":True, "recovery_independently_verified":True})
        return result, marker

    def generate(self, data, root):
        inventory = root/"capsule-inventory.json"
        inventory.write_text(json.dumps(data))
        def git(argv, **kwargs):
            self.assertEqual(argv[0], "git")
            self.assertEqual(kwargs["timeout"], 10)
            return M.AB.UPDATER_REVISION+"\n" if argv[1] == "rev-parse" else ""
        with patch("subprocess.check_output", side_effect=git):
            return M.generate(inventory, root/"capsule-plan")

    def test_distinct_capsule_cases_remain_unexecuted_on_both_families(self):
        for device in ("jetson-orin-nano", "jetson-agx-thor"):
            with self.subTest(device=device), tempfile.TemporaryDirectory() as tmp:
                root=Path(tmp); data, marker=self.fixture(root, device)
                result=self.generate(data, root)
                self.assertFalse(marker.exists())
                self.assertEqual(result["audit_entries"], ["1682"])
                self.assertEqual(result["hardware_execution"], "not_run")
                self.assertEqual({c["status"] for c in result["cases"]}, {"not_run"})
                self.assertIn("agent_sync_boundary_fault_injection", {c["id"] for c in result["cases"]})
                commands=json.loads((root/"capsule-plan/commands.json").read_text())
                self.assertNotIn("--json", commands["agent_capsule_update"])
                self.assertIn("--binary", commands["agent_capsule_update"])
                self.assertEqual(commands["console_stage_without_reboot"][:2], ["/usr/bin/wendyos-update", "install"])
                self.assertNotIn("rollback", commands)
                guide=(root/"capsule-plan/procedure.txt").read_text()
                self.assertIn("FILE_CAPSULE_DELIVERY", guide)
                self.assertIn("absence alone is neither success nor loss", guide)
                self.assertIn("can still\n    succeed", guide)
                self.assertNotIn("@ESP@", guide)
                with self.assertRaises(FileExistsError): self.generate(data, root)

    def test_fail_closed_before_output_for_changed_inputs_and_unqualified_transition(self):
        for fault in ("capsule_hash", "marker", "rootfs_hash", "base_change", "same_firmware", "same_esrt", "recovery", "antirollback", "guid", "timeout", "url", "updater_source"):
            with self.subTest(fault=fault), tempfile.TemporaryDirectory() as tmp:
                root=Path(tmp); data, marker=self.fixture(root)
                path=Path(data["payload_inspection"]); ins=json.loads(path.read_text())
                if fault=="capsule_hash": ins["capsule_sha256"]="0"*64
                elif fault=="marker": ins["bootloader_marker_present"]=False
                elif fault=="rootfs_hash": ins["rootfs_sha256"]="0"*64
                elif fault=="base_change": (root/"out/snapshot-config.json").write_text("changed")
                elif fault=="same_firmware": data["transition"]["to_firmware"]=data["transition"]["from_firmware"]
                elif fault=="same_esrt": data["transition"]["to_esrt_version"]="1"
                elif fault=="recovery": data["transition"]["recovery_independently_verified"]=False
                elif fault=="antirollback": data["transition"]["antirollback_compatible"]=False
                elif fault=="guid": data["transition"]["esrt_firmware_guid"]="unknown"
                elif fault=="timeout": data["boot_seconds"]=999999
                elif fault=="url":
                    desc=Path(data["descriptor"]); value=json.loads(desc.read_text()); value["url"]+="?secret=never"; desc.write_text(json.dumps(value))
                elif fault=="updater_source": (root/"source/internal/connector/tegrauefi/swap-slot.go").write_text("changed")
                path.write_text(json.dumps(ins))
                with self.assertRaises(ValueError): self.generate(data, root)
                self.assertFalse(marker.exists())
                self.assertFalse((root/"capsule-plan").exists())


if __name__=="__main__": unittest.main()
