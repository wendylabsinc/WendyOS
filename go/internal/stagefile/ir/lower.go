package ir

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/stagefile/gpu"
	"github.com/wendylabsinc/wendy/go/internal/stagefile/spec"
)

// supportedLangs mirrors the languages codegen can render. Lower rejects
// anything else here so an unsupported language fails at lowering rather
// than surfacing from a backend deep in the pipeline.
var supportedLangs = map[string]bool{
	"rust": true, "go": true, "swift": true,
	"npm": true, "yarn": true, "pnpm": true,
}

// Options carries the build-time facts a Stagefile does not contain but
// lowering must resolve against.
//
// All three are pinned inputs, not ambient state. Downloads and CUDAProfile
// come from the lockfile, and Platform is the architecture the caller is
// building for. They are resolved here, at lower time, rather than in a
// backend so that everything the cache key hashes is the same value the
// Dockerfile renders — a backend that re-derived any of them could drift
// from the key while still producing a Dockerfile that builds.
type Options struct {
	// Images maps external image refs to lockfile digests. It is used in the
	// semantic graph for cache-mount scoping as well as by cache-key callers.
	Images map[string]string
	// Platform is the target platform (e.g. "linux/arm64"), or "" to let
	// the builder decide.
	Platform string
	// Downloads maps a download URL to the sha256 resolved for it, for
	// downloads that declared none of their own.
	Downloads map[string]string
	// CUDAProfile is the GPU profile resolved for Platform. Required if any
	// stage declares cuda:.
	CUDAProfile *gpu.Profile
	// CacheScope is a stable project identity for compiler caches that hold
	// project-specific object files (currently Swift's scratch tree).
	CacheScope string
}

