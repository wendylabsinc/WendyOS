// Package stagefile is the library facade for the Stagefile compiler — a
// YAML build descriptor (build.stagefile.yaml) that compiles to a real
// Dockerfile with structural safety guarantees a hand-written Dockerfile
// doesn't get by default (lockfile digest-pinning, shell-safe quoting, no
// raw-shell escape hatch). It exposes a single entry point, CompileFile.
// Vendored from github.com/joannisorlandos/stagefile (same author) so
// wendy build/wendy run has no external dependency on that private repo.
package stagefile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/stagefile/codegen"
	dockerignorepkg "github.com/wendylabsinc/wendy/go/internal/stagefile/dockerignore"
	"github.com/wendylabsinc/wendy/go/internal/stagefile/gpu"
	"github.com/wendylabsinc/wendy/go/internal/stagefile/ir"
	"github.com/wendylabsinc/wendy/go/internal/stagefile/lock"
	"github.com/wendylabsinc/wendy/go/internal/stagefile/spec"
)

// baseResolver is the underlying registry lookup every CompileFile ultimately
// reaches. It is a package var purely so tests can exercise the memoization in
// sharedResolver without touching a live registry.
var baseResolver lock.Resolver = lock.CraneResolver

// baseHasher is the underlying download hash every CompileFile ultimately
// reaches, and a package var for the same reason baseResolver is: so tests
// can exercise the memoization without fetching anything.
var baseHasher lock.Hasher = lock.HTTPHasher

// sharedResolver is the process-wide resolver CompileFile uses. Memoizing here
// rather than per-call is what makes a compose project cheap: its services each
// compile their own Stagefile, they typically share a base image, and those
// compiles run concurrently — so without a shared memo they would all issue the
// same registry lookup simultaneously. The indirection through baseResolver
// keeps the memo established once while leaving the underlying lookup
// swappable in tests.
var sharedResolver = lock.Memoize(func(ref string) (string, error) {
	return baseResolver(ref)
})

// sharedHasher memoizes download hashing for the same reason sharedResolver
// memoizes registry lookups, and it matters more here: the compose services
// of one project can declare the same model URL, and hashing it twice means
// downloading it twice.
var sharedHasher = lock.Hasher(lock.Memoize(func(url string) (string, error) {
	return baseHasher(url)
}))

// Option configures a CompileFile call.
type Option func(*options)

type options struct {
	progress     func(url string)
	gpuArch      string
	buildProfile string
	source       string
	ros2Distro   string
	ros2RMW      string
}

// WithSource names which Stagefile in dir to compile, for a project that
// carries several (see SourceNames). Defaults to the canonical SourceName.
// An unrecognised name is not silently ignored — CompileFile rejects it, so a
// typo'd variant fails here rather than compiling the wrong file.
func WithSource(name string) Option {
	return func(o *options) { o.source = name }
}

// BuildProfileDebug and BuildProfileRelease are the two compile profiles a
// Stagefile build stage understands.
const (
	BuildProfileRelease = "release"
	BuildProfileDebug   = "debug"
)

// WithGPUArch names the GPU architecture (the gpu_arch a device reports, e.g.
// "sm_87") this build targets. It is required if any stage declares cuda: and
// ignored otherwise, which is why it is an option rather than a parameter:
// the overwhelming majority of builds have no GPU stage and should not have to
// answer the question.
func WithGPUArch(arch string) Option {
	return func(o *options) { o.gpuArch = arch }
}

// WithProgress registers a callback invoked before each download that has to
// be fetched to be pinned. Without it a first build hashing a few hundred
// megabytes of model weights is indistinguishable from a hang: resolution
// runs inline inside CompileFile, which otherwise prints nothing at all.
func WithProgress(f func(url string)) Option {
	return func(o *options) { o.progress = f }
}

// WithBuildProfile overrides the profile of every build stage that has one, so
// `wendy run --debug` produces a debuggable binary from a Stagefile whose
// checked-in profile is release. Stages of a language with no release/debug
// notion (go, npm/yarn/pnpm) are unaffected.
//
// Only "release" and "debug" are accepted; any other value is ignored rather
// than applied, because this override lands after spec validation and the
// profile is interpolated into the generated RUN line.
func WithBuildProfile(profile string) Option {
	return func(o *options) {
		if profile == BuildProfileRelease || profile == BuildProfileDebug {
			o.buildProfile = profile
		}
	}
}

// WithROS2Runtime teaches the compiler about the runtime framework selected in
// wendy.json. Stagefiles describe application dependencies; they should not
// have to repeat the middleware package implied by frameworks.ros2. The CLI
// passes resolved, validated values here and the compiler idempotently adds the
// matching package to the final stage's APT install.
func WithROS2Runtime(distro, rmw string) Option {
	return func(o *options) {
		o.ros2Distro = strings.ToLower(strings.TrimSpace(distro))
		o.ros2RMW = strings.ToLower(strings.TrimSpace(rmw))
	}
}

