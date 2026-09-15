package dockerignore

import (
	"os"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/stagefile/ir"
	"github.com/wendylabsinc/wendy/go/internal/stagefile/spec"
)

// The allowlist has two derivations reached by different routes: codegen holds
// the spec and writes a .dockerignore, llbgen holds only the graph and filters
// an llb.Local. They must describe the same set of files. A path in one and
// not the other is a file that exists for one backend and not the other — a
// build that succeeds and produces a different image.
//
// The comparison is on Derive's exact output, not on the two path sets. A "!"
// allowlist is last-match-wins, so two orderings of one set are not
// automatically the same filter — which is why the graph walk deliberately
// reproduces the spec walk's order rather than relying on the sets matching.
func TestGraphAndSpecAgreeOnTheAllowlist(t *testing.T) {
	cases := []struct {
		name string
		file *spec.File
	}{
		{
			"local copies and every file-reading install",
			&spec.File{Version: 1, Stages: []spec.Stage{{
				Name: "app", From: "python:3.12-slim",
				Install: &spec.Install{
					Pip: []spec.PipInstall{
						{Requirements: "requirements.txt"},
						{Requirements: "requirements-gpu.txt"},
					},
					Npm: &spec.NpmInstall{Manager: "pnpm"},
					Uv:  &spec.UvInstall{},
				},
				Copy: []spec.CopyEntry{
					{From: "local", Paths: []string{"app.py", "src/lib.py"}, Dest: "/srv/"},
				},
			}}},
		},
		{
			"installs that read nothing contribute nothing",
			&spec.File{Version: 1, Stages: []spec.Stage{{
				Name: "app", From: "ubuntu:22.04",
				Install: &spec.Install{
					Apt:   &spec.AptInstall{Packages: []string{"curl"}},
					CMake: []spec.CMakeInstall{{Repository: "https://example.test/a.git", Commit: "a1"}},
				},
				Copy: []spec.CopyEntry{{From: "local", Paths: []string{"app.py"}}},
			}}},
		},
		{
			"a cross-stage copy is not a context path",
			&spec.File{Version: 1, Stages: []spec.Stage{
				{Name: "build", From: "golang:1", Copy: []spec.CopyEntry{
					{From: "local", Paths: []string{"go.mod"}},
				}},
				{Name: "app", From: "debian:12", Copy: []spec.CopyEntry{
					{From: "build", Paths: []string{"/out/app"}, Dest: "/usr/local/bin/app"},
				}},
			}},
		},
		{
			"downloads and extraction read nothing from the context",
			&spec.File{Version: 1, Stages: []spec.Stage{{
				Name: "app", From: "debian:12",
				Download: []spec.Download{
					{URL: "https://example.test/m.tar.gz", SHA256: "aaa", Dest: "/m", Extract: "tar.gz"},
				},
				Copy: []spec.CopyEntry{{From: "local", Paths: []string{"app.py"}}},
			}}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, err := ir.Lower(tc.file, ir.Options{Platform: "linux/arm64"})
			if err != nil {
				t.Fatalf("Lower: %v", err)
			}
			fromGraph, err := LocalPathsFromGraph(g)
			if err != nil {
				t.Fatalf("LocalPathsFromGraph: %v", err)
			}
			if got, want := Derive(fromGraph), Derive(LocalPaths(tc.file)); got != want {
				t.Fatalf("allowlists differ:\n from graph:\n%s\n from spec:\n%s", got, want)
			}
			if len(fromGraph) == 0 {
				t.Fatal("no paths compared — the fixture is not exercising the walk")
			}
		})
	}
}

// The shipped fixtures are the strongest version of the same check: whatever
// they contain, both routes must agree about it.
func TestShippedFixturesAgreeOnTheAllowlist(t *testing.T) {
	for _, fixture := range []string{"example.stagefile.yaml", "npmbuild.stagefile.yaml"} {
		t.Run(fixture, func(t *testing.T) {
			data, err := os.ReadFile("../testdata/" + fixture)
			if err != nil {
				t.Fatal(err)
			}
			f, err := spec.Parse(data)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			g, err := ir.Lower(f, ir.Options{Platform: "linux/arm64"})
			if err != nil {
				t.Fatalf("Lower: %v", err)
			}
			fromGraph, err := LocalPathsFromGraph(g)
			if err != nil {
				t.Fatalf("LocalPathsFromGraph: %v", err)
			}
			if got, want := Derive(fromGraph), Derive(LocalPaths(f)); got != want {
				t.Fatalf("%s allowlists differ:\n from graph:\n%s\n from spec:\n%s", fixture, got, want)
			}
		})
	}
}

// Patterns and Derive are one derivation rendered two ways; a divergence would
// mean the filter llbgen applies and the file codegen writes disagree.
func TestPatternsMatchTheDerivedFile(t *testing.T) {
	paths := []string{"app.py", "src/lib.py", "requirements.txt"}
	want := Derive(paths)
	got := ""
	for _, p := range Patterns(paths) {
		got += p + "\n"
	}
	if got != want {
		t.Fatalf("patterns and file differ:\n patterns:\n%s\n file:\n%s", got, want)
	}
}

func TestLocalPathsFromGraphRejectsANilExecPayload(t *testing.T) {
	g := &ir.Graph{
		Nodes:  []ir.Node{{Kind: ir.OpImage, Image: &ir.ImageOp{Ref: "debian:12"}}, {Kind: ir.OpExec}},
		Stages: []ir.Stage{{Name: "app", Final: 1}},
	}
	if _, err := LocalPathsFromGraph(g); err == nil {
		t.Fatal("accepted an exec node with a nil payload")
	}
}

// The walk is driven by stage ranges, so a stage pointing outside the node
// list must be reported rather than silently skipping or panicking.
func TestLocalPathsFromGraphRejectsAnOutOfRangeStage(t *testing.T) {
	g := &ir.Graph{
		Nodes:  []ir.Node{{Kind: ir.OpImage, Image: &ir.ImageOp{Ref: "debian:12"}}},
		Stages: []ir.Stage{{Name: "app", Final: 7}},
	}
	if _, err := LocalPathsFromGraph(g); err == nil {
		t.Fatal("accepted a stage whose final node is outside the graph")
	}
}
