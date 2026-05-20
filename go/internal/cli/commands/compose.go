package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"

	"github.com/charmbracelet/lipgloss"
	"github.com/distribution/reference"
	"gopkg.in/yaml.v3"

	"github.com/wendylabsinc/wendy/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/internal/cli/tui"
	"github.com/wendylabsinc/wendy/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

// normalizeImageRef canonicalises a Docker short reference (e.g.
// "python:3.11-slim", "nginx") to a fully-qualified form
// ("docker.io/library/python:3.11-slim") that the agent's containerd
// reference parser accepts. References that already include a registry,
// digest, or tag pass through unchanged. When parsing fails (malformed
// reference), the original input is returned and the agent surfaces the
// resulting error.
func normalizeImageRef(ref string) string {
	named, err := reference.ParseNormalizedNamed(strings.TrimSpace(ref))
	if err != nil {
		return ref
	}
	return reference.TagNameOnly(named).String()
}

// composeConfig is a minimal representation of a docker-compose file.
type composeConfig struct {
	Services map[string]composeService `yaml:"services"`
}

type composeService struct {
	Image       string    `yaml:"image"`
	Build       yaml.Node `yaml:"build"` // string or build object
	Command     yaml.Node `yaml:"command"`
	Environment yaml.Node `yaml:"environment"` // map or list
	Ports       []string  `yaml:"ports"`
	Volumes     []string  `yaml:"volumes"`
	DependsOn   yaml.Node `yaml:"depends_on"` // list or map
	Restart     string    `yaml:"restart"`
	NetworkMode string    `yaml:"network_mode"`
}

type composeBuildConfig struct {
	Context    string            `yaml:"context"`
	Dockerfile string            `yaml:"dockerfile"`
	Args       map[string]string `yaml:"args"`
}

// parseComposeFile reads and parses a docker-compose file.
func parseComposeFile(dir string) (*composeConfig, string, error) {
	for _, name := range []string{"docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml"} {
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var cfg composeConfig
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return nil, "", fmt.Errorf("parsing %s: %w", name, err)
		}
		return &cfg, name, nil
	}
	return nil, "", fmt.Errorf("no docker-compose file found in %s", dir)
}

// composeBuildContext returns the build context dir and Dockerfile for a service.
// Returns ("", "", nil) when the service uses a pre-built image.
func composeBuildContext(svc composeService, projectDir string) (ctxDir, dockerfile string, buildArgs map[string]string, err error) {
	if svc.Build.IsZero() {
		return "", "", nil, nil
	}

	switch svc.Build.Kind {
	case yaml.ScalarNode:
		// build: ./path
		ctxDir = filepath.Join(projectDir, svc.Build.Value)
		return ctxDir, "Dockerfile", nil, nil

	case yaml.MappingNode:
		var bc composeBuildConfig
		if err := svc.Build.Decode(&bc); err != nil {
			return "", "", nil, fmt.Errorf("decoding build config: %w", err)
		}
		ctxDir = projectDir
		if bc.Context != "" {
			ctxDir = filepath.Join(projectDir, bc.Context)
		}
		df := "Dockerfile"
		if bc.Dockerfile != "" {
			if err := validateDockerfileName(bc.Dockerfile); err != nil {
				return "", "", nil, fmt.Errorf("compose dockerfile: %w", err)
			}
			if _, err := confinedDockerfilePath(ctxDir, bc.Dockerfile); err != nil {
				return "", "", nil, fmt.Errorf("compose dockerfile: %w", err)
			}
			df = bc.Dockerfile
		}
		return ctxDir, df, bc.Args, nil
	}

	return "", "", nil, fmt.Errorf("unsupported build directive (yaml kind %d); expected a path string or a mapping", svc.Build.Kind)
}

// composeCommand returns the command for a service as a slice. Sequence form
// preserves each argv element verbatim. Scalar form is shell-split into argv
// tokens, matching docker-compose's documented behaviour.
func composeCommand(svc composeService) []string {
	if svc.Command.IsZero() {
		return nil
	}
	switch svc.Command.Kind {
	case yaml.ScalarNode:
		return shellSplit(svc.Command.Value)
	case yaml.SequenceNode:
		var parts []string
		_ = svc.Command.Decode(&parts)
		return parts
	}
	return nil
}