// Lower converts a validated spec.File into a Graph.
//
// Node order within a stage matches the order a backend must emit, and that
// ordering is load-bearing in three places:
//
//   - Fetches come first. A download is the largest and most stable thing in
//     a stage; behind the installs, a bumped pip package would re-fetch every
//     model.
//   - Extraction comes after the installs, because it is the only position
//     where `extract: zip` can rely on unzip having been declared in
//     install.apt.packages.
//   - Copy comes after every install and before build, so that editing app
//     source never reruns an install. Reversing install and copy was a real
//     defect once already (see codegen/golden_test.go).
func Lower(f *spec.File, opts Options) (*Graph, error) {
	g := &Graph{}
	stageFinal := map[string]int{}

	for stageIndex, s := range f.Stages {
		if s.CUDA && opts.CUDAProfile == nil {
			return nil, fmt.Errorf("stage %q declares cuda: but no GPU target was resolved for this build", s.Name)
		}

		platform := opts.Platform
		if s.Platform == "build" {
			platform = "$BUILDPLATFORM"
		}
		install := s.Install
		if install == nil {
			install = &spec.Install{}
		}

		// Large CUDA wheels and user pip dependencies are sibling stages. Their
		// linked copies keep APT/application edits out of those cache chains.
		cudaStage := -1
		if s.CUDA {
			name := generatedStageName("stagefile-cuda-runtime", stageIndex, f.Stages)
			cudaStage = addGeneratedCUDAStage(g, name, stageIndex, s, platform, stageFinal, opts.Images, opts.CUDAProfile)
		}
		pipStage := -1
		if len(install.Pip) > 0 {
			name := generatedStageName("stagefile-pip-deps", stageIndex, f.Stages)
			pipStage = addGeneratedPipStage(g, name, stageIndex, s, platform, stageFinal, opts.Images, install, opts.CUDAProfile)
		}

		image := Node{Kind: OpImage, Image: &ImageOp{
			Ref: s.From,
			// An absent pin: means pinned; `pin: false` is the visible
			// deviation. Resolving the tri-state pointer here means no
			// backend has to know that nil means true.
			Unpinned: s.Pin != nil && !*s.Pin,
			Platform: platform,
			// Cloned, like every other spec-owned value below: a caller
			// that patches its spec.File in place and lowers again must not
			// silently corrupt this graph — and now that a graph is hashed
			// into a cache key, corrupting it means corrupting the key too.
			Args:    maps.Clone(s.Args),
			Env:     stageEnv(&s, opts.CUDAProfile),
			Workdir: s.Workdir,
		}}
		if from, ok := stageFinal[s.From]; ok {
			image.Image.FromStage = true
			image.Image.Unpinned = true
			image.Inputs = []int{from}
		}
		cur := g.add(image)

		fetches, err := fetchNodes(s.Download, opts.Downloads)
		if err != nil {
			return nil, fmt.Errorf("stage %q: %w", s.Name, err)
		}
		for _, fo := range fetches {
			cur = g.add(Node{Kind: OpFetch, Inputs: []int{cur}, Fetch: fo})
		}

		if install.Apt != nil {
			apt := aptParams(install.Apt)
			if !image.Image.FromStage && !image.Image.Unpinned {
				apt.Base = resolvedBase(s.From, opts.Images)
			}
			cur = g.add(execNode(cur, &ExecOp{Recipe: RecipeApt, Apt: apt}))
		}
		if install.Apk != nil {
			cur = g.add(execNode(cur, &ExecOp{Recipe: RecipeApk, Apk: &ApkParams{
				Packages:     slices.Clone(install.Apk.Packages),
				Cache:        install.Apk.Cache,
				Repositories: slices.Clone(install.Apk.Repositories),
			}}))
		}
		for i, c := range install.CMake {
			cur = g.add(execNode(cur, &ExecOp{Recipe: RecipeCMake, CMake: cmakeParams(i, c)}))
		}
		if cudaStage >= 0 {
			cur = g.add(Node{Kind: OpCopy, Inputs: []int{cur, cudaStage}, Copy: &CopyOp{
				Paths: []string{CUDAPythonRoot}, Dest: CUDAPythonRoot, Link: true,
			}})
		}
		if pipStage >= 0 {
			cur = g.add(Node{Kind: OpCopy, Inputs: []int{cur, pipStage}, Copy: &CopyOp{
				Paths: []string{PipOverlayRoot + "/"}, Dest: "/", Link: true,
			}})
		}
		if install.Npm != nil {
			manager := install.Npm.Manager
			if manager == "" {
				manager = "npm"
			}
			cur = g.add(execNode(cur, &ExecOp{Recipe: RecipeNpm, Npm: &NpmParams{
				Manager: manager,
				// spec.NpmManifest is the file every supported manager reads
				// alongside its lockfile; it is the single source of truth
				// dockerignore also reads, so backends and the cache key
				// agree with the build context on one value.
				Manifest: spec.NpmManifest,
				// Derived from the defaulted manager, not the raw spec
				// value: NpmLockfile("") and NpmLockfile("npm") agree today,
				// but deriving the two fields of one params struct from two
				// different values is how they drift apart later.
				Lockfile:   spec.NpmLockfile(manager),
				Production: install.Npm.Production,
			}}))
		}
		if install.Uv != nil {
			cur = g.add(execNode(cur, &ExecOp{Recipe: RecipeUv, Uv: &UvParams{
				Extras: slices.Clone(install.Uv.Extras),
				Dev:    install.Uv.Dev,
				Files:  slices.Clone(spec.UvLocalFiles),
			}}))
		}

		for i, d := range s.Download {
			if d.Extract == "" {
				continue
			}
			cur = g.add(execNode(cur, &ExecOp{Recipe: RecipeExtract, Extract: &ExtractParams{
				Archive: downloadStagingPath(i, d.Extract),
				Dest:    d.Dest,
				Format:  d.Extract,
			}}))
		}

		// Collection runs after every install (it reads what they produced)
		// and before copy: (so editing app source never reruns it).
		if s.CUDA {
			cur = g.add(execNode(cur, &ExecOp{Recipe: RecipeCUDACollect, CUDACollect: &CUDACollectParams{
				LibDir:   opts.CUDAProfile.LibDir,
				ConfPath: CUDAConfPath,
			}}))
		}

		for _, c := range s.Copy {
			dest, err := copyDest(c)
			if err != nil {
				return nil, fmt.Errorf("stage %q: %w", s.Name, err)
			}
			n := Node{Kind: OpCopy, Inputs: []int{cur}, Copy: &CopyOp{
				Paths: slices.Clone(c.Paths),
				Dest:  dest,
				Owner: c.Owner,
				Mode:  c.Mode,
			}}
			if c.From == "local" {
				n.Copy.FromLocal = true
			} else {
				src, ok := stageFinal[c.From]
				if !ok {
					return nil, fmt.Errorf("stage %q: copy from unknown stage %q", s.Name, c.From)
				}
				n.Inputs = append(n.Inputs, src)
			}
			cur = g.add(n)
		}

		if s.Build != nil {
			if !supportedLangs[s.Build.Lang] {
				return nil, fmt.Errorf("stage %q: unsupported build.lang %q (supported: go, rust, swift, npm, yarn, pnpm)", s.Name, s.Build.Lang)
			}
			profile := s.Build.Profile
			if profile == "" {
				profile = "release"
			}
			script := s.Build.Script
			if script == "" {
				script = "build"
			}
			cur = g.add(execNode(cur, &ExecOp{Recipe: RecipeBuild, Build: &BuildParams{
				Lang:       s.Build.Lang,
				Profile:    profile,
				Product:    s.Build.Product,
				Script:     script,
				From:       s.From,
				CacheScope: opts.CacheScope,
			}}))
		}

		st := Stage{Name: s.Name, Final: cur, SourceIndex: stageIndex, User: stageUser(&s)}
		if s.Healthcheck != nil {
			st.Healthcheck = &Healthcheck{
				Exec:        slices.Clone(s.Healthcheck.Exec),
				Interval:    s.Healthcheck.Interval,
				Timeout:     s.Healthcheck.Timeout,
				StartPeriod: s.Healthcheck.StartPeriod,
				Retries:     s.Healthcheck.Retries,
			}
		}
		if s.Entrypoint != nil {
			// Cloned for the same reason as CopyOp.Paths above: codegen
			// distinguishes a nil Entrypoint (no ENTRYPOINT line) from a
			// non-nil one, so slices.Clone's nil-preserving behavior matters
			// here, not just the defensive copy.
			st.Entrypoint = entrypointArgv(s.Entrypoint)
		}
		st.Cmd = slices.Clone(s.Cmd)
		g.Stages = append(g.Stages, st)
		stageFinal[s.Name] = cur
	}
	return g, nil
}

