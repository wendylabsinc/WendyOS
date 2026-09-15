package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func hashOrFatal(t *testing.T, dir string, args map[string]string) string {
	t.Helper()
	h, err := computeBuildInputHash(dir, "", "linux/arm64", "", args, nil)
	if err != nil {
		t.Fatalf("computeBuildInputHash: %v", err)
	}
	return h
}

func TestComputeBuildInputHash_StableAndSensitive(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "Dockerfile", "FROM python:3.11-slim\nCOPY app.py .\n")
	writeFile(t, dir, "app.py", "print('v1')\n")
	args := map[string]string{"WENDY_DEBUG": "false"}

	base := hashOrFatal(t, dir, args)

	// Identical inputs → identical hash.
	if got := hashOrFatal(t, dir, args); got != base {
		t.Fatalf("hash not stable: %s != %s", got, base)
	}

	// Changing a context file MUST change the hash (no missed change).
	writeFile(t, dir, "app.py", "print('v2')\n")
	if got := hashOrFatal(t, dir, args); got == base {
		t.Fatal("hash unchanged after editing app.py")
	}
}

func TestComputeBuildInputHash_BuildArgsAndDockerfile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "Dockerfile", "FROM python:3.11-slim\n")
	writeFile(t, dir, "app.py", "print('hi')\n")

	base := hashOrFatal(t, dir, map[string]string{"WENDY_DEBUG": "false"})

	// A different build arg value changes the hash.
	if got := hashOrFatal(t, dir, map[string]string{"WENDY_DEBUG": "true"}); got == base {
		t.Fatal("hash unchanged after build-arg change")
	}

	// A Dockerfile edit changes the hash.
	writeFile(t, dir, "Dockerfile", "FROM python:3.12-slim\n")
	if got := hashOrFatal(t, dir, map[string]string{"WENDY_DEBUG": "false"}); got == base {
		t.Fatal("hash unchanged after Dockerfile change")
	}
}

