// Package recipe is the single definition of what each Stagefile install and
// build step actually does: the commands to run, the files it stages first,
// the network fetches it pins, and the cache directories it mounts.
//
// It exists because there is now more than one backend. codegen renders these
// steps to Dockerfile text; llbgen renders the same steps to BuildKit LLB. If
// either derived its own commands, the two backends would build subtly
// different images and only an end-to-end differential test would notice.
//
// Nothing here is Dockerfile syntax. A step says "run these shell clauses with
// these cache mounts" or "fetch this URL to this path against this digest";
// how a clause list becomes one instruction, and how a fetch becomes an ADD or
// an llb.HTTP source, belongs to a backend.
package recipe

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/stagefile/ir"
)

// CacheMount is a persistent build cache directory.
type CacheMount struct {
	Dir string
	// ID scopes what may share this mount. Empty means BuildKit's default,
	// which is to scope by target path — right for the package-manager
	// caches, whose contents are self-describing (a wheel names its own
	// platform), and wrong for a build tree, where the same path in two
	// different builds holds object files that must never meet.
	ID string
	// Locked serializes concurrent access. Wendy builds up to four services
	// at once on top of BuildKit's own parallelism; an unlocked mount lets
	// them collide inside the package manager, where the waiting is
	// invisible. Every mount this package emits is locked; the field exists
	// so a backend cannot emit one that isn't by forgetting to.
	Locked bool
}

// PreCopy stages build-context files into the image before the commands run.
// Dest is resolved here, not inferred by a backend from how many paths there
// are — both backends must place these files identically. pip stages a single
// requirements file to itself; npm and uv stage their manifest and lockfile
// together into "./".
type PreCopy struct {
	Paths []string
	Dest  string
}

// Fetch is a checksum-pinned network fetch the builder performs itself, so no
// shell runs inside the container and the bytes are verified before any layer
// can read them. Checksum is always "sha256:<hex>" and never empty: an
// unpinned fetch is not representable.
type Fetch struct {
	URL      string
	Dest     string
	Checksum string
	Mode     string
	Owner    string
}

// RunSpec is one execution step, backend-agnostic.
type RunSpec struct {
	// Command is the step's shell clauses, each already assembled and
	// shell-quoted. Most recipes have exactly one. apt has two — the install
	// and the list cleanup — and cmake has eight, kept separate rather than
	// pre-joined so each backend decides how to combine them: codegen renders
	// the tail as "\" continuation lines, matching the Dockerfile it has
	// always emitted, while a backend with no notion of Dockerfile line
	// breaks can join them with " && ". Every interpolated value is
	// shell-quoted by this package, and no spec field carries raw shell, so
	// nothing user-supplied reaches the shell unquoted.
	Command []string
	// PreCopy is the build-context files staged into the image before
	// Command runs, or nil if the step stages nothing.
	PreCopy     *PreCopy
	CacheMounts []CacheMount
}

// Step is one operation in a recipe. Exactly one of Run and Fetch is non-nil.
//
// A recipe is a sequence rather than a single run because apt's declared
// repositories interleave the two: bootstrap ca-certificates, fetch each
// pinned signing key, write each sources.list entry, then install.
type Step struct {
	Run   *RunSpec
	Fetch *Fetch
}

func run(cmd ...string) Step { return Step{Run: &RunSpec{Command: cmd}} }

// FetchFor translates a download node into the same Fetch shape a recipe's
// own fetches use, so a backend has one fetch renderer rather than two.
//
// ir.Lower refuses to build a fetch node without a resolved checksum, so the
// guard here only catches a hand-built graph — but an unpinned network fetch
// is exactly what this design excludes, so it must fail rather than render.
func FetchFor(f *ir.FetchOp) (Fetch, error) {
	if f.Checksum == "" {
		return Fetch{}, fmt.Errorf("recipe: fetch for %q has no checksum", f.URL)
	}
	return Fetch{
		URL:      f.URL,
		Dest:     f.Dest,
		Checksum: f.Checksum,
		Mode:     f.Mode,
		Owner:    f.Owner,
	}, nil
}

