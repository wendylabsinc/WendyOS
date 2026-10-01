package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

var (
	appleContainerCommandContext = exec.CommandContext
	appleContainerLookPath       = exec.LookPath
	appleContainerHostGOOS       = func() string { return runtime.GOOS }
	appleContainerHostGOARCH     = func() string { return runtime.GOARCH }
)

var (
	appleContainerContainerNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
	appleContainerLabelKeyRe      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*(/[A-Za-z0-9][A-Za-z0-9_.-]*)?$`)

	// Label values intentionally permit only the characters emitted by Wendy's
	// entitlement annotations: alphanumerics plus _ . / : = , and -. Shell
	// metacharacters, whitespace, @ digests, quotes, and control bytes are rejected.
	appleContainerLabelValueRe = regexp.MustCompile(`^[A-Za-z0-9_./:=,-]*$`)
)

const (
	appleContainerMaxJSONDepth       = 32
	appleContainerMaxJSONOutputBytes = 1 << 20
)

type appleContainerBuildContext struct {
	ImageName     string
	ContainerName string
	cmd           *exec.Cmd
}

// AppleContainerProvider builds and runs Dockerfile/Containerfile projects
// with Apple's container CLI on Apple silicon Macs.
type AppleContainerProvider struct{}

func (p *AppleContainerProvider) Key() string         { return ProviderKeyAppleContainer }
func (p *AppleContainerProvider) DisplayName() string { return "Apple Container" }

func (p *AppleContainerProvider) IsAvailable(ctx context.Context) bool {
	if !appleContainerSupportedHost(appleContainerHostGOOS(), appleContainerHostGOARCH()) {
		return false
	}
	if _, err := appleContainerLookPath("container"); err != nil {
		return false
	}
	return appleContainerCommandContext(ctx, "container", "--version").Run() == nil
}

func appleContainerSupportedHost(goos, goarch string) bool {
	return goos == "darwin" && goarch == "arm64"
}

func (p *AppleContainerProvider) CheckRequirements(ctx context.Context) error {
	if !appleContainerSupportedHost(appleContainerHostGOOS(), appleContainerHostGOARCH()) {
		return fmt.Errorf("Apple Container requires an Apple silicon Mac")
	}
	if err := ensureAppleContainerInstalled(ctx); err != nil {
		return err
	}
	if err := appleContainerCommandContext(ctx, "container", "--version").Run(); err != nil {
		return fmt.Errorf("container CLI is not usable: %w", err)
	}
	for _, service := range appleContainerServices {
		if err := ensureAppleContainerServiceRunning(ctx, service); err != nil {
			return err
		}
	}
	return nil
}

func (p *AppleContainerProvider) DiscoverDevices(ctx context.Context) ([]models.ExternalDevice, error) {
	// Same bound as DockerProvider.DiscoverDevices: a wedged `container`
	// apiserver must not stall discovery.
	timeout := ProbeTimeout(ctx)
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if !p.IsAvailable(probeCtx) {
		if probeTimedOut(ctx, probeCtx) {
			return nil, &ProbeTimeoutError{Runtime: p.DisplayName(), After: timeout}
		}
		return nil, nil
	}
	version := appleContainerVersion(probeCtx)
	return []models.ExternalDevice{
		{
			ID:              p.Key(),
			DisplayName:     p.DisplayName(),
			ProviderKey:     p.Key(),
			IsWendyDevice:   false,
			AgentVersion:    version,
			OS:              "linux",
			CPUArchitecture: "arm64",
		},
	}, nil
}

func appleContainerVersion(ctx context.Context) string {
	out, err := appleContainerCommandContext(ctx, "container", "--version").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func (p *AppleContainerProvider) SupportedBuildTypes() []string {
	return []string{"docker"}
}

func (p *AppleContainerProvider) CanBuild(projectPath string) bool {
	return hasContainerBuildFile(projectPath)
}

func (p *AppleContainerProvider) Build(ctx context.Context, device models.ExternalDevice, projectPath, projectType, product string, debug bool) (*BuiltApp, error) {
	return p.BuildWithDockerfile(ctx, device, projectPath, product, "", "", debug)
}

func (p *AppleContainerProvider) BuildWithType(ctx context.Context, device models.ExternalDevice, projectPath, product, buildType string, debug bool) (*BuiltApp, error) {
	return p.BuildWithDockerfile(ctx, device, projectPath, product, buildType, "", debug)
}

func (p *AppleContainerProvider) BuildWithDockerfile(ctx context.Context, device models.ExternalDevice, projectPath, product, buildType, dockerfile string, debug bool) (*BuiltApp, error) {
	switch strings.TrimSpace(strings.ToLower(buildType)) {
	case "", "docker":
	default:
		return nil, fmt.Errorf("Apple Container supports Dockerfile/Containerfile builds only, not %s", buildType)
	}
	if err := p.CheckRequirements(ctx); err != nil {
		return nil, err
	}

	buildContext, err := appleContainerBuildContextPath(projectPath)
	if err != nil {
		return nil, fmt.Errorf("resolving project path: %w", err)
	}

	imageName := strings.ToLower(product) + ":latest"
	platform := "linux/arm64"
	if device.CPUArchitecture != "" {
		platform = "linux/" + device.CPUArchitecture
	}
	args := []string{"build", "--platform", platform, "-t", imageName}
	if dockerfile == "" {
		dockerfile = defaultContainerBuildFile(projectPath)
	}
	if dockerfile != "" {
		resolved, err := confinedProviderDockerfilePath(projectPath, dockerfile)
		if err != nil {
			return nil, err
		}
		args = append(args, "-f", resolved)
	}
	args = append(args, buildContext)

	cmd := appleContainerCommandContext(ctx, "container", args...)
	cmd.Dir = buildContext
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("container build: %w", err)
	}
	return &BuiltApp{
		ProviderKey: p.Key(),
		Device:      device,
		AppName:     product,
		Context: &appleContainerBuildContext{
			ImageName:     imageName,
			ContainerName: product,
		},
	}, nil
}

func appleContainerBuildContextPath(projectPath string) (string, error) {
	buildContext, err := filepath.Abs(projectPath)
	if err != nil {
		return "", err
	}
	// Apple Container 1.0.0 mishandles symlink-resolved /private/tmp build
	// contexts. When macOS reports /private/tmp, pass the equivalent /tmp path.
	if appleContainerHostGOOS() == "darwin" {
		if normalized, ok := appleContainerTmpAlias(buildContext); ok {
			return normalized, nil
		}
	}
	return buildContext, nil
}

func appleContainerTmpAlias(path string) (string, bool) {
	const privateTmp = "/private/tmp"
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", false
	}
	if canonical != privateTmp && !strings.HasPrefix(canonical, privateTmp+"/") {
		return "", false
	}
	candidate := "/tmp" + strings.TrimPrefix(canonical, privateTmp)
	candidateCanonical, err := filepath.EvalSymlinks(candidate)
	if err != nil || candidateCanonical != canonical {
		return "", false
	}
	return candidate, true
}

func validateAppleContainerKeyValueArg(kind, key, value string) error {
	if key == "" {
		return fmt.Errorf("invalid %s: key is empty", kind)
	}
	if !appleContainerLabelKeyRe.MatchString(key) {
		return fmt.Errorf("invalid %s key %q: must contain only ASCII label characters and at most one '/' prefix separator", kind, key)
	}
	if !appleContainerLabelValueRe.MatchString(value) {
		return fmt.Errorf("invalid %s %q: value must contain only safe ASCII label value characters", kind, key)
	}
	return nil
}

func validateAppleContainerContainerName(name string) error {
	if !appleContainerContainerNameRe.MatchString(name) {
		return fmt.Errorf("invalid Apple Container container name %q: must match [A-Za-z0-9][A-Za-z0-9_.-]*", name)
	}
	return nil
}

func appleContainerKeyValueArg(kind, key, value string) (string, error) {
	if err := validateAppleContainerKeyValueArg(kind, key, value); err != nil {
		return "", err
	}
	return key + "=" + value, nil
}

func confinedProviderDockerfilePath(projectPath, dockerfile string) (string, error) {
	if err := validateContainerBuildFileName(dockerfile); err != nil {
		return "", err
	}
	absProject, err := filepath.EvalSymlinks(projectPath)
	if err != nil {
		return "", fmt.Errorf("resolving project path: %w", err)
	}
	escapes := func(rel string) bool {
		return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(absProject, dockerfile))
	if err != nil {
		return "", fmt.Errorf("dockerfile %q: %w", dockerfile, err)
	}
	absProject, err = filepath.EvalSymlinks(projectPath)
	if err != nil {
		return "", fmt.Errorf("resolving project path: %w", err)
	}
	rel, err := filepath.Rel(absProject, resolved)
	if err != nil || escapes(rel) {
		return "", fmt.Errorf("dockerfile %q must be within the project directory", dockerfile)
	}
	if appleContainerHostGOOS() == "darwin" {
		if normalized, ok := appleContainerTmpAlias(resolved); ok {
			return normalized, nil
		}
	}
	return resolved, nil
}

func (p *AppleContainerProvider) Run(ctx context.Context, app *BuiltApp, detach bool, output chan<- RunOutput) error {
	defer close(output)

	bc, ok := app.Context.(*appleContainerBuildContext)
	if !ok {
		return fmt.Errorf("Apple Container provider: invalid build context")
	}
	if err := validateAppleContainerContainerName(bc.ContainerName); err != nil {
		return err
	}
	if err := p.CheckRequirements(ctx); err != nil {
		return err
	}
	if err := p.removeManagedContainer(ctx, bc.ContainerName); err != nil {
		return err
	}

	managedLabel, err := appleContainerKeyValueArg("label", "wendy.managed", "true")
	if err != nil {
		return err
	}
	args := []string{"run", "--name", bc.ContainerName, "--label", managedLabel}
	for k, v := range appconfig.BuildEntitlementAnnotations(app.Entitlements) {
		label, err := appleContainerKeyValueArg("label", k, v)
		if err != nil {
			return err
		}
		args = append(args, "--label", label)
	}
	if detach {
		args = append(args, "--detach")
	}
	args = append(args, bc.ImageName)

	cmd := appleContainerCommandContext(ctx, "container", args...)
	bc.cmd = cmd

	if detach {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("container run: %w", err)
		}
		output <- RunOutput{Type: RunOutputStarted}
		return nil
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("container run: %w", err)
	}

	output <- RunOutput{Type: RunOutputStarted}

	done := make(chan struct{})
	go scanLines(stdoutPipe, output, RunOutputStdout, done)
	go scanLines(stderrPipe, output, RunOutputStderr, done)

	<-done
	<-done
	return cmd.Wait()
}

func (p *AppleContainerProvider) Stop(ctx context.Context, app *BuiltApp) error {
	bc, ok := app.Context.(*appleContainerBuildContext)
	if !ok {
		return fmt.Errorf("Apple Container provider: invalid build context")
	}
	if err := validateAppleContainerContainerName(bc.ContainerName); err != nil {
		return err
	}
	cmd := appleContainerCommandContext(ctx, "container", "stop", bc.ContainerName)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("container stop: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

func (p *AppleContainerProvider) GetDeviceInfo(ctx context.Context, device models.ExternalDevice) (*ProviderDeviceInfo, error) {
	return nil, nil
}

func (p *AppleContainerProvider) removeManagedContainer(ctx context.Context, name string) error {
	if err := validateAppleContainerContainerName(name); err != nil {
		return err
	}
	managed, err := p.containerHasManagedLabel(ctx, name)
	if err != nil {
		return err
	}
	if !managed {
		return nil
	}
	cmd := appleContainerCommandContext(ctx, "container", "delete", "--force", name)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("container delete: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

func (p *AppleContainerProvider) containerHasManagedLabel(ctx context.Context, name string) (bool, error) {
	if err := validateAppleContainerContainerName(name); err != nil {
		return false, err
	}
	cmd := appleContainerCommandContext(ctx, "container", "inspect", name)
	out, truncated, err := appleContainerCombinedOutputLimited(cmd, appleContainerMaxJSONOutputBytes)
	if truncated {
		return false, fmt.Errorf("container inspect output exceeds %d bytes", appleContainerMaxJSONOutputBytes)
	}
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if appleContainerInspectNotFound(msg) {
			return false, nil
		}
		if msg != "" {
			return false, fmt.Errorf("container inspect: %s: %w", msg, err)
		}
		return false, fmt.Errorf("container inspect: %w", err)
	}
	return appleContainerInspectHasManagedLabel(out), nil
}

func appleContainerInspectNotFound(output string) bool {
	s := strings.ToLower(output)
	return strings.Contains(s, "not found") ||
		strings.Contains(s, "no such container") ||
		strings.Contains(s, "does not exist")
}

func appleContainerInspectHasManagedLabel(data []byte) bool {
	if len(data) > appleContainerMaxJSONOutputBytes {
		return false
	}
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return false
	}
	return jsonContainsManagedLabel(v)
}

func jsonContainsManagedLabel(v any) bool {
	return jsonContainsManagedLabelDepth(v, 0)
}

func jsonContainsManagedLabelDepth(v any, depth int) bool {
	if depth > appleContainerMaxJSONDepth {
		return false
	}
	switch value := v.(type) {
	case []any:
		for _, item := range value {
			if jsonContainsManagedLabelDepth(item, depth+1) {
				return true
			}
		}
	case map[string]any:
		for k, item := range value {
			if k == "wendy.managed" && fmt.Sprint(item) == "true" {
				return true
			}
			if jsonContainsManagedLabelDepth(item, depth+1) {
				return true
			}
		}
	}
	return false
}

func (p *AppleContainerProvider) ListContainers(ctx context.Context) ([]ContainerInfo, error) {
	cmd := appleContainerCommandContext(ctx, "container", "list", "--all", "--format", "json")
	out, truncated, err := appleContainerOutputLimited(cmd, appleContainerMaxJSONOutputBytes)
	if truncated {
		return nil, fmt.Errorf("container list output exceeds %d bytes", appleContainerMaxJSONOutputBytes)
	}
	if err != nil {
		return nil, fmt.Errorf("container list: %w", err)
	}

	var containers []ContainerInfo
	for _, info := range appleContainerListInfos(out) {
		if info.Name == "" {
			continue
		}
		managed, err := p.containerHasManagedLabel(ctx, info.Name)
		if err != nil {
			return nil, err
		}
		if managed {
			containers = append(containers, info)
		}
	}
	return containers, nil
}

func appleContainerListInfos(data []byte) []ContainerInfo {
	if len(data) > appleContainerMaxJSONOutputBytes {
		return nil
	}
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return nil
	}
	var entries []any
	switch value := v.(type) {
	case []any:
		entries = value
	case map[string]any:
		if items, ok := value["items"].([]any); ok {
			entries = items
		}
	}

	containers := make([]ContainerInfo, 0, len(entries))
	for _, entry := range entries {
		m, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		image := firstJSONText(m, "image", "Image")
		if image == "" {
			if cfg := firstJSONMap(m, "configuration", "Configuration"); cfg != nil {
				if img := firstJSONMap(cfg, "image", "Image"); img != nil {
					image = firstJSONText(img, "reference", "Reference")
				}
			}
		}
		state := firstJSONText(m, "state", "State")
		status := firstJSONText(m, "status", "Status")
		if statusMap := firstJSONMap(m, "status", "Status"); statusMap != nil {
			if nestedState := firstJSONText(statusMap, "state", "State"); nestedState != "" {
				if state == "" {
					state = nestedState
				}
				if status == "" {
					status = nestedState
				}
			}
		}
		containers = append(containers, ContainerInfo{
			Name:   firstJSONText(m, "id", "ID", "name", "Name"),
			Image:  image,
			State:  state,
			Status: status,
		})
	}
	return containers
}

type limitedOutputBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *limitedOutputBuffer) Write(p []byte) (int, error) {
	remaining := b.limit - b.buf.Len()
	if remaining > 0 {
		if len(p) <= remaining {
			_, _ = b.buf.Write(p)
		} else {
			_, _ = b.buf.Write(p[:remaining])
			b.truncated = true
		}
	} else if len(p) > 0 {
		b.truncated = true
	}
	return len(p), nil
}

func (b *limitedOutputBuffer) Bytes() []byte {
	return b.buf.Bytes()
}

func appleContainerOutputLimited(cmd *exec.Cmd, limit int) ([]byte, bool, error) {
	var stdout limitedOutputBuffer
	stdout.limit = limit
	cmd.Stdout = &stdout
	err := cmd.Run()
	return stdout.Bytes(), stdout.truncated, err
}

func appleContainerCombinedOutputLimited(cmd *exec.Cmd, limit int) ([]byte, bool, error) {
	var combined limitedOutputBuffer
	combined.limit = limit
	cmd.Stdout = &combined
	cmd.Stderr = &combined
	err := cmd.Run()
	return combined.Bytes(), combined.truncated, err
}

func firstJSONText(m map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := m[key]; ok {
			switch v := value.(type) {
			case string:
				return v
			case json.Number:
				return v.String()
			case float64, bool:
				return fmt.Sprint(v)
			}
		}
	}
	return ""
}

func firstJSONMap(m map[string]any, keys ...string) map[string]any {
	for _, key := range keys {
		if value, ok := m[key].(map[string]any); ok {
			return value
		}
	}
	return nil
}

func (p *AppleContainerProvider) StartContainer(ctx context.Context, name string) error {
	if err := validateAppleContainerContainerName(name); err != nil {
		return err
	}
	cmd := appleContainerCommandContext(ctx, "container", "start", name)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("container start: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

func (p *AppleContainerProvider) StopContainer(ctx context.Context, name string) error {
	if err := validateAppleContainerContainerName(name); err != nil {
		return err
	}
	cmd := appleContainerCommandContext(ctx, "container", "stop", name)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("container stop: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

func (p *AppleContainerProvider) RemoveContainer(ctx context.Context, name string) error {
	if err := validateAppleContainerContainerName(name); err != nil {
		return err
	}
	cmd := appleContainerCommandContext(ctx, "container", "delete", "--force", name)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("container delete: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}