// composeArgv splits a service's command into a (cmd, extraArgs) pair suitable
// for CreateContainerRequest.Cmd / UserArgs. cmd is guaranteed to be a single
// shell-safe token (no whitespace) so the agent's strings.Fields(cmd) split is
// a no-op; the remaining argv tokens flow through UserArgs unchanged so
// arguments containing whitespace (e.g. a `-c <script>` body) are preserved.
func composeArgv(svc composeService) (string, []string) {
	parts := composeCommand(svc)
	if len(parts) == 0 {
		return "", nil
	}
	return parts[0], parts[1:]
}

// shellSplit performs minimal POSIX-style splitting on a string command:
// whitespace separates tokens, and pairs of single or double quotes group a
// run of characters into one token. Backslash escapes are not interpreted —
// callers needing those should use the YAML sequence form.
func shellSplit(s string) []string {
	var (
		tokens []string
		cur    strings.Builder
		quote  rune
		inTok  bool
	)
	flush := func() {
		if inTok {
			tokens = append(tokens, cur.String())
			cur.Reset()
			inTok = false
		}
	}
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
				continue
			}
			cur.WriteRune(r)
			inTok = true
		case r == '\'' || r == '"':
			quote = r
			inTok = true
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			flush()
		default:
			cur.WriteRune(r)
			inTok = true
		}
	}
	flush()
	return tokens
}

// composeEnv returns environment variables for a service as KEY=VALUE strings.
// Mapping values may be strings, numbers, bools, or null (inherit from process env).
// Sequence entries may be "KEY=VALUE" or bare "KEY" (inherit from process env).
func composeEnv(svc composeService) []string {
	if svc.Environment.IsZero() {
		return nil
	}
	var result []string
	switch svc.Environment.Kind {
	case yaml.MappingNode:
		var m map[string]any
		if err := svc.Environment.Decode(&m); err == nil {
			for k, v := range m {
				if v == nil {
					if inherited, ok := os.LookupEnv(k); ok {
						result = append(result, k+"="+inherited)
					}
					continue
				}
				result = append(result, k+"="+fmt.Sprint(v))
			}
		}
	case yaml.SequenceNode:
		var list []string
		if err := svc.Environment.Decode(&list); err == nil {
			for _, entry := range list {
				if strings.Contains(entry, "=") {
					result = append(result, entry)
					continue
				}
				if inherited, ok := os.LookupEnv(entry); ok {
					result = append(result, entry+"="+inherited)
				}
			}
		}
	}
	return result
}

// composeRestartPolicy converts a compose restart string to a proto RestartPolicy.
func composeRestartPolicy(restart string) *agentpb.RestartPolicy {
	switch restart {
	case "always", "unless-stopped":
		return &agentpb.RestartPolicy{Mode: agentpb.RestartPolicyMode_UNLESS_STOPPED}
	case "on-failure":
		return &agentpb.RestartPolicy{Mode: agentpb.RestartPolicyMode_ON_FAILURE}
	case "no", "":
		return &agentpb.RestartPolicy{Mode: agentpb.RestartPolicyMode_NO}
	default:
		return &agentpb.RestartPolicy{Mode: agentpb.RestartPolicyMode_DEFAULT}
	}
}

// parseComposeVolume parses a docker-compose short volume spec into its
// (source, target, mode) parts. Handles three forms:
//
//	target                       — anonymous volume
//	source:target                — named volume or bind mount
//	source:target:options        — with options like "ro" or "rw"
//
// Windows-style absolute paths (e.g. "C:\\data:/in") are detected when the
// first segment is a single drive letter and merged back with the second.
// Returns empty strings when the input cannot be parsed.
func parseComposeVolume(v string) (source, target, mode string) {
	parts := strings.Split(v, ":")
	// Re-merge a leading "<letter>:<path>" Windows-style drive prefix.
	if len(parts) >= 2 && len(parts[0]) == 1 {
		c := parts[0][0]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') {
			parts = append([]string{parts[0] + ":" + parts[1]}, parts[2:]...)
		}
	}
	switch len(parts) {
	case 1:
		return "", parts[0], ""
	case 2:
		return parts[0], parts[1], ""
	case 3:
		return parts[0], parts[1], parts[2]
	default:
		return "", "", ""
	}
}

