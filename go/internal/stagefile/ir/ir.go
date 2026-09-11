// Package ir is the typed intermediate representation between a parsed
// Stagefile and any backend that renders it. A Graph is a flat,
// topologically ordered list of Nodes whose edges are indices into that
// list — never pointers, because Node must serialize deterministically for
// cache-key computation (see package cachekey).
//
// Two rules hold everywhere in this package:
//
//  1. No node carries a rendered shell string. Nodes carry typed recipe
//     parameters; rendering and quoting belong to a backend.
//  2. No node payload aliases a spec type. spec grows fields as DSL coverage
//     gaps close; if ir aliased those types, a new spec field would silently
//     change cache keys fleet-wide. Every field crosses the boundary through
//     Lower, by hand, on purpose.
package ir

// OpKind is the closed set of node operations. Adding a kind is a
// deliberate extension of the model, not a convenience.
type OpKind string

const (
	OpImage OpKind = "image"
	OpExec  OpKind = "exec"
	OpCopy  OpKind = "copy"
	OpFetch OpKind = "fetch"
)

// Recipe identifies a typed exec operation and the version of its
// compilation rules. Version participates in the cache key: bumping it
// invalidates every node using that recipe, everywhere. Treat a bump as a
// migration, not a refactor.
type Recipe struct {
	Name    string
	Version int
}

var (
	RecipeApt         = Recipe{Name: "apt", Version: 1}
	RecipeApk         = Recipe{Name: "apk", Version: 1}
	RecipeCMake       = Recipe{Name: "cmake", Version: 1}
	RecipePip         = Recipe{Name: "pip", Version: 1}
	RecipeNpm         = Recipe{Name: "npm", Version: 1}
	RecipeUv          = Recipe{Name: "uv", Version: 1}
	RecipeExtract     = Recipe{Name: "extract", Version: 1}
	RecipeCUDACollect = Recipe{Name: "cuda-collect", Version: 1}
	RecipeBuild       = Recipe{Name: "build", Version: 1}
)

// DefaultUser is the distroless-style non-root numeric UID a backend
// substitutes when a final stage leaves Stage.User empty. A numeric UID needs
// no /etc/passwd entry, so it works on any base image.
//
// It lives beside the field it defaults rather than inside one backend,
// because every backend must substitute the same value: two backends holding
// their own copy is how a Dockerfile build and an LLB build of one Stagefile
// end up running as different users.
const DefaultUser = "65532"

// Graph is a lowered Stagefile.
type Graph struct {
	Nodes  []Node
	Stages []Stage
}

// Stage names a build stage and points at the node holding its final state.
type Stage struct {
	Name  string
	Final int
	// Healthcheck, Entrypoint, Cmd, and User are stage-level image config
	// rather than filesystem operations, so they hang off the stage, not
	// off a node. They never enter a cache key: changing an entrypoint does
	// not invalidate a built filesystem.
	//
	// Everything that DOES change the filesystem — the base image, the
	// stage's env/args/workdir, its target platform — lives on the stage's
	// ImageOp instead, where the key covers it and every later node in the
	// stage inherits it through the input chain.
	Healthcheck *Healthcheck
	Entrypoint  []string
	Cmd         []string
	User        string
}

// Healthcheck is the container health probe, exec form only.
type Healthcheck struct {
	Exec        []string
	Interval    string
	Timeout     string
	StartPeriod string
	Retries     int
}

// Node is one operation. Exactly one of Image/Exec/Copy/Fetch is non-nil,
// matching Kind.
type Node struct {
	Kind   OpKind
	Inputs []int
	Image  *ImageOp
	Exec   *ExecOp
	Copy   *CopyOp
	Fetch  *FetchOp
}

// ImageOp is a stage's initial state: the base image by its original
// reference, plus the build-time settings every later operation in the
// stage runs under. The resolved digest is supplied separately at key time
// and at render time, so the graph itself stays independent of any
// particular lockfile state.
//
// Args, Env, and Workdir live here rather than on Stage because they are
// not image config — they change what every install and build in the stage
// does, so they must reach the cache key. Hanging them off the stage's
// first node is what gets them there: every later node lists it as an
// ancestor, so its key propagates down the whole stage.
type ImageOp struct {
	Ref string
	// Unpinned marks the `pin: false` images that exist solely in a local
	// daemon store and have no registry digest to pin against.
	//
	// The field is negated so that the zero value is the pinned, safe one.
	// A hand-built graph — which cachekey explicitly supports — that forgot
	// to set a positive Pinned field would key off the image's tag instead
	// of its digest, silently narrowing what the key covers. Forgetting this
	// field costs nothing instead.
	Unpinned bool
	// Platform is the resolved platform for this stage: the build's target
	// platform, or "$BUILDPLATFORM" for a `platform: build` stage, or "" to
	// let the builder decide. It is hashed because a stage pinned to the
	// build platform produces different bytes than the same stage built for
	// the target.
	Platform string
	Args     map[string]string
	Env      map[string]string
	Workdir  string
}