// For returns the steps for one exec op. platform is the stage's resolved
// target platform, used only to scope cache mounts — two architectures must
// not share a wheel cache or a build tree.
func For(x *ir.ExecOp, platform string) ([]Step, error) {
	switch {
	case x.Apt != nil:
		return aptSteps(x.Apt), nil
	case x.Apk != nil:
		return apkSteps(x.Apk), nil
	case x.CMake != nil:
		return cmakeSteps(x.CMake, platform), nil
	case x.Pip != nil:
		return pipSteps(x.Pip, platform), nil
	case x.Npm != nil:
		return npmSteps(x.Npm)
	case x.Uv != nil:
		return uvSteps(x.Uv, platform)
	case x.Extract != nil:
		return extractSteps(x.Extract), nil
	case x.CUDACollect != nil:
		return cudaCollectSteps(x.CUDACollect), nil
	case x.Build != nil:
		return buildSteps(x.Build)
	default:
		return nil, fmt.Errorf("recipe: exec op %q has no params", x.Recipe.Name)
	}
}

// ShellQuote wraps s in single quotes so shell metacharacters in it — including
// the ">" in an ordinary version specifier like "flask>=2.0" — are never given
// special meaning. Strictly more complete than a denylist: it needs no
// enumeration of "dangerous" characters, several of which are also legal and
// necessary in real package specifiers.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// ScopedCacheID hashes the parts that decide whether two builds can share a
// cache into an opaque BuildKit mount id. Callers pass every input that changes
// what lands in the cache and nothing else: too narrow a scope makes builds
// contend for no benefit, too wide a scope makes them miss each other's work.
func ScopedCacheID(kind string, parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		io.WriteString(h, p)
		h.Write([]byte{0})
	}
	return "stagefile-" + kind + "-" + hex.EncodeToString(h.Sum(nil))[:16]
}

// pipCacheID scopes pip's wheel cache to the index set it can download from and
// the platform whose wheels it stores.
//
// Both matter because the mount stays locked: without an id, BuildKit scopes by
// target path, so every pip install in every concurrently-built service queues
// on ONE lock — including installs that could never share a wheel because they
// pull from different indexes or build for different architectures. Scoping
// removes that false contention while keeping the real sharing.
//
// p.Index is the group's effective index, which for a cuda: group is the one
// ir.Lower took from the resolved GPU profile rather than from the Stagefile —
// scoping on the declared index would put every GPU group in the one
// unnamed-index cache alongside PyPI wheels, which is exactly the mixing this
// ID exists to prevent.
func pipCacheID(p *ir.PipParams, platform string) string {
	parts := append([]string{platform, p.Index}, p.ExtraIndex...)
	return ScopedCacheID("pip", parts...)
}

// cmakeCacheID scopes a cmake build tree to the project and the architecture it
// is compiled for, and deliberately to nothing else. Commit is excluded so
// bumping a pin recompiles only what changed — the reason the cache exists.
// Everything else the Stagefile can set (build type, prefix, defines) is an
// ordinary CMake cache variable that CMake reconfigures in place; the target
// platform is the one input it cannot detect, because the compiler sits at the
// same path in either rootfs.
func cmakeCacheID(repository, platform string) string {
	return ScopedCacheID("cmake", repository, platform)
}

