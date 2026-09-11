package cachekey

import (
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/stagefile/ir"
	"github.com/wendylabsinc/wendy/go/internal/stagefile/spec"
)

func mustLower(t *testing.T, f *spec.File) *ir.Graph {
	t.Helper()
	g, err := ir.Lower(f, ir.Options{})
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	return g
}

func pipStage(name, from, reqs string, aptPkgs []string) spec.Stage {
	s := spec.Stage{Name: name, From: from, Install: &spec.Install{
		Pip: []spec.PipInstall{{Requirements: reqs}},
	}}
	if aptPkgs != nil {
		s.Install.Apt = &spec.AptInstall{Packages: aptPkgs}
	}
	return s
}

func testInputs() Inputs {
	return Inputs{
		Images: map[string]string{"python:3.12-slim": "sha256:aaa", "debian:12": "sha256:bbb"},
		Files:  map[string]string{"requirements.txt": "sha256:reqs1"},
	}
}

// The core property: the same pip install on the same base yields the same
// key across two unrelated Stagefiles.
func TestKeyIsStableAcrossUnrelatedFiles(t *testing.T) {
	a := mustLower(t, &spec.File{Version: 1, Stages: []spec.Stage{
		pipStage("deps", "python:3.12-slim", "requirements.txt", nil),
	}})
	b := mustLower(t, &spec.File{Version: 1, Stages: []spec.Stage{
		{Name: "unrelated", From: "debian:12"},
		pipStage("otherName", "python:3.12-slim", "requirements.txt", nil),
	}})

	ka, err := Key(a, a.Stages[0].Final, testInputs())
	if err != nil {
		t.Fatalf("Key(a): %v", err)
	}
	kb, err := Key(b, b.Stages[1].Final, testInputs())
	if err != nil {
		t.Fatalf("Key(b): %v", err)
	}
	if ka != kb {
		t.Fatalf("keys differ across unrelated files:\n a=%s\n b=%s", ka, kb)
	}
	if !strings.HasPrefix(ka, "sha256:") {
		t.Fatalf("key %q lacks sha256: prefix", ka)
	}
}

// The soundness property: a different dependency closure must key
// differently, even with identical pip params.
func TestKeyDiffersWhenClosureDiffers(t *testing.T) {
	a := mustLower(t, &spec.File{Version: 1, Stages: []spec.Stage{
		pipStage("deps", "python:3.12-slim", "requirements.txt", []string{"foo"}),
	}})
	b := mustLower(t, &spec.File{Version: 1, Stages: []spec.Stage{
		pipStage("deps", "python:3.12-slim", "requirements.txt", []string{"bar"}),
	}})

	ka, _ := Key(a, a.Stages[0].Final, testInputs())
	kb, _ := Key(b, b.Stages[0].Final, testInputs())
	if ka == kb {
		t.Fatal("pip keyed identically over different apt closures — cache is unsound")
	}
}

func TestKeyDiffersWhenInputFileChanges(t *testing.T) {
	g := mustLower(t, &spec.File{Version: 1, Stages: []spec.Stage{
		pipStage("deps", "python:3.12-slim", "requirements.txt", nil),
	}})

	in1 := testInputs()
	k1, _ := Key(g, g.Stages[0].Final, in1)

	in2 := testInputs()
	in2.Files["requirements.txt"] = "sha256:reqs2"
	k2, _ := Key(g, g.Stages[0].Final, in2)

	if k1 == k2 {
		t.Fatal("key ignored requirements.txt content")
	}
}

func TestKeyDiffersWhenBaseDigestChanges(t *testing.T) {
	g := mustLower(t, &spec.File{Version: 1, Stages: []spec.Stage{
		pipStage("deps", "python:3.12-slim", "requirements.txt", nil),
	}})

	k1, _ := Key(g, g.Stages[0].Final, testInputs())
	in2 := testInputs()
	in2.Images["python:3.12-slim"] = "sha256:ccc"
	k2, _ := Key(g, g.Stages[0].Final, in2)

	if k1 == k2 {
		t.Fatal("key ignored the base image digest")
	}
}

func TestKeyErrorsOnMissingImageDigest(t *testing.T) {
	g := mustLower(t, &spec.File{Version: 1, Stages: []spec.Stage{
		{Name: "app", From: "alpine:3.20"},
	}})
	if _, err := Key(g, 0, testInputs()); err == nil {
		t.Fatal("Key succeeded with no resolved digest for alpine:3.20")
	}
}

