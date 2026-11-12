"""Fetch a trusted revision after admission, then execute the bounded worker."""
import os
from pathlib import Path
import shutil
import subprocess
import sys

state = Path("/state")
checkout = Path("/work/checkout")
shutil.rmtree(checkout, ignore_errors=True)
checkout.mkdir(parents=True)
subprocess.run(["git", "init", "-q", str(checkout)], check=True)
subprocess.run(["git", "-C", str(checkout), "fetch", "--depth=1",
                os.environ.get("SOURCE_REPOSITORY", "https://github.com/wendylabsinc/WendyOS.git"),
                os.environ.get("SOURCE_REF", "main")], check=True)
subprocess.run(["git", "-C", str(checkout), "archive", "--format=tar",
                "--output=" + str(state / "source.tar"), "FETCH_HEAD"], check=True)
shutil.rmtree(checkout)
os.execv(sys.executable, [sys.executable, str(Path(__file__).with_name("worker.py")),
                         "--archive", str(state / "source.tar")])
