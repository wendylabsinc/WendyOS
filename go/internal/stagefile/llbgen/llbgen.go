// Package llbgen compiles a lowered ir.Graph into a BuildKit LLB definition —
// the second backend behind the same IR that codegen renders to Dockerfile
// text. Both read their commands, staged files, fetches, and cache mounts from
// package recipe and walk the graph the same way, so the image this backend
// describes is the image codegen's Dockerfile builds.
//
// Emit is pure: it takes a graph plus already-resolved image digests and image
// configs, and returns bytes. It opens no sockets, reads no files, and
// consults no clock, which is what lets a caller compare two definitions for
// equality. Resolving digests and configs against a registry, and solving the
// definition against a BuildKit client, belong to the caller — as does
// applying the returned ImageConfig to the exported image.
//
// # Why Emit takes image configs
//
// A Dockerfile RUN inherits the base image's ENV and WORKDIR because the
// builder resolves the image config before executing it. An llb.State carries
// neither until something calls WithImageConfig on it, and that has to happen
// on the base-image state before any dependent Run or File is built — which is
// inside this function, not after it. Emit marshals before returning, so a
// caller holding only a *llb.Definition cannot apply a config without
// re-implementing Emit.
//
// So configs arrives the same way digests do: keyed by base-image ref,
// resolved by the caller, required. Neither gap it closes is cosmetic. Without
// the environment, `/bin/sh -c` falls back to its compiled-in PATH and any
// tooling outside /usr/bin goes missing. Without the working directory, llb
// normalizes a relative copy destination against "/" instead, so on a base
// image with WORKDIR set, a recipe that stages requirements.txt puts it
// somewhere the Dockerfile backend does not — a build that succeeds and
// produces a different image, which is worse than one that fails.
package llbgen

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/containerd/platforms"
	"github.com/distribution/reference"
	"github.com/moby/buildkit/client/llb"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/wendylabsinc/wendy/go/internal/stagefile/dockerignore"
	"github.com/wendylabsinc/wendy/go/internal/stagefile/ir"
	"github.com/wendylabsinc/wendy/go/internal/stagefile/recipe"
)

// LocalContextName is the LLB name of the build context. A caller solving the
// definition must register the context directory under this same name, which
// is why it is exported: package solve mounts the context under it, and a
// second copy of the string in that package is a rename away from a definition
// whose local source nothing satisfies.
const LocalContextName = "context"

// localUniqueID pins the "local.unique" attribute BuildKit stamps onto every
// local source. Left alone, llb.NewConstraints seeds it with identity.NewID()
// — a fresh random string per marshal — which alone would make two Emit calls
// on one graph produce different bytes. Fixing it is not merely a way to pass
// a determinism test: an identical Stagefile and context must compile to an
// identical definition for the content-addressed cache built on top of this to
// hit at all. The attribute exists to keep one client's local content from
// colliding with another's in the cache; BuildKit already scopes local sources
// by session, which is what actually separates concurrent clients.
const localUniqueID = "wendy-stagefile-context"

// buildPlatformSentinel is the value ir.Lower puts on a `platform: build`
// stage. It is a Dockerfile variable, and LLB has no variables — see
// Options.BuildPlatform.
const buildPlatformSentinel = "$BUILDPLATFORM"

// ImageConfig is the part of a built image that is metadata rather than
// filesystem. It comes from the final stage, the same place codegen reads it,
// and is returned separately because LLB describes a filesystem and has no
// place to carry it.
type ImageConfig struct {
	Entrypoint  []string
	Cmd         []string
	User        string
	Env         map[string]string
	Workdir     string
	Healthcheck *ir.Healthcheck
}

