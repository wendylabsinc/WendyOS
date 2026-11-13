package espidftoolchain

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestProjectToolchain(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "sample with spaces")
	if err := os.Mkdir(project, 0755); err != nil {
		t.Fatal(err)
	}
	image := "espressif/idf@sha256:" + strings.Repeat("a", 64)
	write := func(p ProjectToolchain) {
		data, _ := json.Marshal(p)
		if err := os.WriteFile(filepath.Join(project, "wendy-idf.json"), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	p, err := LoadProjectToolchain(project, "esp32s31")
	if err != nil || p.Image != "" {
		t.Fatalf("default: %+v, %v", p, err)
	}
	calls := stubExecCommandContext(t, func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "true")
	})
	p.command(context.Background(), "build")
	if !reflect.DeepEqual((*calls)[0], []string{"eim", "run", "idf.py build", DefaultVersion}) {
		t.Fatal(*calls)
	}
	write(ProjectToolchain{Image: image, WorkspaceRoot: "..", Target: "esp32s31", Preview: true})
	p, err = LoadProjectToolchain(project, "esp32s31")
	if err != nil {
		t.Fatal(err)
	}
	p.command(context.Background(), "set-target", "esp32s31")
	got := (*calls)[1]
	if len(got) < 5 || got[3] != "--name" || !strings.HasPrefix(got[4], "wendy-idf-") {
		t.Fatalf("container is not named for cancellation: %v", got)
	}
	want := []string{"docker", "run", "--rm", "--name", got[4], "-v", p.root + ":/project", "-w", "/project/sample with spaces", image, "idf.py", "--preview", "set-target", "esp32s31"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if _, err := LoadProjectToolchain(project, "esp32s3"); err == nil {
		t.Fatal("accepted mismatched target")
	}
	for _, bad := range []ProjectToolchain{
		{Image: "espressif/idf:latest", Target: "esp32s31"},
		{Image: image, Target: "esp32s31", WorkspaceRoot: root},
		{Image: image, Target: "esp32s31", WorkspaceRoot: "../elsewhere"},
	} {
		write(bad)
		if _, err := LoadProjectToolchain(project, "esp32s31"); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
}

func TestProjectToolchainCancelRemovesContainer(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sleep")
	}
	calls := stubExecCommandContext(t, func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if args[0] == "run" {
			return exec.CommandContext(ctx, "sleep", "30")
		}
		return exec.CommandContext(ctx, "true")
	})
	p := &ProjectToolchain{Image: "espressif/idf@sha256:" + strings.Repeat("a", 64), root: t.TempDir(), relative: "."}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := p.command(ctx, "build")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := cmd.Wait(); err == nil {
		t.Fatal("cancelled build succeeded")
	}
	name := (*calls)[0][4]
	if len(*calls) != 2 || !reflect.DeepEqual((*calls)[1], []string{"docker", "rm", "--force", name}) {
		t.Fatalf("cancellation left the container running: %v", *calls)
	}
}

