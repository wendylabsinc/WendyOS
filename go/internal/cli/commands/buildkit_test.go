package commands

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestNormalizeImageBuilder_Buildkit(t *testing.T) {
	got, err := normalizeImageBuilder("buildkit")
	if err != nil {
		t.Fatalf("normalizeImageBuilder(buildkit) error = %v", err)
	}
	if got != imageBuilderBuildkit {
		t.Fatalf("got %q, want %q", got, imageBuilderBuildkit)
	}
}

func TestBuildkitOCIArgs(t *testing.T) {
	args := buildkitOCIArgs("/work", "/work", "Dockerfile", "linux/arm64",
		map[string]string{"FOO": "bar", "ABC": "1"}, "/tmp/out.tar")
	want := []string{
		"build",
		"--frontend", "dockerfile.v0",
		"--local", "context=/work",
		"--local", "dockerfile=/work",
		"--opt", "filename=Dockerfile",
		"--opt", "platform=linux/arm64",
		"--opt", "build-arg:ABC=1", // sorted keys → ABC before FOO
		"--opt", "build-arg:FOO=bar",
		"--output", "type=oci,dest=/tmp/out.tar",
	}
	if !slices.Equal(args, want) {
		t.Fatalf("buildkitOCIArgs mismatch:\n got: %v\nwant: %v", args, want)
	}
}

func TestBuildkitOCIDirArgs(t *testing.T) {
	args := buildkitOCIDirArgs("/work", "/work", "Dockerfile", "linux/arm64", nil, "/tmp/layout")
	if !slices.Contains(args, "type=oci,dest=/tmp/layout,compression=uncompressed,tar=false") {
		t.Fatalf("buildkitOCIDirArgs = %v, want persistent directory output", args)
	}
}

func TestBuildkitImageStoreArgs(t *testing.T) {
	args := buildkitImageStoreArgs("/work", "/work", "Containerfile", "linux/arm64",
		"sh.wendy.test:latest", map[string]string{"FOO": "bar", "ABC": "1"})
	want := []string{
		"build",
		"--frontend", "dockerfile.v0",
		"--local", "context=/work",
		"--local", "dockerfile=/work",
		"--opt", "filename=Containerfile",
		"--opt", "platform=linux/arm64",
		"--opt", "build-arg:ABC=1",
		"--opt", "build-arg:FOO=bar",
		"--output", "type=image,name=sh.wendy.test:latest,store=true,unpack=true,oci-mediatypes=true",
	}
	if !slices.Equal(args, want) {
		t.Fatalf("buildkitImageStoreArgs mismatch:\n got: %v\nwant: %v", args, want)
	}
}

func TestBuildkitOCIExportUsesManagedRuntimeEndpoint(t *testing.T) {
	original := localBuildkitCommandContext
	t.Cleanup(func() { localBuildkitCommandContext = original })

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(wendyBuildkitHostEnv, "unix:///tmp/wendy-buildkitd.sock")

	var gotArgs []string
	localBuildkitCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if name != "buildctl" {
			t.Fatalf("command = %q, want buildctl", name)
		}
		gotArgs = append([]string(nil), args...)
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestBuildkitStoreHelperProcess")
		cmd.Env = append(os.Environ(), "GO_WANT_BUILDKIT_STORE_HELPER=1")
		return cmd
	}

	err := buildImageToOCILayoutWithBuildkit(
		context.Background(), dir, "Dockerfile", "linux/arm64", nil,
		filepath.Join(dir, "image.tar"), &bytes.Buffer{}, &bytes.Buffer{},
	)
	if err != nil {
		t.Fatalf("buildImageToOCILayoutWithBuildkit: %v", err)
	}
	if len(gotArgs) < 2 || gotArgs[0] != "--addr" || gotArgs[1] != "unix:///tmp/wendy-buildkitd.sock" {
		t.Fatalf("args = %v, want managed runtime --addr prefix", gotArgs)
	}
	if !slices.Contains(gotArgs, "type=oci,dest="+filepath.Join(dir, "image.tar")) {
		t.Fatalf("args = %v, want OCI export destination", gotArgs)
	}
}