// Options carries the externally-resolved facts Emit needs.
type Options struct {
	// Images maps every base-image ref in the graph to its resolved
	// "sha256:..." digest. An unpinned image (ir.ImageOp.Unpinned) needs no
	// entry.
	Images map[string]string
	// Configs maps every base-image ref to the raw OCI image-config JSON its
	// digest resolves to. Required for every pinned ref — see the package doc.
	Configs map[string][]byte
	// Platform is the target platform and must be non-empty; see the platform
	// check in Emit for why an empty one is refused rather than defaulted.
	Platform string
	// BuildPlatform is the platform of the machine performing the build, used
	// only by stages that declared `platform: build`. It is required only if
	// the graph contains one: a Dockerfile expresses that stage with the
	// $BUILDPLATFORM variable, and LLB has no variables to expand, so the
	// value has to be supplied rather than deferred.
	BuildPlatform string
}

// Emit compiles a lowered graph into an LLB definition plus the final stage's
// image config.
func Emit(g *ir.Graph, opts Options) (*llb.Definition, *ImageConfig, error) {
	if len(g.Stages) == 0 {
		// Marshalling an empty graph would yield a definition for the empty
		// filesystem: a build that succeeds and produces nothing.
		return nil, nil, fmt.Errorf("graph has no stages")
	}

	// codegen omits --platform when platform is empty and lets the builder
	// choose. LLB has no way to say that: llb.NewConstraints always fills a
	// platform in, defaulting to whatever host is marshalling. On a project
	// that cross-builds arm64 device images from x86 CI, accepting "" would
	// quietly stamp the CI runner's architecture onto the build. Refusing is
	// the only honest option, since the value cannot be left unset.
	if opts.Platform == "" {
		return nil, nil, fmt.Errorf("platform is required: LLB cannot express an unset platform and would otherwise pin the host that compiled this build")
	}
	target, err := platforms.Parse(opts.Platform)
	if err != nil {
		return nil, nil, fmt.Errorf("platform %q: %w", opts.Platform, err)
	}

	// The build context is filtered to the same allowlist codegen writes into
	// .dockerignore. An unfiltered local source would put files in front of
	// glob and directory copies that the Dockerfile build never sees, and
	// would re-transfer the whole tree whenever any unrelated file changed.
	localPaths, err := dockerignore.LocalPathsFromGraph(g)
	if err != nil {
		return nil, nil, err
	}

	e := &emitter{
		graph:  g,
		opts:   opts,
		target: target,
		states: make([]llb.State, len(g.Nodes)),
		local: llb.Local(
			LocalContextName,
			llb.LocalUniqueID(localUniqueID),
			// BuildKit matches these with the same matcher that reads a
			// .dockerignore file, "!" negations included, so passing the
			// patterns here is the same filter codegen writes to disk.
			//
			// Deliberately not llb.FollowPaths: it narrows the transfer with a
			// second, differently-implemented matcher, and codegen has no
			// counterpart to it — `docker build` sends the whole tree minus
			// .dockerignore. Any path the two matchers disagree about would
			// vanish from this backend's context without an error.
			llb.ExcludePatterns(dockerignore.Patterns(localPaths)),
			// Without a shared key BuildKit re-transfers the context every
			// build instead of diffing it against the previous snapshot.
			llb.SharedKeyHint(LocalContextName),
		),
	}

	// Nodes belong to the stage whose range they fall in. Walking stages and
	// slicing the node list is exactly how codegen traverses the graph; the
	// two backends must agree on which operations land in which stage.
	start := 0
	for _, st := range g.Stages {
		if st.Final < start || st.Final >= len(g.Nodes) {
			return nil, nil, fmt.Errorf("stage %q: final node %d is outside the graph's %d nodes", st.Name, st.Final, len(g.Nodes))
		}
		// Every node in a stage is constrained to that stage's platform, which
		// is the target unless the stage pinned itself to the build platform.
		stagePlatform := target
		if p := g.Nodes[start].Image; p != nil && p.Platform == buildPlatformSentinel {
			if opts.BuildPlatform == "" {
				return nil, nil, fmt.Errorf("stage %q declares platform: build, but no build platform was supplied: LLB has no $BUILDPLATFORM to expand", st.Name)
			}
			bp, err := platforms.Parse(opts.BuildPlatform)
			if err != nil {
				return nil, nil, fmt.Errorf("build platform %q: %w", opts.BuildPlatform, err)
			}
			stagePlatform = bp
		}
		e.platform = stagePlatform
		e.platformKey = ""
		if p := g.Nodes[start].Image; p != nil {
			e.platformKey = p.Platform
		}
		e.constraints = []llb.ConstraintsOpt{llb.Platform(stagePlatform)}

		for i := start; i <= st.Final; i++ {
			s, err := e.node(g.Nodes[i], i)
			if err != nil {
				return nil, nil, fmt.Errorf("stage %q: %w", st.Name, err)
			}
			e.states[i] = s
		}
		start = st.Final + 1
	}

	final := g.Stages[len(g.Stages)-1]
	// The final stage's own platform constraint, not the last one the loop
	// happened to leave behind — marshalling under a build-platform stage's
	// constraint would describe the wrong architecture for the exported image.
	e.constraints = []llb.ConstraintsOpt{llb.Platform(target)}
	def, err := e.states[final.Final].Marshal(context.TODO(), e.constraints...)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal LLB: %w", err)
	}

	cfg := &ImageConfig{
		Entrypoint:  final.Entrypoint,
		Cmd:         final.Cmd,
		User:        final.User,
		Healthcheck: final.Healthcheck,
	}
	if cfg.User == "" {
		cfg.User = ir.DefaultUser
	}
	// Env and Workdir come off the final stage's image node, where ir.Lower
	// put them, so the exported image carries what the Dockerfile's ENV and
	// WORKDIR would have baked in.
	if im := g.Nodes[finalImageIndex(g, len(g.Stages)-1)].Image; im != nil {
		cfg.Env = im.Env
		cfg.Workdir = im.Workdir
	}
	return def, cfg, nil
}