// aptSteps sets up any declared repositories, then installs.
//
// The repository preamble is: a ca-certificates bootstrap (an https
// sources.list URL fails apt-get update without it — stock ubuntu/debian
// images don't ship it), the pinned signing key fetched by the builder itself
// so it can't drift, and one sources.list.d entry per repository.
func aptSteps(a *ir.AptParams) []Step {
	var steps []Step
	if len(a.Repositories) > 0 {
		steps = append(steps, run(
			"apt-get update && apt-get install -y --no-install-recommends ca-certificates",
			"rm -rf /var/lib/apt/lists/*",
		))
		for _, r := range a.Repositories {
			ext := ".gpg"
			if r.KeyFormat == "armored" {
				ext = ".asc"
			}
			keyring := "/etc/apt/keyrings/" + r.Name + ext
			steps = append(steps, Step{Fetch: &Fetch{
				URL: r.KeyURL,
				// ir.Lower strips any "sha256:" the Stagefile wrote, so
				// there is one spelling to render and to hash.
				Checksum: "sha256:" + r.KeySHA256,
				Dest:     keyring,
				Mode:     "0644",
			}})
			var srcLines []string
			for _, suite := range r.Suites {
				srcLines = append(srcLines, ShellQuote(fmt.Sprintf("deb [signed-by=%s] %s %s %s",
					keyring, r.URL, suite, strings.Join(r.Components, " "))))
			}
			steps = append(steps, run(fmt.Sprintf("printf '%%s\\n' %s > /etc/apt/sources.list.d/%s.list",
				strings.Join(srcLines, " "), r.Name)))
		}
	}

	parts := []string{"apt-get", "update", "&&", "apt-get", "install", "-y"}
	if !a.Recommends {
		parts = append(parts, "--no-install-recommends")
	}
	for _, p := range a.Packages {
		parts = append(parts, ShellQuote(p))
	}
	return append(steps, run(strings.Join(parts, " "), "rm -rf /var/lib/apt/lists/*"))
}

func apkSteps(a *ir.ApkParams) []Step {
	var steps []Step
	if len(a.Repositories) > 0 {
		var quoted []string
		for _, r := range a.Repositories {
			quoted = append(quoted, ShellQuote(r))
		}
		steps = append(steps, run(fmt.Sprintf("printf '%%s\\n' %s >> /etc/apk/repositories", strings.Join(quoted, " "))))
	}
	parts := []string{"apk", "add"}
	if !a.Cache {
		parts = append(parts, "--no-cache")
	}
	for _, p := range a.Packages {
		parts = append(parts, ShellQuote(p))
	}
	return append(steps, run(strings.Join(parts, " ")))
}

func cmakeSteps(c *ir.CMakeParams, platform string) []Step {
	// Root, Prefix, and BuildType are used verbatim: ir.Lower resolved the
	// scratch path from the install's position in its stage and defaulted the
	// other two, so re-deriving any of them here would let a backend drift
	// from what the cache key hashed.
	sourceDir := c.Root + "/source"
	buildDir := c.Root + "/build"

	configure := []string{
		"cmake", "-S", ShellQuote(sourceDir), "-B", ShellQuote(buildDir),
		ShellQuote("-DCMAKE_BUILD_TYPE=" + c.BuildType),
		ShellQuote("-DCMAKE_INSTALL_PREFIX=" + c.Prefix),
	}
	keys := make([]string, 0, len(c.Defines))
	for k := range c.Defines {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		configure = append(configure, ShellQuote("-D"+k+"="+c.Defines[k]))
	}

	build := "cmake --build " + ShellQuote(buildDir)
	if c.Jobs > 0 {
		build += " --parallel " + strconv.Itoa(c.Jobs)
	}

	return []Step{{Run: &RunSpec{
		Command: []string{
			"git init " + ShellQuote(sourceDir),
			"git -C " + ShellQuote(sourceDir) + " remote add origin " + ShellQuote(c.Repository),
			"git -C " + ShellQuote(sourceDir) + " fetch --depth 1 origin " + ShellQuote(c.Commit),
			"git -C " + ShellQuote(sourceDir) + " checkout --detach FETCH_HEAD",
			strings.Join(configure, " "),
			build,
			"cmake --install " + ShellQuote(buildDir),
			// Only the source tree is removed. The build tree is a cache
			// mount and never enters the layer, so deleting it would throw
			// away the object files the next commit bump wants — while
			// removing nothing from the image.
			"rm -rf " + ShellQuote(sourceDir),
		},
		CacheMounts: []CacheMount{{Dir: buildDir, ID: cmakeCacheID(c.Repository, platform), Locked: true}},
	}}}
}

