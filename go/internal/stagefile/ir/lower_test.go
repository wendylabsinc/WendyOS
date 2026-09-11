package ir

import (
	"reflect"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/stagefile/spec"
)

func TestLowerSingleStageImageOnly(t *testing.T) {
	f := &spec.File{
		Version: 1,
		Stages:  []spec.Stage{{Name: "app", From: "debian:12"}},
	}
	g, err := Lower(f, Options{})
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	if len(g.Nodes) != 1 {
		t.Fatalf("got %d nodes, want 1", len(g.Nodes))
	}
	if g.Nodes[0].Kind != OpImage {
		t.Fatalf("got kind %q, want %q", g.Nodes[0].Kind, OpImage)
	}
	if g.Nodes[0].Image.Ref != "debian:12" {
		t.Fatalf("got ref %q, want debian:12", g.Nodes[0].Image.Ref)
	}
	if len(g.Stages) != 1 || g.Stages[0].Name != "app" || g.Stages[0].Final != 0 {
		t.Fatalf("got stages %+v, want one stage app→0", g.Stages)
	}
}

func TestLowerAptThenPipChainsInputs(t *testing.T) {
	f := &spec.File{
		Version: 1,
		Stages: []spec.Stage{{
			Name: "deps",
			From: "python:3.12-slim",
			Install: &spec.Install{
				Apt: &spec.AptInstall{Packages: []string{"build-essential"}},
				Pip: []spec.PipInstall{{Requirements: "requirements.txt"}},
			},
		}},
	}
	g, err := Lower(f, Options{})
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	if len(g.Nodes) != 6 {
		t.Fatalf("got %d nodes, want 6 (pip helper plus app stage)", len(g.Nodes))
	}
	if g.Nodes[2].Exec.Recipe != RecipePip {
		t.Fatalf("node 2 recipe = %+v, want pip", g.Nodes[2].Exec.Recipe)
	}
	if g.Nodes[4].Exec.Recipe != RecipeApt {
		t.Fatalf("node 4 recipe = %+v, want apt", g.Nodes[4].Exec.Recipe)
	}
	// Pip is deliberately independent of the app's APT chain.
	if len(g.Nodes[2].Inputs) != 1 || g.Nodes[2].Inputs[0] != 1 {
		t.Fatalf("pip inputs = %v, want [1]", g.Nodes[2].Inputs)
	}
	if g.Nodes[2].Exec.Pip.Requirements != "requirements.txt" {
		t.Fatalf("pip requirements = %q", g.Nodes[2].Exec.Pip.Requirements)
	}
	if got := g.Stages[len(g.Stages)-1].Final; got != 5 {
		t.Fatalf("stage final = %d, want 5", got)
	}
}

func TestLowerCopyFromPriorStageReferencesItsFinalNode(t *testing.T) {
	f := &spec.File{
		Version: 1,
		Stages: []spec.Stage{
			{Name: "deps", From: "python:3.12-slim",
				Install: &spec.Install{Apt: &spec.AptInstall{Packages: []string{"gcc"}}}},
			{Name: "app", From: "python:3.12-slim",
				Copy: []spec.CopyEntry{{From: "deps", Paths: []string{"/usr/lib"}}}},
		},
	}
	g, err := Lower(f, Options{})
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	depsFinal := g.Stages[0].Final
	var copyNode *Node
	for i := range g.Nodes {
		if g.Nodes[i].Kind == OpCopy {
			copyNode = &g.Nodes[i]
		}
	}
	if copyNode == nil {
		t.Fatal("no copy node lowered")
	}
	// Inputs[0] is the stage this copy lands on; Inputs[1] is the source stage.
	if len(copyNode.Inputs) != 2 || copyNode.Inputs[1] != depsFinal {
		t.Fatalf("copy inputs = %v, want [_, %d]", copyNode.Inputs, depsFinal)
	}
	if copyNode.Copy.FromLocal {
		t.Fatal("copy from a named stage must not be marked FromLocal")
	}
}

func TestLowerCopyFromLocalHasNoStageInput(t *testing.T) {
	f := &spec.File{
		Version: 1,
		Stages: []spec.Stage{{
			Name: "app", From: "debian:12",
			Copy: []spec.CopyEntry{{From: "local", Paths: []string{"app.py"}}},
		}},
	}
	g, err := Lower(f, Options{})
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	copyNode := g.Nodes[g.Stages[0].Final]
	if copyNode.Kind != OpCopy || !copyNode.Copy.FromLocal {
		t.Fatalf("final node = %+v, want a local copy", copyNode)
	}
	if len(copyNode.Inputs) != 1 {
		t.Fatalf("local copy inputs = %v, want exactly the base", copyNode.Inputs)
	}
}