func TestKeyErrorsOnMissingFileDigest(t *testing.T) {
	g := mustLower(t, &spec.File{Version: 1, Stages: []spec.Stage{
		pipStage("deps", "python:3.12-slim", "absent.txt", nil),
	}})
	if _, err := Key(g, g.Stages[0].Final, testInputs()); err == nil {
		t.Fatal("Key succeeded with no digest for absent.txt")
	}
}

// Entrypoint and user are image config, not filesystem content; changing
// them must not invalidate a built node.
func TestKeyIgnoresEntrypointAndUser(t *testing.T) {
	base := pipStage("deps", "python:3.12-slim", "requirements.txt", nil)
	a := mustLower(t, &spec.File{Version: 1, Stages: []spec.Stage{base}})

	withCfg := base
	withCfg.Entrypoint = &spec.Entrypoint{Exec: []string{"python3", "x.py"}}
	withCfg.User = "1000"
	b := mustLower(t, &spec.File{Version: 1, Stages: []spec.Stage{withCfg}})

	ka, _ := Key(a, a.Stages[0].Final, testInputs())
	kb, _ := Key(b, b.Stages[0].Final, testInputs())
	if ka != kb {
		t.Fatal("entrypoint/user changed the filesystem key")
	}
}

func copyStage(name, from, srcStage string, paths []string, dest string) spec.Stage {
	return spec.Stage{Name: name, From: from, Copy: []spec.CopyEntry{
		{From: srcStage, Paths: paths, Dest: dest},
	}}
}

// TestKeyChangesWithCrossStageCopy covers the OpCopy branch for a
// stage-to-stage copy (FromLocal == false), which no existing test
// exercises: it must react both to its own declared paths and to the
// content produced by the stage it copies from, and it must do so via the
// source stage's key, never its index.
func TestKeyChangesWithCrossStageCopy(t *testing.T) {
	build := func(basePkgs []string, copyPaths []string) *ir.Graph {
		return mustLower(t, &spec.File{Version: 1, Stages: []spec.Stage{
			{Name: "base", From: "debian:12", Install: &spec.Install{
				Apt: &spec.AptInstall{Packages: basePkgs},
			}},
			copyStage("app", "debian:12", "base", copyPaths, "dest.txt"),
		}})
	}
	in := testInputs()

	g1 := build([]string{"curl"}, []string{"a.txt"})
	k1, err := Key(g1, g1.Stages[1].Final, in)
	if err != nil {
		t.Fatalf("Key: %v", err)
	}

	// Different declared source paths must change the key even though a
	// cross-stage copy looks up no per-file digest (that lookup only
	// applies to FromLocal copies).
	g2 := build([]string{"curl"}, []string{"b.txt"})
	k2, err := Key(g2, g2.Stages[1].Final, in)
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	if k1 == k2 {
		t.Fatal("changing the copy's source paths did not change the key")
	}

	// Changing the source stage's own content must change the copy's key,
	// because a dependency is folded in by its key, not its index.
	g3 := build([]string{"wget"}, []string{"a.txt"})
	k3, err := Key(g3, g3.Stages[1].Final, in)
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	if k1 == k3 {
		t.Fatal("changing the source stage's content did not change the dependent copy's key")
	}
}

// TestKeyConvergesOnEquivalentCopyDests pins the payoff of resolving
// CopyOp.Dest at lower time: two spellings of the same copy are one build
// and must be one cache entry. An omitted dest means "the path itself", and
// BuildKit requires a multi-source dest to end in "/", so "/app" and "/app/"
// describe the same destination.
func TestKeyConvergesOnEquivalentCopyDests(t *testing.T) {
	in := testInputs()
	in.Files["app.py"] = "sha256:app"
	in.Files["b.py"] = "sha256:b"

	keyOf := func(t *testing.T, entry spec.CopyEntry) string {
		t.Helper()
		g := mustLower(t, &spec.File{Version: 1, Stages: []spec.Stage{
			{Name: "app", From: "python:3.12-slim", Copy: []spec.CopyEntry{entry}},
		}})
		k, err := Key(g, g.Stages[0].Final, in)
		if err != nil {
			t.Fatalf("Key: %v", err)
		}
		return k
	}

	implicit := keyOf(t, spec.CopyEntry{From: "local", Paths: []string{"app.py"}})
	explicit := keyOf(t, spec.CopyEntry{From: "local", Paths: []string{"app.py"}, Dest: "app.py"})
	if implicit != explicit {
		t.Error("an omitted dest and the equivalent explicit dest keyed differently")
	}

	noSlash := keyOf(t, spec.CopyEntry{From: "local", Paths: []string{"app.py", "b.py"}, Dest: "/app"})
	slash := keyOf(t, spec.CopyEntry{From: "local", Paths: []string{"app.py", "b.py"}, Dest: "/app/"})
	if noSlash != slash {
		t.Error("a multi-path dest keyed differently with and without its required trailing slash")
	}

	// The normalization must not flatten genuinely different destinations.
	if other := keyOf(t, spec.CopyEntry{From: "local", Paths: []string{"app.py"}, Dest: "/srv/app.py"}); other == implicit {
		t.Error("two different destinations collapsed onto one key")
	}
}

