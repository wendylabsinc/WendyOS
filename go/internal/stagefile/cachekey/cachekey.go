// Package cachekey computes stable content-addressed keys for ir nodes.
//
// A key covers a node's semantic dependency closure: its recipe and
// version, its declared parameters, the digests of any input files it
// reads, and — recursively — the keys of the nodes it runs on. It does not
// cover the node's position in the source file, the text of any rendered
// command, or any unrelated sibling stage.
//
// That is the difference between this and a Docker layer ID, and the
// property it buys is this one: two projects that declare the same
// operations, in the same order, on the same base converge on the same key —
// no matter what else their Stagefiles contain, what their stages are named,
// or which file they live in.
//
// It is not a key over the resulting filesystem. Two builds that reach an
// identical rootfs by different routes still key differently: reordering two
// independent installs, or splitting one apt install into two, changes the op
// sequence and therefore the key. Those are missed cache hits, never wrong
// hits — the direction a cache is allowed to be wrong in.
package cachekey

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/stagefile/ir"
)

// keyFormatVersion prefixes every key. Bumping it invalidates every cached
// node everywhere; it exists for changes to the keying scheme itself, as
// distinct from ir.Recipe.Version which scopes an invalidation to one
// recipe.
// Version 2 added the target platform and the npm manifest digest to the
// key, and moved copy-destination normalization ahead of it; all three
// change every key they touch, so they were folded into one bump before the
// format shipped.
// Version 3 extended the model to the rest of the DSL — the stage's
// args/env/workdir/platform, fetches and their extraction, apt repositories,
// cmake, uv, pip indexes, and the copy/build/npm fields that had no ir
// representation before. Everything it adds is a build input that was
// previously invisible to the key, so it is one bump rather than one per
// field.
const keyFormatVersion = 3

// Inputs supplies the externally-resolved facts a key depends on: base
// image digests (from the lockfile) and build-context path digests.
//
// Files maps a context-relative path to a digest of its content. For a
// directory that must be a digest over the whole tree — names, modes, and
// contents — because a copy of a directory depends on all of it. Computing
// these belongs to the caller; stage 3 supplies them from the build
// context, and stage 1 only ever receives them from tests. Key treats a
// missing entry as an error rather than hashing an empty string, so a
// caller that forgets a path cannot silently produce a key that collides
// across different content.
//
// Platform is the target platform the build is compiled for ("linux/arm64"
// and so on). It is part of the key rather than part of the graph because
// the graph describes what to build and the platform describes what machine
// the result runs on: the same node built for two architectures produces two
// different rootfs. It cannot be inferred from the base image digest either
// — a multi-arch base resolves to one index digest shared by every
// architecture — so without this field an arm64 build and an amd64 build of
// the same Stagefile would key identically and poison each other across a
// shared cache.
//
// There is no production caller of this field yet — Platform is hashed, but
// nothing today wires it to a real value. Whoever adds the first production
// caller MUST pass exactly the platform string given to codegen.Generate for
// the same build; an empty Platform is only correct when the build
// genuinely does not pin an architecture. Getting this wrong is silent:
// lock.CraneResolver (lock/resolve.go) calls crane.Digest(ref) with no
// platform, so it stores an architecture-ambiguous multi-arch index digest —
// the image digest folded into the rest of the key cannot disambiguate
// architecture on its own. Platform is the only field that can, so a
// caller that passes an empty or mismatched platform here lets two
// different architectures collide on one cache key.
type Inputs struct {
	Images   map[string]string
	Files    map[string]string
	Platform string
}

// keyState is the per-call recursion state for one top-level Key call. It
// must never outlive that call or be shared across two calls with different
// Inputs: memo is only valid for the exact (graph, Inputs) pair it was built
// against, and a memo entry computed under one Inputs served to another
// would be a wrong cache key served with high confidence — the worst kind of
// bug this package can have. Key constructs a fresh keyState on every call
// for exactly this reason; nothing here is package-level or reused.
type keyState struct {
	// ancestors is the set of node indices currently on the recursion
	// stack, for O(1) cycle detection. path is the same stack in visit
	// order, kept only so a detected cycle can be reported by the route
	// that reached it rather than by the single index where it closed.
	ancestors map[int]bool
	path      []int
	// memo caches each node's finished key by index, so a diamond-shaped
	// closure (two nodes sharing a common ancestor) computes that ancestor's
	// key once per Key call instead of once per path that reaches it. An
	// entry is written only after write() returns successfully — never
	// before recursing into a node — so the memo can't paper over the cycle
	// guard above: a node still on the stack has no memo entry yet.
	memo map[int]string
}

// Key returns the "sha256:<hex>" key of node idx in g.
//
// Key is exported and, per package doc, may be called against a hand-built
// Graph rather than only ir.Lower's output — Lower itself only ever
// produces a DAG, but a caller assembling a Graph by hand can wire up a
// cycle. keyPath guards against that with the set of node indices currently
// on the recursion stack (ancestors), not a global visited set: a DAG may
// legitimately reach the same node from two different paths (e.g. two
// stages copying from the same base), and that must not be mistaken for a
// cycle.
func Key(g *ir.Graph, idx int, in Inputs) (string, error) {
	st := &keyState{ancestors: map[int]bool{}, memo: map[int]string{}}
	return keyPath(g, idx, in, st)
}

