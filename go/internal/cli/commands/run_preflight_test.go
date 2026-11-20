package commands

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestRunPreflightReportsIndependentBlockers(t *testing.T) {
	old := localBuildDiskAvail
	localBuildDiskAvail = func(string) (int64, bool) { return 1, true }
	t.Cleanup(func() { localBuildDiskAvail = old })
	deps := doctorTestDeps(t)
	deps.run = func(_ context.Context, _ string, args ...string) error {
		if strings.Join(args, " ") != "buildx version" {
			t.Fatalf("unexpected probe %v", args)
		}
		return errors.New("plugin unavailable")
	}
	err := checkRunBuildPrerequisites(context.Background(), "docker", true, nil, deps)
	if err == nil || !errors.Is(err, errDiskSpaceLow) || !errors.Is(err, errBuilderUnavailable) {
		t.Fatalf("lost independent blockers: %v", err)
	}
	if !strings.Contains(err.Error(), "Buildx") || !strings.Contains(err.Error(), "disk space") {
		t.Fatal(err)
	}
}

func TestRunPreflightLeavesOtherBuildersAndDockerBootstrapAlone(t *testing.T) {
	old := localBuildDiskAvail
	localBuildDiskAvail = func(string) (int64, bool) { return 100 << 30, true }
	t.Cleanup(func() { localBuildDiskAvail = old })
	deps := doctorTestDeps(t)
	deps.run = func(context.Context, string, ...string) error { t.Fatal("unexpected Docker probe"); return nil }
	if err := checkRunBuildPrerequisites(context.Background(), "buildkit", true, nil, deps); err != nil {
		t.Fatal(err)
	}
	deps.lookPath = func(string) (string, error) { return "", os.ErrNotExist }
	if err := checkRunBuildPrerequisites(context.Background(), "docker", true, nil, deps); err != nil {
		t.Fatal("must retain existing interactive Docker setup", err)
	}
}

func withAmpleBuildDisk(t *testing.T) {
	t.Helper()
	old := localBuildDiskAvail
	localBuildDiskAvail = func(string) (int64, bool) { return 100 << 30, true }
	t.Cleanup(func() { localBuildDiskAvail = old })
}

// A probe that ran out of time says nothing about Buildx. Refusing the build
// as "Buildx is not installed" would block a working setup on a slow start.
func TestRunPreflightIgnoresABuildxProbeThatTimesOut(t *testing.T) {
	withAmpleBuildDisk(t)
	old := buildxProbeTimeout
	buildxProbeTimeout = 20 * time.Millisecond
	t.Cleanup(func() { buildxProbeTimeout = old })
	deps := doctorTestDeps(t)
	deps.run = func(ctx context.Context, _ string, _ ...string) error {
		<-ctx.Done()
		return ctx.Err()
	}
	if err := checkRunBuildPrerequisites(context.Background(), "docker", true, nil, deps); err != nil {
		t.Fatalf("a timed-out probe refused the build: %v", err)
	}
}

// The Buildx refusal carries the same install steps as the one a failed
// build reports (buildxMissingError).
func TestRunPreflightGivesTheBuildxInstallSteps(t *testing.T) {
	withAmpleBuildDisk(t)
	deps := doctorTestDeps(t)
	deps.run = func(context.Context, string, ...string) error { return errors.New("exit status 1") }
	err := checkRunBuildPrerequisites(context.Background(), "docker", true, nil, deps)
	if !errors.Is(err, errBuildxMissing) || ErrorClass(err) != "builder_unavailable" {
		t.Fatalf("err = %v (class %q)", err, ErrorClass(err))
	}
	if steps := strings.Join(NextSteps(err), "\n"); !strings.Contains(steps, "https://docs.docker.com/go/buildx/") {
		t.Fatalf("next steps = %q, want the Buildx install link", steps)
	}
}

// A full disk keeps its disk_full class when Buildx is missing too: that
// class is how the MCP run tool knows the run was refused before deploying.
func TestRunPreflightKeepsTheDiskFullClassAlongsideBuildx(t *testing.T) {
	fakeAllBuildDisks(t, 1)
	deps := doctorTestDeps(t)
	deps.run = func(context.Context, string, ...string) error { return errors.New("exit status 1") }
	err := checkRunBuildPrerequisites(context.Background(), "docker", true, nil, deps)
	if !errors.Is(err, errBuildxMissing) || ErrorClass(err) != "disk_full" {
		t.Fatalf("err = %v (class %q), want disk_full with the Buildx cause kept", err, ErrorClass(err))
	}
}

// Toolchain builds (Swift, Xcode) use no image builder, but a full disk here
// and on the device are still independent blockers.
func TestToolchainPreflightReportsBothFullDisksTogether(t *testing.T) {
	fakeAllBuildDisks(t, 1<<20)
	err := checkToolchainBuildPrerequisites(context.Background(), t.TempDir(), deviceWithFree(100<<20))
	if ErrorClass(err) != "disk_full" || !strings.Contains(err.Error(), "to build") || !strings.Contains(err.Error(), "on the device") {
		t.Fatalf("err = %v (class %q), want both disks reported", err, ErrorClass(err))
	}
	fakeAllBuildDisks(t, 100<<30)
	if err := checkToolchainBuildPrerequisites(context.Background(), t.TempDir(), deviceWithFree(100<<30)); err != nil {
		t.Fatal(err)
	}
}

func TestComposePreflightReportsBothFullDisksTogether(t *testing.T) {
	fakeAllBuildDisks(t, 1<<20)
	conn, dir, builds := startComposeBuildProject(t)
	cacheLinuxDeviceWithFree(conn, 100<<20)
	err := runComposeWithAgent(context.Background(), conn, dir, runOptions{deploy: true})
	if ErrorClass(err) != "disk_full" || !strings.Contains(err.Error(), "to build") || !strings.Contains(err.Error(), "on the device") {
		t.Fatalf("err = %v (class %q), want both disks reported", err, ErrorClass(err))
	}
	if len(*builds) != 0 {
		t.Fatalf("built %d time(s) before refusing", len(*builds))
	}
}
