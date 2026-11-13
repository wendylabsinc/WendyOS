package commands

import (
	"os"
	"path/filepath"
	"runtime"
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

// TestComputeBuildInputHash_FollowsASymlinkedContextRoot: BuildKit follows a
// symlinked context root (wendy run --prefix ./link, a symlinked $PWD, a
// symlinked Compose or multi-service context), but filepath.WalkDir does not
// descend into one. The hash must see the files behind the link, or a pinned
// project reached that way never sees a source edit.
func TestComputeBuildInputHash_FollowsASymlinkedContextRoot(t *testing.T) {
	useContextDigestCacheDir(t)
	parent := t.TempDir()
	real := filepath.Join(parent, "real")
	writeFile(t, real, "Dockerfile", "FROM scratch\nCOPY . /app\n")
	writeFile(t, real, "app.py", "print('v1')\n")
	link := filepath.Join(parent, "link")
	if err := os.Symlink("real", link); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}

	viaLink := hashOrFatal(t, link, nil)
	if viaReal := hashOrFatal(t, real, nil); viaReal != viaLink {
		t.Fatalf("hash via the link %s != hash via the real path %s", viaLink, viaReal)
	}
	writeFile(t, real, "app.py", "print('v2')\n")
	if got := hashOrFatal(t, link, nil); got == viaLink {
		t.Fatal("editing a file behind a symlinked context root did not change the hash")
	}
}

// TestComputeBuildInputHash_TracksSpecialModeBits: COPY keeps setuid, setgid
// and sticky along with the permission bits, so chmod u+s must change the
// hash.
func TestComputeBuildInputHash_TracksSpecialModeBits(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no setuid bit")
	}
	useContextDigestCacheDir(t)
	dir := t.TempDir()
	writeFile(t, dir, "Dockerfile", "FROM scratch\nCOPY . /app\n")
	writeFile(t, dir, "tool", "#!/bin/sh\n")
	p := filepath.Join(dir, "tool")
	if err := os.Chmod(p, 0o755); err != nil {
		t.Fatal(err)
	}
	base := hashOrFatal(t, dir, nil)
	if err := os.Chmod(p, 0o755|os.ModeSetuid); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(p); err != nil || info.Mode()&os.ModeSetuid == 0 {
		t.Skipf("cannot set the setuid bit here (%v)", err)
	}
	if got := hashOrFatal(t, dir, nil); got == base {
		t.Fatal("chmod u+s did not change the hash")
	}
}

// TestComputeBuildInputHash_TracksSymlinks: COPY copies a symlink as a
// symlink, so retargeting one changes the image even though no file changed.
func TestComputeBuildInputHash_TracksSymlinks(t *testing.T) {
	useContextDigestCacheDir(t)
	dir := t.TempDir()
	writeFile(t, dir, "Dockerfile", "FROM scratch\nCOPY . /app\n")
	writeFile(t, dir, "config/v1.yaml", "mode: a\n")
	writeFile(t, dir, "config/v2.yaml", "mode: b\n")
	link := filepath.Join(dir, "config", "current.yaml")
	if err := os.Symlink("v1.yaml", link); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	base := hashOrFatal(t, dir, nil)
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("v2.yaml", link); err != nil {
		t.Fatal(err)
	}
	if got := hashOrFatal(t, dir, nil); got == base {
		t.Fatal("retargeting a symlink in the context did not change the hash")
	}
}

// TestComputeBuildInputHash_TracksEmptyDirectories: COPY creates the empty
// directories it finds, with their mode bits, so adding one or changing its
// mode changes the image. An ignored one does not.
func TestComputeBuildInputHash_TracksEmptyDirectories(t *testing.T) {
	useContextDigestCacheDir(t)
	dir := t.TempDir()
	writeFile(t, dir, "Dockerfile", "FROM scratch\nCOPY . /app\n")
	writeFile(t, dir, ".dockerignore", "cache/\n")
	writeFile(t, dir, "app.py", "print('hi')\n")
	base := hashOrFatal(t, dir, nil)

	if err := os.Mkdir(filepath.Join(dir, "cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := hashOrFatal(t, dir, nil); got != base {
		t.Fatal("adding an ignored empty directory changed the hash")
	}
	uploads := filepath.Join(dir, "uploads")
	if err := os.Mkdir(uploads, 0o755); err != nil {
		t.Fatal(err)
	}
	withUploads := hashOrFatal(t, dir, nil)
	if withUploads == base {
		t.Fatal("adding an empty directory did not change the hash")
	}
	if runtime.GOOS == "windows" {
		return // no directory mode bits to change
	}
	if err := os.Chmod(uploads, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := hashOrFatal(t, dir, nil); got == withUploads {
		t.Fatal("chmod on a directory did not change the hash")
	}
}

// TestComputeBuildInputHash_ArgAndEnvLinesAreUnambiguous: a value holding a
// newline or an '=' must not be able to spell another set of args or env.
func TestComputeBuildInputHash_ArgAndEnvLinesAreUnambiguous(t *testing.T) {
	useContextDigestCacheDir(t)
	dir := t.TempDir()
	writeFile(t, dir, "Dockerfile", "FROM scratch\n")
	hash := func(args map[string]string, env []string) string {
		t.Helper()
		h, err := computeBuildInputHash(dir, "", "linux/arm64", "", args, env)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	for _, tc := range []struct {
		name         string
		args1, args2 map[string]string
	}{
		{"newline in a value", map[string]string{"A": "1\narg B=2"}, map[string]string{"A": "1", "B": "2"}},
		{"'=' in a key", map[string]string{"A=1": "2"}, map[string]string{"A": "1=2"}},
	} {
		if hash(tc.args1, nil) == hash(tc.args2, nil) {
			t.Errorf("build args %q and %q hash the same (%s)", tc.args1, tc.args2, tc.name)
		}
	}
	if hash(nil, []string{"A=1\nenv B=2"}) == hash(nil, []string{"A=1", "B=2"}) {
		t.Error(`env ["A=1\nenv B=2"] and ["A=1", "B=2"] hash the same`)
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
	// every recorded fingerprint by changing the salt. v3 (WDY-3216) marks the
	// switch to one line per context entry (files with mode bits and content
	// digests, directories, symlinks) and quoted args and env. Earlier salts
	// (v1, v2, then the bare string) must never come back: a fingerprint
	// recorded under one of them could match again.
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
	if !strings.Contains(string(data), `"wendy-deploy-fingerprint-v3\n"`) {
		t.Fatalf("deploy fingerprint salt must be %q; hash was %s", "wendy-deploy-fingerprint-v3", h)
	}
	for _, retired := range []string{`"wendy-deploy-fingerprint\n"`, `"wendy-deploy-fingerprint-v1\n"`, `"wendy-deploy-fingerprint-v2\n"`} {
		if strings.Contains(string(data), retired) {
			t.Fatalf("retired salt %s is back in deployfastpath.go", retired)
		}
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