// ExecOp is a typed operation. Exactly one params pointer is non-nil, and
// it must correspond to Recipe.
type ExecOp struct {
	Recipe      Recipe
	Apt         *AptParams
	Apk         *ApkParams
	CMake       *CMakeParams
	Pip         *PipParams
	Npm         *NpmParams
	Uv          *UvParams
	Extract     *ExtractParams
	CUDACollect *CUDACollectParams
	Build       *BuildParams
}

// AptParams installs Debian/Ubuntu packages, optionally from extra
// repositories set up first.
type AptParams struct {
	Packages     []string
	Recommends   bool
	Repositories []AptRepository
}

// AptRepository is one extra apt source, with its signing key pinned by
// digest. KeyFormat is "armored" or "" (binary), already defaulted.
type AptRepository struct {
	Name       string
	URL        string
	Suites     []string
	Components []string
	KeyURL     string
	// KeySHA256 is the bare hex digest, "sha256:" prefix already stripped
	// at lower time so no backend has to decide whether to strip it.
	KeySHA256 string
	KeyFormat string
}

// ApkParams installs Alpine packages. Alpine has no Recommends concept, so
// this is a separate type from AptParams rather than a shared one: Cache
// opts in to keeping /var/cache/apk in the layer.
type ApkParams struct {
	Packages     []string
	Cache        bool
	Repositories []string
}

// CMakeParams builds and installs one commit-pinned CMake project. Prefix
// and BuildType are defaulted at lower time, so a backend never has to know
// what "unset" means.
//
// Root is the scratch directory this install checks out and builds under,
// resolved from the install's position in its stage. It is resolved here for
// the same reason ExtractParams.Archive is: it depends on where the install
// sits among its siblings, and a backend that recounted that position could
// disagree with the one the key hashed.
type CMakeParams struct {
	Repository string
	Commit     string
	Prefix     string
	BuildType  string
	Defines    map[string]string
	Jobs       int
	Root       string
}

// PipParams installs from a requirements file, an explicit list, or both.
//
// Index is the group's effective index, already resolved: a group that
// declared cuda: carries the GPU profile's index here, not a flag saying to
// go look one up. That resolution is what lets the cache key cover which
// index the wheels came from without the key having to know about GPUs.
type PipParams struct {
	Requirements string
	Packages     []string
	Index        string
	ExtraIndex   []string
}

// NpmParams records the resolved manager, the manifest it reads, and the
// lockfile that pins its resolution — all resolved at lower time so no
// backend re-derives them and drifts.
//
// Manifest is named explicitly rather than assumed to be "package.json" by
// each backend, because it is a build input in its own right: the install
// reads scripts, engines, and dependency ranges from it, so editing it
// without touching the lockfile still changes the resulting filesystem and
// must therefore change the cache key.
type NpmParams struct {
	Manager    string
	Manifest   string
	Lockfile   string
	Production bool
}

// UvParams syncs from pyproject.toml + uv.lock. Files names them explicitly,
// for the same reason NpmParams.Manifest does: they are build inputs, and a
// key that did not cover them would serve a stale rootfs after a dependency
// edit.
type UvParams struct {
	Extras []string
	Dev    bool
	Files  []string
}

// ExtractParams unpacks an archive a Fetch node staged. Archive is the
// staging path that fetch wrote, resolved at lower time so the two nodes
// cannot disagree about the filename.
type ExtractParams struct {
	Archive string
	Dest    string
	Format  string
}

// CUDACollectParams links the CUDA runtime wheels' shared objects into one
// directory and registers it with the dynamic loader. Both paths are
// resolved from the pinned GPU profile at lower time.
type CUDACollectParams struct {
	LibDir   string
	ConfPath string
}

// BuildParams is a language compile step with its profile resolved.
type BuildParams struct {
	Lang    string
	Profile string
	Product string
	Script  string
}

// FetchOp downloads one URL into the stage. Checksum is mandatory and fully
// resolved at lower time ("sha256:<hex>"): the Stagefile's own pin, or
// failing that the one recorded in the lockfile. An unpinned fetch is not
// representable.
//
// Dest is where the bytes land — the declared destination, or the staging
// path when the download is extracted later in the stage.
type FetchOp struct {
	URL      string
	Dest     string
	Checksum string
	Mode     string
	Owner    string
}

// CopyOp promotes paths into the stage this node belongs to. When FromLocal
// is set the source is the build context and Inputs holds only the base;
// otherwise Inputs is [base, sourceStageFinalNode].
//
// Dest is the final destination, fully resolved at lower time: defaulted to
// the single source path when the spec omits it, and suffixed with "/" when
// more than one path is copied. Resolving it here rather than in a backend
// is what makes `copy: {paths: [app.py]}` and
// `copy: {paths: [app.py], dest: app.py}` — the same build — reach the same
// cache key.
type CopyOp struct {
	FromLocal bool
	Paths     []string
	Dest      string
	Owner     string
	Mode      string
}
