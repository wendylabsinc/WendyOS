package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/wendylabsinc/wendy/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/internal/cli/tui"
	"github.com/wendylabsinc/wendy/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

const maxConcurrentBuilds = 4

// resolveServiceSubset returns the services to build when --service is
// specified. The named service and all transitive dependsOn entries are
// included; if the name is empty all services are returned unchanged.
func resolveServiceSubset(services map[string]*appconfig.ServiceConfig, only string) (map[string]*appconfig.ServiceConfig, error) {
	if only == "" {
		return services, nil
	}

	svc, ok := services[only]
	if !ok {
		return nil, fmt.Errorf("--service %q not found in services map", only)
	}

	subset := map[string]*appconfig.ServiceConfig{only: svc}
	var walk func(name string)
	walk = func(name string) {
		for _, dep := range services[name].DependsOn {
			if _, seen := subset[dep]; !seen {
				subset[dep] = services[dep]
				walk(dep)
			}
		}
	}
	walk(only)
	return subset, nil
}

// serviceTopoOrder returns service names in topological order so that every
// service appears after its dependsOn entries. Cycles are broken by appending
// remaining names at the end (compose.go uses the same approach).
func serviceTopoOrder(services map[string]*appconfig.ServiceConfig) []string {
	visited := make(map[string]bool, len(services))
	ordered := make([]string, 0, len(services))

	var visit func(name string)
	visit = func(name string) {
		if visited[name] {
			return
		}
		visited[name] = true
		if svc, ok := services[name]; ok {
			for _, dep := range svc.DependsOn {
				visit(dep)
			}
		}
		ordered = append(ordered, name)
	}

	// Iterate in a stable order: collect names, sort, then visit.
	names := make([]string, 0, len(services))
	for n := range services {
		names = append(names, n)
	}
	stableSort(names)
	for _, n := range names {
		visit(n)
	}
	return ordered
}

// stableSort sorts a string slice in place.
func stableSort(ss []string) {
	for i := 1; i < len(ss); i++ {
		for j := i; j > 0 && ss[j] < ss[j-1]; j-- {
			ss[j], ss[j-1] = ss[j-1], ss[j]
		}
	}
}

