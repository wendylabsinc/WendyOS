package espidftoolchain

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// ProjectToolchain selects an opt-in, digest-pinned ESP-IDF container. Projects
// without wendy-idf.json retain the standard EIM build path.
type ProjectToolchain struct {
	Image          string `json:"image"`
	WorkspaceRoot  string `json:"workspaceRoot"`
	Target         string `json:"target"`
	Preview        bool   `json:"preview"`
	root, relative string
	outputOwner    string
}

func LoadProjectToolchain(projectPath, target string) (*ProjectToolchain, error) {
	data, err := os.ReadFile(filepath.Join(projectPath, "wendy-idf.json"))
	if os.IsNotExist(err) {
		return &ProjectToolchain{}, nil
	}
	if err != nil {
		return nil, err
	}
	var p ProjectToolchain
	d := json.NewDecoder(strings.NewReader(string(data)))
	d.DisallowUnknownFields()
	if err := d.Decode(&p); err != nil {
		return nil, fmt.Errorf("wendy-idf.json: %w", err)
	}
	if !regexp.MustCompile(`^espressif/idf@sha256:[0-9a-f]{64}$`).MatchString(p.Image) {
		return nil, fmt.Errorf("wendy-idf.json image must pin espressif/idf by SHA-256 digest")
	}
	if p.Target == "" || p.Target != target {
		return nil, fmt.Errorf("project targets %q, device targets %q", p.Target, target)
	}
	if filepath.IsAbs(p.WorkspaceRoot) {
		return nil, fmt.Errorf("workspaceRoot must be relative to the project")
	}
	project, err := filepath.Abs(projectPath)
	if err != nil {
		return nil, err
	}
	project, err = filepath.EvalSymlinks(project)
	if err != nil {
		return nil, err
	}
	p.root, err = filepath.EvalSymlinks(filepath.Join(project, p.WorkspaceRoot))
	if err != nil {
		return nil, err
	}
	p.relative, err = filepath.Rel(p.root, project)
	if err != nil || p.relative == ".." || strings.HasPrefix(p.relative, ".."+string(filepath.Separator)) || p.root == filepath.VolumeName(p.root)+string(filepath.Separator) {
		return nil, fmt.Errorf("workspaceRoot must be the project directory or a containing workspace, not the filesystem root")
	}
	return &p, nil
}

func (p *ProjectToolchain) Ensure(ctx context.Context) error {
	if p.Image == "" {
		return EnsureVersion(ctx)
	}
	p.outputOwner = ""
	cmd := execCommandContext(ctx, "docker", "info", "--format", "{{json .}}")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ESP-IDF container requires Docker: %w: %s", err, strings.TrimSpace(string(output)))
	}
	var info dockerEngineInfo
	if err := json.Unmarshal(output, &info); err != nil {
		return fmt.Errorf("reading Docker engine info: %w", err)
	}
	if info.needsOwnershipRestore(runtime.GOOS, os.Getuid()) {
		p.outputOwner = fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	}
	return nil
}

type dockerEngineInfo struct {
	OperatingSystem string
	SecurityOptions []string
}

func (info dockerEngineInfo) needsOwnershipRestore(goos string, uid int) bool {
	if goos != "linux" || uid <= 0 || strings.Contains(strings.ToLower(info.OperatingSystem), "docker desktop") {
		return false
	}
	for _, option := range info.SecurityOptions {
		name := strings.SplitN(option, ",", 2)[0]
		if name == "name=rootless" || name == "name=userns" {
			return false
		}
	}
	return true
}

// Run executes idf.py and restores generated file ownership even after a failed
// or cancelled build. Mosaico patches /opt/esp/idf, so it must build as root.
// A nil or zero toolchain uses EIM.
func (p *ProjectToolchain) Run(ctx context.Context, projectPath string, args ...string) error {
	cmd := p.command(ctx, args...)
	cmd.Dir = projectPath
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if p == nil || p.outputOwner == "" {
		return err
	}
	return errors.Join(err, p.restoreOutputOwnership(ctx))
}

func (p *ProjectToolchain) restoreOutputOwnership(ctx context.Context) error {
	// Restrict cleanup to IDF outputs, including the manifest add-dependency
	// writes. Do not recursively chown the shared workspace or follow symlinks.
	var paths []string
	for _, path := range []string{"build", "managed_components", "sdkconfig", "sdkconfig.old", "dependencies.lock", "main/idf_component.yml"} {
		if _, err := os.Lstat(filepath.Join(p.root, p.relative, path)); err == nil {
			paths = append(paths, "./"+path)
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("checking ESP-IDF output ownership: %w", err)
		}
	}
	if len(paths) == 0 {
		return nil
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	args := []string{"-v", filepath.Join(p.root, p.relative) + ":/project", "-w", "/project", "--entrypoint", "find", p.Image, "-P"}
	args = append(args, paths...)
	args = append(args, "-xdev", "-uid", "0", "-exec", "chown", "-h", "--", p.outputOwner, "{}", "+")
	if output, err := dockerRunCommand(cleanupCtx, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("restoring ESP-IDF output ownership: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (p *ProjectToolchain) command(ctx context.Context, args ...string) *exec.Cmd {
	if p == nil || p.Image == "" {
		return IdfCommandContext(ctx, args...)
	}
	command := []string{"-v", p.root + ":/project", "-w", filepath.ToSlash(filepath.Join("/project", p.relative)), p.Image, "idf.py"}
	if p.Preview {
		command = append(command, "--preview")
	}
	return dockerRunCommand(ctx, append(command, args...)...)
}

func dockerRunCommand(ctx context.Context, args ...string) *exec.Cmd {
	name := "wendy-idf-" + rand.Text()
	command := append([]string{"run", "--rm", "--name", name}, args...)
	cmd := execCommandContext(ctx, "docker", command...)
	// Killing the docker client alone would leave its container building in
	// the workspace, where a retried build would race it.
	cmd.Cancel = func() error {
		removeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = execCommandContext(removeCtx, "docker", "rm", "--force", name).Run()
		return cmd.Process.Kill()
	}
	return cmd
}