func npmStage(name, from, manager string) spec.Stage {
	return spec.Stage{Name: name, From: from, Install: &spec.Install{
		Npm: &spec.NpmInstall{Manager: manager},
	}}
}

// TestKeyDiffersForNpmManagerAndLockfile covers the Npm branch of
// writeExec, which no existing test exercises: both the resolved manager
// name and the lockfile's content digest must participate in the key.
func TestKeyDiffersForNpmManagerAndLockfile(t *testing.T) {
	inputsFor := func(manager, lockDigest string) Inputs {
		in := testInputs()
		in.Files["package.json"] = "sha256:pkgjson1"
		in.Files[spec.NpmLockfile(manager)] = lockDigest
		return in
	}

	gNpm := mustLower(t, &spec.File{Version: 1, Stages: []spec.Stage{
		npmStage("deps", "python:3.12-slim", "npm"),
	}})
	gPnpm := mustLower(t, &spec.File{Version: 1, Stages: []spec.Stage{
		npmStage("deps", "python:3.12-slim", "pnpm"),
	}})

	kNpm, err := Key(gNpm, gNpm.Stages[0].Final, inputsFor("npm", "sha256:lock1"))
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	kPnpm, err := Key(gPnpm, gPnpm.Stages[0].Final, inputsFor("pnpm", "sha256:lock1"))
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	if kNpm == kPnpm {
		t.Fatal("switching npm manager (npm -> pnpm) did not change the key")
	}

	kNpmOtherLock, err := Key(gNpm, gNpm.Stages[0].Final, inputsFor("npm", "sha256:lock2"))
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	if kNpm == kNpmOtherLock {
		t.Fatal("changing the lockfile digest did not change the key")
	}
}

// TestKeyDiffersForNpmManifest is the regression test for a key that
// covered the lockfile but not package.json. The install reads both — and
// codegen COPYs both — so editing scripts, engines, or a dependency range
// without regenerating the lockfile changes the built filesystem. A key
// blind to that serves a stale rootfs from a shared cache.
func TestKeyDiffersForNpmManifest(t *testing.T) {
	g := mustLower(t, &spec.File{Version: 1, Stages: []spec.Stage{
		npmStage("deps", "python:3.12-slim", "npm"),
	}})

	inputs := func(manifestDigest string) Inputs {
		in := testInputs()
		in.Files["package.json"] = manifestDigest
		in.Files["package-lock.json"] = "sha256:lock1"
		return in
	}

	k1, err := Key(g, g.Stages[0].Final, inputs("sha256:pkgjson1"))
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	k2, err := Key(g, g.Stages[0].Final, inputs("sha256:pkgjson2"))
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	if k1 == k2 {
		t.Fatal("editing package.json did not change the key — a stale rootfs would be served")
	}
}

// A manifest digest the caller forgot to supply must be an error, never a
// silently-hashed empty string: the latter collides across every possible
// package.json.
func TestKeyErrorsOnMissingNpmManifestDigest(t *testing.T) {
	g := mustLower(t, &spec.File{Version: 1, Stages: []spec.Stage{
		npmStage("deps", "python:3.12-slim", "npm"),
	}})
	in := testInputs()
	in.Files["package-lock.json"] = "sha256:lock1"

	if _, err := Key(g, g.Stages[0].Final, in); err == nil {
		t.Fatal("Key succeeded with no digest for package.json")
	}
}