// runMultiServiceWithAgent orchestrates the full build → push → create →
// stream pipeline for a multi-service wendy.json on a single agent.
func runMultiServiceWithAgent(ctx context.Context, conn *grpcclient.AgentConnection, cwd string, appCfg *appconfig.AppConfig, opts runOptions) error {
	services, err := resolveServiceSubset(appCfg.Services, opts.service)
	if err != nil {
		return err
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
	platform := resolveAgentPlatform(appCfg.Platform, agentOS, architecture)

	regPort := registryPort(agentOS)
	registryAddr, proxyCleanup, err := resolveRegistryForAgent(ctx, conn, regPort)
	if err != nil {
		return err
	}
	defer proxyCleanup()

	buildArgs := map[string]string{
		"WENDY_PLATFORM": wendyPlatform(versionResp.GetDeviceType()),
		"WENDY_DEBUG":    fmt.Sprintf("%t", opts.debug),
	}
	if dt := versionResp.GetDeviceType(); dt != "" {
		buildArgs["WENDY_DEVICE_TYPE"] = dt
	}
	if versionResp.HasGpu != nil {
		buildArgs["WENDY_HAS_GPU"] = fmt.Sprintf("%t", versionResp.GetHasGpu())
	}
	if v := versionResp.GetGpuVendor(); v != "" {
		buildArgs["WENDY_GPU_VENDOR"] = v
	}
	if jv := versionResp.GetJetpackVersion(); jv != "" {
		buildArgs["WENDY_JETPACK_VERSION"] = jv
	}
	if cv := versionResp.GetCudaVersion(); cv != "" {
		buildArgs["WENDY_CUDA_VERSION"] = cv
	}

	// Build all service images in parallel, then create and start containers.
	if err := buildServicesParallel(ctx, cwd, appCfg.AppID, services, registryAddr, platform, buildArgs, conn.IsMTLS); err != nil {
		return err
	}

	// Create containers in dependency order.
	ordered := serviceTopoOrder(services)
	for _, name := range ordered {
		svc := services[name]
		deviceImage := fmt.Sprintf("localhost:%d/%s-%s:latest", regPort,
			strings.ToLower(appCfg.AppID), strings.ToLower(name))

		serviceCfg := &appconfig.AppConfig{
			AppID:        fmt.Sprintf("%s-%s", appCfg.AppID, name),
			Platform:     appCfg.Platform,
			Entitlements: svc.Entitlements,
		}
		appConfigData, err := json.Marshal(serviceCfg)
		if err != nil {
			return fmt.Errorf("marshaling config for service %s: %w", name, err)
		}

		restartPolicy := resolveRestartPolicy(opts)
		createReq := &agentpb.CreateContainerRequest{
			ImageName:     deviceImage,
			AppName:       serviceCfg.AppID,
			AppConfig:     appConfigData,
			RestartPolicy: restartPolicy,
		}

		cliLogln("Creating container for service %s...", name)
		if err := createContainerWithProgress(ctx, conn.ContainerService, createReq); err != nil {
			return fmt.Errorf("creating container for service %s: %w", name, err)
		}
		cliLogln("Service %s container created.", name)
	}

	if opts.deploy {
		cliLogln("App group %s created (not started, --deploy).", appCfg.AppID)
		return nil
	}

	// Start all containers and multiplex log output with per-service prefixes.
	return startAndStreamServices(ctx, conn, appCfg.AppID, ordered, opts)
}

// buildServicesParallel builds all service images concurrently (up to
// maxConcurrentBuilds at a time). Progress is shown via a Bubbletea multi-
// spinner in interactive terminals and via plain log lines otherwise.
func buildServicesParallel(
	ctx context.Context,
	cwd, appID string,
	services map[string]*appconfig.ServiceConfig,
	registryAddr, platform string,
	buildArgs map[string]string,
	useMTLS bool,
) error {
	names := make([]string, 0, len(services))
	for n := range services {
		names = append(names, n)
	}
	stableSort(names)

	type result struct {
		name string
		err  error
		dur  time.Duration
	}

	results := make(chan result, len(names))
	sem := make(chan struct{}, maxConcurrentBuilds)

	var prog *tea.Program
	if isInteractiveTerminal() {
		title := fmt.Sprintf("Building %d service(s)...", len(names))
		m := tui.NewMultiSpinner(title, names)
		prog = tea.NewProgram(m)
	}

	var wg sync.WaitGroup
	for _, name := range names {
		wg.Add(1)
		go func(name string, svc *appconfig.ServiceConfig) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			if prog != nil {
				prog.Send(tui.MultiSpinnerStartMsg{Name: name})
			} else {
				cliLogln("Building service %s...", name)
			}

			start := time.Now()
			contextDir := filepath.Join(cwd, svc.Context)
			imageName := fmt.Sprintf("%s/%s-%s:latest",
				registryAddr, strings.ToLower(appID), strings.ToLower(name))

			err := buildAndPushImage(ctx, contextDir, registryAddr, imageName, platform, buildArgs, os.Stdout, useMTLS)
			dur := time.Since(start)

			if prog != nil {
				prog.Send(tui.MultiSpinnerDoneMsg{Name: name, Err: err, Dur: dur})
			} else if err != nil {
				cliLogln("Service %s build failed: %v", name, err)
			} else {
				cliLogln("Service %s built (%s).", name, dur.Round(time.Millisecond))
			}

			results <- result{name: name, err: err, dur: dur}
		}(name, services[name])
	}

	// Wait for all goroutines, close the results channel, then signal TUI done.
	go func() {
		wg.Wait()
		close(results)
		if prog != nil {
			prog.Send(tui.MultiSpinnerAllDoneMsg{})
		}
	}()

	if prog != nil {
		if _, runErr := prog.Run(); runErr != nil {
			return fmt.Errorf("build progress TUI: %w", runErr)
		}
	}

	// Collect errors from all builds.
	var errs []error
	for r := range results {
		if r.err != nil {
			errs = append(errs, fmt.Errorf("service %s: %w", r.name, r.err))
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

var serviceLogStyle = lipgloss.NewStyle().Foreground(tui.ColorInfo)

// startAndStreamServices starts all service containers and streams their
// combined output to stdout/stderr with a "[serviceName] " prefix per line.
// This is a best-effort multiplexer; proper per-service log routing is handled
// by WDY-893 (multiplexed AttachContainer).
func startAndStreamServices(ctx context.Context, conn *grpcclient.AgentConnection, appID string, ordered []string, opts runOptions) error {
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()

	// Ctrl+C stops all services.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	go func() {
		<-sigCh
		cliLogln("\nStopping services...")
		for _, name := range ordered {
			_, _ = conn.ContainerService.StopContainer(context.Background(), &agentpb.StopContainerRequest{
				AppName: fmt.Sprintf("%s-%s", appID, name),
			})
		}
		runCancel()
	}()

	if opts.detach {
		for _, name := range ordered {
			containerName := fmt.Sprintf("%s-%s", appID, name)
			stream, err := conn.ContainerService.StartContainer(runCtx, &agentpb.StartContainerRequest{
				AppName: containerName,
			})
			if err != nil {
				return fmt.Errorf("starting service %s: %w", name, err)
			}
			if _, err := stream.Recv(); err != nil && err != io.EOF {
				return fmt.Errorf("waiting for service %s to start: %w", name, err)
			}
		}
		cliLogln("App group %s running in detached mode.", appID)
		return nil
	}

	type logLine struct {
		service string
		stdout  bool
		data    []byte
	}
	lines := make(chan logLine, 256)

	var wg sync.WaitGroup
	for _, name := range ordered {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			containerName := fmt.Sprintf("%s-%s", appID, name)
			stream, err := conn.ContainerService.StartContainer(runCtx, &agentpb.StartContainerRequest{
				AppName: containerName,
			})
			if err != nil {
				if runCtx.Err() == nil {
					cliLogln("Warning: starting service %s: %v", name, err)
				}
				return
			}
			for {
				resp, recvErr := stream.Recv()
				if recvErr == io.EOF {
					return
				}
				if recvErr != nil {
					if runCtx.Err() == nil {
						cliLogln("Warning: service %s stream: %v", name, recvErr)
					}
					return
				}
				if out := resp.GetStdoutOutput(); out != nil {
					lines <- logLine{service: name, stdout: true, data: out.GetData()}
				}
				if out := resp.GetStderrOutput(); out != nil {
					lines <- logLine{service: name, stdout: false, data: out.GetData()}
				}
			}
		}(name)
	}

	go func() {
		wg.Wait()
		close(lines)
	}()

	cliLogln("App group %s started (%d services).", appID, len(ordered))

	for line := range lines {
		prefix := serviceLogStyle.Render(fmt.Sprintf("[%s] ", line.service))
		if line.stdout {
			fmt.Fprint(os.Stdout, prefix)
			os.Stdout.Write(line.data)
		} else {
			fmt.Fprint(os.Stderr, prefix)
			os.Stderr.Write(line.data)
		}
	}

	if runCtx.Err() != nil {
		cliLogln("\nApp group %s stopped.", appID)
		return nil
	}
	cliLogln("\nApp group %s stopped.", appID)
	return nil
}