// composeAppConfig builds an AppConfig for a compose service.
// It synthesises network entitlements from ports/network_mode and persist
// entitlements from named volumes.
func composeAppConfig(projectName, serviceName string, svc composeService) *appconfig.AppConfig {
	appID := projectName + "-" + serviceName

	var entitlements []appconfig.Entitlement

	// Network entitlement.
	if svc.NetworkMode == "host" {
		entitlements = append(entitlements, appconfig.Entitlement{
			Type: appconfig.EntitlementNetwork,
			Mode: "host",
		})
	} else if len(svc.Ports) > 0 {
		var ports []appconfig.PortMapping
		for _, p := range svc.Ports {
			// Parse "host:container" or "container" format.
			parts := strings.SplitN(p, ":", 2)
			var pm appconfig.PortMapping
			if len(parts) == 2 {
				fmt.Sscanf(parts[0], "%d", &pm.Host)
				fmt.Sscanf(parts[1], "%d", &pm.Container)
			} else {
				fmt.Sscanf(parts[0], "%d", &pm.Container)
				pm.Host = pm.Container
			}
			if pm.Host > 0 && pm.Container > 0 {
				ports = append(ports, pm)
			}
		}
		if len(ports) > 0 {
			entitlements = append(entitlements, appconfig.Entitlement{
				Type:  appconfig.EntitlementNetwork,
				Ports: ports,
			})
		}
	}

	// Persist entitlements from named volumes (skip host-bind mounts ./path:).
	for _, v := range svc.Volumes {
		source, target, _ := parseComposeVolume(v)
		if source == "" || target == "" {
			continue
		}
		// Named volumes start with a letter; bind mounts start with . or /
		// (or, on Windows, a drive letter like "C:\\…" — already merged into source).
		if strings.HasPrefix(source, ".") || strings.HasPrefix(source, "/") {
			continue
		}
		if len(source) >= 2 && source[1] == ':' {
			// Windows-style absolute path bind mount.
			continue
		}
		entitlements = append(entitlements, appconfig.Entitlement{
			Type: appconfig.EntitlementPersist,
			Name: source,
			Path: target,
		})
	}

	return &appconfig.AppConfig{
		AppID:        appID,
		Entitlements: entitlements,
	}
}

// serviceOrder returns service names sorted by depends_on so dependencies
// start before dependents. It returns an error if any depends_on entry
// references an undefined service. Cycles are ignored; remaining services are
// appended at the end.
func serviceOrder(cfg *composeConfig) ([]string, error) {
	// Build dependency map and validate that every dependency is a defined service.
	deps := make(map[string][]string, len(cfg.Services))
	for name, svc := range cfg.Services {
		var depends []string
		switch svc.DependsOn.Kind {
		case yaml.SequenceNode:
			_ = svc.DependsOn.Decode(&depends)
		case yaml.MappingNode:
			var m map[string]interface{}
			if svc.DependsOn.Decode(&m) == nil {
				for k := range m {
					depends = append(depends, k)
				}
			}
		}
		for _, dep := range depends {
			if _, ok := cfg.Services[dep]; !ok {
				return nil, fmt.Errorf("service %q depends on unknown service %q", name, dep)
			}
		}
		deps[name] = depends
	}

	var ordered []string
	visited := make(map[string]bool)

	var visit func(name string)
	visit = func(name string) {
		if visited[name] {
			return
		}
		visited[name] = true
		for _, dep := range deps[name] {
			visit(dep)
		}
		ordered = append(ordered, name)
	}

	for name := range cfg.Services {
		visit(name)
	}
	return ordered, nil
}

// serviceLogPalette is the fixed color rotation for service name prefixes.
var serviceLogPalette = []lipgloss.Color{
	tui.Sky500,                // cyan-ish
	tui.Amber500,              // yellow
	tui.Emerald400,            // green
	lipgloss.Color("#c084fc"), // magenta
	lipgloss.Color("#60a5fa"), // blue
	tui.Red500,                // red
}

