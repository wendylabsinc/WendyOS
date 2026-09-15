package llbgen

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/stagefile/ir"
	"github.com/wendylabsinc/wendy/go/internal/stagefile/spec"
)

// configWithUserShell builds a base image config carrying a non-root User and a
// custom SHELL, the two fields the Dockerfile frontend inherits that
// WithImageConfig does not.
func configWithUserShell(t *testing.T, osName, arch, user string, shell []string) []byte {
	t.Helper()
	inner := map[string]any{"Env": []string{"PATH=/usr/local/bin:/usr/bin:/bin"}}
	if user != "" {
		inner["User"] = user
	}
	if shell != nil {
		inner["Shell"] = shell
	}
	cfg, err := json.Marshal(map[string]any{
		"os": osName, "architecture": arch, "config": inner,
	})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// A final stage built `from: <prior stage>` inherits the root external image's
// config: FinalBaseConfig must walk the FromStage chain rather than look up the
// stage name (which nothing resolves), and Emit must accumulate the whole
// chain's ENV/WORKDIR so the exported image keeps every stage's contribution.
func TestFinalStageFromPriorStageInheritsRootConfig(t *testing.T) {
	f := &spec.File{Version: 1, Stages: []spec.Stage{
		{Name: "build", From: "debian:12", Workdir: "/build", Env: map[string]string{"A": "1"}},
		{Name: "app", From: "build", Env: map[string]string{"B": "2"}, Cmd: []string{"/app"}},
	}}
	g := lower(t, f, ir.Options{})
	rootCfg := testConfig(t, "linux", "arm64")
	configs := map[string][]byte{"debian:12": rootCfg}
	images := map[string]string{"debian:12": "sha256:" + strings.Repeat("d", 64)}

	base, err := FinalBaseConfig(g, configs)
	if err != nil {
		t.Fatalf("FinalBaseConfig: %v", err)
	}
	if !bytes.Equal(base, rootCfg) {
		t.Fatalf("FinalBaseConfig returned %s, want the root debian:12 config", base)
	}

	_, cfg, err := Emit(g, Options{Images: images, Configs: configs, Platform: testPlatform})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if cfg.Env["A"] != "1" || cfg.Env["B"] != "2" {
		t.Fatalf("final env = %v, want A=1 (build stage) and B=2 (app stage) accumulated", cfg.Env)
	}
	if cfg.Workdir != "/build" {
		t.Fatalf("final workdir = %q, want /build inherited from the build stage", cfg.Workdir)
	}
}

// A later stage in a FromStage chain overrides an earlier one's ENV/WORKDIR,
// matching how a Dockerfile's derived stage inherits then overrides.
func TestFromStageChainLaterStageOverrides(t *testing.T) {
	f := &spec.File{Version: 1, Stages: []spec.Stage{
		{Name: "build", From: "debian:12", Workdir: "/build", Env: map[string]string{"A": "1", "SHARED": "old"}},
		{Name: "app", From: "build", Workdir: "/app", Env: map[string]string{"SHARED": "new"}, Cmd: []string{"/app"}},
	}}
	g := lower(t, f, ir.Options{})
	configs := map[string][]byte{"debian:12": testConfig(t, "linux", "arm64")}
	images := map[string]string{"debian:12": "sha256:" + strings.Repeat("d", 64)}

	_, cfg, err := Emit(g, Options{Images: images, Configs: configs, Platform: testPlatform})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if cfg.Env["SHARED"] != "new" {
		t.Fatalf("SHARED = %q, want the app stage's value to override the build stage's", cfg.Env["SHARED"])
	}
	if cfg.Workdir != "/app" {
		t.Fatalf("workdir = %q, want /app (app stage overrides build stage)", cfg.Workdir)
	}
}

// A base image whose config ends on a non-root user must run the stage's build
// steps as that user (matching the Dockerfile frontend), and under the base's
// SHELL when it declares one — not silently as root under /bin/sh.
func TestBaseImageUserAndShellApplyToRunSteps(t *testing.T) {
	f := &spec.File{Version: 1, Stages: []spec.Stage{
		{Name: "app", From: "grafana/grafana:10",
			Install: &spec.Install{Apt: &spec.AptInstall{Packages: []string{"curl"}}}},
	}}
	g := lower(t, f, ir.Options{})
	configs := map[string][]byte{
		"grafana/grafana:10": configWithUserShell(t, "linux", "arm64", "472", []string{"/bin/bash", "-c"}),
	}
	images := map[string]string{"grafana/grafana:10": "sha256:" + strings.Repeat("e", 64)}

	def, _, err := Emit(g, Options{Images: images, Configs: configs, Platform: testPlatform})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	ops := emitOps(t, def)
	execs := 0
	for _, op := range ops {
		e := op.GetExec()
		if e == nil {
			continue
		}
		execs++
		if e.Meta.User != "472" {
			t.Fatalf("run op user = %q, want 472 inherited from the base config", e.Meta.User)
		}
		if len(e.Meta.Args) < 2 || e.Meta.Args[0] != "/bin/bash" || e.Meta.Args[1] != "-c" {
			t.Fatalf("run op args = %v, want the base SHELL /bin/bash -c", e.Meta.Args)
		}
	}
	if execs == 0 {
		t.Fatal("no exec op found — the fixture produced no RUN steps")
	}
}

// With no base SHELL and a root base, run steps keep the /bin/sh -c default and
// run as root: the fix must not disturb the common case.
func TestBaseImageDefaultsShellAndRootUser(t *testing.T) {
	f := &spec.File{Version: 1, Stages: []spec.Stage{
		{Name: "app", From: "debian:12",
			Install: &spec.Install{Apt: &spec.AptInstall{Packages: []string{"curl"}}}},
	}}
	g := lower(t, f, ir.Options{})
	configs := map[string][]byte{"debian:12": testConfig(t, "linux", "arm64")}
	images := map[string]string{"debian:12": "sha256:" + strings.Repeat("d", 64)}

	def, _, err := Emit(g, Options{Images: images, Configs: configs, Platform: testPlatform})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	for _, op := range emitOps(t, def) {
		e := op.GetExec()
		if e == nil {
			continue
		}
		if e.Meta.User != "" {
			t.Fatalf("run op user = %q, want empty (root) for a base with no configured user", e.Meta.User)
		}
		if len(e.Meta.Args) < 2 || e.Meta.Args[0] != "/bin/sh" || e.Meta.Args[1] != "-c" {
			t.Fatalf("run op args = %v, want the default /bin/sh -c", e.Meta.Args)
		}
	}
}

// The local-source unique ID is derived from the context directory, so two
// projects compile to definitions with different local sources (no cross-project
// context sharing) while one project stays byte-stable across compiles.
func TestLocalUniqueIDIsPerContext(t *testing.T) {
	if a, b := localUniqueIDFor("/projects/one"), localUniqueIDFor("/projects/two"); a == b {
		t.Fatalf("distinct context dirs produced the same unique ID %q", a)
	}
	if a, b := localUniqueIDFor("/projects/one"), localUniqueIDFor("/projects/one"); a != b {
		t.Fatalf("same context dir produced different unique IDs %q and %q", a, b)
	}
	if got := localUniqueIDFor(""); got != localUniqueID {
		t.Fatalf("empty context dir = %q, want the constant fallback %q", got, localUniqueID)
	}
}