func pipSteps(p *ir.PipParams, platform string) []Step {
	var preCopy *PreCopy
	if p.Requirements != "" {
		preCopy = &PreCopy{Paths: []string{p.Requirements}, Dest: p.Requirements}
	}
	// No --no-cache-dir here: the cache mount below already keeps pip's cache
	// out of the image layer, and disabling the cache would force a full wheel
	// re-download every time this layer rebuilds.
	parts := []string{"pip", "install"}
	if p.Index != "" {
		parts = append(parts, "--index-url", ShellQuote(p.Index))
	}
	for _, u := range p.ExtraIndex {
		parts = append(parts, "--extra-index-url", ShellQuote(u))
	}
	if p.Requirements != "" {
		parts = append(parts, "-r", ShellQuote(p.Requirements))
	}
	for _, pkg := range p.Packages {
		parts = append(parts, ShellQuote(pkg))
	}
	return []Step{{Run: &RunSpec{
		Command:     []string{strings.Join(parts, " ")},
		PreCopy:     preCopy,
		CacheMounts: []CacheMount{{Dir: "/root/.cache/pip", ID: pipCacheID(p, platform), Locked: true}},
	}}}
}

func npmSteps(n *ir.NpmParams) ([]Step, error) {
	// Both filenames come from the node, not from a constant here, so a
	// backend copies exactly the files the cache key hashed. An empty one
	// would stage nothing under a name the install then can't find, so refuse
	// rather than emit it — ir.Lower always sets both.
	if n.Manifest == "" || n.Lockfile == "" {
		return nil, fmt.Errorf("npm node has manifest %q and lockfile %q; both are required", n.Manifest, n.Lockfile)
	}
	var cmd, cacheDir string
	switch n.Manager {
	case "yarn":
		cmd, cacheDir = "yarn install --frozen-lockfile", "/root/.cache/yarn"
		if n.Production {
			cmd += " --production"
		}
	case "pnpm":
		cmd, cacheDir = "pnpm install --frozen-lockfile", "/root/.local/share/pnpm/store"
		if n.Production {
			cmd += " --prod"
		}
	default:
		cmd, cacheDir = "npm ci", "/root/.npm"
		if n.Production {
			cmd += " --omit=dev"
		}
	}
	return []Step{{Run: &RunSpec{
		Command:     []string{cmd},
		PreCopy:     &PreCopy{Paths: []string{n.Manifest, n.Lockfile}, Dest: "./"},
		CacheMounts: []CacheMount{{Dir: cacheDir, Locked: true}},
	}}}, nil
}

func uvSteps(u *ir.UvParams, platform string) ([]Step, error) {
	// Same contract as npmSteps: the filenames come from the node, so a
	// backend stages exactly what the key hashed.
	if len(u.Files) == 0 {
		return nil, fmt.Errorf("uv node names no files to stage; both the key and the install need them")
	}
	parts := []string{"uv", "sync", "--frozen"}
	if !u.Dev {
		parts = append(parts, "--no-dev")
	}
	for _, e := range u.Extras {
		parts = append(parts, "--extra", ShellQuote(e))
	}
	return []Step{{Run: &RunSpec{
		Command:     []string{strings.Join(parts, " ")},
		PreCopy:     &PreCopy{Paths: u.Files, Dest: "./"},
		CacheMounts: []CacheMount{{Dir: "/root/.cache/uv", ID: ScopedCacheID("uv", platform), Locked: true}},
	}}}, nil
}

// extractSteps unpacks an archive a fetch staged. A remote fetch never
// auto-extracts the way a local tarball does, so unpacking is necessarily its
// own step — which is also why it runs after install: and can therefore use a
// tool (unzip) that install.apt declared.
func extractSteps(x *ir.ExtractParams) []Step {
	staged := ShellQuote(x.Archive)
	dest := ShellQuote(x.Dest)
	var unpack string
	switch x.Format {
	case "zip":
		unpack = fmt.Sprintf("unzip -q %s -d %s", staged, dest)
	default:
		unpack = fmt.Sprintf("tar -xzf %s -C %s", staged, dest)
	}
	// The staged archive is removed in the same step that unpacks it: left to
	// a later layer it would still be in this one, and the image would carry
	// both the tarball and its contents. These are one clause rather than
	// three because a backend that split them into separate layers would
	// reintroduce exactly that.
	return []Step{run(fmt.Sprintf("mkdir -p %s && %s && rm %s", dest, unpack, staged))}
}

