import tempfile
from pathlib import Path
import unittest
from unittest.mock import patch

import host_probe


class HostProbeTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        self.write("proc/100/environ", "WENDY_APP_ID=sh.wendy.spare-compute\0")
        self.write("proc/100/cgroup", "0::/our-app\n")
        self.write("proc/200/environ", "OTHER=yes\0")
        self.write("proc/200/cgroup", "0::/foreground\n")
        self.write("sys/fs/cgroup/our-app/cpu.stat", "usage_usec 4000000\n")
        self.write("proc/stat", "cpu  10 0 5 100 2 0 0 0 0 0\n")
        self.write("proc/meminfo", "MemTotal: 10000 kB\nMemAvailable: 8000 kB\n")
        self.write("proc/sys/kernel/random/boot_id", "boot\n")
        self.write("proc/driver/nvidia/version", "driver")

    def tearDown(self):
        self.temp.cleanup()

    def write(self, path, value):
        target = self.root / path
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(value)

    def snapshot(self, processes, memory="1000"):
        rows = [["0", "GPU-0", "0", memory, "40000", "Disabled", "Disabled"]]
        with patch("host_probe.Path", side_effect=lambda p: self.root / str(p).lstrip("/")), \
             patch("host_probe.query", return_value=rows), \
             patch("host_probe.gpu_processes", return_value={"GPU-0": [int(pid) for gpu, pid in processes]}):
            return host_probe.snapshot("sh.wendy.spare-compute")

    def test_identifies_own_and_foreign_gpu_processes_by_cgroup(self):
        own = self.snapshot([["GPU-0", "100"]])
        self.assertEqual(own["own_cpu_usec"], 4000000)
        self.assertFalse(own["gpus"][0]["foreign"])
        foreign = self.snapshot([["GPU-0", "200"]])
        self.assertTrue(foreign["gpus"][0]["foreign"])
        self.assertEqual(foreign["memory_available"], 8000 * 1024)

    def test_unresolved_gpu_owner_is_foreign(self):
        self.assertTrue(self.snapshot([["GPU-0", "999"]])["gpus"][0]["foreign"])

    def test_missing_memory_is_not_zero_or_free(self):
        result = self.snapshot([], "[N/A]")
        self.assertTrue(result["gpu_known"])
        self.assertIsNone(result["gpus"][0]["free_mib"])

    def test_xml_collects_graphics_and_other_gpu_contexts(self):
        import subprocess
        xml = """<nvidia_smi_log><gpu><uuid>GPU-0</uuid><processes>
        <process_info><pid>200</pid><type>G</type></process_info>
        <process_info><pid>300</pid><type>O</type></process_info>
        </processes></gpu></nvidia_smi_log>"""
        with patch("host_probe.subprocess.run", return_value=subprocess.CompletedProcess([], 0, xml)):
            self.assertEqual(host_probe.gpu_processes(), {"GPU-0": [200, 300]})

    def test_missing_app_cgroup_is_an_error(self):
        self.write("proc/100/environ", "OTHER=yes\0")
        with self.assertRaises(ValueError):
            self.snapshot([])


if __name__ == "__main__":
    unittest.main()
