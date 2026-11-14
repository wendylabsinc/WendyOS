"""Execute the guest preflight with private command fixtures, never RPM installs."""
import os
from pathlib import Path
import shlex
import subprocess
import sys
import tempfile
import unittest

SCRIPT=Path(__file__).resolve().parent/"fixtures/driver-vm/preflight.sh"


class DriverVMPreflightTests(unittest.TestCase):
    def run_preflight(self, **changes):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp);bin_dir=root/"bin";bin_dir.mkdir();kernels=root/"kernels";kernels.mkdir()
            (kernels/"7.2.3-300.fc45.x86_64").mkdir()
            commands={
                "rpm": '''import os,sys
package=sys.argv[-1]
if package==os.environ.get("TEST_RPM_FAILURE"):raise SystemExit(1)
values={"systemd":os.environ["TEST_SYSTEMD_RPM"],"kernel-core":os.environ["TEST_KERNEL_CORE"],"kernel-devel":os.environ["TEST_KERNEL_DEVEL"]}
print(values[package])
''',
                "systemd-sysext": '''import os,sys
if sys.argv[-1]=="--version":print(os.environ["TEST_SYSEXT_VERSION"])
elif sys.argv[-1]=="--help":
    if os.environ.get("TEST_HELP_FAILURE"):raise SystemExit(2)
    print(os.environ["TEST_SYSEXT_HELP"])
else:raise SystemExit("only read-only version/help probes are permitted")
'''}
            for name,code in commands.items():
                path=bin_dir/name
                path.write_text("#!/bin/sh\nexec "+shlex.quote(sys.executable)+" -c "+shlex.quote(code)+' "$@"\n')
                path.chmod(0o755)
            env={"PATH":str(bin_dir)+os.pathsep+os.defpath,"TEST_SYSTEMD_RPM":"262.1-1.fc45.x86_64","TEST_KERNEL_CORE":"7.2.3-300.fc45.x86_64","TEST_KERNEL_DEVEL":"7.2.3-300.fc45.x86_64","TEST_SYSEXT_VERSION":"systemd 262 (262.1-1.fc45)\n+PAM +AUDIT","TEST_SYSEXT_HELP":"  --mutable=MODE\n  --always-refresh=BOOL"}
            env.update(changes)
            return subprocess.run(["bash","-euo","pipefail","-c",'source "$1"; driver_vm_preflight "$2"; printf "SELECTED_KERNEL=%s\\n" "$driver_vm_kernel_release"',"preflight",str(SCRIPT),str(kernels)],env=env,text=True,capture_output=True,timeout=5)

    def test_current_guest_reports_binary_features_and_matching_kernel(self):
        result=self.run_preflight()
        self.assertEqual(result.returncode,0,result.stdout+result.stderr)
        self.assertIn("systemd=262.1-1.fc45.x86_64",result.stdout)
        self.assertIn("Driver VM preflight verified",result.stdout)
        self.assertIn("SELECTED_KERNEL=7.2.3-300.fc45.x86_64",result.stdout)
        result=self.run_preflight(TEST_SYSEXT_VERSION="systemd 260 (260-1.fixture)")
        self.assertEqual(result.returncode,0,result.stderr)

    def test_fedora44_failure_names_actual_dependency_and_keeps_production_flag(self):
        result=self.run_preflight(TEST_SYSTEMD_RPM="259.10-1.fc44.x86_64",TEST_SYSEXT_VERSION="systemd 259 (259.10-1.fc44)",TEST_SYSEXT_HELP="  --mutable=MODE")
        self.assertNotEqual(result.returncode,0)
        self.assertIn("kernel-core=7.2.3-300.fc45.x86_64",result.stdout)
        self.assertIn("systemd >=260",result.stderr)
        self.assertIn("--always-refresh",result.stderr)
        self.assertIn("259.10-1.fc44.x86_64",result.stderr)
        self.assertIn("do not remove the production flag",result.stderr)
        self.assertNotIn("preflight verified",result.stdout)

    def test_version_alone_cannot_hide_missing_binary_features(self):
        for option in ("--mutable","--always-refresh"):
            with self.subTest(option=option):
                keep="--always-refresh=BOOL" if option=="--mutable" else "--mutable=MODE"
                result=self.run_preflight(TEST_SYSEXT_HELP=keep)
                self.assertNotEqual(result.returncode,0)
                self.assertIn("lacks required "+option,result.stderr)
        result=self.run_preflight(TEST_HELP_FAILURE="1")
        self.assertNotEqual(result.returncode,0)
        self.assertIn("--help failed",result.stderr)

    def test_kernel_pair_and_build_tree_fail_with_actionable_details(self):
        for values,needle in (({"TEST_KERNEL_DEVEL":"wrong-devel"},"exactly matching"),({"TEST_KERNEL_CORE":"not-installed","TEST_KERNEL_DEVEL":"not-installed"},"build tree is absent"),({"TEST_KERNEL_CORE":"one\ntwo"},"exactly matching")):
            with self.subTest(values=values):
                result=self.run_preflight(**values)
                self.assertNotEqual(result.returncode,0)
                self.assertIn(needle,result.stderr)
                self.assertNotIn("preflight verified",result.stdout)

    def test_query_and_unparseable_binary_fail_closed(self):
        result=self.run_preflight(TEST_RPM_FAILURE="systemd")
        self.assertNotEqual(result.returncode,0)
        self.assertIn("cannot query",result.stderr)
        result=self.run_preflight(TEST_SYSEXT_VERSION="unknown")
        self.assertNotEqual(result.returncode,0)
        self.assertIn("cannot identify",result.stderr)


if __name__=="__main__":unittest.main()