// serviceLogWriter buffers output for a single service and writes complete
// lines prefixed with a color-coded, column-aligned service name.
// It is safe to call Write from a single goroutine; Flush drains any partial line.
type serviceLogWriter struct {
	mu     *sync.Mutex // shared with all writers so lines don't interleave
	dest   *os.File
	buf    strings.Builder
	prefix string // pre-rendered "[name]  " with padding and color
}

func newServiceLogWriters(names []string) (stdout, stderr map[string]*serviceLogWriter) {
	mu := &sync.Mutex{}
	maxLen := 0
	for _, n := range names {
		if len(n) > maxLen {
			maxLen = len(n)
		}
	}
	stdout = make(map[string]*serviceLogWriter, len(names))
	stderr = make(map[string]*serviceLogWriter, len(names))
	for i, name := range names {
		color := serviceLogPalette[i%len(serviceLogPalette)]
		style := lipgloss.NewStyle().Foreground(color).Bold(true)
		errStyle := lipgloss.NewStyle().Foreground(color).Bold(true)
		padding := strings.Repeat(" ", maxLen-len(name)+1)
		prefix := style.Render("["+name+"]") + padding
		stdout[name] = &serviceLogWriter{mu: mu, dest: os.Stdout, prefix: prefix}
		stderr[name] = &serviceLogWriter{mu: mu, dest: os.Stderr, prefix: errStyle.Render("["+name+"]") + padding}
	}
	return stdout, stderr
}

func (w *serviceLogWriter) Write(p []byte) {
	for _, b := range p {
		if b == '\n' {
			w.mu.Lock()
			fmt.Fprintln(w.dest, w.prefix+w.buf.String())
			w.buf.Reset()
			w.mu.Unlock()
		} else {
			w.buf.WriteByte(b)
		}
	}
}

func (w *serviceLogWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.buf.Len() > 0 {
		fmt.Fprintln(w.dest, w.prefix+w.buf.String())
		w.buf.Reset()
	}
}

