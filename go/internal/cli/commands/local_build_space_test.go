package commands

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// fakeBuildDisks reports free bytes per directory; any other directory's free
// space cannot be read.
func fakeBuildDisks(t *testing.T, free map[string]int64) {
	t.Helper()
	orig := localBuildDiskAvail
	t.Cleanup(func() { localBuildDiskAvail = orig })
	localBuildDiskAvail = func(path string) (int64, bool) {
		n, ok := free[path]
		return n, ok
	}
}

// fakeAllBuildDisks reports the same free bytes for every directory.
func fakeAllBuildDisks(t *testing.T, free int64) {
	t.Helper()
	orig := localBuildDiskAvail
	t.Cleanup(func() { localBuildDiskAvail = orig })
	localBuildDiskAvail = func(string) (int64, bool) { return free, true }
}

func TestLocalBuildSpaceRefusesWhenADiskIsNearlyFull(t *testing.T) {
	fakeBuildDisks(t, map[string]int64{"/cache": 40 << 30, "/project": 2 << 30})
	steps := []string{"Run `wendy cache clear` to delete Wendy's cached builds and downloads."}

	err := checkLocalBuildSpace([]string{"/cache", "/project"}, steps)
	if ErrorClass(err) != "disk_full" {
		t.Fatalf("class = %q, want disk_full (err %v)", ErrorClass(err), err)
	}
	if msg := err.Error(); !strings.Contains(msg, "2.0 GiB free") || !strings.Contains(msg, "/project") {
		t.Fatalf("message does not name the full disk and its free space: %q", msg)
	}
	// The MCP run tool recognizes the refusal by this phrase
	// (localBuildSpaceRefusal in package mcp).
	if !strings.HasPrefix(err.Error(), "not enough free disk space to build") {
		t.Fatalf("message no longer starts with the phrase the MCP run tool matches: %q", err)
	}
	if got := NextSteps(err); !slices.Equal(got, steps) {
		t.Fatalf("next steps = %q, want %q", got, steps)
	}
}

func TestLocalBuildSpaceWarnsButBuildsWhenSpaceIsLow(t *testing.T) {
	fakeBuildDisks(t, map[string]int64{"/cache": 6 << 30})
	var err error
	stderr := captureStderr(t, func() { err = checkLocalBuildSpace([]string{"/cache"}, nil) })
	if err != nil {
		t.Fatalf("refused a build that may fit: %v", err)
	}
	if !strings.Contains(stderr, "6.0 GiB free") || !strings.Contains(stderr, "/cache") {
		t.Fatalf("no warning naming the disk: %q", stderr)
	}
}

func TestLocalBuildSpaceIsQuietWithPlentyFree(t *testing.T) {
	fakeBuildDisks(t, map[string]int64{"/cache": 50 << 30})
	var err error
	stderr := captureStderr(t, func() { err = checkLocalBuildSpace([]string{"/cache"}, nil) })
	if err != nil || stderr != "" {
		t.Fatalf("err = %v, stderr = %q; want neither", err, stderr)
	}
}

func TestLocalBuildSpaceDoesNotBlockWhenFreeSpaceIsUnknown(t *testing.T) {
	fakeBuildDisks(t, map[string]int64{})
	var err error
	stderr := captureStderr(t, func() { err = checkLocalBuildSpace([]string{"/cache"}, nil) })
	if err != nil || stderr != "" {
		t.Fatalf("err = %v, stderr = %q; an unreadable disk must not block or warn", err, stderr)
	}

	// An unreadable directory does not hide a readable one that is full.
	fakeBuildDisks(t, map[string]int64{"/project": 1 << 30})
	if err := checkLocalBuildSpace([]string{"/cache", "/project"}, nil); ErrorClass(err) != "disk_full" {
		t.Fatalf("err = %v, want a disk_full refusal for /project", err)
	}
}

// On Linux Docker keeps images and build cache under its own data directory,
// often a separate partition. On macOS and Windows that data lives in a
// virtual disk under the user's home, on the cache directory's disk.
func TestImageBuildSpaceCountsDockerDataOnLinux(t *testing.T) {
	origGOOS, origRoot := imageBuilderHostGOOS, dockerDataRoot
	t.Cleanup(func() { imageBuilderHostGOOS, dockerDataRoot = origGOOS, origRoot })
	dockerDataRoot = t.TempDir()
	fakeBuildDisks(t, map[string]int64{dockerDataRoot: 1 << 30})

	imageBuilderHostGOOS = func() string { return "linux" }
	err := checkLocalBuildSpace(imageBuildSpaceDirs(), nil)
	if ErrorClass(err) != "disk_full" || !strings.Contains(err.Error(), dockerDataRoot) {
		t.Fatalf("linux: err = %v, want a refusal naming %s", err, dockerDataRoot)
	}

	imageBuilderHostGOOS = func() string { return "darwin" }
	if err := checkLocalBuildSpace(imageBuildSpaceDirs(), nil); err != nil {
		t.Fatalf("darwin: Docker's data directory is not on this host: %v", err)
	}

	imageBuilderHostGOOS = func() string { return "linux" }
	dockerDataRoot = filepath.Join(t.TempDir(), "missing")
	fakeBuildDisks(t, map[string]int64{dockerDataRoot: 1 << 30})
	if err := checkLocalBuildSpace(imageBuildSpaceDirs(), nil); err != nil {
		t.Fatalf("linux without Docker data: %v", err)
	}
}

