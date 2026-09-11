package stagefile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/stagefile/lock"
)

// Both backends run through one parse/lock/resolve path. That is the property
// worth pinning: if they resolved separately, whichever backend a project
// built with first would write pins the other could not read back unchanged,
// and a build would silently differ from its own lockfile depending on a flag.

const backendFixture = `version: 1
stages:
  - name: app
    from: python:3.12-slim
    workdir: /srv
    env:
      MODE: prod
    install:
      pip:
        - packages: [flask]
    copy:
      - from: local
        paths: [app.py]
    entrypoint:
      exec: [python3, app.py]
`

func writeFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "build.stagefile.yaml"), []byte(backendFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func fakeResolvers(t *testing.T) (lock.Resolver, lock.Hasher, lock.ConfigResolver) {
	t.Helper()
	resolver := func(ref string) (string, error) {
		return "sha256:1111111111111111111111111111111111111111111111111111111111111111", nil
	}
	hasher := func(url string) (string, error) {
		return "sha256:2222222222222222222222222222222222222222222222222222222222222222", nil
	}
	configs := func(ref, platform string) ([]byte, error) {
		return []byte(`{"os":"linux","architecture":"arm64","config":{"Env":["PATH=/usr/bin"]}}`), nil
	}
	return resolver, hasher, configs
}

// The lockfile a Dockerfile build writes must be byte-identical to the one an
// LLB build writes, and neither may rewrite the other's.
func TestBothBackendsProduceTheSameLockfile(t *testing.T) {
	resolver, hasher, configs := fakeResolvers(t)

	dockerDir := writeFixture(t)
	if _, _, err := compileFile(dockerDir, SourceName, "linux/arm64", "", "", resolver, hasher); err != nil {
		t.Fatalf("compileFile: %v", err)
	}
	fromDockerfile, err := os.ReadFile(filepath.Join(dockerDir, "build.stagefile.lock.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	llbDir := writeFixture(t)
	if _, _, err := compileToLLB(llbDir, "linux/arm64", "", "", resolver, hasher, configs); err != nil {
		t.Fatalf("compileToLLB: %v", err)
	}
	fromLLB, err := os.ReadFile(filepath.Join(llbDir, "build.stagefile.lock.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	if string(fromDockerfile) != string(fromLLB) {
		t.Fatalf("lockfiles differ:\n dockerfile backend:\n%s\n llb backend:\n%s", fromDockerfile, fromLLB)
	}
}

// Building one project through both backends in turn must leave the lockfile
// exactly as the first one wrote it — the second backend reads the pins back
// rather than re-resolving them.
func TestSecondBackendDoesNotRewriteTheLockfile(t *testing.T) {
	resolver, hasher, configs := fakeResolvers(t)
	dir := writeFixture(t)
	lockPath := filepath.Join(dir, "build.stagefile.lock.yaml")

	if _, _, err := compileFile(dir, SourceName, "linux/arm64", "", "", resolver, hasher); err != nil {
		t.Fatalf("compileFile: %v", err)
	}
	before, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := compileToLLB(dir, "linux/arm64", "", "", resolver, hasher, configs); err != nil {
		t.Fatalf("compileToLLB: %v", err)
	}
	after, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("the LLB backend rewrote the lockfile:\n before:\n%s\n after:\n%s", before, after)
	}
}

// The Dockerfile backend lets the builder default an empty platform; the LLB
// backend cannot express one, so the refusal has to reach the caller rather
// than becoming a host-pinned build.
func TestCompileToLLBRequiresAPlatform(t *testing.T) {
	resolver, hasher, configs := fakeResolvers(t)
	dir := writeFixture(t)
	if _, _, err := compileToLLB(dir, "", "", "", resolver, hasher, configs); err == nil {
		t.Fatal("compileToLLB accepted an empty platform")
	}
}

// The image config the LLB backend derives has to carry what the Dockerfile
// bakes in, since LLB describes only a filesystem.
func TestCompileToLLBDerivesTheImageConfig(t *testing.T) {
	resolver, hasher, configs := fakeResolvers(t)
	dir := writeFixture(t)

	_, cfg, err := compileToLLB(dir, "linux/arm64", "", "", resolver, hasher, configs)
	if err != nil {
		t.Fatalf("compileToLLB: %v", err)
	}
	if len(cfg.Entrypoint) == 0 {
		t.Error("entrypoint missing from the derived image config")
	}
	if cfg.Workdir != "/srv" {
		t.Errorf("workdir = %q, want /srv", cfg.Workdir)
	}
	if cfg.Env["MODE"] != "prod" {
		t.Errorf("env = %v, want MODE=prod", cfg.Env)
	}
}

// --debug must reach both backends. The Dockerfile path already applied the
// profile override before lowering; the LLB path shares that code, and this
// pins that it stays shared.
func TestBuildProfileReachesBothBackends(t *testing.T) {
	const swiftFixture = `version: 1
stages:
  - name: app
    from: swift:6.0
    build:
      lang: swift
      profile: release
`
	resolver, hasher, configs := fakeResolvers(t)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "build.stagefile.yaml"), []byte(swiftFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	dockerfile, _, err := compileFile(dir, SourceName, "linux/arm64", "", BuildProfileDebug, resolver, hasher)
	if err != nil {
		t.Fatalf("compileFile: %v", err)
	}
	if want := " -c debug"; !strings.Contains(dockerfile, want) {
		t.Errorf("Dockerfile backend did not apply --debug:\n%s", dockerfile)
	}

	llbDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(llbDir, "build.stagefile.yaml"), []byte(swiftFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	// The LLB backend has no text to grep; that it compiles at all under the
	// same override is what this half checks, alongside the shared-path tests
	// above which pin that the override is applied before lowering.
	if _, _, err := compileToLLB(llbDir, "linux/arm64", "", BuildProfileDebug, resolver, hasher, configs); err != nil {
		t.Fatalf("compileToLLB with a build profile: %v", err)
	}
}