// runComposeWithAgent orchestrates a docker-compose project on a WendyOS device:
// builds service images, pushes them to the device registry, creates containers,
// and streams their combined output.
func runComposeWithAgent(ctx context.Context, conn *grpcclient.AgentConnection, projectDir string, opts runOptions) error {
	cfg, composeFilename, err := parseComposeFile(projectDir)
	if err != nil {
		return err
	}
	if len(cfg.Services) == 0 {
		return fmt.Errorf("%s defines no services", composeFilename)
	}

	if err := requireRegistryAuth(ctx, conn); err != nil {
		return err
	}

	versionResp, err := conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
	if err != nil {
		return fmt.Errorf("querying device version: %w", err)
	}
	agentOS := versionResp.GetOs()
	architecture := versionResp.GetCpuArchitecture()
	if architecture == "" {
		architecture = "arm64"
	}
	platform := resolveAgentPlatform("", agentOS, architecture)

	regPort := registryPort(agentOS)
	registryAddr, proxyCleanup, err := resolveRegistryForAgent(ctx, conn, regPort)
	if err != nil {
		return err
	}
	defer proxyCleanup()

	// Use the project directory name as the project name.
	projectName := strings.ToLower(filepath.Base(projectDir))

	// Build and push each service that has a build directive.
	for name, svc := range cfg.Services {
		ctxDir, dockerfile, buildArgs, err := composeBuildContext(svc, projectDir)
		if err != nil {
			return fmt.Errorf("service %s: %w", name, err)
		}
		if ctxDir == "" {
			continue // uses pre-built image
		}

		imageName := fmt.Sprintf("%s/%s-%s:latest", registryAddr, projectName, name)
		allBuildArgs := map[string]string{
			"WENDY_PLATFORM": wendyPlatform(versionResp.GetDeviceType()),
			"WENDY_DEBUG":    fmt.Sprintf("%t", opts.debug),
		}
		// Mirror the single-container build path so compose-built Dockerfiles
		// see the same WendyOS device hints (e.g. for GPU base-image selection).
		if deviceType := versionResp.GetDeviceType(); deviceType != "" {
			allBuildArgs["WENDY_DEVICE_TYPE"] = deviceType
		}
		if versionResp.HasGpu != nil {
			allBuildArgs["WENDY_HAS_GPU"] = fmt.Sprintf("%t", versionResp.GetHasGpu())
		}
		if vendor := versionResp.GetGpuVendor(); vendor != "" {
			allBuildArgs["WENDY_GPU_VENDOR"] = vendor
		}
		if jv := versionResp.GetJetpackVersion(); jv != "" {
			allBuildArgs["WENDY_JETPACK_VERSION"] = jv
		}
		if cv := versionResp.GetCudaVersion(); cv != "" {
			allBuildArgs["WENDY_CUDA_VERSION"] = cv
		}
		for k, v := range buildArgs {
			allBuildArgs[k] = v
		}

		cliLogln("Building image for service %s...", name)

		buildDockerfile := dockerfile
		if buildDockerfile == "Dockerfile" {
			buildDockerfile = ""
		}
		if err := buildAndPushImage(ctx, ctxDir, registryAddr, imageName, platform, buildDockerfile, allBuildArgs, os.Stdout, conn.IsMTLS); err != nil {
			return fmt.Errorf("building service %s: %w", name, err)
		}
		cliLogln("Service %s image built and pushed.", name)
	}

	cliRestartPolicy := resolveRestartPolicy(opts)

	// Create all containers in dependency order.
	ordered, err := serviceOrder(cfg)
	if err != nil {
		return err
	}
	for _, name := range ordered {
		svc := cfg.Services[name]
		appCfg := composeAppConfig(projectName, name, svc)

		// Determine image: built image or declared image. Public image refs
		// like "python:3.11-slim" must be canonicalised to "docker.io/library/…"
		// because the agent's containerd reference parser only accepts
		// fully-qualified names.
		ctxDir, _, _, _ := composeBuildContext(svc, projectDir)
		var imageName string
		if ctxDir != "" {
			imageName = fmt.Sprintf("localhost:%d/%s-%s:latest", regPort, projectName, name)
		} else if svc.Image != "" {
			imageName = normalizeImageRef(svc.Image)
		} else {
			return fmt.Errorf("service %s: no image or build directive", name)
		}

		appConfigData, err := json.Marshal(appCfg)
		if err != nil {
			return fmt.Errorf("marshaling config for service %s: %w", name, err)
		}

		// Split the compose command into argv: the first token becomes Cmd
		// (the agent runs strings.Fields on it, so it must contain no
		// whitespace) and the rest are passed verbatim through UserArgs so
		// arguments like a multi-line `python3 -c <script>` survive intact.
		cmd, extraArgs := composeArgv(svc)

		// CLI flags take precedence over per-service compose restart policies;
		// when the CLI didn't specify one (DEFAULT), honour the service's restart.
		restartPolicy := cliRestartPolicy
		if restartPolicy.GetMode() == agentpb.RestartPolicyMode_DEFAULT && svc.Restart != "" {
			restartPolicy = composeRestartPolicy(svc.Restart)
		}

		// TODO: compose `environment:` values aren't sent to the device yet.
		// CreateContainerRequest has no env field, and stuffing env strings
		// into UserArgs (the previous behaviour) appended them to argv. Add a
		// proto env field and plumb composeEnv(svc) through it.

		createReq := &agentpb.CreateContainerRequest{
			ImageName:     imageName,
			AppName:       appCfg.AppID,
			AppConfig:     appConfigData,
			Cmd:           cmd,
			RestartPolicy: restartPolicy,
			UserArgs:      extraArgs,
		}

		cliLogln("Creating container for service %s (%s)...", name, appCfg.AppID)
		if err := createContainerWithProgress(ctx, conn.ContainerService, createReq); err != nil {
			return fmt.Errorf("creating container for service %s: %w", name, err)
		}
		cliLogln("Container %s created.", appCfg.AppID)
	}

	if opts.deploy {
		cliLogln("All %d service containers created (not started).", len(cfg.Services))
		return nil
	}

	// Start all containers and stream their output concurrently.
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	defer signal.Stop(sigCh)
	go func() {
		select {
		case <-sigCh:
		case <-runCtx.Done():
			return
		}
		// Stop in reverse dependency order.
		stopCtx := context.Background()
		stopped := 0
		fmt.Println()
		for i := len(ordered) - 1; i >= 0; i-- {
			name := ordered[i]
			svc := cfg.Services[name]
			appCfg := composeAppConfig(projectName, name, svc)
			cliLogln("Stopping %s...", name)
			_, _ = conn.ContainerService.StopContainer(stopCtx, &agentpb.StopContainerRequest{
				AppName: appCfg.AppID,
			})
			stopped++
		}
		cliLogln("Stopped %d service(s).", stopped)
		runCancel()
	}()

	if opts.detach {
		for _, name := range ordered {
			svc := cfg.Services[name]
			appCfg := composeAppConfig(projectName, name, svc)
			stream, err := conn.ContainerService.StartContainer(ctx, &agentpb.StartContainerRequest{
				AppName: appCfg.AppID,
			})
			if err != nil {
				return fmt.Errorf("starting service %s: %w", name, err)
			}
			if _, err := stream.Recv(); err != nil && err != io.EOF {
				return fmt.Errorf("waiting for service %s start: %w", name, err)
			}
			cliLogln("Service %s started.", name)
		}
		cliLogln("All services running in detached mode.")
		projectID := strings.ToLower(filepath.Base(projectDir))
		cliLogln("Run 'wendy logs %s' to stream logs.", projectID)
		return nil
	}

	// Attached mode: stream output from all containers concurrently with
	// color-coded, column-aligned service name prefixes.
	serviceNames := make([]string, len(ordered))
	copy(serviceNames, ordered)
	stdoutWriters, stderrWriters := newServiceLogWriters(serviceNames)

	var wg sync.WaitGroup
	errCh := make(chan error, len(ordered))

	for _, name := range ordered {
		svc := cfg.Services[name]
		appCfg := composeAppConfig(projectName, name, svc)

		wg.Add(1)
		go func(serviceName, appID string) {
			defer wg.Done()
			outW := stdoutWriters[serviceName]
			errW := stderrWriters[serviceName]
			defer outW.Flush()
			defer errW.Flush()

			stream, streamErr := conn.ContainerService.AttachContainer(runCtx)
			if streamErr == nil {
				streamErr = stream.Send(&agentpb.AttachContainerRequest{
					RequestType: &agentpb.AttachContainerRequest_AppName{AppName: appID},
				})
				if streamErr != nil {
					_ = stream.CloseSend()
				}
			}
			if streamErr != nil {
				// Fall back to the server-streaming StartContainer when AttachContainer is unavailable.
				startStream, startErr := conn.ContainerService.StartContainer(runCtx, &agentpb.StartContainerRequest{
					AppName: appID,
				})
				if startErr != nil {
					errCh <- fmt.Errorf("starting service %s: %w", serviceName, startErr)
					return
				}
				for {
					resp, recvErr := startStream.Recv()
					if recvErr == io.EOF {
						return
					}
					if recvErr != nil {
						if runCtx.Err() != nil {
							return
						}
						errCh <- fmt.Errorf("service %s: %w", serviceName, recvErr)
						return
					}
					if out := resp.GetStdoutOutput(); out != nil {
						outW.Write(out.GetData())
					}
					if out := resp.GetStderrOutput(); out != nil {
						errW.Write(out.GetData())
					}
				}
			}

			for {
				resp, recvErr := stream.Recv()
				if recvErr == io.EOF {
					return
				}
				if recvErr != nil {
					if runCtx.Err() != nil {
						return
					}
					errCh <- fmt.Errorf("service %s: %w", serviceName, recvErr)
					return
				}
				if out := resp.GetStdoutOutput(); out != nil {
					outW.Write(out.GetData())
				}
				if out := resp.GetStderrOutput(); out != nil {
					errW.Write(out.GetData())
				}
			}
		}(name, appCfg.AppID)
	}

	cliLogln("All services started.")

	wg.Wait()

	select {
	case err := <-errCh:
		if runCtx.Err() == nil {
			return err
		}
	default:
	}

	if runCtx.Err() == nil {
		cliLogln("\nAll services stopped.")
	}
	return nil
}
