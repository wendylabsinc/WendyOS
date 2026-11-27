package main

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// These imports execute from the generated artifact, with Python's ambient
// module search disabled. Fake Go/native bytes are never executed. This checks
// packaging closure and digest admission, not ROS compilation or VM readiness.
func TestCloudPackagedImportClosureAndNativeSeal(t *testing.T) {
	o := testOptions(t)
	if _, err := stage(o, fakeBuilder); err != nil {
		t.Fatal(err)
	}
	releaseDir := filepath.Join(t.TempDir(), "release")
	r, err := release(releaseOptions{Out: releaseDir, Repo: o.Repo, Arch: o.Arch}, fakeBuilder)
	if err != nil {
		t.Fatal(err)
	}
	const script = `
import importlib
from pathlib import Path
import sys

app = Path(sys.argv[1]).resolve()
sys.path.insert(0, str(app))
for name in ('sim.go2_cloud_live', 'sim.go2_bridge.__main__', 'sim.go2_bridge.transport'):
    module = importlib.import_module(name)
    assert Path(module.__file__).resolve().is_relative_to(app), name
import boot
if sys.argv[2] == 'stage':
    boot.checked_config(app)
else:
    import run_bundle
    import seal_release
    for name in ('world-go2-sensors', 'world-go2-cloud-live'):
        (app/name).write_bytes(('unexecuted review fixture '+name).encode())
    seal_release.seal(app)
    run_bundle.checked_release(sys.argv[3], app)
    cloud = app/'world-go2-cloud-live'
    original = cloud.read_bytes()
    cloud.write_bytes(b'changed compiled helper')
    try:
        run_bundle.checked_release(sys.argv[3], app)
    except boot.BootError:
        pass
    else:
        raise AssertionError('changed native cloud helper accepted')
    cloud.write_bytes(original)
    cloud.unlink()
    try:
        run_bundle.checked_release(sys.argv[3], app)
    except FileNotFoundError:
        pass
    else:
        raise AssertionError('absent native cloud helper accepted')
`
	for _, item := range []struct{ mode, dir string }{
		{"stage", filepath.Join(o.Out, "project")},
		{"release", filepath.Join(releaseDir, "project")},
	} {
		t.Run(item.mode, func(t *testing.T) {
			cmd := exec.Command("python3", "-I", "-B", "-c", script, item.dir, item.mode, r.ReleaseID)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("packaged import or integrity check failed: %v\n%s", err, out)
			}
		})
	}
}
