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

	"gopkg.in/yaml.v3"

	"github.com/wendylabsinc/wendy/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

// composeConfig is a minimal representation of a docker-compose file.
type composeConfig struct {
	Services map[string]composeService `yaml:"services"`
}

type composeService struct {
	Image       string      `yaml:"image"`
	Build       yaml.Node   `yaml:"build"` // string or build object
	Command     yaml.Node   `yaml:"command"`
	Environment yaml.Node   `yaml:"environment"` // map or list
	Ports       []string    `yaml:"ports"`
	Volumes     []string    `yaml:"volumes"`
	DependsOn   yaml.Node   `yaml:"depends_on"` // list or map
	Restart     string      `yaml:"restart"`
	NetworkMode string      `yaml:"network_mode"`
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
			df = bc.Dockerfile
		}
		return ctxDir, df, bc.Args, nil
	}

	return "", "", nil, nil
}

// composeCommand returns the command for a service as a slice.
func composeCommand(svc composeService) []string {
	if svc.Command.IsZero() {
		return nil
	}
	switch svc.Command.Kind {
	case yaml.ScalarNode:
		return []string{svc.Command.Value}
	case yaml.SequenceNode:
		var parts []string
		_ = svc.Command.Decode(&parts)
		return parts
	}
	return nil
}

// composeEnv returns environment variables for a service as KEY=VALUE strings.
func composeEnv(svc composeService) []string {
	if svc.Environment.IsZero() {
		return nil
	}
	var result []string
	switch svc.Environment.Kind {
	case yaml.MappingNode:
		var m map[string]string
		if err := svc.Environment.Decode(&m); err == nil {
			for k, v := range m {
				result = append(result, k+"="+v)
			}
		}
	case yaml.SequenceNode:
		var list []string
		if err := svc.Environment.Decode(&list); err == nil {
			result = list
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
		parts := strings.SplitN(v, ":", 2)
		if len(parts) < 2 {
			continue
		}
		hostPart := parts[0]
		containerPath := parts[1]
		// Named volumes start with a letter; bind mounts start with . or /
		if strings.HasPrefix(hostPart, ".") || strings.HasPrefix(hostPart, "/") {
			continue
		}
		entitlements = append(entitlements, appconfig.Entitlement{
			Type: appconfig.EntitlementPersist,
			Name: hostPart,
			Path: containerPath,
		})
	}

	return &appconfig.AppConfig{
		AppID:        appID,
		Entitlements: entitlements,
	}
}

// serviceOrder returns service names sorted by depends_on so dependencies
// start before dependents. Cycles are ignored; any remaining services are
// appended at the end.
func serviceOrder(cfg *composeConfig) []string {
	// Build dependency map.
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
	return ordered
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
	registryAddr, proxyCleanup, err := resolveRegistry(ctx, conn.Host, regPort)
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
		for k, v := range buildArgs {
			allBuildArgs[k] = v
		}

		cliLogln("Building image for service %s...", name)

		// If a non-default Dockerfile is specified, we need to pass it via -f.
		// buildAndPushImage always uses "." as the path; for compose we pass the
		// build context dir and rely on a Dockerfile inside it (or via ARG).
		// We temporarily write a wrapper that delegates to the real Dockerfile when
		// the dockerfile field differs. For the common case (Dockerfile in context),
		// it just works.
		if dockerfile != "Dockerfile" {
			// Rewrite -f by creating a temp Dockerfile that uses the named file.
			// Actually, buildAndPushImage doesn't support -f. We need a small workaround:
			// copy the Dockerfile to the context dir as "Dockerfile" temporarily.
			// For now, just error with a helpful message.
			return fmt.Errorf("service %s: custom Dockerfile path %q is not yet supported; rename it to 'Dockerfile'", name, dockerfile)
		}

		if err := buildAndPushImage(ctx, ctxDir, registryAddr, imageName, platform, allBuildArgs, os.Stdout, conn.IsMTLS); err != nil {
			return fmt.Errorf("building service %s: %w", name, err)
		}
		cliLogln("Service %s image built and pushed.", name)
	}

	restartPolicy := resolveRestartPolicy(opts)

	// Create all containers in dependency order.
	ordered := serviceOrder(cfg)
	for _, name := range ordered {
		svc := cfg.Services[name]
		appCfg := composeAppConfig(projectName, name, svc)

		// Determine image: built image or declared image.
		ctxDir, _, _, _ := composeBuildContext(svc, projectDir)
		var imageName string
		if ctxDir != "" {
			imageName = fmt.Sprintf("localhost:%d/%s-%s:latest", regPort, projectName, name)
		} else if svc.Image != "" {
			imageName = svc.Image
		} else {
			return fmt.Errorf("service %s: no image or build directive", name)
		}

		appConfigData, err := json.Marshal(appCfg)
		if err != nil {
			return fmt.Errorf("marshaling config for service %s: %w", name, err)
		}

		cmd := ""
		if parts := composeCommand(svc); len(parts) > 0 {
			cmd = strings.Join(parts, " ")
		}

		envArgs := composeEnv(svc)

		createReq := &agentpb.CreateContainerRequest{
			ImageName:     imageName,
			AppName:       appCfg.AppID,
			AppConfig:     appConfigData,
			Cmd:           cmd,
			RestartPolicy: restartPolicy,
			UserArgs:      envArgs,
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
	go func() {
		<-sigCh
		cliLogln("\nStopping all services...")
		stopCtx := context.Background()
		for _, name := range ordered {
			svc := cfg.Services[name]
			appCfg := composeAppConfig(projectName, name, svc)
			_, _ = conn.ContainerService.StopContainer(stopCtx, &agentpb.StopContainerRequest{
				AppName: appCfg.AppID,
			})
		}
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
		return nil
	}

	// Attached mode: stream output from all containers concurrently, prefixed by service name.
	var wg sync.WaitGroup
	errCh := make(chan error, len(ordered))

	for _, name := range ordered {
		svc := cfg.Services[name]
		appCfg := composeAppConfig(projectName, name, svc)

		wg.Add(1)
		go func(serviceName, appID string) {
			defer wg.Done()

			stream, err := conn.ContainerService.StartContainer(runCtx, &agentpb.StartContainerRequest{
				AppName: appID,
			})
			if err != nil {
				errCh <- fmt.Errorf("starting service %s: %w", serviceName, err)
				return
			}

			prefix := "[" + serviceName + "] "
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
					_, _ = fmt.Fprint(os.Stdout, prefix+string(out.GetData()))
				}
				if out := resp.GetStderrOutput(); out != nil {
					_, _ = fmt.Fprint(os.Stderr, prefix+string(out.GetData()))
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