func TestBuildkitPersistentOCIExportUsesDirectoryOutput(t *testing.T) {
	original := localBuildkitCommandContext
	t.Cleanup(func() { localBuildkitCommandContext = original })

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(wendyBuildkitHostEnv, "unix:///tmp/wendy-buildkitd.sock")

	var gotArgs []string
	localBuildkitCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if name != "buildctl" {
			t.Fatalf("command = %q, want buildctl", name)
		}
		gotArgs = append([]string(nil), args...)
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestBuildkitStoreHelperProcess")
		cmd.Env = append(os.Environ(), "GO_WANT_BUILDKIT_STORE_HELPER=1")
		return cmd
	}

	layout := filepath.Join(dir, "layout")
	err := buildImageToOCILayoutDir(
		context.Background(), dir, "Dockerfile", "linux/arm64", nil,
		imageBuilderBuildkit, layout, &bytes.Buffer{}, &bytes.Buffer{},
	)
	if err != nil {
		t.Fatalf("buildImageToOCILayoutDir(buildkit): %v", err)
	}
	if len(gotArgs) < 2 || gotArgs[0] != "--addr" || gotArgs[1] != "unix:///tmp/wendy-buildkitd.sock" {
		t.Fatalf("args = %v, want managed runtime --addr prefix", gotArgs)
	}
	if !slices.Contains(gotArgs, "type=oci,dest="+layout+",compression=uncompressed,tar=false") {
		t.Fatalf("args = %v, want persistent OCI directory output", gotArgs)
	}
	if info, statErr := os.Stat(layout); statErr != nil || !info.IsDir() {
		t.Fatalf("persistent layout directory was not created: info=%v err=%v", info, statErr)
	}
}