func keyPath(g *ir.Graph, idx int, in Inputs, st *keyState) (string, error) {
	if idx < 0 || idx >= len(g.Nodes) {
		return "", fmt.Errorf("cachekey: node index %d out of range", idx)
	}
	if st.ancestors[idx] {
		return "", fmt.Errorf("cachekey: cycle detected: %s", cyclePath(st.path, idx))
	}
	if k, ok := st.memo[idx]; ok {
		return k, nil
	}
	st.ancestors[idx] = true
	st.path = append(st.path, idx)
	defer func() {
		delete(st.ancestors, idx)
		st.path = st.path[:len(st.path)-1]
	}()

	h := sha256.New()
	e := enc{h: h}
	e.tag("stagefile-key")
	e.int(keyFormatVersion)
	// Platform is folded in here, alongside the format version, rather than
	// into any individual node: it applies uniformly to the whole build, and
	// every node's key must differ between architectures.
	e.str(in.Platform)
	if err := write(e, g, idx, in, st); err != nil {
		return "", err
	}
	key := "sha256:" + hex.EncodeToString(h.Sum(nil))
	// Recorded only now, after write has fully succeeded: a partially
	// computed or errored node must never be memoized, and a node still
	// mid-recursion (an ancestor of itself) is caught above before this
	// point is ever reached.
	st.memo[idx] = key
	return key, nil
}

// cyclePath renders the full recursion route from the top-level Key call
// down to the repeated node, not the minimal repeating loop within it: a
// call starting at the cycle prints just the loop (e.g. "2 -> 4 -> 2"), but
// a call starting two hops earlier prints the leading nodes too (e.g.
// "0 -> 1 -> 3 -> 4 -> 3"). The extra leading nodes are deliberate — they
// show a caller debugging a hand-built graph how it got there, not just
// where it closed.
func cyclePath(path []int, idx int) string {
	parts := make([]string, 0, len(path)+1)
	for _, p := range path {
		parts = append(parts, strconv.Itoa(p))
	}
	parts = append(parts, strconv.Itoa(idx))
	return strings.Join(parts, " -> ")
}

func write(e enc, g *ir.Graph, idx int, in Inputs, st *keyState) error {
	n := g.Nodes[idx]

	// Inputs are folded in by key, not by index — an index is a position in
	// this particular file and must never reach the hash.
	e.tag("inputs")
	e.int(len(n.Inputs))
	for _, dep := range n.Inputs {
		sub, err := keyPath(g, dep, in, st)
		if err != nil {
			return err
		}
		e.str(sub)
	}

	e.tag(string(n.Kind))
	switch n.Kind {
	case ir.OpImage:
		if n.Image == nil {
			return fmt.Errorf("cachekey: node %d has kind %q but nil Image payload", idx, n.Kind)
		}
		if !n.Image.Unpinned {
			digest, ok := in.Images[n.Image.Ref]
			if !ok {
				return fmt.Errorf("cachekey: no resolved digest for image %q", n.Image.Ref)
			}
			// The ref itself is excluded on purpose: two tags pointing at the
			// same digest are the same rootfs and must share a key.
			e.str(digest)
		} else {
			// An unpinned image has no digest anywhere — that is what
			// `pin: false` means. The ref is all there is to hash, so the key
			// covers which image was named and cannot cover what it
			// contained: a locally-loaded image rebuilt under the same tag
			// keys identically. That is a real limitation of `pin: false`,
			// not of this package, and it is why the ref is hashed under its
			// own tag rather than in the same position a digest would take —
			// so an unpinned node can never collide with a pinned one whose
			// digest happens to equal the ref string.
			e.tag("unpinned")
			e.str(n.Image.Ref)
		}
		e.str(n.Image.Platform)
		e.kv(n.Image.Args)
		e.kv(n.Image.Env)
		e.str(n.Image.Workdir)
	case ir.OpFetch:
		if n.Fetch == nil {
			return fmt.Errorf("cachekey: node %d has kind %q but nil Fetch payload", idx, n.Kind)
		}
		// The checksum is what makes a fetch keyable at all: it pins the
		// bytes, so two builds fetching the same content from the same URL
		// share a key even across a re-lock. The URL is hashed alongside it
		// because the same bytes reached from a different URL is a different
		// declaration, and Dest/Mode/Owner because they decide where the
		// bytes land and how they are owned.
		e.str(n.Fetch.URL)
		e.str(n.Fetch.Checksum)
		e.str(n.Fetch.Dest)
		e.str(n.Fetch.Mode)
		e.str(n.Fetch.Owner)
	case ir.OpCopy:
		if n.Copy == nil {
			return fmt.Errorf("cachekey: node %d has kind %q but nil Copy payload", idx, n.Kind)
		}
		e.bool(n.Copy.FromLocal)
		e.strs(n.Copy.Paths)
		e.str(n.Copy.Dest)
		e.str(n.Copy.Owner)
		e.str(n.Copy.Mode)
		if n.Copy.FromLocal {
			for _, p := range n.Copy.Paths {
				d, ok := in.Files[p]
				if !ok {
					return fmt.Errorf("cachekey: no digest for context path %q", p)
				}
				e.str(d)
			}
		}
	case ir.OpExec:
		if n.Exec == nil {
			return fmt.Errorf("cachekey: node %d has kind %q but nil Exec payload", idx, n.Kind)
		}
		if err := writeExec(e, n.Exec, in); err != nil {
			return err
		}
	default:
		return fmt.Errorf("cachekey: unhandled node kind %q", n.Kind)
	}
	return nil
}