// finalImageIndex returns the index of stage si's image node, which is always
// the first node in the stage's range.
func finalImageIndex(g *ir.Graph, si int) int {
	if si == 0 {
		return 0
	}
	return g.Stages[si-1].Final + 1
}

// emitter carries the state one Emit call threads through the graph walk.
type emitter struct {
	graph *ir.Graph
	opts  Options
	// target is the build's target platform; platform is the one currently
	// being emitted under, which differs only inside a `platform: build`
	// stage.
	target   ocispecs.Platform
	platform ocispecs.Platform
	// platformKey is the platform string ir stored on the stage's image node,
	// passed to recipe verbatim. It is deliberately not platforms.Format of
	// the parsed value: codegen scopes its cache mounts by the stored string,
	// and a normalized spelling here would give the two backends different
	// cache ids for one stage — including for a `platform: build` stage,
	// whose stored value is a Dockerfile variable rather than a platform.
	platformKey string
	constraints []llb.ConstraintsOpt
	// states is parallel to graph.Nodes: states[i] is the filesystem node i
	// produces. Edges in the graph are indices, so this is the whole mapping.
	states []llb.State
	local  llb.State
}

// node compiles one graph node. Every payload guard here has a counterpart in
// codegen: a Kind whose matching payload is nil can only come from a
// hand-built graph, and reporting it beats indexing into nil and panicking.
func (e *emitter) node(n ir.Node, i int) (llb.State, error) {
	switch n.Kind {
	case ir.OpImage:
		if n.Image == nil {
			return llb.State{}, fmt.Errorf("node %d has kind %q but nil Image payload", i, n.Kind)
		}
		if n.Image.FromStage {
			base, err := e.input(n, 0)
			if err != nil {
				return llb.State{}, fmt.Errorf("stage image node %d: %w", i, err)
			}
			for _, key := range sortedKeys(n.Image.Env) {
				base = base.AddEnv(key, n.Image.Env[key])
			}
			if n.Image.Workdir != "" {
				base = base.Dir(n.Image.Workdir)
			}
			return base, nil
		}
		return e.baseImage(n.Image)

	case ir.OpFetch:
		if n.Fetch == nil {
			return llb.State{}, fmt.Errorf("node %d has kind %q but nil Fetch payload", i, n.Kind)
		}
		base, err := e.input(n, 0)
		if err != nil {
			return llb.State{}, fmt.Errorf("fetch node %d: %w", i, err)
		}
		f, err := recipe.FetchFor(n.Fetch)
		if err != nil {
			return llb.State{}, fmt.Errorf("fetch node %d: %w", i, err)
		}
		return e.fetchState(f, base)

	case ir.OpExec:
		if n.Exec == nil {
			return llb.State{}, fmt.Errorf("node %d has kind %q but nil Exec payload", i, n.Kind)
		}
		base, err := e.input(n, 0)
		if err != nil {
			return llb.State{}, fmt.Errorf("exec node %d: %w", i, err)
		}
		return e.execState(n.Exec, base)

	case ir.OpCopy:
		if n.Copy == nil {
			return llb.State{}, fmt.Errorf("node %d has kind %q but nil Copy payload", i, n.Kind)
		}
		base, err := e.input(n, 0)
		if err != nil {
			return llb.State{}, fmt.Errorf("copy node %d: %w", i, err)
		}
		return e.copyState(n, base)

	default:
		return llb.State{}, fmt.Errorf("unhandled node kind %q", n.Kind)
	}
}

