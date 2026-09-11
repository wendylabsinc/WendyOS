package recipe

import (
	"reflect"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/stagefile/ir"
)

func mustFor(t *testing.T, x *ir.ExecOp, platform string) []Step {
	t.Helper()
	steps, err := For(x, platform)
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	return steps
}

// commands flattens a step list to "run:<clause0>" / "fetch:<url>" markers, so
// a test can assert the shape of a sequence without restating every clause.
func kinds(steps []Step) []string {
	var out []string
	for _, s := range steps {
		switch {
		case s.Fetch != nil:
			out = append(out, "fetch:"+s.Fetch.URL)
		case s.Run != nil:
			out = append(out, "run")
		default:
			out = append(out, "empty")
		}
	}
	return out
}

func TestShellQuoteEscapesEmbeddedSingleQuote(t *testing.T) {
	got := ShellQuote(`it's`)
	want := `'it'"'"'s'`
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// Every op kind ir can hold must produce steps. A kind that falls through
// returns an error, which is how a new recipe added to ir without a
// definition here surfaces at build time rather than as a silently missing
// install.
func TestForCoversEveryExecKind(t *testing.T) {
	ops := []*ir.ExecOp{
		{Recipe: ir.RecipeApt, Apt: &ir.AptParams{Packages: []string{"curl"}}},
		{Recipe: ir.RecipeApk, Apk: &ir.ApkParams{Packages: []string{"musl-dev"}}},
		{Recipe: ir.RecipeCMake, CMake: &ir.CMakeParams{Repository: "r", Commit: "c", Prefix: "/usr/local", BuildType: "Release", Root: "/tmp/x"}},
		{Recipe: ir.RecipePip, Pip: &ir.PipParams{Packages: []string{"flask"}}},
		{Recipe: ir.RecipeNpm, Npm: &ir.NpmParams{Manager: "npm", Manifest: "package.json", Lockfile: "package-lock.json"}},
		{Recipe: ir.RecipeUv, Uv: &ir.UvParams{Files: []string{"pyproject.toml", "uv.lock"}}},
		{Recipe: ir.RecipeExtract, Extract: &ir.ExtractParams{Archive: "/tmp/a.tar.gz", Dest: "/m", Format: "tar.gz"}},
		{Recipe: ir.RecipeCUDACollect, CUDACollect: &ir.CUDACollectParams{LibDir: "/opt/cuda", ConfPath: "/etc/ld.so.conf.d/x.conf"}},
		{Recipe: ir.RecipeBuild, Build: &ir.BuildParams{Lang: "go"}},
	}
	for _, x := range ops {
		steps, err := For(x, "linux/arm64")
		if err != nil {
			t.Errorf("For(%s): %v", x.Recipe.Name, err)
			continue
		}
		if len(steps) == 0 {
			t.Errorf("For(%s) returned no steps", x.Recipe.Name)
		}
	}
}

func TestForRejectsAnEmptyExecOp(t *testing.T) {
	if _, err := For(&ir.ExecOp{Recipe: ir.Recipe{Name: "mystery"}}, ""); err == nil {
		t.Fatal("For accepted an exec op with no params")
	}
}

// apt's declared repositories interleave runs and fetches — the reason a
// recipe is a sequence rather than a single run.
func TestAptRepositoriesProduceTheFullPreamble(t *testing.T) {
	steps := mustFor(t, &ir.ExecOp{Recipe: ir.RecipeApt, Apt: &ir.AptParams{
		Packages: []string{"ros-humble-desktop"},
		Repositories: []ir.AptRepository{{
			Name: "ros2", URL: "http://packages.ros.org/ros2/ubuntu",
			Suites: []string{"jammy"}, Components: []string{"main"},
			KeyURL: "https://example.test/ros.key", KeySHA256: "abc123",
		}},
	}}, "")

	want := []string{"run", "fetch:https://example.test/ros.key", "run", "run"}
	if got := kinds(steps); !reflect.DeepEqual(got, want) {
		t.Fatalf("step sequence = %v, want %v", got, want)
	}
	// ca-certificates first: an https sources.list URL fails apt-get update
	// without it on a stock ubuntu/debian image.
	if !strings.Contains(steps[0].Run.Command[0], "ca-certificates") {
		t.Errorf("first step = %q, want the ca-certificates bootstrap", steps[0].Run.Command[0])
	}
	key := steps[1].Fetch
	if key.Checksum != "sha256:abc123" {
		t.Errorf("key checksum = %q, want sha256:abc123", key.Checksum)
	}
	if key.Dest != "/etc/apt/keyrings/ros2.gpg" {
		t.Errorf("keyring = %q, want the binary-format .gpg path", key.Dest)
	}
	if !strings.Contains(steps[2].Run.Command[0], "/etc/apt/sources.list.d/ros2.list") {
		t.Errorf("sources step = %q", steps[2].Run.Command[0])
	}
}

func TestArmoredAptKeyGetsAnAscKeyring(t *testing.T) {
	steps := mustFor(t, &ir.ExecOp{Recipe: ir.RecipeApt, Apt: &ir.AptParams{
		Repositories: []ir.AptRepository{{Name: "vendor", KeyURL: "https://example.test/k", KeySHA256: "d", KeyFormat: "armored"}},
	}}, "")
	if got := steps[1].Fetch.Dest; got != "/etc/apt/keyrings/vendor.asc" {
		t.Fatalf("keyring = %q, want the armored .asc path", got)
	}
}

// A stage with no repositories is one plain install, and the list cleanup
// stays a separate clause so a backend decides how to join it.
func TestAptWithoutRepositoriesIsOneRunWithTwoClauses(t *testing.T) {
	steps := mustFor(t, &ir.ExecOp{Recipe: ir.RecipeApt,
		Apt: &ir.AptParams{Packages: []string{"curl"}}}, "")
	if len(steps) != 1 || steps[0].Run == nil {
		t.Fatalf("got %v, want a single run", kinds(steps))
	}
	if len(steps[0].Run.Command) != 2 {
		t.Fatalf("clauses = %v, want the install and the list cleanup", steps[0].Run.Command)
	}
	if !strings.Contains(steps[0].Run.Command[0], "--no-install-recommends") {
		t.Error("recommends are not opted out of by default")
	}
	if !strings.Contains(steps[0].Run.Command[0], `'curl'`) {
		t.Error("package name is not shell-quoted")
	}
}

func TestApkOptsOutOfTheCacheByDefault(t *testing.T) {
	steps := mustFor(t, &ir.ExecOp{Recipe: ir.RecipeApk,
		Apk: &ir.ApkParams{Packages: []string{"musl-dev"}}}, "")
	if !strings.Contains(steps[0].Run.Command[0], "--no-cache") {
		t.Fatalf("apk command = %q, want --no-cache", steps[0].Run.Command[0])
	}

	cached := mustFor(t, &ir.ExecOp{Recipe: ir.RecipeApk,
		Apk: &ir.ApkParams{Packages: []string{"musl-dev"}, Cache: true}}, "")
	if strings.Contains(cached[0].Run.Command[0], "--no-cache") {
		t.Fatal("cache: true still passed --no-cache")
	}
}

// pip stages its requirements file to itself; npm and uv stage their manifest
// and lockfile together into "./". Both backends must place these identically,
// which is why Dest is resolved here rather than inferred from path count.
func TestPreCopyDestinationsAreResolvedHere(t *testing.T) {
	pip := mustFor(t, &ir.ExecOp{Recipe: ir.RecipePip,
		Pip: &ir.PipParams{Requirements: "requirements.txt"}}, "")
	if got := pip[0].Run.PreCopy; got == nil || got.Dest != "requirements.txt" ||
		!reflect.DeepEqual(got.Paths, []string{"requirements.txt"}) {
		t.Errorf("pip PreCopy = %+v", got)
	}

	npm := mustFor(t, &ir.ExecOp{Recipe: ir.RecipeNpm,
		Npm: &ir.NpmParams{Manager: "npm", Manifest: "package.json", Lockfile: "package-lock.json"}}, "")
	if got := npm[0].Run.PreCopy; got == nil || got.Dest != "./" ||
		!reflect.DeepEqual(got.Paths, []string{"package.json", "package-lock.json"}) {
		t.Errorf("npm PreCopy = %+v", got)
	}

	uv := mustFor(t, &ir.ExecOp{Recipe: ir.RecipeUv,
		Uv: &ir.UvParams{Files: []string{"pyproject.toml", "uv.lock"}}}, "")
	if got := uv[0].Run.PreCopy; got == nil || got.Dest != "./" ||
		!reflect.DeepEqual(got.Paths, []string{"pyproject.toml", "uv.lock"}) {
		t.Errorf("uv PreCopy = %+v", got)
	}
}

// A pip group with no requirements file stages nothing — a PreCopy with no
// paths would render a COPY with no sources.
func TestPipWithoutRequirementsStagesNothing(t *testing.T) {
	steps := mustFor(t, &ir.ExecOp{Recipe: ir.RecipePip,
		Pip: &ir.PipParams{Packages: []string{"flask"}}}, "")
	if steps[0].Run.PreCopy != nil {
		t.Fatalf("PreCopy = %+v, want nil", steps[0].Run.PreCopy)
	}
}

func TestNpmAndUvRefuseMissingFilenames(t *testing.T) {
	if _, err := For(&ir.ExecOp{Recipe: ir.RecipeNpm, Npm: &ir.NpmParams{Manager: "npm"}}, ""); err == nil {
		t.Error("npm accepted an empty manifest and lockfile")
	}
	if _, err := For(&ir.ExecOp{Recipe: ir.RecipeUv, Uv: &ir.UvParams{}}, ""); err == nil {
		t.Error("uv accepted an empty file list")
	}
}

// Every cache mount this package emits is locked. An unlocked mount lets four
// concurrently-built services collide inside the package manager, where the
// waiting is invisible.
func TestEveryCacheMountIsLocked(t *testing.T) {
	ops := []*ir.ExecOp{
		{Recipe: ir.RecipePip, Pip: &ir.PipParams{Packages: []string{"flask"}}},
		{Recipe: ir.RecipeNpm, Npm: &ir.NpmParams{Manager: "npm", Manifest: "package.json", Lockfile: "package-lock.json"}},
		{Recipe: ir.RecipeUv, Uv: &ir.UvParams{Files: []string{"pyproject.toml"}}},
		{Recipe: ir.RecipeCMake, CMake: &ir.CMakeParams{Repository: "r", Commit: "c", Root: "/tmp/x"}},
		{Recipe: ir.RecipeBuild, Build: &ir.BuildParams{Lang: "swift", Profile: "release"}},
	}
	for _, x := range ops {
		for _, s := range mustFor(t, x, "linux/arm64") {
			if s.Run == nil {
				continue
			}
			for _, m := range s.Run.CacheMounts {
				if !m.Locked {
					t.Errorf("%s mount %q is not locked", x.Recipe.Name, m.Dir)
				}
			}
		}
	}
}

// pip's cache is scoped by index set and platform: same index and platform may
// share a mount, anything else must not.
func TestPipCacheScoping(t *testing.T) {
	id := func(p *ir.PipParams, platform string) string {
		return mustFor(t, &ir.ExecOp{Recipe: ir.RecipePip, Pip: p}, platform)[0].Run.CacheMounts[0].ID
	}
	pypi := &ir.PipParams{Packages: []string{"flask"}}
	jetson := &ir.PipParams{Packages: []string{"torch"}, Index: "https://pypi.jetson.example/simple"}
	extra := &ir.PipParams{Packages: []string{"flask"}, ExtraIndex: []string{"https://extra.example/simple"}}

	if id(pypi, "linux/arm64") == id(jetson, "linux/arm64") {
		t.Error("a vendor-index group shares pip's cache with a PyPI group")
	}
	if id(pypi, "linux/arm64") == id(pypi, "linux/amd64") {
		t.Error("two architectures share one pip wheel cache")
	}
	if id(pypi, "linux/arm64") == id(extra, "linux/arm64") {
		t.Error("an extra index does not scope the cache")
	}
	// Same declaration, same scope — otherwise the cache never hits.
	if id(pypi, "linux/arm64") != id(&ir.PipParams{Packages: []string{"other"}}, "linux/arm64") {
		t.Error("two PyPI groups on one platform do not share a cache")
	}
}

// A cmake build tree is scoped to project and architecture and deliberately
// not to the commit: bumping a pin must recompile only what changed, which is
// the whole reason the cache exists.
func TestCMakeCacheScopingExcludesTheCommit(t *testing.T) {
	id := func(repo, commit, platform string) string {
		x := &ir.ExecOp{Recipe: ir.RecipeCMake, CMake: &ir.CMakeParams{
			Repository: repo, Commit: commit, Prefix: "/usr/local", BuildType: "Release", Root: "/tmp/x"}}
		return mustFor(t, x, platform)[0].Run.CacheMounts[0].ID
	}
	if id("a", "c1", "linux/arm64") != id("a", "c2", "linux/arm64") {
		t.Error("bumping the commit changed the build-tree cache id")
	}
	if id("a", "c1", "linux/arm64") == id("b", "c1", "linux/arm64") {
		t.Error("two projects share one build tree")
	}
	if id("a", "c1", "linux/arm64") == id("a", "c1", "linux/amd64") {
		t.Error("two architectures share one build tree")
	}
}

// The build tree is mounted at the directory cmake writes objects into —
// mounting anywhere else would cache nothing.
func TestCMakeMountsItsBuildDirectory(t *testing.T) {
	steps := mustFor(t, &ir.ExecOp{Recipe: ir.RecipeCMake, CMake: &ir.CMakeParams{
		Repository: "r", Commit: "c", Prefix: "/usr/local", BuildType: "Release", Root: "/tmp/stagefile-cmake-0"}}, "")
	if got := steps[0].Run.CacheMounts[0].Dir; got != "/tmp/stagefile-cmake-0/build" {
		t.Fatalf("mount dir = %q, want the build tree", got)
	}
	// The source tree is removed but the build tree is not: it is a cache
	// mount, so deleting it would throw away the objects the next bump wants.
	last := steps[0].Run.Command[len(steps[0].Run.Command)-1]
	if !strings.Contains(last, "/source") || strings.Contains(last, "/build") {
		t.Fatalf("cleanup clause = %q, want it to remove only the source tree", last)
	}
}

func TestExtractPicksTheToolFromTheFormat(t *testing.T) {
	zip := mustFor(t, &ir.ExecOp{Recipe: ir.RecipeExtract,
		Extract: &ir.ExtractParams{Archive: "/tmp/a.zip", Dest: "/m", Format: "zip"}}, "")
	if !strings.Contains(zip[0].Run.Command[0], "unzip -q") {
		t.Errorf("zip command = %q", zip[0].Run.Command[0])
	}
	tar := mustFor(t, &ir.ExecOp{Recipe: ir.RecipeExtract,
		Extract: &ir.ExtractParams{Archive: "/tmp/a.tar.gz", Dest: "/m", Format: "tar.gz"}}, "")
	if !strings.Contains(tar[0].Run.Command[0], "tar -xzf") {
		t.Errorf("tar command = %q", tar[0].Run.Command[0])
	}
	// Unpack and removal are one clause: split across layers, the image would
	// carry both the archive and its contents.
	if len(tar[0].Run.Command) != 1 || !strings.Contains(tar[0].Run.Command[0], "&& rm ") {
		t.Errorf("extract clauses = %v, want one clause ending in the removal", tar[0].Run.Command)
	}
}

func TestBuildVariants(t *testing.T) {
	cmd := func(b *ir.BuildParams) string {
		return mustFor(t, &ir.ExecOp{Recipe: ir.RecipeBuild, Build: b}, "")[0].Run.Command[0]
	}
	if got := cmd(&ir.BuildParams{Lang: "rust", Profile: "release", Product: "server"}); got != "cargo build --release --bin 'server'" {
		t.Errorf("rust = %q", got)
	}
	if got := cmd(&ir.BuildParams{Lang: "go"}); got != "go build ./..." {
		t.Errorf("go = %q", got)
	}
	if got := cmd(&ir.BuildParams{Lang: "go", Product: "./cmd/serve"}); got != "go build -o /usr/local/bin/ './cmd/serve'" {
		t.Errorf("go with a product = %q", got)
	}
	// Always spelled out: bare `swift build` already means debug, so an
	// implicit debug build is indistinguishable from a forgotten flag.
	if got := cmd(&ir.BuildParams{Lang: "swift", Profile: "debug"}); got != "swift build -c debug" {
		t.Errorf("swift = %q", got)
	}
	if got := cmd(&ir.BuildParams{Lang: "pnpm", Script: "bundle"}); got != "pnpm run 'bundle'" {
		t.Errorf("pnpm = %q", got)
	}
}

func TestBuildRejectsAnUnknownLang(t *testing.T) {
	if _, err := For(&ir.ExecOp{Recipe: ir.RecipeBuild, Build: &ir.BuildParams{Lang: "cobol"}}, ""); err == nil {
		t.Fatal("For accepted an unsupported build.lang")
	}
}

func TestFetchForRequiresAChecksum(t *testing.T) {
	if _, err := FetchFor(&ir.FetchOp{URL: "https://example.test/x.bin", Dest: "/x"}); err == nil {
		t.Fatal("FetchFor accepted an unpinned fetch")
	}
	got, err := FetchFor(&ir.FetchOp{URL: "https://example.test/x.bin", Dest: "/x", Checksum: "sha256:aaa", Mode: "0755"})
	if err != nil {
		t.Fatalf("FetchFor: %v", err)
	}
	want := Fetch{URL: "https://example.test/x.bin", Dest: "/x", Checksum: "sha256:aaa", Mode: "0755"}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}