func writeExec(e enc, x *ir.ExecOp, in Inputs) error {
	e.str(x.Recipe.Name)
	e.int(x.Recipe.Version)
	switch {
	case x.Apt != nil:
		e.strs(x.Apt.Packages)
		e.bool(x.Apt.Recommends)
		// A declared repository changes which packages the install can even
		// resolve, so it is as much a build input as the package list. The
		// key is hashed by URL and digest rather than by fetching it: the
		// digest is the content, and it is mandatory in the spec.
		e.int(len(x.Apt.Repositories))
		for _, r := range x.Apt.Repositories {
			e.str(r.Name)
			e.str(r.URL)
			e.strs(r.Suites)
			e.strs(r.Components)
			e.str(r.KeyURL)
			e.str(r.KeySHA256)
			e.str(r.KeyFormat)
		}
	case x.Apk != nil:
		e.strs(x.Apk.Packages)
		e.bool(x.Apk.Cache)
		e.strs(x.Apk.Repositories)
	case x.CMake != nil:
		// Commit is hashed even though cmakeCacheID deliberately excludes it:
		// the cache id decides what may share a build tree, while the key
		// decides what may reuse a finished rootfs. A different commit is a
		// different rootfs.
		e.str(x.CMake.Repository)
		e.str(x.CMake.Commit)
		e.str(x.CMake.Prefix)
		e.str(x.CMake.BuildType)
		e.kv(x.CMake.Defines)
		e.int(x.CMake.Jobs)
		e.str(x.CMake.Root)
	case x.Pip != nil:
		e.str(x.Pip.Requirements)
		e.strs(x.Pip.Packages)
		// The index set is hashed because the same package name resolves to
		// different wheels from different indexes — which is the entire point
		// of a cuda: group, where ir.Lower has already substituted the GPU
		// profile's index here.
		e.str(x.Pip.Index)
		e.strs(x.Pip.ExtraIndex)
		if x.Pip.Requirements != "" {
			d, ok := in.Files[x.Pip.Requirements]
			if !ok {
				return fmt.Errorf("cachekey: no digest for %q", x.Pip.Requirements)
			}
			e.str(d)
		}
	case x.Npm != nil:
		e.str(x.Npm.Manager)
		e.str(x.Npm.Manifest)
		e.str(x.Npm.Lockfile)
		e.bool(x.Npm.Production)
		// The manifest is hashed exactly like the lockfile, and for the same
		// reason: the install reads both, so an edit to either changes the
		// resulting filesystem. A key that covered only the lockfile would
		// serve a stale rootfs for any change to package.json's scripts,
		// engines, or dependency ranges that left the lockfile untouched.
		for _, f := range []string{x.Npm.Manifest, x.Npm.Lockfile} {
			d, ok := in.Files[f]
			if !ok {
				return fmt.Errorf("cachekey: no digest for %q", f)
			}
			e.str(d)
		}
	case x.Uv != nil:
		e.strs(x.Uv.Extras)
		e.bool(x.Uv.Dev)
		e.strs(x.Uv.Files)
		// pyproject.toml and uv.lock are hashed for the same reason npm's
		// pair is: `uv sync --frozen` reads both, and neither the Dockerfile
		// nor the image lockfile varies with their contents, so without this
		// a dependency edit would change nothing the key can see.
		for _, f := range x.Uv.Files {
			d, ok := in.Files[f]
			if !ok {
				return fmt.Errorf("cachekey: no digest for %q", f)
			}
			e.str(d)
		}
	case x.Extract != nil:
		e.str(x.Extract.Archive)
		e.str(x.Extract.Dest)
		e.str(x.Extract.Format)
	case x.CUDACollect != nil:
		e.str(x.CUDACollect.LibDir)
		e.str(x.CUDACollect.ConfPath)
	case x.Build != nil:
		e.str(x.Build.Lang)
		e.str(x.Build.Profile)
		e.str(x.Build.Product)
		e.str(x.Build.Script)
	default:
		return fmt.Errorf("cachekey: exec node has no params")
	}
	return nil
}
