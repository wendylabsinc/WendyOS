package commands

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
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
	err := checkRunBuildPrerequisites(context.Background(), "docker", nil, deps)
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
	if err := checkRunBuildPrerequisites(context.Background(), "buildkit", nil, deps); err != nil {
		t.Fatal(err)
	}
	deps.lookPath = func(string) (string, error) { return "", os.ErrNotExist }
	if err := checkRunBuildPrerequisites(context.Background(), "docker", nil, deps); err != nil {
		t.Fatal("must retain existing interactive Docker setup", err)
	}
}