func TestDockerOwnershipRestoreSelection(t *testing.T) {
	for _, tc := range []struct {
		name, goos, operatingSystem string
		uid                         int
		security                    []string
		want                        bool
	}{
		{"rootful Linux", "linux", "Ubuntu", 1000, nil, true},
		{"rootless", "linux", "Ubuntu", 1000, []string{"name=seccomp,profile=builtin", "name=rootless"}, false},
		{"user namespaces", "linux", "Ubuntu", 1000, []string{"name=userns"}, false},
		{"Desktop Linux", "linux", "Docker Desktop", 1000, nil, false},
		{"macOS", "darwin", "OrbStack", 501, nil, false},
		{"Windows", "windows", "Docker Desktop", -1, nil, false},
		{"root user", "linux", "Ubuntu", 0, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := dockerEngineInfo{OperatingSystem: tc.operatingSystem, SecurityOptions: tc.security}
			if got := info.needsOwnershipRestore(tc.goos, tc.uid); got != tc.want {
				t.Fatalf("restore = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestProjectToolchainRestoresOwnership(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	for _, outcome := range []string{"success", "failure", "cancelled", "cleanup failure"} {
		t.Run(outcome, func(t *testing.T) {
			root := t.TempDir()
			project := filepath.Join(root, "sample with spaces")
			if err := os.Mkdir(project, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(project, "sdkconfig"), nil, 0644); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := stubExecCommandContext(t, func(ctx context.Context, name string, args ...string) *exec.Cmd {
				if slices.Contains(args, "--entrypoint") {
					if ctx.Err() != nil {
						t.Fatal("cleanup inherited cancellation")
					}
					if _, ok := ctx.Deadline(); !ok {
						t.Fatal("cleanup has no timeout")
					}
					if outcome == "cleanup failure" {
						return exec.CommandContext(ctx, "sh", "-c", "exit 9")
					}
					return exec.CommandContext(ctx, "true")
				}
				if args[0] == "rm" {
					return exec.CommandContext(ctx, "true")
				}
				if outcome == "cancelled" {
					cancel()
				}
				if outcome == "failure" {
					return exec.CommandContext(ctx, "sh", "-c", "exit 7")
				}
				return exec.CommandContext(ctx, "true")
			})
			p := &ProjectToolchain{Image: "test-image", root: root, relative: "sample with spaces", outputOwner: "1000:1001"}
			err := p.Run(ctx, project, "build")
			if (err == nil) != (outcome == "success") {
				t.Fatalf("Run = %v", err)
			}
			if outcome == "failure" {
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != 7 {
					t.Fatalf("lost build exit status: %v", err)
				}
			}
			if outcome == "cleanup failure" && !strings.Contains(err.Error(), "restoring ESP-IDF output ownership") {
				t.Fatal(err)
			}
			got := (*calls)[len(*calls)-1]
			want := []string{"-v", project + ":/project", "-w", "/project", "--entrypoint", "find", p.Image, "-P", "./sdkconfig", "-xdev", "-uid", "0", "-exec", "chown", "-h", "--", "1000:1001", "{}", "+"}
			if !reflect.DeepEqual(got[5:], want) {
				t.Fatalf("cleanup = %v, want %v", got[5:], want)
			}
		})
	}
}

// Run in a Linux container as root to check real filesystem ownership, including
// preservation of other owners and symlinks pointing outside the output tree.
func TestProjectToolchainOwnershipFilesystem(t *testing.T) {
	if runtime.GOOS != "linux" || os.Getuid() != 0 {
		t.Skip("requires Linux root")
	}
	project := t.TempDir()
	outside := filepath.Join(t.TempDir(), "untouched")
	if err := os.WriteFile(outside, nil, 0644); err != nil {
		t.Fatal(err)
	}
	stubExecCommandContext(t, func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if i := slices.Index(args, "-P"); i >= 0 {
			cmd := exec.CommandContext(ctx, "find", args[i:]...)
			cmd.Dir = project
			return cmd
		}
		return exec.CommandContext(ctx, "sh", "-c", `mkdir -p build managed_components main
 touch build/output sdkconfig sdkconfig.old dependencies.lock main/idf_component.yml managed_components/component build/other source.c
 chown 2000:2001 build/other
 ln -s "$1" build/link
 exit 7`, "build", outside)
	})
	p := &ProjectToolchain{Image: "test-image", root: project, relative: ".", outputOwner: "1000:1001"}
	if err := p.Run(context.Background(), project, "build"); err == nil {
		t.Fatal("expected build failure")
	}
	for path, owner := range map[string]string{
		"build": "1000:1001", "build/output": "1000:1001", "build/link": "1000:1001",
		"managed_components/component": "1000:1001", "sdkconfig": "1000:1001",
		"sdkconfig.old": "1000:1001", "dependencies.lock": "1000:1001", "main/idf_component.yml": "1000:1001",
		"build/other": "2000:2001", "source.c": "0:0", outside: "0:0",
	} {
		if !filepath.IsAbs(path) {
			path = filepath.Join(project, path)
		}
		got, err := exec.Command("stat", "-c", "%u:%g", path).Output()
		if err != nil || strings.TrimSpace(string(got)) != owner {
			t.Errorf("%s owner = %q, %v; want %s", path, got, err, owner)
		}
	}
}
