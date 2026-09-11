// Package dockerignore derives .dockerignore content from the local copy
// paths declared in a Stagefile, rather than asking the author to
// hand-maintain a denylist. Every file that can enter a build is named
// explicitly in a copy.from: local entry, so the allowlist is exact.
package dockerignore

import (
	"fmt"
	"path"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/stagefile/ir"
	"github.com/wendylabsinc/wendy/go/internal/stagefile/recipe"
	"github.com/wendylabsinc/wendy/go/internal/stagefile/spec"
)

// LocalPaths collects every path referenced by a copy.from: local entry
// across every stage of f, in file order, without duplicates.
func LocalPaths(f *spec.File) []string {
	seen := map[string]bool{}
	var paths []string
	add := func(p string) {
		if p != "" && !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	for _, s := range f.Stages {
		for _, c := range s.Copy {
			if c.From != "local" {
				continue
			}
			for _, p := range c.Paths {
				add(p)
			}
		}
		for _, p := range s.Install.LocalFiles() {
			add(p)
		}
	}
	return paths
}

// isContextRoot reports whether a declared copy path is the build context
// itself — `.`, `./`, `/`, or any spelling that cleans to one of those.
//
// Such a path defeats the allowlist rather than narrowing it. `copy: {paths:
// [.]}` would compile to `*` followed by `!.` and `!./**`, and BuildKit cleans
// `./**` to `**`, so the deny-all is undone on the next line and nothing is
// ignored at all. A repo whose .dockerignore excludes a 4 GB .build directory
// would ship it anyway, and COPY it into the image.
//
// There is also nothing to derive here: a stage that copies the whole context
// has already said "everything", so the only ignore rules that can be right are
// the project's own.
func isContextRoot(raw string) bool {
	switch path.Clean(raw) {
	case ".", "/":
		return true
	}
	return false
}

// Derive returns .dockerignore content that denies everything except the given
// paths, or "" when no allowlist can be expressed — see isContextRoot.
//
// "" means "write no ignore file", not "ignore nothing". The distinction
// matters because BuildKit prefers <dockerfile>.dockerignore over
// .dockerignore: a generated file always wins, so emitting one that ignores
// nothing silently disables the project's own .dockerignore, while emitting
// none leaves it in charge.
func Derive(paths []string) string {
	for _, raw := range paths {
		if isContextRoot(raw) {
			return ""
		}
	}
	return strings.Join(allowLines(paths), "\n") + "\n"
}

// LocalPathsFromGraph collects the same allowlist as LocalPaths, but from a
// lowered graph rather than the spec it came from.
//
// Two consumers need the allowlist and reach it by different routes: codegen
// writes a .dockerignore beside a Dockerfile and holds the spec, while llbgen
// filters an llb.Local and holds only the graph. Deriving the graph side from
// the same recipe definitions that decide what each step stages — rather than
// from a second list of "which recipes read files" — is what keeps the two
// from disagreeing about which files a build may see. A path missing from one
// of them is a file that exists for one backend and not the other.
func LocalPathsFromGraph(g *ir.Graph) ([]string, error) {
	seen := map[string]bool{}
	var paths []string
	add := func(p string) {
		if p != "" && !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	// Generated dependency stages now precede their app stage in the graph.
	// Collect declared local copies first and recipe inputs second, matching
	// LocalPaths' externally visible order while de-duplicating helper stages.
	start := 0
	type stageRange struct{ source, start, final int }
	var ranges []stageRange
	maxSource := -1
	for si, st := range g.Stages {
		if st.Final < start || st.Final >= len(g.Nodes) {
			return nil, fmt.Errorf("stage %d: final node %d is outside the graph's %d nodes", si, st.Final, len(g.Nodes))
		}
		ranges = append(ranges, stageRange{st.SourceIndex, start, st.Final})
		if st.SourceIndex > maxSource {
			maxSource = st.SourceIndex
		}
		start = st.Final + 1
	}
	for source := 0; source <= maxSource; source++ {
		for _, r := range ranges {
			if r.source != source {
				continue
			}
			for i := r.start; i <= r.final; i++ {
				n := g.Nodes[i]
				if n.Kind == ir.OpCopy && n.Copy != nil && n.Copy.FromLocal {
					for _, p := range n.Copy.Paths {
						add(p)
					}
				}
			}
		}
		for _, r := range ranges {
			if r.source != source {
				continue
			}
			for i := r.start; i <= r.final; i++ {
				n := g.Nodes[i]
				if n.Kind != ir.OpExec {
					continue
				}
				if n.Exec == nil {
					return nil, fmt.Errorf("node %d has kind %q but nil Exec payload", i, n.Kind)
				}
				staged, err := recipe.StagedFiles(n.Exec)
				if err != nil {
					return nil, fmt.Errorf("node %d: %w", i, err)
				}
				for _, p := range staged {
					add(p)
				}
			}
		}
	}
	return paths, nil
}

// Patterns returns the allowlist as the exclude-pattern list BuildKit applies
// to a local source, which is the same matcher — "!" negations included — that
// reads the .dockerignore file Derive writes. One derivation, two renderings.
func Patterns(paths []string) []string {
	for _, raw := range paths {
		if isContextRoot(raw) {
			return nil
		}
	}
	return allowLines(paths)
}

func allowLines(paths []string) []string {
	lines := []string{"*"}
	seen := map[string]bool{}
	add := func(p string) {
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		lines = append(lines, "!"+p)
	}
	for _, raw := range paths {
		p := strings.TrimPrefix(raw, "./")
		// Ensure parent dirs are reachable when unignoring nested files.
		for dir := p; ; {
			i := strings.LastIndex(dir, "/")
			if i <= 0 {
				break
			}
			add(dir[:i+1])
			dir = dir[:i]
		}
		add(p)
		// Also allow directory contents when p refers to a directory.
		add(p + "/")
		add(p + "/**")
	}
	return lines
}