// CompileFile reads a Stagefile from dir — the canonical build.stagefile.yaml
// unless WithSource names a variant — resolves any missing lockfile image refs
// against a live registry (existing pins are never touched — only an explicit
// re-lock changes them), writes/updates that source's lockfile in dir, and
// returns the compiled Dockerfile text and the derived .dockerignore text.
//
// Safe to call concurrently for different directories: the lockfile and both
// generated files are written via temp-file + rename, and the registry lookups
// behind sharedResolver are deduplicated across callers. Two variants in the
// SAME directory are equally safe, because each owns a distinct lockfile.
func CompileFile(dir, platform string, opts ...Option) (dockerfile, dockerignore string, err error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	source := o.source
	if source == "" {
		source = SourceName
	}
	if !IsSourceName(source) {
		return "", "", fmt.Errorf("%q is not a Stagefile name: expected %s or a variant of it such as prod%s", source, SourceName, sourceSuffix)
	}
	hasher := sharedHasher
	if o.progress != nil {
		hasher = func(url string) (string, error) {
			o.progress(url)
			return sharedHasher(url)
		}
	}
	return compileFileWithFramework(dir, source, platform, o.gpuArch, o.buildProfile, o.ros2Distro, o.ros2RMW, sharedResolver, hasher)
}

// NeedsGPUTarget reports whether ANY Stagefile in dir declares a cuda: stage,
// and therefore cannot be compiled without knowing the GPU architecture it is
// being built for.
//
// The CLI asks before it compiles: a GPU project has to resolve its target
// device first, while every other project keeps the cheaper ordering that
// compiles without connecting to anything. Because the answer is needed before
// the build file has been chosen, it deliberately spans the whole family rather
// than one variant — the cost of being wrong is one extra GetAgentVersion RPC,
// against a variant build that would otherwise fail with "a stage declares
// cuda: but this build has no GPU target".
//
// A missing or unparseable Stagefile is not this function's error to report —
// it answers false and lets the compile produce the real diagnostic.
func NeedsGPUTarget(dir string) bool {
	for _, name := range SourceNames(dir) {
		if NeedsGPUTargetFile(dir, name) {
			return true
		}
	}
	return false
}

// NeedsGPUTargetFile is NeedsGPUTarget for one named Stagefile in dir.
//
// source is validated the same way CompileFile validates it, so the exported
// pair cannot disagree about what counts as a Stagefile. It also keeps
// filepath.Join from resolving a caller's "../" segments into a read outside
// dir: the grammar admits no dots or separators, so a name that reaches the
// Join is always a bare filename.
func NeedsGPUTargetFile(dir, source string) bool {
	if !IsSourceName(source) {
		return false
	}
	raw, err := os.ReadFile(filepath.Join(dir, source))
	if err != nil {
		return false
	}
	f, err := spec.Parse(raw)
	if err != nil {
		return false
	}
	for _, s := range f.Stages {
		if s.CUDA {
			return true
		}
	}
	return false
}

