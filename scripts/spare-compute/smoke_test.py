#!/usr/bin/env python3
"""Exercise the built image with real Go fuzzing, mutations and race tests."""
import json
import os
from pathlib import Path
import subprocess
import tempfile
import shutil
import time
import uuid

HERE = Path(__file__).resolve().parent
IMAGE = os.environ.get("SPARE_TEST_IMAGE", "wendy-spare-compute:local")


def run(*args, **kwargs):
    return subprocess.run(args, check=True, text=True, **kwargs)


with tempfile.TemporaryDirectory() as tmp:
    root = Path(tmp).resolve()
    repo = root / "repo"
    repo.mkdir()
    (repo / "go.mod").write_text("module example.com/sparetest\n\ngo 1.27.4\n")
    (repo / "sample").mkdir()
    (repo / "sample/sample.go").write_text('''package sample
func Positive(n int) bool { return n > 0 }
func Sum(a, b int) int { return a + b }
''')
    (repo / "sample/sample_test.go").write_text('''package sample
import "testing"
func TestPositive(t *testing.T) {
    if !Positive(1) || Positive(-1) { t.Fatal("bad sign") }
}
func TestSum(t *testing.T) { if Sum(0, 0) != 0 { t.Fatal("bad sum") } }
func FuzzPositive(f *testing.F) {
    f.Add(1)
    f.Fuzz(func(t *testing.T, n int) { _ = Positive(n) })
}
''')
    run("git", "init", "-q", str(repo))
    run("git", "-C", str(repo), "add", ".")
    run("git", "-C", str(repo), "-c", "user.name=Smoke", "-c", "user.email=smoke@example.invalid",
        "-c", "commit.gpgsign=false", "commit", "-qm", "fixture")
    archive = root / "source.tar"
    run("git", "-C", str(repo), "archive", "--format=tar", f"--output={archive}", "HEAD")
    config = root / "config.json"
    config.write_text(json.dumps({"fuzz_seconds": 2, "job_seconds": 120,
        "fuzz": [{"package": "sample", "target": "FuzzPositive"}],
        "mutation": ["sample"], "race": ["sample"]}))
    # Wendy persist mounts are noexec; Go must execute from /work instead.
    run("docker", "run", "--rm", "--init", "--user=10001:10001", "--read-only",
        "--entrypoint=python3", "--tmpfs", "/state:rw,noexec,mode=1777",
        "--tmpfs", "/work:rw,exec,mode=1777", "--cpus=2", "--memory=2g",
        "-e", "MIN_FREE_GIB=0", "-e", "MODES=fuzz",
        "-v", f"{archive}:/input/source.tar:ro", "-v", f"{config}:/opt/worker/config.json:ro",
        IMAGE, "-c", "import sys,json,pathlib; sys.path.insert(0,'/opt/worker'); import worker; "
        "sys.argv=['worker','--once']; worker.main(); "
        "r=next(pathlib.Path('/state/reports').glob('*/result.json')); "
        "assert json.loads(r.read_text())['status']=='completed'; print('PASS: noexec persistence')")
    volume = "wendy-spare-smoke-" + uuid.uuid4().hex[:8]
    run("docker", "volume", "create", volume, stdout=subprocess.DEVNULL)
    try:
        for mode in ("fuzz", "mutation", "race"):
            run("docker", "run", "--rm", "--init", "--user=10001:10001", "--entrypoint=python3", "--read-only", "--cap-drop=ALL",
                "--security-opt=no-new-privileges", "--cpus=2", "--memory=2g", "--pids-limit=512",
                "-e", "MIN_FREE_GIB=0", "-e", "WORK_ROOT=/state/work", "-e", "WORK_TMP=/state/tmp", "-e", "MODES=" + mode,
                "-v", f"{archive}:/input/source.tar:ro", "-v", f"{config}:/opt/worker/config.json:ro",
                "-v", volume + ":/state", IMAGE, "/opt/worker/worker.py", "--once")
        script = '''import json,pathlib
reports = [json.loads(p.read_text()) for p in pathlib.Path('/state/reports').glob('*/result.json')]
assert len(reports) == 3, reports
assert all(r['status'] == 'completed' for r in reports), reports
mutations = list(pathlib.Path('/state/reports').glob('*/mutations.json'))
assert len(mutations) == 1 and mutations[0].stat().st_size > 0
summary = json.loads(mutations[0].read_text())
assert summary['mutants_killed'] > 0 and summary['mutants_lived'] > 0, summary
print('PASS: real fuzz, mutation report, and race test in container')
'''
        run("docker", "run", "--rm", "--entrypoint", "python3", "-v", volume + ":/state",
            IMAGE, "-c", script)
    finally:
        run("docker", "volume", "rm", volume, stdout=subprocess.DEVNULL)

run("docker", "run", "--rm", "--init", "--entrypoint=python3",
    "-v", str(HERE / "lease_smoke.py") + ":/tests/lease_smoke.py:ro",
    IMAGE, "/tests/lease_smoke.py")