func TestDockerfileBasesContentPinned(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	tests := []struct {
		name       string
		dockerfile string
		want       bool
	}{
		{name: "scratch", dockerfile: "FROM scratch\n", want: true},
		{name: "digest pinned", dockerfile: "FROM --platform=linux/arm64 python:3.12@" + digest + " AS base\nFROM base AS app\n", want: true},
		{name: "mutable tag", dockerfile: "FROM python:3.12\n"},
		{name: "arg expanded", dockerfile: "ARG BASE=python:3.12\nFROM ${BASE}\n"},
		{name: "mixed multistage", dockerfile: "FROM alpine@" + digest + " AS build\nFROM ubuntu:24.04\n"},
		{name: "continued pinned", dockerfile: "FROM --platform=linux/arm64 \\\n alpine@" + digest + " AS base\nFROM base\n", want: true},
		{name: "no from", dockerfile: "RUN true\n"},
		{name: "split flag rejected", dockerfile: "FROM --platform linux/arm64 alpine@" + digest + "\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, dir, "Dockerfile", tt.dockerfile)
			got, err := dockerfileBasesContentPinned(dir, "")
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("dockerfileBasesContentPinned() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDockerfileBasesContentPinned_ConfinesDefaultDockerfile(t *testing.T) {
	outside := t.TempDir()
	writeFile(t, outside, "Dockerfile", "FROM scratch\n")

	dir := t.TempDir()
	if err := os.Symlink(filepath.Join(outside, "Dockerfile"), filepath.Join(dir, "Dockerfile")); err != nil {
		t.Fatal(err)
	}

	if _, err := dockerfileBasesContentPinned(dir, ""); err == nil {
		t.Fatal("expected default Dockerfile symlink outside project to be rejected")
	}
}

func TestComputeBuildInputHash_HonorsDockerignore(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "Dockerfile", "FROM python:3.11-slim\nCOPY app.py .\n")
	writeFile(t, dir, "app.py", "print('hi')\n")
	writeFile(t, dir, ".dockerignore", "*.log\nbuild/\n")
	writeFile(t, dir, "debug.log", "noise\n")
	writeFile(t, dir, "build/out.bin", "artifact\n")

	base := hashOrFatal(t, dir, nil)

	// Changing an ignored file does NOT change the hash (the optimization).
	writeFile(t, dir, "debug.log", "different noise\n")
	if got := hashOrFatal(t, dir, nil); got != base {
		t.Fatal("hash changed after editing a .dockerignore'd file")
	}

	// A file inside an ignored directory is also excluded.
	writeFile(t, dir, "build/out.bin", "rebuilt artifact\n")
	if got := hashOrFatal(t, dir, nil); got != base {
		t.Fatal("hash changed after editing a file in an ignored directory")
	}

	// A non-ignored file still flips the hash.
	writeFile(t, dir, "app.py", "print('changed')\n")
	if got := hashOrFatal(t, dir, nil); got == base {
		t.Fatal("hash unchanged after editing a non-ignored file")
	}
}

// The fingerprint must hash the build's REAL input set: when the resolved
// dockerfile carries its own <dockerfile>.dockerignore (BuildKit's
// per-Dockerfile precedence — the Stagefile flow derives a deny-all allowlist
// there), that file governs the walk, not the context's .dockerignore.
// Otherwise editing a README invalidates the fingerprint and forces a rebuild
// of an identical image.
func TestComputeBuildInputHash_PerDockerfileIgnoreAllowlist(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "Dockerfile.generated", "FROM python:3.11-slim\nCOPY main.py main.py\n")
	writeFile(t, dir, "Dockerfile.generated.dockerignore", "*\n!main.py\n!requirements.txt\n")
	writeFile(t, dir, "main.py", "print('v1')\n")
	writeFile(t, dir, "requirements.txt", "mcp\n")
	writeFile(t, dir, "README.md", "docs v1\n")

	hash := func() string {
		t.Helper()
		h, err := computeBuildInputHash(dir, "Dockerfile.generated", "linux/arm64", "", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}

	base := hash()
	writeFile(t, dir, "README.md", "docs v2\n")
	if got := hash(); got != base {
		t.Fatal("hash changed after editing a file excluded by the per-Dockerfile allowlist")
	}
	writeFile(t, dir, "main.py", "print('v2')\n")
	if got := hash(); got == base {
		t.Fatal("hash unchanged after editing an allowlisted file")
	}
}

// A directory with a re-included descendant must stay walkable: with
// "*" + "!src/app.py", the walk may not SkipDir at src/ or the allowlisted
// file's changes would be missed entirely (stale-skip, the unsafe direction).
func TestComputeBuildInputHash_NestedNegationDescends(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "Dockerfile.generated", "FROM python:3.11-slim\nCOPY src/app.py app.py\n")
	writeFile(t, dir, "Dockerfile.generated.dockerignore", "*\n!src/app.py\n")
	writeFile(t, dir, "src/app.py", "print('v1')\n")

	hash := func() string {
		t.Helper()
		h, err := computeBuildInputHash(dir, "Dockerfile.generated", "linux/arm64", "", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}

	base := hash()
	writeFile(t, dir, "src/app.py", "print('v2')\n")
	if got := hash(); got == base {
		t.Fatal("hash unchanged after editing a re-included file inside an excluded directory")
	}
}

func TestDockerIgnoreMatcher(t *testing.T) {
	di := &dockerIgnore{patterns: []string{"node_modules", "*.pyc", "dist", "secrets/key.pem"}}
	cases := []struct {
		path string
		want bool
	}{
		{"node_modules", true},
		{"node_modules/left-pad/index.js", true},
		{"app.pyc", true},
		{"pkg/mod.pyc", true}, // basename glob
		{"dist/bundle.js", true},
		{"secrets/key.pem", true},
		{"app.py", false},
		{"secrets/other.pem", false},
		{"README.md", false},
	}
	for _, c := range cases {
		if got := di.matches(c.path); got != c.want {
			t.Errorf("matches(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestDockerIgnoreNegationIsIgnored(t *testing.T) {
	// Negations re-include files; the conservative matcher ignores them, so the
	// re-included file is simply hashed (safe — never under-excludes).
	dir := t.TempDir()
	writeFile(t, dir, ".dockerignore", "*.log\n!keep.log\n")
	di := loadDockerIgnore(dir)
	if di.matches("keep.log") {
		t.Fatal("negated pattern should not exclude keep.log")
	}
	if !di.matches("other.log") {
		t.Fatal("*.log should still exclude other.log")
	}
}

// TestComputeBuildInputHash_EnvChangesHash covers WDY-2040: env is applied at
// container create, so an env-only change produces an identical image and must
// still invalidate the fingerprint that lets a redeploy skip the build.
func TestComputeBuildInputHash_EnvChangesHash(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	base, err := computeBuildInputHash(dir, "", "linux/arm64", "", nil, nil)
	if err != nil {
		t.Fatalf("computeBuildInputHash: %v", err)
	}
	withEnv, err := computeBuildInputHash(dir, "", "linux/arm64", "", nil, []string{"LOG_LEVEL=debug"})
	if err != nil {
		t.Fatalf("computeBuildInputHash: %v", err)
	}
	changed, err := computeBuildInputHash(dir, "", "linux/arm64", "", nil, []string{"LOG_LEVEL=info"})
	if err != nil {
		t.Fatalf("computeBuildInputHash: %v", err)
	}

	if base == withEnv {
		t.Error("adding env did not change the hash")
	}
	if withEnv == changed {
		t.Error("changing an env value did not change the hash")
	}
}

func TestBuildInputHashSalt(t *testing.T) {
	// The fingerprint is salted so a change to the hash inputs can invalidate
	// every recorded fingerprint by changing the salt. Assert the salt is the
	// current bare string and carries no lingering version suffix.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h, err := computeBuildInputHash(dir, "Dockerfile", "linux/arm64", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("deployfastpath.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"wendy-deploy-fingerprint\n"`) {
		t.Fatalf("deploy fingerprint salt must be %q; hash was %s", "wendy-deploy-fingerprint", h)
	}
	if strings.Contains(string(data), `"wendy-deploy-fingerprint-v`) {
		t.Fatal("a versioned deploy fingerprint salt string still lingers in deployfastpath.go")
	}
}

func TestComputeDeployDesiredHash_RuntimeChangesInvalidate(t *testing.T) {
	baseCfg := &appconfig.AppConfig{AppID: "demo", Version: "1.0.0"}
	base, err := computeDeployDesiredHash("sha256:image", baseCfg, []string{"serve"}, []string{"LOG_LEVEL=info"}, &agentpb.RestartPolicy{Mode: agentpb.RestartPolicyMode_UNLESS_STOPPED})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		cfg  *appconfig.AppConfig
		args []string
		env  []string
		mode agentpb.RestartPolicyMode
	}{
		{name: "version", cfg: &appconfig.AppConfig{AppID: "demo", Version: "2.0.0"}, args: []string{"serve"}, env: []string{"LOG_LEVEL=info"}, mode: agentpb.RestartPolicyMode_UNLESS_STOPPED},
		{name: "entitlement", cfg: &appconfig.AppConfig{AppID: "demo", Version: "1.0.0", Entitlements: []appconfig.Entitlement{{Type: "network/host"}}}, args: []string{"serve"}, env: []string{"LOG_LEVEL=info"}, mode: agentpb.RestartPolicyMode_UNLESS_STOPPED},
		{name: "arguments", cfg: baseCfg, args: []string{"worker"}, env: []string{"LOG_LEVEL=info"}, mode: agentpb.RestartPolicyMode_UNLESS_STOPPED},
		{name: "environment", cfg: baseCfg, args: []string{"serve"}, env: []string{"LOG_LEVEL=debug"}, mode: agentpb.RestartPolicyMode_UNLESS_STOPPED},
		{name: "restart policy", cfg: baseCfg, args: []string{"serve"}, env: []string{"LOG_LEVEL=info"}, mode: agentpb.RestartPolicyMode_NO},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := computeDeployDesiredHash("sha256:image", tt.cfg, tt.args, tt.env, &agentpb.RestartPolicy{Mode: tt.mode})
			if err != nil {
				t.Fatal(err)
			}
			if got == base {
				t.Fatalf("runtime change did not invalidate desired-state hash: %s", got)
			}
		})
	}
}

func TestComputeDeployDesiredHash_CLIOnlyChangesDoNotInvalidate(t *testing.T) {
	baseCfg := &appconfig.AppConfig{AppID: "demo", Version: "1.0.0"}
	changedCfg := &appconfig.AppConfig{
		AppID:     "demo",
		Version:   "1.0.0",
		Readiness: &appconfig.ReadinessConfig{TCPSocket: &appconfig.TCPSocketProbe{Port: 8080}},
		Hooks:     &appconfig.HooksConfig{PostStart: &appconfig.HookCommand{CLI: "open http://device"}},
	}
	policy := &agentpb.RestartPolicy{Mode: agentpb.RestartPolicyMode_UNLESS_STOPPED}
	base, err := computeDeployDesiredHash("sha256:image", baseCfg, nil, nil, policy)
	if err != nil {
		t.Fatal(err)
	}
	got, err := computeDeployDesiredHash("sha256:image", changedCfg, nil, nil, policy)
	if err != nil {
		t.Fatal(err)
	}
	if got != base {
		t.Fatalf("CLI-only readiness/hook change invalidated container identity: %s != %s", got, base)
	}
}
