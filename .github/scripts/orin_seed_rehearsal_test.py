#!/usr/bin/env python3
"""Portable checks for full-flashpack accounting and honest result admission."""
import importlib.util
import io
import json
from pathlib import Path
import tarfile
import unittest

SPEC = importlib.util.spec_from_file_location("orin_rehearsal", Path(__file__).with_name("test-orin-seed-rehearsal.py"))
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)


class OrinRehearsalTests(unittest.TestCase):
    def archive(self, members):
        output = io.BytesIO()
        with tarfile.open(fileobj=output, mode="w") as archive:
            for name, content in members:
                member = tarfile.TarInfo(name)
                member.size = len(content)
                archive.addfile(member, io.BytesIO(content))
        output.seek(0)
        return output

    def test_accounts_actual_tar_headers_without_extracting(self):
        manifest = json.dumps({"wendyos_version": "0.20.2", "layout": {
            "config_image": "flash/config.img", "partition_layout": "flash/layout.xml"}}).encode()
        layout = b'<partition_layout><device><partition name="config"><size>0x200000000</size></partition></device></partition_layout>'
        result = MODULE.archive_accounting(self.archive([
            ("./flash/root.img", b"root-data" * 512), ("./flash/config.img", b"config" * 100),
            ("./flash/layout.xml", layout), ("./manifest.json", manifest)]))
        self.assertEqual(result["expanded_bytes"], 9 * 512 + 600 + len(manifest) + len(layout))
        self.assertEqual(result["config_bytes"], 600)
        self.assertEqual(result["config_partition_bytes"], 8 * MODULE.GIB)
        self.assertEqual(result["virtual_disk_bytes"], 128 * MODULE.GIB)
        self.assertEqual(result["required_free_bytes"], 2 * result["expanded_bytes"] + 68 * MODULE.GIB)

    def test_rejects_unsafe_or_duplicate_headers(self):
        for members in [[("../outside", b"x")], [("same", b"x"), ("same", b"y")]]:
            with self.assertRaises(ValueError):
                MODULE.archive_accounting(self.archive(members))

    def test_normal_requires_all_three_real_verification_markers(self):
        text = "--- PASS: TestOrinFullInstallSeedSim (1s)\n" + "".join(
            f"ORIN_SEED_VERIFIED run={run} device=jetson-orin-nano\n" for run in (1, 2, 3))
        MODULE.verify_log(text)
        for bad in [text.replace("run=3", "run=2"), text + "--- SKIP: some prerequisite", text.split("ORIN_SEED_VERIFIED")[0]]:
            with self.assertRaises(ValueError):
                MODULE.verify_log(bad)

    def test_control_requires_full_writer_then_failed_flashed_readback(self):
        text = ("run 1: wrote in 1m; config image 123 bytes\n"
                "run 1 flashed: reading wendy.conf: not found\n"
                "--- FAIL: TestOrinFullInstallSeedSim (60s)\n")
        MODULE.verify_log(text, control=True)
        for bad in [text.replace("flashed", "injected"), text.replace("wrote in", "failed before"),
                    "compile error", text + "--- SKIP:"]:
            with self.assertRaises(ValueError):
                MODULE.verify_log(bad, control=True)


if __name__ == "__main__":
    unittest.main()
