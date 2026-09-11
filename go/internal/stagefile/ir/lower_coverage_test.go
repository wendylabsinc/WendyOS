package ir

import (
	"reflect"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/stagefile/gpu"
	"github.com/wendylabsinc/wendy/go/internal/stagefile/spec"
)

// These tests cover the resolution Lower took over from codegen when the IR
// grew to the whole DSL. Each one pins a decision that used to be made at
// render time and is now made once, at lower time, so that the Dockerfile and
// the cache key cannot disagree about it.

func testProfile() *gpu.Profile {
	return &gpu.Profile{
		Arch:        "sm_87",
		Board:       "Orin",
		CUDAVersion: "12.6",
		Index:       "https://pypi.jetson.example/simple",
		Runtime:     []string{"nvidia-cuda-runtime-cu12"},
		LibDir:      "/opt/stagefile/cuda",
	}
}

func execNodesOf(g *Graph) []*ExecOp {
	var out []*ExecOp
	for i := range g.Nodes {
		if g.Nodes[i].Exec != nil {
			out = append(out, g.Nodes[i].Exec)
		}
	}
	return out
}

// A cuda: stage resolves everything the GPU implies at lower time: the wheel
// index for its own group, the separate PyPI runtime group after it, the
// collect step, the loader path, and root.
func TestLowerResolvesCUDAStage(t *testing.T) {
	p := testProfile()
	g, err := Lower(&spec.File{Version: 1, Stages: []spec.Stage{{
		Name: "app", From: "ubuntu:22.04", CUDA: true,
		Install: &spec.Install{Pip: []spec.PipInstall{
			{Packages: []string{"torch"}, CUDA: true},
			{Packages: []string{"flask"}},
		}},
	}}}, Options{CUDAProfile: p})
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}

	img := g.Nodes[0].Image
	if got := img.Env[spec.LDLibraryPath]; got != p.LibDir {
		t.Errorf("%s = %q, want %q", spec.LDLibraryPath, got, p.LibDir)
	}
	if g.Stages[0].User != "root" {
		t.Errorf("user = %q, want root — a GPU stage needs /dev/nvmap", g.Stages[0].User)
	}

	var pipGroups []*PipParams
	var collect *CUDACollectParams
	for _, x := range execNodesOf(g) {
		switch {
		case x.Pip != nil:
			pipGroups = append(pipGroups, x.Pip)
		case x.CUDACollect != nil:
			collect = x.CUDACollect
		}
	}
	if len(pipGroups) != 3 {
		t.Fatalf("got %d pip groups, want 3 (cuda wheels, runtime, pypi)", len(pipGroups))
	}
	if pipGroups[0].Index != p.Index {
		t.Errorf("cuda group index = %q, want the profile's %q", pipGroups[0].Index, p.Index)
	}
	// The runtime lands between the GPU group and the ordinary one, and with
	// no index, so it resolves from PyPI.
	if !reflect.DeepEqual(pipGroups[1].Packages, p.Runtime) {
		t.Errorf("group 1 = %v, want the runtime %v", pipGroups[1].Packages, p.Runtime)
	}
	if pipGroups[1].Index != "" {
		t.Errorf("runtime group index = %q, want empty so it resolves from PyPI", pipGroups[1].Index)
	}
	if pipGroups[2].Index != "" {
		t.Errorf("plain group index = %q, want empty", pipGroups[2].Index)
	}
	if collect == nil || collect.LibDir != p.LibDir || collect.ConfPath != CUDAConfPath {
		t.Fatalf("collect = %+v, want LibDir %q and ConfPath %q", collect, p.LibDir, CUDAConfPath)
	}
}

// A GPU stage that installs no GPU wheels of its own still gets the runtime:
// it may be loading CUDA through something apt or cmake installed.
func TestLowerCUDAStageWithNoPipStillGetsRuntime(t *testing.T) {
	p := testProfile()
	g, err := Lower(&spec.File{Version: 1, Stages: []spec.Stage{
		{Name: "app", From: "ubuntu:22.04", CUDA: true},
	}}, Options{CUDAProfile: p})
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	var got []string
	for _, x := range execNodesOf(g) {
		if x.Pip != nil {
			got = x.Pip.Packages
		}
	}
	if !reflect.DeepEqual(got, p.Runtime) {
		t.Fatalf("runtime group = %v, want %v", got, p.Runtime)
	}
}

