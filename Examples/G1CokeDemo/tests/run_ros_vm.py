"""Run ROS transport checks in an existing VM container without installing tools."""
import argparse
import base64
import json
from pathlib import Path
import shlex
import subprocess
import sys

parser = argparse.ArgumentParser()
parser.add_argument("--cli", required=True)
parser.add_argument("--device", default="vm:g1-sim")
parser.add_argument("--app", default="g1-coke-demo")
args = parser.parse_args()
root = Path(__file__).resolve().parents[1]
files = {str(p): base64.b64encode((root / p).read_bytes()).decode() for p in
         [Path("coke_demo/__init__.py"), Path("coke_demo/ros_bridge.py"), Path("tests/test_ros_integration.py")]}
code = '''import base64,importlib,json,pathlib,runpy,sys,tempfile,types
files=json.loads(PAYLOAD)
with tempfile.TemporaryDirectory(prefix="coke-ros-check-") as folder:
 root=pathlib.Path(folder)
 for name,content in files.items():
  p=root/name;p.parent.mkdir(parents=True,exist_ok=True);p.write_bytes(base64.b64decode(content))
 sys.path.insert(0,str(root))
 # The test only uses pytest to skip when ROS is absent. ROS is present here;
 # keep the same test body without adding pytest to a deployed container.
 sys.modules["pytest"]=types.SimpleNamespace(importorskip=importlib.import_module)
 test=runpy.run_path(str(root/"tests/test_ros_integration.py"))
 test["test_real_dds_command_sensors_and_services"]()
 print("PASS: real ROS 2 DDS commands, sensors, transforms, clock, late subscribers, reset service")
'''.replace("PAYLOAD", repr(json.dumps(files)))
# Isolate the synthetic test clock and robot from the live app graph.
command = "source /opt/ros/humble/setup.bash && ROS_DOMAIN_ID=77 python3 -c " + shlex.quote(code)
sys.exit(subprocess.run([args.cli, "--device", args.device, "device", "attach", args.app,
                         "--", "/bin/bash", "-lc", command]).returncode)