// baseImage pins a base image to its resolved digest, applies its config, and
// then the stage's own prologue.
//
// The reference is parsed here rather than left to llb.Image, which stashes a
// parse failure inside the state and surfaces it from Marshal as a bare
// "invalid reference format" with no indication of which image or digest was
// at fault. This is the one place llbgen is stricter than codegen: codegen
// concatenates ref and digest into text, so a malformed digest becomes a
// Dockerfile that fails at build time, far from the lockfile that produced it.
//
// The config is applied to this state before anything is built on top, which
// is the only point at which it can be: WithImageConfig returns a new State,
// and every dependent Run and File captures the state as it is when they are
// constructed. The stage's own env and workdir go on afterwards, in that same
// order, because a Dockerfile's ENV and WORKDIR override what the base image
// declared rather than being overridden by it.
func (e *emitter) baseImage(im *ir.ImageOp) (llb.State, error) {
	ref := im.Ref
	if !im.Unpinned {
		digest, ok := e.opts.Images[ref]
		if !ok || digest == "" {
			return llb.State{}, fmt.Errorf("no resolved digest for %q; run `stagefile lock`", ref)
		}
		ref = im.Ref + "@" + digest
	}
	if _, err := reference.ParseNormalizedNamed(ref); err != nil {
		return llb.State{}, fmt.Errorf("image %q is not a valid reference: %w", ref, err)
	}

	cfg, ok := e.opts.Configs[im.Ref]
	if !ok {
		// Defaulting to no config is what would make "empty environment" the
		// silent normal case. An absent config is a caller that has not
		// resolved the image, not a caller that wants a bare one.
		return llb.State{}, fmt.Errorf("no resolved image config for %q; resolve it alongside the digest", im.Ref)
	}
	if err := e.checkConfigPlatform(im.Ref, cfg); err != nil {
		return llb.State{}, err
	}

	opts := make([]llb.ImageOption, 0, len(e.constraints))
	for _, c := range e.constraints {
		opts = append(opts, c)
	}
	st, err := llb.Image(ref, opts...).WithImageConfig(cfg)
	if err != nil {
		return llb.State{}, fmt.Errorf("applying image config for %q: %w", im.Ref, err)
	}

	// Sorted, matching the order codegen emits ENV lines in, so that a value
	// referring to another variable resolves the same way under both backends.
	for _, k := range sortedKeys(im.Env) {
		st = st.AddEnv(k, im.Env[k])
	}
	if im.Workdir != "" {
		st = st.Dir(im.Workdir)
	}
	// Args are deliberately not applied. A Dockerfile ARG is a build-time
	// variable that this compiler never interpolates — no recipe reads one,
	// because no spec field carries raw shell — so it changes nothing about
	// what runs. It reaches the cache key (where a declared default is part of
	// the declaration) and the Dockerfile (where a human may pass --build-arg),
	// and there is nothing for it to do here.
	return st, nil
}