// TestLowerResolvesCopyDest pins the contract stated on CopyOp: Dest that
// reaches a backend or the cache key is always final, never the raw spec
// value. Leaving it raw made `paths: [app.py]` and
// `paths: [app.py], dest: app.py` — the same build — key differently.
func TestLowerResolvesCopyDest(t *testing.T) {
	cases := []struct {
		name  string
		entry spec.CopyEntry
		want  string
	}{
		{"omitted dest defaults to the sole path", spec.CopyEntry{From: "local", Paths: []string{"app.py"}}, "app.py"},
		{"explicit dest is kept", spec.CopyEntry{From: "local", Paths: []string{"app.py"}, Dest: "/srv/app.py"}, "/srv/app.py"},
		{"multi-path dest gains a trailing slash", spec.CopyEntry{From: "local", Paths: []string{"a.txt", "b.txt"}, Dest: "/app"}, "/app/"},
		{"multi-path dest keeps its trailing slash", spec.CopyEntry{From: "local", Paths: []string{"a.txt", "b.txt"}, Dest: "/app/"}, "/app/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, err := Lower(&spec.File{Version: 1, Stages: []spec.Stage{
				{Name: "app", From: "debian:12", Copy: []spec.CopyEntry{tc.entry}},
			}}, Options{})
			if err != nil {
				t.Fatalf("Lower: %v", err)
			}
			if got := g.Nodes[g.Stages[0].Final].Copy.Dest; got != tc.want {
				t.Fatalf("dest = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestLowerDoesNotAliasSpecSlices guards against every []string field Lower
// copies onto a node payload — Apt/Apk/Pip Packages, Copy.Paths, and
// Stage.Entrypoint — sharing backing memory with the spec.File Lower reads.
// Aliasing would let a caller that mutates its spec (or re-lowers it after
// patching an install block in place, the scenario that motivated this
// test) silently corrupt an already-returned graph — and since the graph is
// what cachekey hashes, a corrupted graph now means a corrupted cache key.
func TestLowerDoesNotAliasSpecSlices(t *testing.T) {
	aptPkgs := []string{"curl"}
	apkPkgs := []string{"musl-dev"}
	pipPkgs := []string{"flask"}
	paths := []string{"app.py"}
	exec := []string{"python3", "app.py"}
	f := &spec.File{
		Version: 1,
		Stages: []spec.Stage{{
			Name: "app", From: "debian:12",
			Install: &spec.Install{
				Apt: &spec.AptInstall{Packages: aptPkgs},
				Apk: &spec.ApkInstall{Packages: apkPkgs},
				Pip: []spec.PipInstall{{Packages: pipPkgs}},
			},
			Copy:       []spec.CopyEntry{{From: "local", Paths: paths, Dest: "app.py"}},
			Entrypoint: &spec.Entrypoint{Exec: exec},
		}},
	}
	g, err := Lower(f, Options{})
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}

	wantAptPkgs := []string{"curl"}
	wantApkPkgs := []string{"musl-dev"}
	wantPipPkgs := []string{"flask"}
	wantPaths := []string{"app.py"}
	wantExec := []string{"python3", "app.py"}

	// Mutate the source spec's backing arrays after Lower returns.
	aptPkgs[0] = "corrupted"
	apkPkgs[0] = "corrupted"
	pipPkgs[0] = "corrupted"
	paths[0] = "corrupted.py"
	exec[0] = "corrupted"

	var aptNode, apkNode, pipNode *Node
	for i := range g.Nodes {
		if g.Nodes[i].Exec == nil {
			continue
		}
		switch g.Nodes[i].Exec.Recipe {
		case RecipeApt:
			aptNode = &g.Nodes[i]
		case RecipeApk:
			apkNode = &g.Nodes[i]
		case RecipePip:
			pipNode = &g.Nodes[i]
		}
	}
	if aptNode == nil || apkNode == nil || pipNode == nil {
		t.Fatalf("expected apt, apk, and pip exec nodes, got nodes %+v", g.Nodes)
	}
	if !reflect.DeepEqual(aptNode.Exec.Apt.Packages, wantAptPkgs) {
		t.Fatalf("AptParams.Packages changed after mutating the source spec: got %v, want %v", aptNode.Exec.Apt.Packages, wantAptPkgs)
	}
	if !reflect.DeepEqual(apkNode.Exec.Apk.Packages, wantApkPkgs) {
		t.Fatalf("AptParams.Packages (apk) changed after mutating the source spec: got %v, want %v", apkNode.Exec.Apk.Packages, wantApkPkgs)
	}
	if !reflect.DeepEqual(pipNode.Exec.Pip.Packages, wantPipPkgs) {
		t.Fatalf("PipParams.Packages changed after mutating the source spec: got %v, want %v", pipNode.Exec.Pip.Packages, wantPipPkgs)
	}

	gotCopy := g.Nodes[g.Stages[len(g.Stages)-1].Final].Copy
	if !reflect.DeepEqual(gotCopy.Paths, wantPaths) {
		t.Fatalf("Copy.Paths changed after mutating the source spec: got %v, want %v", gotCopy.Paths, wantPaths)
	}
	if got := g.Stages[len(g.Stages)-1].Entrypoint; !reflect.DeepEqual(got, wantExec) {
		t.Fatalf("Stage.Entrypoint changed after mutating the source spec: got %v, want %v", got, wantExec)
	}
}

// TestLowerPreservesEntrypointNilness pins the nil-ness codegen relies on
// (`st.Entrypoint != nil` decides whether an ENTRYPOINT line is emitted at
// all), across both shapes that must produce it. A stage with no entrypoint
// declared at all must lower to a nil slice, not an empty non-nil one — and
// so must a stage whose declared entrypoint.exec is itself nil, which is the
// shape that actually exercises slices.Clone's nil-preserving behavior: the
// no-entrypoint case never reaches the clone at all, since Lower only calls
// it inside `if s.Entrypoint != nil`.
func TestLowerPreservesEntrypointNilness(t *testing.T) {
	cases := []struct {
		name       string
		entrypoint *spec.Entrypoint
	}{
		{"no entrypoint declared", nil},
		{"entrypoint declared with a nil exec", &spec.Entrypoint{Exec: nil}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, err := Lower(&spec.File{Version: 1, Stages: []spec.Stage{
				{Name: "app", From: "debian:12", Entrypoint: tc.entrypoint},
			}}, Options{})
			if err != nil {
				t.Fatalf("Lower: %v", err)
			}
			if g.Stages[0].Entrypoint != nil {
				t.Fatalf("Entrypoint = %#v, want nil", g.Stages[0].Entrypoint)
			}
		})
	}
}