func TestLowerRejectsCUDAWithoutAProfile(t *testing.T) {
	_, err := Lower(&spec.File{Version: 1, Stages: []spec.Stage{
		{Name: "app", From: "ubuntu:22.04", CUDA: true},
	}}, Options{})
	if err == nil {
		t.Fatal("Lower accepted a cuda: stage with no resolved GPU profile")
	}
}

// An explicit user: still wins over the GPU default.
func TestLowerExplicitUserBeatsCUDARoot(t *testing.T) {
	g, err := Lower(&spec.File{Version: 1, Stages: []spec.Stage{
		{Name: "app", From: "ubuntu:22.04", CUDA: true, User: "1000"},
	}}, Options{CUDAProfile: testProfile()})
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	if g.Stages[0].User != "1000" {
		t.Fatalf("user = %q, want the declared 1000", g.Stages[0].User)
	}
}

// A download lowers to two nodes at different points in the stage, and they
// must agree on the staging path — the reason it is resolved here rather
// than recomputed by a backend.
func TestLowerFetchAndExtractAgreeOnStagingPath(t *testing.T) {
	g, err := Lower(&spec.File{Version: 1, Stages: []spec.Stage{{
		Name: "app", From: "debian:12",
		Download: []spec.Download{
			{URL: "https://example.test/model.tar.gz", SHA256: "abc", Dest: "/models", Extract: "tar.gz"},
			{URL: "https://example.test/plain.bin", SHA256: "sha256:def", Dest: "/opt/plain.bin", Mode: "0755"},
		},
		Install: &spec.Install{Apt: &spec.AptInstall{Packages: []string{"unzip"}}},
	}}}, Options{})
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}

	var fetches []*FetchOp
	var extracts []*ExtractParams
	fetchIdx, aptIdx, extractIdx := -1, -1, -1
	for i := range g.Nodes {
		switch {
		case g.Nodes[i].Fetch != nil:
			fetches = append(fetches, g.Nodes[i].Fetch)
			if fetchIdx == -1 {
				fetchIdx = i
			}
		case g.Nodes[i].Exec != nil && g.Nodes[i].Exec.Apt != nil:
			aptIdx = i
		case g.Nodes[i].Exec != nil && g.Nodes[i].Exec.Extract != nil:
			extracts = append(extracts, g.Nodes[i].Exec.Extract)
			extractIdx = i
		}
	}
	if len(fetches) != 2 || len(extracts) != 1 {
		t.Fatalf("got %d fetches and %d extracts, want 2 and 1", len(fetches), len(extracts))
	}
	// Fetch before install, extract after: extract may need a tool the
	// install declared (unzip), and a fetch must not be re-run by an
	// unrelated package bump.
	if !(fetchIdx < aptIdx && aptIdx < extractIdx) {
		t.Fatalf("node order fetch=%d apt=%d extract=%d, want fetch < apt < extract", fetchIdx, aptIdx, extractIdx)
	}
	if fetches[0].Dest != extracts[0].Archive {
		t.Fatalf("fetch dest %q and extract archive %q disagree", fetches[0].Dest, extracts[0].Archive)
	}
	if extracts[0].Dest != "/models" {
		t.Errorf("extract dest = %q, want the declared /models", extracts[0].Dest)
	}
	// Both spellings of the checksum normalize to one.
	if fetches[0].Checksum != "sha256:abc" || fetches[1].Checksum != "sha256:def" {
		t.Errorf("checksums = %q, %q; want both sha256:-prefixed exactly once", fetches[0].Checksum, fetches[1].Checksum)
	}
}