// cudaCollectSteps symlinks every shared object the CUDA runtime wheels
// installed into the profile's LibDir, registers that directory with the
// dynamic loader, and refreshes the cache.
//
// This is the step nobody would think to write. On a JetPack-7 device the
// container runtime injects the host's CUDA 13 via CDI; the wheels installed
// above are CUDA 12. Their sonames differ where that is lucky (libcudart.so.12
// vs .13) and collide where it is not (libcudnn.so.9 exists in both). Without
// this the loader satisfies some of a framework's dependencies from the wheel
// and others from the host, and the failure surfaces on the device as a
// missing symbol — nowhere near the build.
//
// The wheel directory is located by asking Python where it put the package
// rather than by naming a dist-packages path, because that path carries the
// base image's Python version and would silently find nothing after a base
// image bump. `find -exec ln -sf` is the compiler's own command assembled from
// typed fields — no Stagefile string reaches the shell.
func cudaCollectSteps(c *ir.CUDACollectParams) []Step {
	dir := ShellQuote(c.LibDir)
	return []Step{run(
		"mkdir -p "+dir,
		// python3 -c is the compiler's literal, and importing the package pip
		// just installed is also the check that it is there: a GPU stage whose
		// runtime failed to install fails here, not on the device.
		`NVIDIA_DIR="$(python3 -c 'import nvidia, os; print(os.path.dirname(nvidia.__file__))')"`,
		// -exec ln -sf {} dir/ ';' — the trailing slash makes ln treat the
		// destination as a directory, so each link keeps the library's own
		// name rather than overwriting a single file.
		fmt.Sprintf(`find "$NVIDIA_DIR" -name '*.so*' -exec ln -sf '{}' %s ';'`, ShellQuote(c.LibDir+"/")),
		fmt.Sprintf("printf '%%s\\n' %s > %s", dir, ShellQuote(c.ConfPath)),
		"ldconfig",
	)}
}

func buildSteps(b *ir.BuildParams) ([]Step, error) {
	cache := func(dir string) []CacheMount { return []CacheMount{{Dir: dir, Locked: true}} }
	switch b.Lang {
	case "rust":
		cmd := "cargo build"
		if b.Profile == "release" {
			cmd += " --release"
		}
		if b.Product != "" {
			cmd += " --bin " + ShellQuote(b.Product)
		}
		return []Step{{Run: &RunSpec{Command: []string{cmd}, CacheMounts: cache("/root/.cargo")}}}, nil
	case "go":
		cmd := "go build ./..."
		if b.Product != "" {
			// -o with a trailing slash writes the package's binary (named
			// after the package) into that directory.
			cmd = "go build -o /usr/local/bin/ " + ShellQuote(b.Product)
		}
		return []Step{{Run: &RunSpec{Command: []string{cmd}, CacheMounts: cache("/root/.cache/go-build")}}}, nil
	case "swift":
		// Always spell the configuration out. Bare `swift build` already means
		// debug, so an implicit debug build is indistinguishable from someone
		// having forgotten the flag — both in the generated Dockerfile and to
		// the optimizer check that scans for it.
		cmd := "swift build -c " + b.Profile
		if b.Product != "" {
			cmd += " --product " + ShellQuote(b.Product)
		}
		return []Step{{Run: &RunSpec{Command: []string{cmd}, CacheMounts: cache("/root/.swiftpm")}}}, nil
	case "npm", "yarn", "pnpm":
		cacheDir := map[string]string{
			"npm":  "/root/.npm",
			"yarn": "/root/.cache/yarn",
			"pnpm": "/root/.local/share/pnpm/store",
		}[b.Lang]
		return []Step{{Run: &RunSpec{
			Command:     []string{fmt.Sprintf("%s run %s", b.Lang, ShellQuote(b.Script))},
			CacheMounts: cache(cacheDir),
		}}}, nil
	default:
		// ir.Lower already rejects unknown languages before a graph can exist,
		// but a backend that trusts its input silently is how a future caller
		// that builds a graph directly (bypassing Lower) gets a wrong build
		// instead of an error.
		return nil, fmt.Errorf("unsupported build.lang %q (supported: rust, go, swift, npm, yarn, pnpm)", b.Lang)
	}
}
