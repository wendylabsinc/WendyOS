"""Run inside the image as root. No GPU or Agent is required."""
import json
import os
from pathlib import Path
import subprocess
import sys
import time

sys.path.insert(0, "/opt/worker")
from worker import atomic_json

control = Path("/control")
control.mkdir(exist_ok=True)
control.chmod(0o755)
boot = Path("/proc/sys/kernel/random/boot_id").read_text().strip()
heartbeat = Path("/work/heartbeat")
command = ["python3", "-u", "-c", "import time,pathlib; p=pathlib.Path('/work/heartbeat');\nwhile True: p.write_text(str(time.monotonic())); time.sleep(.1)"]
proc = subprocess.Popen(["python3", "/opt/worker/model/lease_runner.py"],
                        env={**os.environ, "WORKLOAD_COMMAND": json.dumps(command), "LANE": "cpu",
                             "WORKLOAD_STATE": "/state"})
try:
    time.sleep(1)
    assert not heartbeat.exists(), "work started without a grant"
    atomic_json(control / "lease.json", {"cpu": True, "boot_id": boot, "issued": time.monotonic()})
    deadline = time.monotonic() + 4
    while not heartbeat.exists() and time.monotonic() < deadline:
        time.sleep(.1)
    assert heartbeat.exists(), "grant did not start work"
    # The workload UID cannot renew or replace its own admission file.
    attempt = subprocess.run(["python3", "-c", "open('/control/lease.json','w').write('{}')"],
                             user=10001, group=10001, extra_groups=[], capture_output=True)
    assert attempt.returncode != 0, "workload can overwrite root-owned grant"
    # Simulate supervisor loss: nobody refreshes the permission.
    time.sleep(9)
    stamp = heartbeat.read_text()
    time.sleep(1)
    assert heartbeat.read_text() == stamp, "work continued after lease expiry"
    print("PASS: no-grant startup, UID isolation and expiry-driven process cleanup", flush=True)
finally:
    proc.terminate()
    proc.wait(timeout=8)