func TestBuildkitCommandArgsWendyEndpointTakesPrecedence(t *testing.T) {
	t.Setenv("BUILDKIT_HOST", "tcp://generic.example:1234")
	t.Setenv(wendyBuildkitHostEnv, "unix:///tmp/wendy-buildkitd.sock")
	got, err := buildkitCommandArgs([]string{"build", "--frontend", "dockerfile.v0"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--addr", "unix:///tmp/wendy-buildkitd.sock", "build", "--frontend", "dockerfile.v0"}
	if !slices.Equal(got, want) {
		t.Fatalf("buildkitCommandArgs = %v, want %v", got, want)
	}
}

func TestBuildkitCommandArgsLeavesBuildkitHostToBuildctl(t *testing.T) {
	t.Setenv("BUILDKIT_HOST", "tcp://generic.example:1234")
	t.Setenv(wendyBuildkitHostEnv, "")
	in := []string{"build", "--frontend", "dockerfile.v0"}
	got, err := buildkitCommandArgs(in)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, in) {
		t.Fatalf("buildkitCommandArgs = %v, want unchanged %v", got, in)
	}
}

func TestBuildkitCommandArgsDiscoversManagedRuntimeSocket(t *testing.T) {
	originalCacheDir := wendyRuntimeCacheDir
	originalSocketPresent := managedBuildkitSocketPresent
	t.Cleanup(func() {
		wendyRuntimeCacheDir = originalCacheDir
		managedBuildkitSocketPresent = originalSocketPresent
	})

	cacheDir := "/cache/wendy"
	socket := filepath.Join(cacheDir, "runtime", "buildkitd.sock")
	wendyRuntimeCacheDir = func() (string, error) { return cacheDir, nil }
	managedBuildkitSocketPresent = func(path string) bool { return path == socket }
	t.Setenv(wendyBuildkitHostEnv, "")
	t.Setenv("BUILDKIT_HOST", "")
	got, err := buildkitCommandArgs([]string{"build"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--addr", "unix://" + socket, "build"}
	if !slices.Equal(got, want) {
		t.Fatalf("buildkitCommandArgs = %v, want %v", got, want)
	}
}

func TestBuildkitCommandArgsPreservesDefaultLocalDaemon(t *testing.T) {
	originalCacheDir := wendyRuntimeCacheDir
	originalSocketPresent := managedBuildkitSocketPresent
	t.Cleanup(func() {
		wendyRuntimeCacheDir = originalCacheDir
		managedBuildkitSocketPresent = originalSocketPresent
	})

	wendyRuntimeCacheDir = func() (string, error) { return "/cache/wendy", nil }
	managedBuildkitSocketPresent = func(string) bool { return false }
	t.Setenv(wendyBuildkitHostEnv, "")
	t.Setenv("BUILDKIT_HOST", "")
	in := []string{"build"}
	got, err := buildkitCommandArgs(in)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, in) {
		t.Fatalf("buildkitCommandArgs = %v, want unchanged %v", got, in)
	}
}

func TestBuildDockerProjectWithBuildkitStoresAndUnpacksInContainerd(t *testing.T) {
	original := localBuildkitCommandContext
	t.Cleanup(func() { localBuildkitCommandContext = original })

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Containerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(wendyBuildkitHostEnv, "unix:///tmp/wendy-buildkitd.sock")

	var gotName string
	var gotArgs []string
	var gotCmd *exec.Cmd
	localBuildkitCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		gotName = name
		gotArgs = append([]string(nil), args...)
		gotCmd = exec.CommandContext(ctx, os.Args[0], "-test.run=TestBuildkitStoreHelperProcess")
		gotCmd.Env = append(os.Environ(), "GO_WANT_BUILDKIT_STORE_HELPER=1")
		return gotCmd
	}

	var progress bytes.Buffer
	var setup bytes.Buffer
	err := buildDockerProjectWithBuildkit(context.Background(), dir, "test-app:latest", "linux/arm64", "Containerfile",
		map[string]string{"TOKEN": "sensitive"}, &progress, &setup)
	if err != nil {
		t.Fatalf("buildDockerProjectWithBuildkit: %v", err)
	}
	if gotName != "buildctl" {
		t.Fatalf("command = %q, want buildctl", gotName)
	}
	if gotCmd == nil {
		t.Fatal("buildctl command was not created")
	}
	if gotCmd.Dir != dir {
		t.Fatalf("command dir = %q, want %q", gotCmd.Dir, dir)
	}
	joined := strings.Join(gotArgs, " ")
	for _, want := range []string{
		"--addr unix:///tmp/wendy-buildkitd.sock",
		"--frontend dockerfile.v0",
		"filename=Containerfile",
		"platform=linux/arm64",
		"type=image,name=test-app:latest,store=true,unpack=true,oci-mediatypes=true",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args %q missing %q", joined, want)
		}
	}
	if strings.Contains(setup.String(), "sensitive") {
		t.Fatalf("setup log leaked build-arg value: %q", setup.String())
	}
	if !strings.Contains(setup.String(), "build-arg:TOKEN=<redacted>") {
		t.Fatalf("setup log did not retain redacted key: %q", setup.String())
	}
}

func TestBuildDockerProjectWithBuildkitMarksSolveFailure(t *testing.T) {
	original := localBuildkitCommandContext
	t.Cleanup(func() { localBuildkitCommandContext = original })
	localBuildkitCommandContext = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestBuildkitStoreHelperProcess")
		cmd.Env = append(os.Environ(), "GO_WANT_BUILDKIT_STORE_HELPER=1", "BUILDKIT_STORE_HELPER_FAIL=1")
		return cmd
	}

	dir := t.TempDir()
	t.Setenv(wendyBuildkitHostEnv, "unix:///tmp/wendy-buildkitd.sock")
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := buildDockerProjectWithBuildkit(context.Background(), dir, "test:latest", "linux/arm64", "Dockerfile", nil, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !isImageBuildFailure(err) {
		t.Fatalf("error = %v, want imageBuildFailedError", err)
	}
	if !strings.Contains(err.Error(), "buildctl build (containerd image store) failed") {
		t.Fatalf("error = %v, want buildctl context", err)
	}
}