func TestLowerRejectsCopyWithNoPaths(t *testing.T) {
	_, err := Lower(&spec.File{Version: 1, Stages: []spec.Stage{
		{Name: "app", From: "debian:12", Copy: []spec.CopyEntry{{From: "local"}}},
	}}, Options{})
	if err == nil {
		t.Fatal("Lower accepted a copy entry with no paths")
	}
}

// TestLowerResolvesNpmManifestAndLockfile pins both npm build inputs on the
// node. The manifest is named rather than assumed by each backend, and the
// lockfile is derived from the defaulted manager so the two fields cannot
// disagree about which manager they describe.
func TestLowerResolvesNpmManifestAndLockfile(t *testing.T) {
	for _, tc := range []struct{ manager, wantManager, wantLock string }{
		{"", "npm", "package-lock.json"},
		{"npm", "npm", "package-lock.json"},
		{"yarn", "yarn", "yarn.lock"},
		{"pnpm", "pnpm", "pnpm-lock.yaml"},
	} {
		g, err := Lower(&spec.File{Version: 1, Stages: []spec.Stage{
			{Name: "app", From: "node:20-slim", Install: &spec.Install{
				Npm: &spec.NpmInstall{Manager: tc.manager},
			}},
		}}, Options{})
		if err != nil {
			t.Fatalf("Lower(%q): %v", tc.manager, err)
		}
		npm := g.Nodes[g.Stages[0].Final].Exec.Npm
		if npm.Manager != tc.wantManager {
			t.Errorf("manager %q: got %q, want %q", tc.manager, npm.Manager, tc.wantManager)
		}
		if npm.Manifest != "package.json" {
			t.Errorf("manager %q: manifest = %q, want package.json", tc.manager, npm.Manifest)
		}
		if npm.Lockfile != tc.wantLock {
			t.Errorf("manager %q: lockfile = %q, want %q", tc.manager, npm.Lockfile, tc.wantLock)
		}
	}
}

func TestLowerRejectsUnknownBuildLang(t *testing.T) {
	f := &spec.File{
		Version: 1,
		Stages: []spec.Stage{{
			Name: "app", From: "debian:12",
			Build: &spec.Build{Lang: "cobol"},
		}},
	}
	if _, err := Lower(f, Options{}); err == nil {
		t.Fatal("Lower accepted an unsupported build.lang")
	}
}