func TestLowerRejectsUnpinnedDownload(t *testing.T) {
	_, err := Lower(&spec.File{Version: 1, Stages: []spec.Stage{{
		Name: "app", From: "debian:12",
		Download: []spec.Download{{URL: "https://example.test/x.bin", Dest: "/x.bin"}},
	}}}, Options{})
	if err == nil {
		t.Fatal("Lower accepted a download with no sha256 in the spec or the lockfile")
	}
}

func TestLowerTakesDownloadChecksumFromTheLockfile(t *testing.T) {
	g, err := Lower(&spec.File{Version: 1, Stages: []spec.Stage{{
		Name: "app", From: "debian:12",
		Download: []spec.Download{{URL: "https://example.test/x.bin", Dest: "/x.bin"}},
	}}}, Options{Downloads: map[string]string{"https://example.test/x.bin": "deadbeef"}})
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	if got := g.Nodes[1].Fetch.Checksum; got != "sha256:deadbeef" {
		t.Fatalf("checksum = %q, want sha256:deadbeef", got)
	}
}

// pin: false is the only way to get an unpinned image, and the zero value of
// the field must not be it — see ir.ImageOp.Unpinned.
func TestLowerResolvesPin(t *testing.T) {
	no := false
	yes := true
	for _, tc := range []struct {
		name         string
		pin          *bool
		wantUnpinned bool
	}{
		{"absent pin defaults to pinned", nil, false},
		{"pin: true is pinned", &yes, false},
		{"pin: false is unpinned", &no, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, err := Lower(&spec.File{Version: 1, Stages: []spec.Stage{
				{Name: "app", From: "local/img:dev", Pin: tc.pin},
			}}, Options{})
			if err != nil {
				t.Fatalf("Lower: %v", err)
			}
			if got := g.Nodes[0].Image.Unpinned; got != tc.wantUnpinned {
				t.Fatalf("Unpinned = %v, want %v", got, tc.wantUnpinned)
			}
		})
	}
}

// platform: build pins a stage to $BUILDPLATFORM regardless of what the build
// targets, and the resolution lands on the node so the key sees it.
func TestLowerResolvesStagePlatform(t *testing.T) {
	g, err := Lower(&spec.File{Version: 1, Stages: []spec.Stage{
		{Name: "web", From: "node:20-slim", Platform: "build"},
		{Name: "app", From: "debian:12"},
	}}, Options{Platform: "linux/arm64"})
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	if got := g.Nodes[g.Stages[0].Final].Image.Platform; got != "$BUILDPLATFORM" {
		t.Errorf("build-platform stage = %q, want $BUILDPLATFORM", got)
	}
	if got := g.Nodes[g.Stages[1].Final].Image.Platform; got != "linux/arm64" {
		t.Errorf("target stage = %q, want linux/arm64", got)
	}
}

// A source: entrypoint is wrapped once, here, so both backends and the image
// config agree on what PID 1 actually is.
func TestLowerWrapsSourcedEntrypoint(t *testing.T) {
	g, err := Lower(&spec.File{Version: 1, Stages: []spec.Stage{{
		Name: "app", From: "ros:humble",
		Entrypoint: &spec.Entrypoint{
			Exec:   []string{"ros2", "run", "demo", "talker"},
			Source: "/opt/ros/humble/setup.bash",
		},
	}}}, Options{})
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	got := g.Stages[0].Entrypoint
	want := []string{"/bin/bash", "-c",
		`source '/opt/ros/humble/setup.bash' && exec "$@"`, "bash",
		"ros2", "run", "demo", "talker"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("entrypoint =\n %q\nwant\n %q", got, want)
	}
}

// The declared argv is passed through as "$@" and never parsed by the shell,
// so a metacharacter in the sourced path cannot escape its quoting.
func TestLowerQuotesTheSourcedPath(t *testing.T) {
	g, err := Lower(&spec.File{Version: 1, Stages: []spec.Stage{{
		Name: "app", From: "debian:12",
		Entrypoint: &spec.Entrypoint{Exec: []string{"app"}, Source: "/opt/a'; rm -rf /; '"},
	}}}, Options{})
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	// Every embedded quote is closed and reopened, so the injected `rm -rf /`
	// stays inside the single-quoted argument to source.
	inner := g.Stages[0].Entrypoint[2]
	want := `source '/opt/a'"'"'; rm -rf /; '"'"'' && exec "$@"`
	if inner != want {
		t.Fatalf("sourced path was not quoted:\n got  %s\n want %s", inner, want)
	}
}