// TestKeyDiffersByPlatform guards against cross-architecture poisoning of a
// shared cache. A multi-arch base image resolves to one index digest for
// every architecture, so nothing else in the key distinguishes an arm64
// build from an amd64 one.
func TestKeyDiffersByPlatform(t *testing.T) {
	g := mustLower(t, &spec.File{Version: 1, Stages: []spec.Stage{
		pipStage("deps", "python:3.12-slim", "requirements.txt", nil),
	}})

	arm := testInputs()
	arm.Platform = "linux/arm64"
	amd := testInputs()
	amd.Platform = "linux/amd64"

	kArm, err := Key(g, g.Stages[0].Final, arm)
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	kAmd, err := Key(g, g.Stages[0].Final, amd)
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	if kArm == kAmd {
		t.Fatal("arm64 and amd64 builds of the same Stagefile share a key — a shared cache would serve the wrong architecture's rootfs")
	}

	// An unset platform is a legitimate caller state (nothing has been
	// resolved yet) and must still key deterministically rather than
	// varying run to run.
	empty1, err := Key(g, g.Stages[0].Final, testInputs())
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	empty2, err := Key(g, g.Stages[0].Final, testInputs())
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	if empty1 != empty2 {
		t.Fatal("an empty Platform keyed non-deterministically")
	}
	if empty1 == kArm {
		t.Fatal("an empty Platform keyed the same as linux/arm64")
	}
}

func buildStage(name, from, lang, profile string) spec.Stage {
	return spec.Stage{Name: name, From: from, Build: &spec.Build{Lang: lang, Profile: profile}}
}

// TestKeyDiffersForBuildLangAndProfile covers the Build branch of
// writeExec, which no existing test exercises: both Lang and Profile must
// participate in the key, since a debug build is a different artifact from
// a release build of the same language.
func TestKeyDiffersForBuildLangAndProfile(t *testing.T) {
	in := testInputs()

	gGoRelease := mustLower(t, &spec.File{Version: 1, Stages: []spec.Stage{
		buildStage("app", "debian:12", "go", "release"),
	}})
	gRustRelease := mustLower(t, &spec.File{Version: 1, Stages: []spec.Stage{
		buildStage("app", "debian:12", "rust", "release"),
	}})
	gGoDebug := mustLower(t, &spec.File{Version: 1, Stages: []spec.Stage{
		buildStage("app", "debian:12", "go", "debug"),
	}})

	kGoRelease, err := Key(gGoRelease, gGoRelease.Stages[0].Final, in)
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	kRustRelease, err := Key(gRustRelease, gRustRelease.Stages[0].Final, in)
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	if kGoRelease == kRustRelease {
		t.Fatal("changing build.lang did not change the key")
	}

	kGoDebug, err := Key(gGoDebug, gGoDebug.Stages[0].Final, in)
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	if kGoRelease == kGoDebug {
		t.Fatal("changing build.profile did not change the key")
	}
}