// checkConfigPlatform refuses a config resolved for a different platform than
// the one being built.
//
// A mismatched config describes a different image than the one being pinned,
// and Emit would apply its Env and WorkingDir to this build without noticing.
// Nothing downstream catches it: the platform WithImageConfig stamps onto the
// state never reaches the definition, because every Run and File here carries
// an explicit llb.Platform constraint and MarshalConstraints prefers the op's
// own constraint over the state's, and the image source op is constructed
// before the config is applied. So this is the only check there is.
//
// The match is OnlyStrict, not Only: Only deliberately matches
// compatible-subset platforms, so an arm/v7 config would satisfy an arm64
// build and a 386 config an amd64 one — precisely the misresolution this guard
// exists to catch. OSVersion and OSFeatures are carried into the comparison
// rather than dropped, since WithImageConfig stamps those too.
func (e *emitter) checkConfigPlatform(ref string, cfg []byte) error {
	var img ocispecs.Image
	if err := json.Unmarshal(cfg, &img); err != nil {
		return fmt.Errorf("image config for %q is not valid JSON: %w", ref, err)
	}
	// Only a config declaring both os and architecture is checked, matching
	// the condition WithImageConfig applies the platform under.
	if img.OS == "" || img.Architecture == "" {
		return nil
	}
	got := ocispecs.Platform{
		OS:           img.OS,
		Architecture: img.Architecture,
		Variant:      img.Variant,
		OSVersion:    img.OSVersion,
		OSFeatures:   img.OSFeatures,
	}
	if !platforms.OnlyStrict(e.platform).Match(got) {
		return fmt.Errorf("image config for %q is %s but this stage builds for %s", ref, platforms.Format(got), platforms.Format(e.platform))
	}
	return nil
}

// fetchState places one checksum-pinned download onto base.
//
// llb.HTTP is the same source BuildKit's Dockerfile frontend uses for an ADD
// with a remote URL: the daemon performs the fetch and verifies the digest
// before any layer can read the bytes, so no shell runs in the container and
// an unpinned fetch is not expressible.
func (e *emitter) fetchState(f recipe.Fetch, base llb.State) (llb.State, error) {
	// The fetched file is named after its destination so the copy below has a
	// single, known source path. Left unset, llb.HTTP names the file after the
	// URL's last path segment, which a redirect or a query string can change
	// without the Stagefile changing.
	name := path.Base(f.Dest)
	if name == "." || name == "/" {
		return llb.State{}, fmt.Errorf("fetch destination %q has no filename", f.Dest)
	}

	httpOpts := []llb.HTTPOption{llb.Filename(name), llb.Checksum(digestOf(f.Checksum))}
	if f.Mode != "" {
		mode, err := parseFileMode(f.Mode)
		if err != nil {
			return llb.State{}, fmt.Errorf("fetch %q: %w", f.URL, err)
		}
		httpOpts = append(httpOpts, llb.Chmod(mode))
	}
	src := llb.HTTP(f.URL, httpOpts...)

	info := &llb.CopyInfo{CreateDestPath: true}
	if f.Owner != "" {
		owner, err := parseChown(f.Owner)
		if err != nil {
			return llb.State{}, fmt.Errorf("fetch %q: %w", f.URL, err)
		}
		info.ChownOpt = owner
	}
	return base.File(llb.Copy(src, name, f.Dest, info), e.constraints...), nil
}