// CUDAConfPath is where a CUDA stage registers its collected library
// directory with the dynamic loader. The 000- prefix puts it first among
// ld.so.conf.d entries.
const CUDAConfPath = "/etc/ld.so.conf.d/000-stagefile-cuda.conf"

const (
	CUDAPythonRoot = "/opt/stagefile/cuda/python"
	PipOverlayRoot = "/opt/stagefile/pip/root"
)

func generatedStageName(prefix string, stageIndex int, stages []spec.Stage) string {
	used := make(map[string]bool, len(stages))
	for _, s := range stages {
		used[s.Name] = true
	}
	base := fmt.Sprintf("%s-%d", prefix, stageIndex)
	name := base
	for suffix := 2; used[name]; suffix++ {
		name = fmt.Sprintf("%s-%d", base, suffix)
	}
	return name
}

func imageNodeForStage(s spec.Stage, platform string, stageFinal map[string]int, settings bool) Node {
	im := &ImageOp{Ref: s.From, Unpinned: s.Pin != nil && !*s.Pin, Platform: platform}
	if settings {
		im.Args, im.Env, im.Workdir = maps.Clone(s.Args), maps.Clone(s.Env), s.Workdir
	}
	n := Node{Kind: OpImage, Image: im}
	if from, ok := stageFinal[s.From]; ok {
		im.FromStage, im.Unpinned, n.Inputs = true, true, []int{from}
	}
	return n
}

func appendGeneratedStage(g *Graph, name string, sourceIndex, final int) {
	g.Stages = append(g.Stages, Stage{Name: name, SourceIndex: sourceIndex, Final: final, User: "root"})
}

func resolvedBase(ref string, images map[string]string) string {
	if digest := images[ref]; digest != "" {
		return ref + "@" + digest
	}
	return ref
}

func pipBootstrapParams(s spec.Stage, install *spec.Install, stageFinal map[string]int, images map[string]string) *PipBootstrapParams {
	p := &PipBootstrapParams{Manager: "apt"}
	seen := map[string]bool{}
	for _, group := range install.Pip {
		for _, pkg := range group.BuildPackages {
			if !seen[pkg] {
				seen[pkg] = true
				p.Packages = append(p.Packages, pkg)
			}
		}
	}
	if install.Apk != nil && install.Apt == nil {
		p.Manager = "apk"
		p.ApkRepositories = slices.Clone(install.Apk.Repositories)
	} else if install.Apt != nil && len(p.Packages) > 0 {
		p.AptRepositories = aptParams(install.Apt).Repositories
	}
	if _, fromStage := stageFinal[s.From]; !fromStage && (s.Pin == nil || *s.Pin) {
		p.AptBase = resolvedBase(s.From, images)
	}
	return p
}