// Swift and Xcode builds write their output into the project.
func TestToolchainBuildSpaceCountsTheProject(t *testing.T) {
	project := t.TempDir()
	fakeBuildDisks(t, map[string]int64{project: 1 << 30})
	err := checkLocalBuildSpace(toolchainBuildSpaceDirs(project), nil)
	if ErrorClass(err) != "disk_full" || !strings.Contains(err.Error(), project) {
		t.Fatalf("err = %v, want a refusal naming %s", err, project)
	}
}

func TestImageBuildSpaceStepsMatchTheBuild(t *testing.T) {
	origGOOS := imageBuilderHostGOOS
	t.Cleanup(func() { imageBuilderHostGOOS = origGOOS })
	imageBuilderHostGOOS = func() string { return "linux" }
	t.Setenv("WENDY_AGENT_SOCKET", "")

	for _, tc := range []struct {
		builder                string
		buildHost              bool
		wantDocker, wantRemote bool
	}{
		{builder: "", buildHost: true, wantDocker: true, wantRemote: true},
		{builder: "docker", buildHost: false, wantDocker: true, wantRemote: false},
		{builder: "apple-container", buildHost: true, wantDocker: false, wantRemote: true},
	} {
		steps := strings.Join(imageBuildSpaceSteps(tc.builder, tc.buildHost), "\n")
		if got := strings.Contains(steps, "docker system prune"); got != tc.wantDocker {
			t.Errorf("builder %q: suggests docker system prune = %v, want %v", tc.builder, got, tc.wantDocker)
		}
		if got := strings.Contains(steps, "--build-host"); got != tc.wantRemote {
			t.Errorf("builder %q, build host %v: suggests --build-host = %v, want %v", tc.builder, tc.buildHost, got, tc.wantRemote)
		}
		if !strings.Contains(steps, "wendy cache clear") {
			t.Errorf("builder %q: no wendy cache clear step", tc.builder)
		}
	}
}

// failDockerUse fails the test if anything runs docker.
func failDockerUse(t *testing.T) {
	t.Helper()
	origCmd, origVersion := imageBuilderCommandContext, dockerVersionOKFn
	t.Cleanup(func() { imageBuilderCommandContext, dockerVersionOKFn = origCmd, origVersion })
	imageBuilderCommandContext = func(_ context.Context, name string, arg ...string) *exec.Cmd {
		t.Errorf("ran %s %v before refusing", name, arg)
		return exec.Command(os.Args[0], "-test.run=^$")
	}
	dockerVersionOKFn = func(context.Context) bool {
		t.Error("probed the Docker daemon before refusing")
		return false
	}
}