// execState renders one recipe onto base: its fetches and staged files first,
// then each command under /bin/sh with its cache mounts attached.
func (e *emitter) execState(x *ir.ExecOp, base llb.State) (llb.State, error) {
	steps, err := recipe.For(x, e.platformKey)
	if err != nil {
		return llb.State{}, err
	}
	if len(steps) == 0 {
		return llb.State{}, fmt.Errorf("recipe %q produced no steps", x.Recipe.Name)
	}

	for _, s := range steps {
		switch {
		case s.Fetch != nil:
			base, err = e.fetchState(*s.Fetch, base)
			if err != nil {
				return llb.State{}, err
			}
		case s.Run != nil:
			base, err = e.runState(s.Run, base, x.Recipe.Name)
			if err != nil {
				return llb.State{}, err
			}
		default:
			return llb.State{}, fmt.Errorf("recipe step for %q is neither a run nor a fetch", x.Recipe.Name)
		}
	}
	return base, nil
}

func (e *emitter) runState(r *recipe.RunSpec, base llb.State, recipeName string) (llb.State, error) {
	if len(r.Command) == 0 {
		return llb.State{}, fmt.Errorf("recipe %q produced a run with no command", recipeName)
	}
	if r.PreCopy != nil {
		stage, err := copyAction(e.local, r.PreCopy.Paths, r.PreCopy.Dest)
		if err != nil {
			return llb.State{}, err
		}
		base = base.File(stage, e.constraints...)
	}

	// recipe hands back clauses rather than one string so codegen can render
	// "\" continuations. LLB has no line breaks to render, so the clauses join
	// with " && " — the same single command the shell sees either way.
	opts := []llb.RunOption{llb.Args([]string{"/bin/sh", "-c", strings.Join(r.Command, " && ")})}
	for _, cm := range r.CacheMounts {
		sharing := llb.CacheMountShared
		if cm.Locked {
			// Wendy builds several services at once on top of BuildKit's own
			// parallelism; a shared mount lets them collide inside the package
			// manager, where the waiting is invisible.
			sharing = llb.CacheMountLocked
		}
		opts = append(opts, llb.AddMount(cm.Dir, llb.Scratch(), llb.AsPersistentCacheDir(cacheID(cm), sharing)))
	}
	for _, c := range e.constraints {
		opts = append(opts, c)
	}
	return base.Run(opts...).Root(), nil
}

// cacheID reproduces the ID BuildKit's own Dockerfile frontend gives a
// `--mount=type=cache,target=`: an explicit id: is used verbatim, and an
// unnamed mount takes its cache-ID namespace, then "/", then the cleaned
// target. The leading "/" is not a typo — the namespace is empty unless a
// build sets BUILDKIT_CACHE_MOUNT_NS, and the frontend concatenates it
// unconditionally. Reproducing the quirk is the point: it means an LLB build
// and a `docker build` of the generated Dockerfile share one warmed package
// cache instead of each maintaining a parallel copy.
func cacheID(m recipe.CacheMount) string {
	if m.ID != "" {
		return m.ID
	}
	return "/" + path.Clean(m.Dir)
}