// TestKeyErrorsOnCycle guards the fix for hand-built graphs: Lower never
// produces a cycle, but Key is exported and must return an error rather
// than recursing forever when a caller wires one up directly. The call is
// bounded by a timeout so a regression back to unbounded recursion fails
// the test instead of hanging the run (a real stack overflow would abort
// the process outright, but a slower infinite loop should still be caught
// deterministically rather than left to the CI job timeout).
func TestKeyErrorsOnCycle(t *testing.T) {
	g := &ir.Graph{Nodes: []ir.Node{
		{Kind: ir.OpImage, Inputs: []int{1}, Image: &ir.ImageOp{Ref: "a"}},
		{Kind: ir.OpImage, Inputs: []int{0}, Image: &ir.ImageOp{Ref: "b"}},
	}}

	type result struct {
		err error
	}
	done := make(chan result, 1)
	go func() {
		_, err := Key(g, 0, testInputs())
		done <- result{err: err}
	}()

	select {
	case r := <-done:
		if r.err == nil {
			t.Fatal("Key succeeded on a graph with a dependency cycle")
		}
		// The error must name the actual route back to the repeated node
		// (e.g. "0 -> 1 -> 0"), not just the index where the cycle closed,
		// so a caller debugging a hand-built graph can see the whole loop.
		if !strings.Contains(r.err.Error(), "0 -> 1 -> 0") {
			t.Fatalf("cycle error %q does not name the cycle path", r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Key did not return on a cyclic graph (unbounded recursion)")
	}
}

// TestKeyErrorsOnCycleReportsLongerPath covers a cycle that closes several
// nodes deep rather than immediately, e.g. "2 -> 4 -> 2", to guard against a
// path implementation that only ever reports the last two nodes.
func TestKeyErrorsOnCycleReportsLongerPath(t *testing.T) {
	g := &ir.Graph{Nodes: []ir.Node{
		{Kind: ir.OpImage, Image: &ir.ImageOp{Ref: "a"}},
		{Kind: ir.OpImage, Image: &ir.ImageOp{Ref: "b"}},
		{Kind: ir.OpImage, Inputs: []int{4}, Image: &ir.ImageOp{Ref: "c"}},
		{Kind: ir.OpImage, Image: &ir.ImageOp{Ref: "d"}},
		{Kind: ir.OpImage, Inputs: []int{2}, Image: &ir.ImageOp{Ref: "e"}},
	}}

	_, err := Key(g, 2, testInputs())
	if err == nil {
		t.Fatal("Key succeeded on a graph with a dependency cycle")
	}
	if !strings.Contains(err.Error(), "2 -> 4 -> 2") {
		t.Fatalf("cycle error %q does not name the cycle path", err)
	}
}

// TestKeyMemoizesSharedAncestorCorrectly covers a diamond-shaped closure:
// two nodes (1 and 2) share a common ancestor (0), and a fourth node (3)
// depends on both. Within one Key call, node 0's key is computed once and
// reused for both branches. That reuse must not paper over the ancestor's
// actual contribution — changing it must still change every key downstream
// of it, on both branches and at the join.
func TestKeyMemoizesSharedAncestorCorrectly(t *testing.T) {
	build := func() *ir.Graph {
		return &ir.Graph{Nodes: []ir.Node{
			{Kind: ir.OpImage, Image: &ir.ImageOp{Ref: "debian:12"}},
			{Kind: ir.OpExec, Inputs: []int{0}, Exec: &ir.ExecOp{
				Recipe: ir.RecipeApt, Apt: &ir.AptParams{Packages: []string{"curl"}},
			}},
			{Kind: ir.OpExec, Inputs: []int{0}, Exec: &ir.ExecOp{
				Recipe: ir.RecipeApt, Apt: &ir.AptParams{Packages: []string{"git"}},
			}},
			{Kind: ir.OpExec, Inputs: []int{1, 2}, Exec: &ir.ExecOp{
				Recipe: ir.RecipeBuild, Build: &ir.BuildParams{Lang: "go", Profile: "release"},
			}},
		}}
	}

	in1 := Inputs{Images: map[string]string{"debian:12": "sha256:aaa"}}
	in2 := Inputs{Images: map[string]string{"debian:12": "sha256:bbb"}}

	g1, g2 := build(), build()

	k1Branch1, err := Key(g1, 1, in1)
	if err != nil {
		t.Fatalf("Key(branch1, in1): %v", err)
	}
	k1Branch2, err := Key(g1, 2, in1)
	if err != nil {
		t.Fatalf("Key(branch2, in1): %v", err)
	}
	k1Join, err := Key(g1, 3, in1)
	if err != nil {
		t.Fatalf("Key(join, in1): %v", err)
	}

	k2Branch1, err := Key(g2, 1, in2)
	if err != nil {
		t.Fatalf("Key(branch1, in2): %v", err)
	}
	k2Branch2, err := Key(g2, 2, in2)
	if err != nil {
		t.Fatalf("Key(branch2, in2): %v", err)
	}
	k2Join, err := Key(g2, 3, in2)
	if err != nil {
		t.Fatalf("Key(join, in2): %v", err)
	}

	if k1Branch1 == k2Branch1 {
		t.Fatal("changing the shared ancestor's digest did not change branch 1's key")
	}
	if k1Branch2 == k2Branch2 {
		t.Fatal("changing the shared ancestor's digest did not change branch 2's key")
	}
	// This is the assertion that actually exercises memoization: computing
	// the join node visits node 0 twice in one Key call (once via node 1,
	// once via node 2). If the memo returned a stale or wrong cached value
	// on the second visit, the join's key could fail to reflect the
	// ancestor's change even though both branches individually reflect it.
	if k1Join == k2Join {
		t.Fatal("changing the shared ancestor's digest did not change the join node's key — memoization served a stale value")
	}
}

// TestKeyErrorsOnNilPayload guards the fix for malformed hand-built nodes:
// a Kind whose matching payload pointer is nil must error, not panic.
func TestKeyErrorsOnNilPayload(t *testing.T) {
	g := &ir.Graph{Nodes: []ir.Node{
		{Kind: ir.OpExec, Exec: nil},
	}}
	if _, err := Key(g, 0, testInputs()); err == nil {
		t.Fatal("Key succeeded on a node with a nil Exec payload")
	}
}