// resolveCUDAProfile returns the GPU profile the stages in f need, or nil if
// none declares cuda:. A GPU stage with no target is an error here rather
// than a CPU-only image, because the difference would only show up on the
// device.
func resolveCUDAProfile(f *spec.File, arch string, l *lock.File) (*gpu.Profile, error) {
	needed := false
	for _, s := range f.Stages {
		if s.CUDA {
			needed = true
			break
		}
	}
	if !needed {
		return nil, nil
	}
	if arch == "" {
		return nil, fmt.Errorf(
			"a stage declares cuda: but this build has no GPU target; run against a device (`wendy run --device ...`), which reads its gpu_arch, or pass --gpu-arch (known: %s)",
			strings.Join(gpu.KnownArches(), ", "))
	}
	p, err := l.ResolveCUDA(arch)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// compileFile is the resolver-injectable implementation behind
// CompileFile, allowing tests to exercise it with a fake resolver and hasher
// instead of a live registry and live URLs.
func compileFile(dir, source, platform, gpuArch, buildProfile string, resolver lock.Resolver, hasher lock.Hasher) (dockerfile, dockerignore string, err error) {
	return compileFileWithFramework(dir, source, platform, gpuArch, buildProfile, "", "", resolver, hasher)
}

func compileFileWithFramework(dir, source, platform, gpuArch, buildProfile, ros2Distro, ros2RMW string, resolver lock.Resolver, hasher lock.Hasher) (dockerfile, dockerignore string, err error) {
	sourcePath := filepath.Join(dir, source)
	raw, err := os.ReadFile(sourcePath)
	if err != nil {
		return "", "", fmt.Errorf("reading %s: %w", sourcePath, err)
	}
	f, err := spec.Parse(raw)
	if err != nil {
		return "", "", err
	}
	applyBuildProfile(f, buildProfile)
	applyROS2Runtime(f, ros2Distro, ros2RMW)

	lockPath := filepath.Join(dir, LockName(source))
	existing, err := lock.Load(lockPath)
	if err != nil {
		return "", "", err
	}
	updated, _, err := lock.Resolve(existing, spec.SourceHash(raw), imageRefs(f), nil, resolver)
	if err != nil {
		return "", "", err
	}
	if _, err := updated.ResolveDownloads(downloadURLs(f), nil, hasher); err != nil {
		return "", "", err
	}
	// Resolved before Save so a first GPU build records its profile in the
	// same write as its image digests, and after Resolve so it can reuse a
	// profile an earlier build already pinned.
	cudaProfile, err := resolveCUDAProfile(f, gpuArch, updated)
	if err != nil {
		return "", "", err
	}
	if err := updated.Save(lockPath); err != nil {
		return "", "", err
	}

	// The project directory is the cache scope: it is what makes two different
	// projects' compiler caches distinct, and it is stable across the rebuilds
	// of one project that the caches exist to speed up. An absolute path means
	// moving a checkout starts from a cold cache, which is the right trade —
	// the alternative keys are either not unique per project or not stable
	// across an edit.
	absDir, err := filepath.Abs(dir)
	if err != nil {
		absDir = dir
	}
	g, err := ir.Lower(f, ir.Options{
		Images: updated.Images, Downloads: updated.Downloads, Platform: platform,
		CUDAProfile: cudaProfile, CacheScope: absDir,
	})
	if err != nil {
		return "", "", err
	}
	dockerfile, err = codegen.GenerateGraph(g, updated.Images)
	if err != nil {
		return "", "", err
	}
	localPaths, err := dockerignorepkg.LocalPathsFromGraph(g)
	if err != nil {
		return "", "", err
	}
	dockerignore = dockerignorepkg.Derive(localPaths)
	return dockerfile, dockerignore, nil
}

// applyROS2Runtime appends the RMW implementation package implied by the
// framework config. The final stage is the runnable image; build-only stages do
// not need the middleware. Package names are constructed only from values the
// appconfig validator has already reduced to its fixed RMW allowlist.
func applyROS2Runtime(f *spec.File, distro, rmw string) {
	if f == nil || len(f.Stages) == 0 || distro == "" || rmw == "" {
		return
	}
	// Keep the compiler-side boundary closed even though normal CLI callers
	// already pass values validated by appconfig. WithROS2Runtime is a public
	// library option and must not turn arbitrary strings into APT packages.
	packageSuffix := map[string]string{
		"rmw_cyclonedds_cpp": "rmw-cyclonedds-cpp",
		"rmw_fastrtps_cpp":   "rmw-fastrtps-cpp",
		"rmw_connextdds":     "rmw-connextdds",
		"rmw_gurumdds_cpp":   "rmw-gurumdds-cpp",
	}[rmw]
	if packageSuffix == "" || !validROS2Distro(distro) {
		return
	}
	pkg := "ros-" + distro + "-" + packageSuffix
	final := &f.Stages[len(f.Stages)-1]
	if final.Install == nil {
		final.Install = &spec.Install{}
	}
	if final.Install.Apt == nil {
		final.Install.Apt = &spec.AptInstall{}
	}
	for _, existing := range final.Install.Apt.Packages {
		if existing == pkg {
			return
		}
	}
	final.Install.Apt.Packages = append(final.Install.Apt.Packages, pkg)
}

func validROS2Distro(distro string) bool {
	for i, r := range distro {
		if (r < 'a' || r > 'z') && (i == 0 || r < '0' || r > '9') {
			return false
		}
	}
	return distro != ""
}

// applyBuildProfile overrides the compile profile of every build stage that has
// one. Node package scripts are skipped: spec validation rejects a profile on
// them outright, so setting one here would generate a Stagefile the same file
// could not have declared. Go is skipped for the same reason it ignores
// profile in codegen — `go build` has no release/debug split.
func applyBuildProfile(f *spec.File, profile string) {
	if profile == "" {
		return
	}
	for i := range f.Stages {
		b := f.Stages[i].Build
		if b == nil {
			continue
		}
		switch b.Lang {
		case "rust", "swift":
			b.Profile = profile
		}
	}
}

// imageRefs collects every distinct from: value across f's stages, in
// file order, without duplicates. Stages that opt out of digest pinning
// (pin: false — local-only images with no registry digest) are skipped so
// the resolver never tries to look them up.
// downloadURLs collects every download url across f's stages that needs
// resolving, in file order, without duplicates. A download that carries its
// own sha256 is skipped: it is already pinned, and fetching it here would
// download the file just to confirm what the Stagefile already states.
func downloadURLs(f *spec.File) []string {
	seen := map[string]bool{}
	var urls []string
	for _, s := range f.Stages {
		for _, d := range s.Download {
			if d.SHA256 != "" || seen[d.URL] {
				continue
			}
			seen[d.URL] = true
			urls = append(urls, d.URL)
		}
	}
	return urls
}

func imageRefs(f *spec.File) []string {
	seen := map[string]bool{}
	priorStages := map[string]bool{}
	var refs []string
	for _, s := range f.Stages {
		if priorStages[s.From] || s.Pin != nil && !*s.Pin {
			priorStages[s.Name] = true
			continue
		}
		if !seen[s.From] {
			seen[s.From] = true
			refs = append(refs, s.From)
		}
		priorStages[s.Name] = true
	}
	return refs
}