// copyState renders one copy node onto base.
//
// A stage-sourced copy whose source node matches no stage, or that is missing
// its source input entirely, is an error rather than a fallback — the same
// reasoning codegen's copyLine documents. Falling back to the local context
// would silently turn a stage-to-stage copy into a context copy: a build that
// succeeds and builds the wrong thing.
func (e *emitter) copyState(n ir.Node, base llb.State) (llb.State, error) {
	// Dest is used verbatim: ir.Lower has already defaulted it and applied the
	// trailing-slash rule for multi-source copies. Re-deriving it here would
	// let this backend drift from the value the cache key hashes. An empty one
	// can only reach here from a hand-built graph, and copying to "" would
	// resolve to the stage root.
	if n.Copy.Dest == "" {
		return llb.State{}, fmt.Errorf("copy node has an empty Dest")
	}

	src := e.local
	if !n.Copy.FromLocal {
		if len(n.Inputs) < 2 {
			return llb.State{}, fmt.Errorf("copy node has %d inputs, want [base, sourceStage]", len(n.Inputs))
		}
		found := false
		for _, st := range e.graph.Stages {
			if st.Final == n.Inputs[1] {
				found = true
				break
			}
		}
		if !found {
			return llb.State{}, fmt.Errorf("copy node's source node %d is not the final node of any stage", n.Inputs[1])
		}
		s, err := e.input(n, 1)
		if err != nil {
			return llb.State{}, err
		}
		src = s
	}

	info := copyInfo()
	if n.Copy.Owner != "" {
		owner, err := parseChown(n.Copy.Owner)
		if err != nil {
			return llb.State{}, err
		}
		info.ChownOpt = owner
	}
	if n.Copy.Mode != "" {
		mode, err := parseFileMode(n.Copy.Mode)
		if err != nil {
			return llb.State{}, err
		}
		info.Mode = &llb.ChmodOpt{Mode: mode}
	}
	action, err := copyActionInfo(src, n.Copy.Paths, n.Copy.Dest, info)
	if err != nil {
		return llb.State{}, err
	}
	if n.Copy.Link {
		// Match Dockerfile COPY --link: build the copy as an independent
		// scratch layer, then merge it over the destination state. This keeps
		// dependency overlays reusable when an unrelated base layer changes.
		linked := llb.Scratch().File(action, e.constraints...)
		return llb.Merge([]llb.State{base, linked}, e.constraints...), nil
	}
	return base.File(action, e.constraints...), nil
}

// copyInfo is BuildKit's Dockerfile-frontend CopyInfo, field for field, so a
// Stagefile copy behaves like the COPY codegen emits: destination parents are
// created, directory contents are copied rather than the directory itself,
// symlinks in the source are followed, and globs expand (and are allowed to
// match nothing, as Dockerfile permits).
func copyInfo() *llb.CopyInfo {
	return &llb.CopyInfo{
		FollowSymlinks:      true,
		CopyDirContentsOnly: true,
		CreateDestPath:      true,
		AllowWildcard:       true,
		AllowEmptyWildcard:  true,
	}
}

func copyAction(src llb.State, paths []string, dest string) (*llb.FileAction, error) {
	return copyActionInfo(src, paths, dest, copyInfo())
}

// copyActionInfo chains every source path into one file operation, the way
// BuildKit's Dockerfile frontend renders a multi-source COPY.
//
// An empty path list yields no action at all, and State.File panics on a nil
// one; ir.Lower rejects an empty copy, so this can only come from a hand-built
// graph — the same class of caller every other guard here exists for.
func copyActionInfo(src llb.State, paths []string, dest string, info *llb.CopyInfo) (*llb.FileAction, error) {
	if len(paths) == 0 {
		return nil, fmt.Errorf("copy to %q has no source paths", dest)
	}
	var action *llb.FileAction
	for _, p := range paths {
		if action == nil {
			action = llb.Copy(src, p, dest, info)
			continue
		}
		action = action.Copy(src, p, dest, info)
	}
	return action, nil
}

// input resolves edge idx of n to the state that node produces. Nodes are
// topologically ordered, so a forward or out-of-range edge is a malformed
// graph rather than something to wait on.
func (e *emitter) input(n ir.Node, idx int) (llb.State, error) {
	if idx >= len(n.Inputs) {
		return llb.State{}, fmt.Errorf("node has %d inputs, want at least %d", len(n.Inputs), idx+1)
	}
	i := n.Inputs[idx]
	if i < 0 || i >= len(e.states) {
		return llb.State{}, fmt.Errorf("input %d is outside the graph's %d nodes", i, len(e.states))
	}
	return e.states[i], nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