func addGeneratedPipStage(g *Graph, name string, sourceIndex int, s spec.Stage, platform string, stageFinal map[string]int, images map[string]string, install *spec.Install, profile *gpu.Profile) int {
	cur := g.add(imageNodeForStage(s, platform, stageFinal, true))
	cur = g.add(execNode(cur, &ExecOp{Recipe: RecipePipBootstrap, PipBootstrap: pipBootstrapParams(s, install, stageFinal, images)}))
	for _, p := range pipParams(install.Pip, profile) {
		p.Root = PipOverlayRoot
		cur = g.add(execNode(cur, &ExecOp{Recipe: RecipePip, Pip: p}))
	}
	appendGeneratedStage(g, name, sourceIndex, cur)
	return cur
}

func addGeneratedCUDAStage(g *Graph, name string, sourceIndex int, s spec.Stage, platform string, stageFinal map[string]int, images map[string]string, profile *gpu.Profile) int {
	cur := g.add(imageNodeForStage(s, platform, stageFinal, false))
	bootstrap := &PipBootstrapParams{Manager: "apt"}
	if _, fromStage := stageFinal[s.From]; !fromStage && (s.Pin == nil || *s.Pin) {
		bootstrap.AptBase = resolvedBase(s.From, images)
	}
	cur = g.add(execNode(cur, &ExecOp{Recipe: RecipePipBootstrap, PipBootstrap: bootstrap}))
	cur = g.add(execNode(cur, &ExecOp{Recipe: RecipePip, Pip: &PipParams{
		Packages: slices.Clone(profile.Runtime), Target: CUDAPythonRoot,
	}}))
	appendGeneratedStage(g, name, sourceIndex, cur)
	return cur
}

// entrypointArgv resolves the entrypoint to the exact argv a backend
// renders, wrapper included.
//
// A source: entrypoint becomes a bash source-then-exec wrapper here rather
// than in a backend: the wrapper is part of what the image runs, so two
// backends resolving it separately is two chances to disagree about what
// the container's PID 1 actually is. The declared argv is passed through
// untouched as "$@", so no Exec argument is ever parsed by the shell.
func entrypointArgv(e *spec.Entrypoint) []string {
	if e.Source == "" {
		return slices.Clone(e.Exec)
	}
	inner := "source " + shellQuoteForWrapper(e.Source) + ` && exec "$@"`
	return append([]string{"/bin/bash", "-c", inner, "bash"}, e.Exec...)
}

