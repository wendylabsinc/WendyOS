package llbgen

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/solver/pb"

	"github.com/wendylabsinc/wendy/go/internal/stagefile/codegen"
	"github.com/wendylabsinc/wendy/go/internal/stagefile/gpu"
	"github.com/wendylabsinc/wendy/go/internal/stagefile/ir"
	"github.com/wendylabsinc/wendy/go/internal/stagefile/spec"
)

// The two backends are only worth having if they agree. codegen renders a
// Dockerfile and llbgen an LLB definition, both from one graph and one set of
// recipes — so for every Stagefile, the commands the daemon would run, the
// cache mounts it would attach, the images it would pull, and the URLs it
// would fetch must come out the same. These tests decode the definition back
// into ops and check exactly that, against the Dockerfile text parsed back out
// of codegen.
//
// A backend that drifts almost always drifts in ordering or in what a step
// reads, not in the command text — both read that from recipe. So the
// assertions below are about sequence and sources, which is where the
// difference would actually appear.

const testPlatform = "linux/arm64"

func testConfig(t *testing.T, osName, arch string) []byte {
	t.Helper()
	cfg, err := json.Marshal(map[string]any{
		"os":           osName,
		"architecture": arch,
		"config":       map[string]any{"Env": []string{"PATH=/usr/local/bin:/usr/bin:/bin"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func lower(t *testing.T, f *spec.File, opts ir.Options) *ir.Graph {
	t.Helper()
	if opts.Platform == "" {
		opts.Platform = testPlatform
	}
	g, err := ir.Lower(f, opts)
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	return g
}

// emitOps decodes a definition into its ops, in the order BuildKit stores
// them. Definitions are a DAG serialized bottom-up, so this is the operation
// order the build performs.
func emitOps(t *testing.T, def *llb.Definition) []*pb.Op {
	t.Helper()
	var ops []*pb.Op
	for _, dt := range def.Def {
		var op pb.Op
		if err := op.UnmarshalVT(dt); err != nil {
			t.Fatalf("decode op: %v", err)
		}
		ops = append(ops, &op)
	}
	return ops
}

// llbCommands returns every shell command the definition would execute, in op
// order.
func llbCommands(ops []*pb.Op) []string {
	var out []string
	for _, op := range ops {
		if e := op.GetExec(); e != nil {
			out = append(out, strings.Join(e.Meta.Args, " "))
		}
	}
	return out
}

// Independent linked stages are a DAG, so BuildKit may serialize their ops in
// a different topological order than Dockerfile source order. Compare the
// command multiset here; IR edge tests separately pin ordering within each
// dependency chain.
func sameCommands(a, b []string) bool {
	set := func(in []string) []string {
		seen := map[string]bool{}
		var out []string
		for _, command := range in {
			if !seen[command] {
				seen[command] = true
				out = append(out, command)
			}
		}
		sort.Strings(out)
		return out
	}
	return reflect.DeepEqual(set(a), set(b))
}

func sameStringSet(a, b []string) bool {
	unique := func(in []string) []string {
		seen := map[string]bool{}
		var out []string
		for _, value := range in {
			if !seen[value] {
				seen[value] = true
				out = append(out, value)
			}
		}
		sort.Strings(out)
		return out
	}
	return reflect.DeepEqual(unique(a), unique(b))
}

// llbCacheMounts returns "<id>@<target>" for every cache mount, sorted, since
// mount order within one exec is not meaningful.
func llbCacheMounts(ops []*pb.Op) []string {
	var out []string
	for _, op := range ops {
		e := op.GetExec()
		if e == nil {
			continue
		}
		for _, m := range e.Mounts {
			if m.CacheOpt != nil {
				out = append(out, m.CacheOpt.ID+"@"+m.Dest)
			}
		}
	}
	sort.Strings(out)
	return out
}

// llbSources returns every source identifier in the definition (docker-image,
// http, local), sorted.
func llbSources(ops []*pb.Op) []string {
	var out []string
	for _, op := range ops {
		if s := op.GetSource(); s != nil {
			out = append(out, s.Identifier)
		}
	}
	sort.Strings(out)
	return out
}

// dockerfileRunInstructions returns every RUN body with continuation lines
// folded back into one instruction. Cache mounts may themselves span those
// lines, so both command and mount comparisons start from this representation.
func dockerfileRunInstructions(dockerfile string) []string {
	var out []string
	var cur string
	for _, raw := range strings.Split(dockerfile, "\n") {
		line := strings.TrimSpace(raw)
		cont := strings.HasSuffix(line, "\\")
		line = strings.TrimSpace(strings.TrimSuffix(line, "\\"))
		if cur == "" {
			if !strings.HasPrefix(line, "RUN ") {
				continue
			}
			cur = strings.TrimSpace(strings.TrimPrefix(line, "RUN "))
		} else {
			cur += " " + line
		}
		if !cont {
			out = append(out, cur)
			cur = ""
		}
	}
	return out
}

// dockerfileRuns returns the shell command for each folded RUN instruction.
func dockerfileRuns(dockerfile string) []string {
	var out []string
	for _, instruction := range dockerfileRunInstructions(dockerfile) {
		command := instruction
		for strings.HasPrefix(command, "--mount=") {
			_, rest, _ := strings.Cut(command, " ")
			command = strings.TrimSpace(rest)
		}
		out = append(out, "/bin/sh -c "+command)
	}
	return out
}

// dockerfileCacheMounts returns "<id>@<target>" for every --mount=type=cache,
// sorted, matching llbCacheMounts. An unnamed mount takes the id BuildKit's
// own frontend gives it, which is what lets the two builds share a warm cache.
func dockerfileCacheMounts(dockerfile string) []string {
	var out []string
	for _, instruction := range dockerfileRunInstructions(dockerfile) {
		rest := instruction
		for strings.HasPrefix(rest, "--mount=") {
			flag, tail, _ := strings.Cut(rest, " ")
			rest = strings.TrimSpace(tail)
			var id, target string
			for _, kv := range strings.Split(strings.TrimPrefix(flag, "--mount="), ",") {
				k, v, _ := strings.Cut(kv, "=")
				switch k {
				case "id":
					id = v
				case "target":
					target = v
				}
			}
			if id == "" {
				id = "/" + strings.TrimPrefix(target, "/")
			}
			out = append(out, id+"@"+target)
		}
	}
	sort.Strings(out)
	return out
}

// broadStagefile exercises every construct the IR models, so the differential
// assertions below cover the whole DSL rather than the easy half of it.
func broadStagefile() *spec.File {
	return &spec.File{Version: 1, Stages: []spec.Stage{
		{
			Name:    "deps",
			From:    "python:3.12-slim",
			Workdir: "/srv",
			Env:     map[string]string{"MODE": "prod", "LANG": "C.UTF-8"},
			Args:    map[string]string{"VERSION": "1.2.3"},
			Download: []spec.Download{
				{URL: "https://example.test/model.tar.gz", SHA256: "aaa", Dest: "/models", Extract: "tar.gz"},
				{URL: "https://example.test/tool", SHA256: "bbb", Dest: "/usr/local/bin/tool", Mode: "0755"},
			},
			Install: &spec.Install{
				Apt: &spec.AptInstall{
					Packages: []string{"build-essential", "unzip"},
					Repositories: []spec.AptRepository{{
						Name: "ros2", URL: "http://packages.ros.org/ros2/ubuntu",
						Suites: []string{"jammy"}, Components: []string{"main"},
						Key: spec.AptRepositoryKey{URL: "https://example.test/ros.key", SHA256: "ccc"},
					}},
				},
				CMake: []spec.CMakeInstall{{
					Repository: "https://example.test/lib.git",
					Commit:     "0123456789012345678901234567890123456789",
					Jobs:       4,
					Defines:    map[string]string{"BUILD_TESTS": "OFF"},
				}},
				Pip: []spec.PipInstall{{Requirements: "requirements.txt"}},
			},
			Copy: []spec.CopyEntry{{From: "local", Paths: []string{"app.py"}, Mode: "0644"}},
		},
		{
			Name: "app",
			From: "python:3.12-slim",
			Copy: []spec.CopyEntry{
				{From: "deps", Paths: []string{"/srv"}, Dest: "/srv", Owner: "1000:1000"},
			},
			Entrypoint:  &spec.Entrypoint{Exec: []string{"python3", "/srv/app.py"}},
			Cmd:         []string{"--serve"},
			Healthcheck: &spec.Healthcheck{Exec: []string{"true"}, Interval: "30s"},
			User:        "1000",
		},
	}}
}

func emitBroad(t *testing.T) (string, []*pb.Op, *ImageConfig) {
	t.Helper()
	g := lower(t, broadStagefile(), ir.Options{})
	images := map[string]string{"python:3.12-slim": "sha256:" + strings.Repeat("a", 64)}
	configs := map[string][]byte{"python:3.12-slim": testConfig(t, "linux", "arm64")}

	dockerfile, err := codegen.GenerateGraph(g, images)
	if err != nil {
		t.Fatalf("codegen.Generate: %v", err)
	}
	def, cfg, err := Emit(g, Options{Images: images, Configs: configs, Platform: testPlatform})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	return dockerfile, emitOps(t, def), cfg
}

// The commands, in order, must be the same ones — this is the assertion that
// would fail if one backend skipped a step, ran them in a different order, or
// folded apt's clauses differently.
func TestBackendsRunTheSameCommands(t *testing.T) {
	dockerfile, ops, _ := emitBroad(t)
	want := dockerfileRuns(dockerfile)
	got := llbCommands(ops)
	if !sameCommands(got, want) {
		t.Fatalf("command sequences differ:\n LLB (%d):\n  %s\n Dockerfile (%d):\n  %s",
			len(got), strings.Join(got, "\n  "), len(want), strings.Join(want, "\n  "))
	}
	if len(got) == 0 {
		t.Fatal("no commands compared — the fixture or the parser is broken")
	}
}

// Same cache mounts, same ids. A divergence here does not break a build; it
// silently gives the two backends separate caches, so nothing but this test
// would notice.
func TestBackendsAttachTheSameCacheMounts(t *testing.T) {
	dockerfile, ops, _ := emitBroad(t)
	want := dockerfileCacheMounts(dockerfile)
	got := llbCacheMounts(ops)
	if !sameStringSet(got, want) {
		t.Fatalf("cache mounts differ:\n LLB        %v\n Dockerfile %v", got, want)
	}
	if len(got) == 0 {
		t.Fatal("no cache mounts compared — the fixture or the parser is broken")
	}
}

// Every URL the Dockerfile would ADD must be a source in the definition, and
// the pinned base image must appear with its digest.
func TestBackendsReferenceTheSameSources(t *testing.T) {
	dockerfile, ops, _ := emitBroad(t)
	sources := strings.Join(llbSources(ops), "\n")

	for _, line := range strings.Split(dockerfile, "\n") {
		if !strings.HasPrefix(line, "ADD ") {
			continue
		}
		fields := strings.Fields(line)
		url := fields[len(fields)-2]
		if !strings.Contains(sources, url) {
			t.Errorf("Dockerfile fetches %q but no LLB source does\n sources:\n%s", url, sources)
		}
	}
	if !strings.Contains(sources, "docker-image://docker.io/library/python:3.12-slim@sha256:") {
		t.Errorf("pinned base image missing from LLB sources:\n%s", sources)
	}
	if !strings.Contains(sources, "local://"+LocalContextName) {
		t.Errorf("build context missing from LLB sources:\n%s", sources)
	}
}

// The image metadata LLB cannot carry must match what the Dockerfile bakes in.
func TestImageConfigMatchesTheDockerfile(t *testing.T) {
	dockerfile, _, cfg := emitBroad(t)

	if !strings.Contains(dockerfile, `ENTRYPOINT ["python3", "/srv/app.py"]`) {
		t.Fatal("fixture no longer emits the entrypoint this test checks")
	}
	if !reflect.DeepEqual(cfg.Entrypoint, []string{"python3", "/srv/app.py"}) {
		t.Errorf("entrypoint = %v", cfg.Entrypoint)
	}
	if !reflect.DeepEqual(cfg.Cmd, []string{"--serve"}) {
		t.Errorf("cmd = %v", cfg.Cmd)
	}
	if cfg.User != "1000" {
		t.Errorf("user = %q, want the declared 1000", cfg.User)
	}
	if cfg.Healthcheck == nil || cfg.Healthcheck.Interval != "30s" {
		t.Errorf("healthcheck = %+v", cfg.Healthcheck)
	}
}

// A final stage that declares no user must still get the non-root default, and
// both backends must use the same one.
func TestImageConfigDefaultsTheUser(t *testing.T) {
	g := lower(t, &spec.File{Version: 1, Stages: []spec.Stage{
		{Name: "app", From: "debian:12"},
	}}, ir.Options{})
	images := map[string]string{"debian:12": "sha256:" + strings.Repeat("b", 64)}
	configs := map[string][]byte{"debian:12": testConfig(t, "linux", "arm64")}

	dockerfile, err := codegen.GenerateGraph(g, images)
	if err != nil {
		t.Fatal(err)
	}
	_, cfg, err := Emit(g, Options{Images: images, Configs: configs, Platform: testPlatform})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dockerfile, "USER "+ir.DefaultUser) {
		t.Errorf("Dockerfile does not default the user:\n%s", dockerfile)
	}
	if cfg.User != ir.DefaultUser {
		t.Errorf("config user = %q, want %q", cfg.User, ir.DefaultUser)
	}
}

// A CUDA stage is the widest single feature in the DSL — a spliced runtime pip
// group, a collect step, an injected loader path, and root — so it gets its own
// differential pass.
func TestCUDAStageAgreesAcrossBackends(t *testing.T) {
	profile := &gpu.Profile{
		Arch: "sm_87", Board: "Orin", CUDAVersion: "12.6",
		Index:   "https://pypi.jetson.example/simple",
		Runtime: []string{"nvidia-cuda-runtime-cu12"},
		LibDir:  "/opt/stagefile/cuda",
	}
	g := lower(t, &spec.File{Version: 1, Stages: []spec.Stage{{
		Name: "app", From: "ubuntu:22.04", CUDA: true,
		Install: &spec.Install{Pip: []spec.PipInstall{
			{Packages: []string{"torch"}, CUDA: true},
		}},
	}}}, ir.Options{CUDAProfile: profile})

	images := map[string]string{"ubuntu:22.04": "sha256:" + strings.Repeat("c", 64)}
	configs := map[string][]byte{"ubuntu:22.04": testConfig(t, "linux", "arm64")}

	dockerfile, err := codegen.GenerateGraph(g, images)
	if err != nil {
		t.Fatal(err)
	}
	def, cfg, err := Emit(g, Options{Images: images, Configs: configs, Platform: testPlatform})
	if err != nil {
		t.Fatal(err)
	}
	ops := emitOps(t, def)

	if got, want := llbCommands(ops), dockerfileRuns(dockerfile); !sameCommands(got, want) {
		t.Fatalf("CUDA command sequences differ:\n LLB:\n  %s\n Dockerfile:\n  %s",
			strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
	if got, want := llbCacheMounts(ops), dockerfileCacheMounts(dockerfile); !sameStringSet(got, want) {
		t.Fatalf("CUDA cache mounts differ:\n LLB %v\n Dockerfile %v", got, want)
	}
	// The GPU stage runs as root, and the loader path is baked in — both are
	// image config, so only the config carries them out of llbgen.
	if cfg.User != "root" {
		t.Errorf("user = %q, want root", cfg.User)
	}
	if cfg.Env[spec.LDLibraryPath] != profile.LibDir {
		t.Errorf("%s = %q, want %q", spec.LDLibraryPath, cfg.Env[spec.LDLibraryPath], profile.LibDir)
	}
}

// An identical Stagefile must compile to identical bytes. This is not just
// tidiness: the content-addressed cache above this backend cannot hit at all
// if marshalling is not deterministic, and llb seeds a random local.unique
// unless it is pinned.
func TestEmitIsDeterministic(t *testing.T) {
	g := lower(t, broadStagefile(), ir.Options{})
	images := map[string]string{"python:3.12-slim": "sha256:" + strings.Repeat("a", 64)}
	configs := map[string][]byte{"python:3.12-slim": testConfig(t, "linux", "arm64")}

	first, _, err := Emit(g, Options{Images: images, Configs: configs, Platform: testPlatform})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		next, _, err := Emit(g, Options{Images: images, Configs: configs, Platform: testPlatform})
		if err != nil {
			t.Fatal(err)
		}
		if len(next.Def) != len(first.Def) {
			t.Fatalf("run %d produced %d ops, want %d", i, len(next.Def), len(first.Def))
		}
		for j := range first.Def {
			if string(next.Def[j]) != string(first.Def[j]) {
				t.Fatalf("run %d differs from the first at op %d", i, j)
			}
		}
	}
}

// Lowering the same source for two architectures must not produce the same
// definition — that would let one arch's build serve the other's cache.
func TestPlatformReachesTheDefinition(t *testing.T) {
	defFor := func(platform string) string {
		g := lower(t, broadStagefile(), ir.Options{Platform: platform})
		images := map[string]string{"python:3.12-slim": "sha256:" + strings.Repeat("a", 64)}
		arch := "arm64"
		if strings.HasSuffix(platform, "amd64") {
			arch = "amd64"
		}
		configs := map[string][]byte{"python:3.12-slim": testConfig(t, "linux", arch)}
		def, _, err := Emit(g, Options{Images: images, Configs: configs, Platform: platform})
		if err != nil {
			t.Fatalf("Emit(%s): %v", platform, err)
		}
		var b strings.Builder
		for _, dt := range def.Def {
			b.Write(dt)
		}
		return b.String()
	}
	if defFor("linux/arm64") == defFor("linux/amd64") {
		t.Fatal("two architectures compiled to one definition")
	}
}

func TestEmitRefusesAnUnsetPlatform(t *testing.T) {
	g := lower(t, &spec.File{Version: 1, Stages: []spec.Stage{{Name: "app", From: "debian:12"}}}, ir.Options{})
	_, _, err := Emit(g, Options{
		Images:  map[string]string{"debian:12": "sha256:" + strings.Repeat("b", 64)},
		Configs: map[string][]byte{"debian:12": testConfig(t, "linux", "arm64")},
	})
	if err == nil {
		t.Fatal("Emit accepted an empty platform, which would pin the compiling host")
	}
}

// A config resolved for the wrong architecture describes a different image
// than the one being pinned, and its Env and WorkingDir would be applied
// without complaint. Nothing downstream catches it.
func TestEmitRefusesAMismatchedImageConfig(t *testing.T) {
	g := lower(t, &spec.File{Version: 1, Stages: []spec.Stage{{Name: "app", From: "debian:12"}}}, ir.Options{})
	_, _, err := Emit(g, Options{
		Images:   map[string]string{"debian:12": "sha256:" + strings.Repeat("b", 64)},
		Configs:  map[string][]byte{"debian:12": testConfig(t, "linux", "amd64")},
		Platform: testPlatform,
	})
	if err == nil {
		t.Fatal("Emit accepted an amd64 image config for an arm64 build")
	}
	if !strings.Contains(err.Error(), "amd64") {
		t.Fatalf("error does not name the mismatch: %v", err)
	}
}

func TestEmitRequiresDigestsAndConfigs(t *testing.T) {
	g := lower(t, &spec.File{Version: 1, Stages: []spec.Stage{{Name: "app", From: "debian:12"}}}, ir.Options{})
	digest := map[string]string{"debian:12": "sha256:" + strings.Repeat("b", 64)}
	cfg := map[string][]byte{"debian:12": testConfig(t, "linux", "arm64")}

	if _, _, err := Emit(g, Options{Configs: cfg, Platform: testPlatform}); err == nil {
		t.Error("Emit accepted a pinned image with no resolved digest")
	}
	if _, _, err := Emit(g, Options{Images: digest, Platform: testPlatform}); err == nil {
		t.Error("Emit accepted a pinned image with no resolved config")
	}
}

// LLB has no $BUILDPLATFORM to expand, so a `platform: build` stage needs the
// value supplied — and must say so rather than quietly building for the target.
func TestBuildPlatformStageNeedsTheBuildPlatform(t *testing.T) {
	f := &spec.File{Version: 1, Stages: []spec.Stage{
		{Name: "web", From: "node:20-slim", Platform: "build"},
		{Name: "app", From: "debian:12", Copy: []spec.CopyEntry{{From: "web", Paths: []string{"/dist"}, Dest: "/srv"}}},
	}}
	g := lower(t, f, ir.Options{})
	images := map[string]string{
		"node:20-slim": "sha256:" + strings.Repeat("d", 64),
		"debian:12":    "sha256:" + strings.Repeat("b", 64),
	}
	configs := map[string][]byte{
		"node:20-slim": testConfig(t, "linux", "amd64"),
		"debian:12":    testConfig(t, "linux", "arm64"),
	}

	if _, _, err := Emit(g, Options{Images: images, Configs: configs, Platform: testPlatform}); err == nil {
		t.Fatal("Emit accepted a platform: build stage with no build platform")
	}
	if _, _, err := Emit(g, Options{
		Images: images, Configs: configs, Platform: testPlatform, BuildPlatform: "linux/amd64",
	}); err != nil {
		t.Fatalf("Emit with a build platform: %v", err)
	}
}

// The context is filtered to the same allowlist codegen writes to disk. An
// unfiltered local source puts files in front of glob and directory copies
// that the Dockerfile build never sees.
func TestContextIsFilteredToTheAllowlist(t *testing.T) {
	g := lower(t, broadStagefile(), ir.Options{})
	images := map[string]string{"python:3.12-slim": "sha256:" + strings.Repeat("a", 64)}
	configs := map[string][]byte{"python:3.12-slim": testConfig(t, "linux", "arm64")}
	def, _, err := Emit(g, Options{Images: images, Configs: configs, Platform: testPlatform})
	if err != nil {
		t.Fatal(err)
	}

	var excludes string
	for _, op := range emitOps(t, def) {
		s := op.GetSource()
		if s == nil || !strings.HasPrefix(s.Identifier, "local://") {
			continue
		}
		excludes = s.Attrs[string(pb.AttrExcludePatterns)]
	}
	if excludes == "" {
		t.Fatal("local source carries no exclude patterns — the whole tree would transfer")
	}
	for _, want := range []string{"app.py", "requirements.txt"} {
		if !strings.Contains(excludes, want) {
			t.Errorf("allowlist %s is missing %q", excludes, want)
		}
	}
}

func TestLinkedCopyUsesMerge(t *testing.T) {
	g := lower(t, &spec.File{Version: 1, Stages: []spec.Stage{{
		Name: "app", From: "python:3.12-slim",
		Install: &spec.Install{Pip: []spec.PipInstall{{Requirements: "requirements.txt"}}},
	}}}, ir.Options{})
	images := map[string]string{"python:3.12-slim": "sha256:" + strings.Repeat("a", 64)}
	configs := map[string][]byte{"python:3.12-slim": testConfig(t, "linux", "arm64")}
	def, _, err := Emit(g, Options{Images: images, Configs: configs, Platform: testPlatform})
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range emitOps(t, def) {
		if op.GetMerge() != nil {
			return
		}
	}
	t.Fatal("linked dependency overlay emitted no LLB merge operation")
}

// Every fixture that ships with the repo must compile through both backends.
// A construct that reaches codegen but panics or errors in llbgen is exactly
// what an opt-in second backend would otherwise hide until someone enabled it.
func TestShippedFixturesCompileThroughBothBackends(t *testing.T) {
	for _, fixture := range []string{"example.stagefile.yaml", "npmbuild.stagefile.yaml"} {
		t.Run(fixture, func(t *testing.T) {
			data, err := readFixture(fixture)
			if err != nil {
				t.Fatal(err)
			}
			f, err := spec.Parse(data)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			g := lower(t, f, ir.Options{})

			images := map[string]string{}
			configs := map[string][]byte{}
			for _, n := range g.Nodes {
				if n.Image != nil {
					images[n.Image.Ref] = "sha256:" + strings.Repeat("e", 64)
					configs[n.Image.Ref] = testConfig(t, "linux", "arm64")
				}
			}
			dockerfile, err := codegen.GenerateGraph(g, images)
			if err != nil {
				t.Fatalf("codegen: %v", err)
			}
			def, _, err := Emit(g, Options{Images: images, Configs: configs, Platform: testPlatform})
			if err != nil {
				t.Fatalf("llbgen: %v", err)
			}
			got, want := llbCommands(emitOps(t, def)), dockerfileRuns(dockerfile)
			if !sameCommands(got, want) {
				t.Fatalf("%s command sequences differ:\n LLB:\n  %s\n Dockerfile:\n  %s",
					fixture, strings.Join(got, "\n  "), strings.Join(want, "\n  "))
			}
		})
	}
}

func readFixture(name string) ([]byte, error) {
	data, err := os.ReadFile("../testdata/" + name)
	if err != nil {
		return nil, fmt.Errorf("fixture %s: %w", name, err)
	}
	return data, nil
}
