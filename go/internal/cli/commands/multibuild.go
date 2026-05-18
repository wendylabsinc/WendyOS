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
	"sort"
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
	var walk func(name string) error
	walk = func(name string) error {
		svc, ok := services[name]
		if !ok || svc == nil {
			return nil
		}
		for _, dep := range svc.DependsOn {
			if _, seen := subset[dep]; !seen {
				depSvc, ok := services[dep]
				if !ok {
					return fmt.Errorf("service %q depends on unknown service %q", name, dep)
				}
				subset[dep] = depSvc
				if err := walk(dep); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(only); err != nil {
		return nil, err
	}
	return subset, nil
}

// serviceTopoOrder returns service names in topological order so that every
// service appears after its dependsOn entries. Returns an error for cyclic
// graphs. All deps must already be present in services (resolveServiceSubset
// guarantees transitive closure); a missing dep returns an error.
func serviceTopoOrder(services map[string]*appconfig.ServiceConfig) ([]string, error) {
	visited := make(map[string]bool, len(services))
	inStack := make(map[string]bool, len(services))
	ordered := make([]string, 0, len(services))

	var visit func(name string) error
	visit = func(name string) error {
		if visited[name] {
			return nil
		}
		if inStack[name] {
			return fmt.Errorf("cycle detected in dependsOn graph involving service %q", name)
		}
		inStack[name] = true
		if svc, ok := services[name]; ok {
			for _, dep := range svc.DependsOn {
				if _, present := services[dep]; !present {
					return fmt.Errorf("service %q depends on %q which is not in the build subset", name, dep)
				}
				if err := visit(dep); err != nil {
					return err
				}
			}
		}
		delete(inStack, name)
		visited[name] = true
		ordered = append(ordered, name)
		return nil
	}

	// Iterate in a stable order: collect names, sort, then visit.
	names := make([]string, 0, len(services))
	for n := range services {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if err := visit(n); err != nil {
			return nil, err
		}
	}
	return ordered, nil
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
		cliLogln("Warning: agent did not report CPU architecture; assuming arm64.")
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
	}
	if opts.debug {
		cliLogln("Warning: building with WENDY_DEBUG=true — do not deploy to production.")
		buildArgs["WENDY_DEBUG"] = "true"
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
	ordered, err := serviceTopoOrder(services)
	if err != nil {
		return err
	}
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
		if err := createContainerWithoutProgress(ctx, conn.ContainerService, createReq); err != nil {
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
	sort.Strings(names)

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

			buildOut := io.Writer(os.Stdout)
			if prog != nil {
				buildOut = io.Discard
			}
			err := buildAndPushImage(ctx, contextDir, registryAddr, imageName, platform, buildArgs, buildOut, useMTLS)
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
	defer signal.Stop(sigCh)
	go func() {
		<-sigCh
		cliLogln("\nStopping services...")
		runCancel()
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer stopCancel()
		var stopWg sync.WaitGroup
		for _, name := range ordered {
			stopWg.Add(1)
			go func(name string) {
				defer stopWg.Done()
				_, _ = conn.ContainerService.StopContainer(stopCtx, &agentpb.StopContainerRequest{
					AppName: fmt.Sprintf("%s-%s", appID, name),
				})
			}(name)
		}
		stopWg.Wait()
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
					select {
					case lines <- logLine{service: name, stdout: true, data: out.GetData()}:
					case <-runCtx.Done():
						return
					}
				}
				if out := resp.GetStderrOutput(); out != nil {
					select {
					case lines <- logLine{service: name, stdout: false, data: out.GetData()}:
					case <-runCtx.Done():
						return
					}
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
			fmt.Fprintf(os.Stdout, "%s%s", prefix, line.data)
		} else {
			fmt.Fprintf(os.Stderr, "%s%s", prefix, line.data)
		}
	}

	if runCtx.Err() != nil {
		cliLogln("\nApp group %s stopped.", appID)
		return nil
	}
	cliLogln("\nApp group %s stopped.", appID)
	return nil
}