func TestBuildServiceImageLocallyUsesBuildkitContainerdStore(t *testing.T) {
	original := localBuildkitCommandContext
	t.Cleanup(func() { localBuildkitCommandContext = original })

	dir := t.TempDir()
	t.Setenv(wendyBuildkitHostEnv, "unix:///tmp/wendy-buildkitd.sock")
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var gotArgs []string
	localBuildkitCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if name != "buildctl" {
			t.Fatalf("command = %q, want buildctl", name)
		}
		gotArgs = append([]string(nil), args...)
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestBuildkitStoreHelperProcess")
		cmd.Env = append(os.Environ(), "GO_WANT_BUILDKIT_STORE_HELPER=1")
		return cmd
	}

	if err := buildServiceImageLocally(context.Background(), imageBuilderBuildkit, dir, "foo-api:latest", "linux/arm64", "Dockerfile", &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("buildServiceImageLocally: %v", err)
	}
	if !slices.Contains(gotArgs, "type=image,name=foo-api:latest,store=true,unpack=true,oci-mediatypes=true") {
		t.Fatalf("args = %v, want direct image-store output", gotArgs)
	}
}

func TestBuildkitStoreHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_BUILDKIT_STORE_HELPER") != "1" {
		return
	}
	if os.Getenv("BUILDKIT_STORE_HELPER_FAIL") == "1" {
		os.Exit(1)
	}
	os.Exit(0)
}

func TestRedactBuildctlArgsForLog(t *testing.T) {
	in := []string{"--opt", "build-arg:TOKEN=secret", "--output", "type=oci,dest=/x"}
	out := redactBuildctlArgsForLog(in)
	for _, a := range out {
		if a == "build-arg:TOKEN=secret" {
			t.Fatal("build-arg value was not redacted")
		}
	}
	if !slices.Contains(out, "build-arg:TOKEN=<redacted>") {
		t.Fatalf("expected redacted build-arg, got %v", out)
	}
	// Non-build-arg tokens must be preserved unchanged.
	if !slices.Contains(out, "--output") {
		t.Fatalf("--output token missing after redaction, got %v", out)
	}
	if !slices.Contains(out, "type=oci,dest=/x") {
		t.Fatalf("output value token missing after redaction, got %v", out)
	}
}

func TestBuildkitRejectsFlagInjectionBuildArg(t *testing.T) {
	if _, err := sortedValidatedBuildArgKeys(map[string]string{"FOO": "-rm-rf"}); err == nil {
		t.Fatal("expected a build-arg value starting with '-' to be rejected")
	}
}

func TestShouldUseBuildkitOnDevice(t *testing.T) {
	origLook := imageBuilderLookPath
	t.Cleanup(func() { imageBuilderLookPath = origLook })

	// On-device (socket set) + docker absent → use buildkit.
	t.Setenv("WENDY_AGENT_SOCKET", "/run/wendy/agent/agent.sock")
	imageBuilderLookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	if !shouldUseBuildkitOnDevice() {
		t.Error("on-device with no docker should select buildkit")
	}

	// docker present → do not auto-select (let docker handle it).
	imageBuilderLookPath = func(string) (string, error) { return "/usr/bin/docker", nil }
	if shouldUseBuildkitOnDevice() {
		t.Error("docker present must not auto-select buildkit")
	}

	// Off-device (no socket) → never auto-select, regardless of docker.
	t.Setenv("WENDY_AGENT_SOCKET", "")
	imageBuilderLookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	if shouldUseBuildkitOnDevice() {
		t.Error("off-device must not auto-select buildkit")
	}
}