func TestRunRefusesToBuildOnANearlyFullDisk(t *testing.T) {
	isolateFingerprintCache(t)
	fakeAllBuildDisks(t, 1<<30)
	failDockerUse(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &appconfig.AppConfig{AppID: "low-disk", Platform: "linux"}
	opts := runOptions{detach: true}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := &fastPathContainerClient{appName: cfg.AppID, state: agentpb.AppRunningState_STOPPED}
	conn := &grpcclient.AgentConnection{ContainerService: client}
	conn.CacheAgentVersion(&agentpb.GetAgentVersionResponse{Os: "linux", CpuArchitecture: "arm64"})

	err := runWithAgentRecorded(ctx, conn, dir, cfg, opts, newDeployMetrics(opts))
	if ErrorClass(err) != "disk_full" {
		t.Fatalf("err = %v (class %q), want a disk_full refusal", err, ErrorClass(err))
	}
}

// A run whose image is already on the device builds nothing, so a full disk
// must not stop it.
func TestRunFastPathIgnoresLowDiskSpace(t *testing.T) {
	isolateFingerprintCache(t)
	fakeAllBuildDisks(t, 1<<30)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &appconfig.AppConfig{AppID: "low-disk-fastpath", Platform: "linux"}
	opts := runOptions{detach: true}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	version := &agentpb.GetAgentVersionResponse{Os: "linux", CpuArchitecture: "arm64"}
	inputs, err := singleServiceDesiredHash(dir, "", "linux/arm64", resolvedStagefileBackend(ctx),
		map[string]string{"WENDY_PLATFORM": "generic", "WENDY_DEBUG": "false", "WENDY_HAS_CUDA": "false"}, mergeEnvEntries(resolveServiceEnv(cfg), opts.env), cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	saveDeployFingerprint(cfg.AppID, deviceFingerprintKey(version), deployFingerprint{InputHash: inputs, LayerDiffIDs: []string{"layer"}})
	client := &fastPathContainerClient{appName: cfg.AppID, state: agentpb.AppRunningState_STOPPED, presentLayers: map[string]bool{"layer": true}}
	conn := &grpcclient.AgentConnection{ContainerService: client}
	conn.CacheAgentVersion(version)

	if err := runWithAgentRecorded(ctx, conn, dir, cfg, opts, newDeployMetrics(opts)); err != nil {
		t.Fatalf("fast path refused: %v", err)
	}
}

func TestMissingBuildxIsReportedWithItsFix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stub docker uses the POSIX true/false commands")
	}
	origCmd, origVersion := imageBuilderCommandContext, dockerVersionOKFn
	t.Cleanup(func() { imageBuilderCommandContext, dockerVersionOKFn = origCmd, origVersion })
	t.Setenv("HOME", t.TempDir())
	t.Setenv("BUILDX_CONFIG", "")
	t.Setenv("DOCKER_CONFIG", "")
	dockerVersionOKFn = func(context.Context) bool { return true }

	for _, tc := range []struct {
		name          string
		buildxPresent bool
	}{{"buildx missing", false}, {"buildx present", true}} {
		// Every buildx command fails, as it does when the plugin is missing,
		// except `docker buildx version` when the plugin is there.
		imageBuilderCommandContext = func(ctx context.Context, name string, arg ...string) *exec.Cmd {
			if tc.buildxPresent && slices.Equal(arg, []string{"buildx", "version"}) {
				return exec.CommandContext(ctx, "true")
			}
			return exec.CommandContext(ctx, "false")
		}
		for site, setup := range map[string]func() error{
			"oci export builder": func() error { _, err := ensureOCIExportBuilder(context.Background(), &strings.Builder{}); return err },
			"registry builder":   func() error { _, err := ensureRegistryBuilderInstance(context.Background(), "wendy"); return err },
		} {
			err := setup()
			if err == nil {
				t.Fatalf("%s, %s: builder setup succeeded", tc.name, site)
			}
			missing := ErrorClass(err) == "builder_unavailable" && strings.Contains(err.Error(), "Buildx is not installed")
			if missing != !tc.buildxPresent {
				t.Errorf("%s, %s: reported missing Buildx = %v: %v", tc.name, site, missing, err)
			}
			if !tc.buildxPresent && !strings.Contains(strings.Join(NextSteps(err), "\n"), "https://docs.docker.com/go/buildx/") {
				t.Errorf("%s, %s: no install step: %q", tc.name, site, NextSteps(err))
			}
		}
	}
}

// A missing Buildx plugin fails the fast deploy. When the registry push would
// build with Docker too, falling back to it only repeats the same failure; on
// Apple Silicon with no --builder it tries Apple Container first, which needs
// no Buildx, so the fallback stays.
func TestMissingBuildxFallsBackOnlyToAPushThatCanWork(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stub docker uses the POSIX false command")
	}
	for _, tc := range []struct {
		name, builder, goos string
		wantFallback        bool
	}{
		{"docker builder", "docker", "linux", false},
		{"Apple Silicon default", "", "darwin", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateFingerprintCache(t)
			fakeAllBuildDisks(t, 100<<30)
			origCmd, origVersion := imageBuilderCommandContext, dockerVersionOKFn
			origGOOS, origGOARCH := imageBuilderHostGOOS, imageBuilderHostGOARCH
			t.Cleanup(func() {
				imageBuilderCommandContext, dockerVersionOKFn = origCmd, origVersion
				imageBuilderHostGOOS, imageBuilderHostGOARCH = origGOOS, origGOARCH
			})
			t.Setenv("DOCKER_CONFIG", t.TempDir())
			t.Setenv("BUILDX_CONFIG", "")
			t.Setenv("WENDY_AGENT_SOCKET", "")
			imageBuilderHostGOOS = func() string { return tc.goos }
			imageBuilderHostGOARCH = func() string { return "arm64" }
			dockerVersionOKFn = func(context.Context) bool { return true }
			imageBuilderCommandContext = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
				return exec.CommandContext(ctx, "false")
			}

			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			cfg := &appconfig.AppConfig{AppID: "no-buildx", Platform: "linux"}
			opts := runOptions{detach: true, builder: tc.builder}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			client := &fastPathContainerClient{appName: cfg.AppID, state: agentpb.AppRunningState_STOPPED}
			conn := &grpcclient.AgentConnection{ContainerService: client, ProvisioningService: unprovisionedDevice{}}
			conn.CacheAgentVersion(&agentpb.GetAgentVersionResponse{Os: "linux", CpuArchitecture: "arm64"})

			var err error
			stderr := captureStderr(t, func() { err = runWithAgentRecorded(ctx, conn, dir, cfg, opts, newDeployMetrics(opts)) })
			if err == nil {
				t.Fatal("run succeeded without Buildx")
			}
			if fellBack := strings.Contains(stderr, "falling back to registry push"); fellBack != tc.wantFallback {
				t.Fatalf("fell back to the registry push = %v, want %v; err %v\nstderr:\n%s", fellBack, tc.wantFallback, err, stderr)
			}
			if !tc.wantFallback && !errors.Is(err, errBuildxMissing) {
				t.Fatalf("err = %v, want the missing-Buildx error", err)
			}
		})
	}
}
