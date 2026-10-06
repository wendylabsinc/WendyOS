package commands

import (
	"errors"
	"sync"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/internal/stagefile"
)

// forbidBuildInputHashing makes any build-context hash fail the test, proving
// a caller decided against hashing before reading a single context file.
func forbidBuildInputHashing(t *testing.T) {
	t.Helper()
	orig := buildInputHasher
	t.Cleanup(func() { buildInputHasher = orig })
	buildInputHasher = func(cwd, _, _, _ string, _ map[string]string, _ []string) (string, error) {
		t.Errorf("hashed the build context of %s; its bases are not pinned", cwd)
		return "", errors.New("hashing forbidden")
	}
}

// TestPinnedBuildInputHashSkipsUnpinnedContext is WDY-3216 §1: a mutable FROM
// tag can never skip a build, so its (possibly multi-GB) context is not read.
func TestPinnedBuildInputHashSkipsUnpinnedContext(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "Dockerfile", "FROM python:3.12-slim\nCOPY app.py .\n")
	writeFile(t, dir, "app.py", "print('hi')\n")
	forbidBuildInputHashing(t)

	hash, pinned, err := pinnedBuildInputHash(dir, "", "linux/arm64", "", nil, nil)
	if err != nil || pinned || hash != "" {
		t.Fatalf("got (%q, %v, %v), want (\"\", false, nil)", hash, pinned, err)
	}
}

func TestPinnedBuildInputHashMatchesComputeBuildInputHash(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "Dockerfile", "FROM scratch\nCOPY app.py .\n")
	writeFile(t, dir, "app.py", "print('hi')\n")
	args := map[string]string{"WENDY_DEBUG": "false"}
	env := []string{"MODE=prod"}

	want, err := computeBuildInputHash(dir, "", "linux/arm64", "llb", args, env)
	if err != nil {
		t.Fatal(err)
	}
	hash, pinned, err := pinnedBuildInputHash(dir, "", "linux/arm64", "llb", args, env)
	if err != nil || !pinned || hash != want {
		t.Fatalf("got (%q, %v, %v), want (%q, true, nil)", hash, pinned, err, want)
	}
}

func TestPinnedBuildInputHashReportsAMissingDockerfile(t *testing.T) {
	forbidBuildInputHashing(t)
	if _, _, err := pinnedBuildInputHash(t.TempDir(), "", "linux/arm64", "", nil, nil); err == nil {
		t.Fatal("a missing Dockerfile returned no error")
	}
}

func TestSingleServiceDesiredHash(t *testing.T) {
	appCfg := &appconfig.AppConfig{AppID: "demo", Version: "1.0.0"}
	opts := runOptions{userArgs: []string{"serve"}}

	unpinned := t.TempDir()
	writeFile(t, unpinned, "Dockerfile", "FROM python:3.12-slim\n")
	forbidBuildInputHashing(t)
	if _, err := singleServiceDesiredHash(unpinned, "", "linux/arm64", "", nil, nil, appCfg, opts); !errors.Is(err, errBasesNotPinned) {
		t.Fatalf("unpinned error = %v, want errBasesNotPinned", err)
	}

	buildInputHasher = computeBuildInputHash
	pinned := t.TempDir()
	writeFile(t, pinned, "Dockerfile", "FROM scratch\n")
	env := []string{"MODE=prod"}
	inputHash, err := computeBuildInputHash(pinned, "", "linux/arm64", "", nil, env)
	if err != nil {
		t.Fatal(err)
	}
	want, err := computeDeployDesiredHash(inputHash, appCfg, opts.userArgs, env, resolveRestartPolicy(opts))
	if err != nil {
		t.Fatal(err)
	}
	got, err := singleServiceDesiredHash(pinned, "", "linux/arm64", "", nil, env, appCfg, opts)
	if err != nil || got != want {
		t.Fatalf("pinned = (%q, %v), want %q", got, err, want)
	}
}

// TestComputeServicePlansSkipsHashingUnpinnedServices: an unpinned service
// keeps its plan, so the build reuses the resolved (for a Stagefile, compiled)
// build file, but its context is never hashed and it can never be skipped.
func TestComputeServicePlansSkipsHashingUnpinnedServices(t *testing.T) {
	root, services := newServiceTree(t, 2)
	writeFile(t, root, "svc01/Dockerfile", "FROM python:3.12-slim\n")

	orig := planResolveDockerfile
	defer func() { planResolveDockerfile = orig }()
	planResolveDockerfile = func(string, string, bool, string, ...stagefile.Option) (string, error) {
		return "Dockerfile", nil
	}
	origHasher := buildInputHasher
	defer func() { buildInputHasher = origHasher }()
	var (
		mu     sync.Mutex
		hashed []string
	)
	buildInputHasher = func(cwd, dockerfile, platform, backend string, args map[string]string, env []string) (string, error) {
		mu.Lock()
		hashed = append(hashed, cwd)
		mu.Unlock()
		return computeBuildInputHash(cwd, dockerfile, platform, backend, args, env)
	}

	plans := computeServicePlans(root, "linux/arm64", "", "", nil, services, nil)

	if p, ok := plans["svc00"]; !ok || !p.contentPinned || p.inputHash == "" {
		t.Fatalf("pinned svc00 plan = %+v (ok=%v), want a hash", p, ok)
	}
	p, ok := plans["svc01"]
	if !ok {
		t.Fatal("unpinned svc01 lost its plan; the build would resolve its build file again")
	}
	if p.dockerfile != "Dockerfile" || p.contentPinned || p.inputHash != "" {
		t.Fatalf("unpinned svc01 plan = %+v, want the dockerfile only", p)
	}
	if len(hashed) != 1 {
		t.Fatalf("hashed %d contexts (%v), want only svc00's", len(hashed), hashed)
	}
}