// shellQuoteForWrapper single-quotes s for the one place ir itself has to
// build a shell fragment: the source-then-exec entrypoint wrapper, whose
// argv is part of the image config rather than a rendered RUN line. Every
// other shell rendering belongs to a backend.
func shellQuoteForWrapper(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// stageUser resolves the final user, including the reason a GPU stage is
// exempt from the non-root default.
//
// CUDA's memory manager opens /dev/nvmap, which is root-only on a Jetson. A
// GPU stage that took the non-root default would build clean and then fail
// on the device at the first allocation, so the declaration that the stage
// uses the GPU is also the declaration that it needs root. An explicit
// user: still wins. An empty result means "no explicit user", which a
// backend resolves to its own default.
func stageUser(s *spec.Stage) string {
	if s.User != "" {
		return s.User
	}
	if s.CUDA {
		return "root"
	}
	return ""
}

// stageEnv returns the env map for s: the Stagefile's own, plus the loader
// path a CUDA stage needs.
//
// ld.so.conf alone is not enough. A Jetson's CDI injection puts the host's
// CUDA directories on the container's loader path in a position that beats
// ld.so.conf.d, so the collected directory has to be on LD_LIBRARY_PATH to
// win. spec's validateCUDA refuses a Stagefile that sets the variable
// itself, so there is nothing here to merge with.
func stageEnv(s *spec.Stage, cudaProfile *gpu.Profile) map[string]string {
	if !s.CUDA || cudaProfile == nil {
		return maps.Clone(s.Env)
	}
	env := make(map[string]string, len(s.Env)+2)
	maps.Copy(env, s.Env)
	env[spec.LDLibraryPath] = cudaProfile.LibDir
	if existing := env["PYTHONPATH"]; existing != "" {
		env["PYTHONPATH"] = CUDAPythonRoot + ":" + existing
	} else {
		env["PYTHONPATH"] = CUDAPythonRoot
	}
	return env
}

func aptParams(a *spec.AptInstall) *AptParams {
	p := &AptParams{
		Packages:   slices.Clone(a.Packages),
		Recommends: a.Recommends,
	}
	for _, r := range a.Repositories {
		p.Repositories = append(p.Repositories, AptRepository{
			Name:       r.Name,
			URL:        r.URL,
			Suites:     slices.Clone(r.Suites),
			Components: slices.Clone(r.Components),
			KeyURL:     r.Key.URL,
			// Stripped here so every backend renders the same digest text
			// and the key hashes one spelling of it, whichever the
			// Stagefile used.
			KeySHA256: strings.TrimPrefix(r.Key.SHA256, "sha256:"),
			KeyFormat: r.Key.Format,
		})
	}
	return p
}

func cmakeParams(i int, c spec.CMakeInstall) *CMakeParams {
	prefix := c.Prefix
	if prefix == "" {
		prefix = "/usr/local"
	}
	buildType := c.BuildType
	if buildType == "" {
		buildType = "Release"
	}
	return &CMakeParams{
		Repository: c.Repository,
		Commit:     c.Commit,
		Prefix:     prefix,
		BuildType:  buildType,
		Defines:    maps.Clone(c.Defines),
		Jobs:       c.Jobs,
		Root:       fmt.Sprintf("/tmp/stagefile-cmake-%d", i),
	}
}

// pipParams lowers user dependency groups. CUDA runtime packages live in a
// separate generated stage and are intentionally absent here.
func pipParams(groups []spec.PipInstall, cudaProfile *gpu.Profile) []*PipParams {
	var out []*PipParams
	for _, g := range groups {
		index := g.Index
		if g.CUDA && cudaProfile != nil {
			index = cudaProfile.Index
		}
		out = append(out, &PipParams{
			Requirements:  g.Requirements,
			Packages:      slices.Clone(g.Packages),
			BuildPackages: slices.Clone(g.BuildPackages),
			Index:         index,
			ExtraIndex:    slices.Clone(g.ExtraIndex),
		})
	}
	return out
}

// downloadStagingPath is where an archive lands before it is unpacked. It is
// keyed to the download's index within its stage, so identical source always
// compiles to identical bytes — nothing here is random or time-derived.
func downloadStagingPath(i int, extract string) string {
	return fmt.Sprintf("/tmp/stagefile-download-%d.%s", i, extract)
}

func fetchNodes(entries []spec.Download, resolved map[string]string) ([]*FetchOp, error) {
	var out []*FetchOp
	for i, d := range entries {
		checksum, err := downloadChecksum(d, resolved)
		if err != nil {
			return nil, err
		}
		dest := d.Dest
		if d.Extract != "" {
			dest = downloadStagingPath(i, d.Extract)
		}
		out = append(out, &FetchOp{
			URL:      d.URL,
			Dest:     dest,
			Checksum: checksum,
			Mode:     d.Mode,
			Owner:    d.Owner,
		})
	}
	return out, nil
}

// downloadChecksum returns the sha256 to pin d with: the one written in the
// Stagefile, or failing that the one resolved into the lockfile. An
// unpinned download is not representable in the output, so a download with
// neither is an error rather than a plain fetch.
func downloadChecksum(d spec.Download, resolved map[string]string) (string, error) {
	digest := d.SHA256
	if digest == "" {
		digest = resolved[d.URL]
	}
	if digest == "" {
		return "", fmt.Errorf("download %q: no resolved sha256; run a build with network access to pin it, or write sha256: in the Stagefile", d.URL)
	}
	return "sha256:" + strings.TrimPrefix(digest, "sha256:"), nil
}

// copyDest resolves a copy entry's destination to the single value every
// backend renders and the cache key hashes.
//
// Two normalizations happen here rather than in a backend. Defaulting an
// omitted dest to the sole source path means `paths: [app.py]` and
// `paths: [app.py], dest: app.py` are one build with one key instead of two.
// Appending "/" for a multi-path copy is required by BuildKit — a
// multi-source COPY whose destination lacks a trailing slash validates fine
// but hard-fails at docker build — and doing it here means `dest: /app` and
// `dest: /app/` also converge on one key.
func copyDest(c spec.CopyEntry) (string, error) {
	if len(c.Paths) == 0 {
		// spec.Validate already rejects this, but Lower is reachable with a
		// hand-built spec.File and must not index into an empty slice.
		return "", fmt.Errorf("copy from %q: paths must be non-empty", c.From)
	}
	dest := c.Dest
	if dest == "" {
		dest = c.Paths[0]
	}
	if len(c.Paths) > 1 && !strings.HasSuffix(dest, "/") {
		dest += "/"
	}
	return dest, nil
}

func execNode(base int, e *ExecOp) Node {
	return Node{Kind: OpExec, Inputs: []int{base}, Exec: e}
}

func (g *Graph) add(n Node) int {
	g.Nodes = append(g.Nodes, n)
	return len(g.Nodes) - 1
}