// The apt repository key digest is normalized once so no backend has to
// decide whether to strip a prefix, and the key hashes one spelling.
func TestLowerNormalizesAptRepositoryKeyDigest(t *testing.T) {
	for _, in := range []string{"abc123", "sha256:abc123"} {
		g, err := Lower(&spec.File{Version: 1, Stages: []spec.Stage{{
			Name: "app", From: "ubuntu:22.04",
			Install: &spec.Install{Apt: &spec.AptInstall{
				Packages: []string{"ros-humble-desktop"},
				Repositories: []spec.AptRepository{{
					Name: "ros2", URL: "http://packages.ros.org/ros2/ubuntu",
					Suites: []string{"jammy"}, Components: []string{"main"},
					Key: spec.AptRepositoryKey{URL: "https://example.test/ros.key", SHA256: in},
				}},
			}},
		}}}, Options{})
		if err != nil {
			t.Fatalf("Lower(%q): %v", in, err)
		}
		if got := g.Nodes[1].Exec.Apt.Repositories[0].KeySHA256; got != "abc123" {
			t.Errorf("KeySHA256 for input %q = %q, want abc123", in, got)
		}
	}
}

// cmake's scratch root is resolved from the install's position in its stage,
// so two installs in one stage never share a build tree.
func TestLowerGivesEachCMakeInstallItsOwnRoot(t *testing.T) {
	g, err := Lower(&spec.File{Version: 1, Stages: []spec.Stage{{
		Name: "app", From: "ubuntu:22.04",
		Install: &spec.Install{CMake: []spec.CMakeInstall{
			{Repository: "https://example.test/a.git", Commit: "a1"},
			{Repository: "https://example.test/b.git", Commit: "b2"},
		}},
	}}}, Options{})
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	var roots []string
	for _, x := range execNodesOf(g) {
		if x.CMake != nil {
			roots = append(roots, x.CMake.Root)
			// Defaults are resolved here, not left for a backend.
			if x.CMake.Prefix != "/usr/local" || x.CMake.BuildType != "Release" {
				t.Errorf("defaults not resolved: prefix=%q buildType=%q", x.CMake.Prefix, x.CMake.BuildType)
			}
		}
	}
	if len(roots) != 2 || roots[0] == roots[1] {
		t.Fatalf("cmake roots = %v, want two distinct paths", roots)
	}
}

// Lower must not alias the maps it copies out of the spec, for the same
// reason it must not alias the slices: a caller that patches its spec.File in
// place and lowers again would otherwise corrupt an already-returned graph,
// and with it the cache key computed from that graph.
func TestLowerDoesNotAliasSpecMaps(t *testing.T) {
	env := map[string]string{"MODE": "prod"}
	args := map[string]string{"VERSION": "1"}
	defines := map[string]string{"BUILD_TESTS": "OFF"}
	g, err := Lower(&spec.File{Version: 1, Stages: []spec.Stage{{
		Name: "app", From: "ubuntu:22.04", Env: env, Args: args,
		Install: &spec.Install{CMake: []spec.CMakeInstall{
			{Repository: "https://example.test/a.git", Commit: "a1", Defines: defines},
		}},
	}}}, Options{})
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	env["MODE"] = "corrupted"
	args["VERSION"] = "corrupted"
	defines["BUILD_TESTS"] = "corrupted"

	if got := g.Nodes[0].Image.Env["MODE"]; got != "prod" {
		t.Errorf("Env aliased the spec: got %q, want prod", got)
	}
	if got := g.Nodes[0].Image.Args["VERSION"]; got != "1" {
		t.Errorf("Args aliased the spec: got %q, want 1", got)
	}
	if got := execNodesOf(g)[0].CMake.Defines["BUILD_TESTS"]; got != "OFF" {
		t.Errorf("cmake Defines aliased the spec: got %q, want OFF", got)
	}
}
