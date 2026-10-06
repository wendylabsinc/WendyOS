package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/protobuf/proto"
)

// A submitted transaction must never be retried through another transport:
// even a broken response stream may have committed or rolled back on-device.
type submittedDeploymentError struct{ err error }

func (e *submittedDeploymentError) Error() string { return e.err.Error() }
func (e *submittedDeploymentError) Unwrap() error { return e.err }

func isSubmittedDeploymentError(err error) bool {
	var submitted *submittedDeploymentError
	return errors.As(err, &submitted)
}

func addReadinessFlags(cmd *cobra.Command, opts *runOptions) {
	cmd.Flags().BoolVar(&opts.waitReady, "wait-ready", false, "Require an agent-verified readiness probe before deployment succeeds")
	cmd.Flags().DurationVar(&opts.readinessTimeout, "readiness-timeout", 0, "Override the readiness deadline (whole seconds, 1s to 1h)")
}

func validateReadinessOptions(opts runOptions) error {
	if opts.readinessTimeout != 0 && (opts.readinessTimeout < time.Second || opts.readinessTimeout > time.Hour || opts.readinessTimeout%time.Second != 0) {
		return fmt.Errorf("--readiness-timeout must be a whole number of seconds between 1s and 1h")
	}
	if opts.deploy && (opts.waitReady || opts.readinessTimeout != 0) {
		return fmt.Errorf("--deploy only creates containers; omit --deploy to use readiness verification")
	}
	return nil
}

// Call before image builds or container mutation, including for every fleet
// target. Explicit verification is never silently downgraded on older agents.
func configureVerifiedDeployment(version *agentpb.GetAgentVersionResponse, cfg *appconfig.AppConfig, opts *runOptions) error {
	opts.verifiedDeployment = false
	if err := validateReadinessOptions(*opts); err != nil {
		return err
	}
	if opts.deploy {
		return nil
	}
	probe := deploymentReadiness(cfg, *opts)
	if opts.waitReady && !probe.HasProbe() {
		return fmt.Errorf("--wait-ready requires a readiness probe for %s; configure readiness or an HTTP entitlement in wendy.json", cfg.ContainerName())
	}
	supported := agentVersionHasFeature(version, "verified-deployment")
	shared := cfg != nil && appconfig.IsSharedNamespaceIsolation(cfg.Isolation)
	if !supported || shared {
		reason := "the device agent does not support verified deployment; update WendyOS"
		if shared {
			reason = "verified deployment does not support shared-namespace service groups; use isolated services"
		}
		if opts.waitReady || opts.readinessTimeout != 0 {
			return fmt.Errorf("%s", reason)
		}
		if probe.HasProbe() || shared {
			cliNotice("%s; using legacy startup without verified readiness or rollback.", reason)
		}
		return nil
	}
	opts.verifiedDeployment = true
	return nil
}

func configureVerifiedServices(version *agentpb.GetAgentVersionResponse, cfgs map[string]*appconfig.AppConfig, opts *runOptions) error {
	opts.serviceDeployment = true
	names := make([]string, 0, len(cfgs))
	for name := range cfgs {
		names = append(names, name)
	}
	sort.Strings(names)
	allVerified := len(names) > 0
	for _, name := range names {
		serviceOpts := *opts
		if err := configureVerifiedDeployment(version, cfgs[name], &serviceOpts); err != nil {
			return fmt.Errorf("service %s: %w", name, err)
		}
		allVerified = allVerified && serviceOpts.verifiedDeployment
	}
	opts.verifiedDeployment = allVerified
	return nil
}

func layerRequestFromCreate(req *agentpb.CreateContainerRequest) *agentpb.RunContainerLayersRequest {
	return &agentpb.RunContainerLayersRequest{
		ImageName: req.GetImageName(), AppName: req.GetAppName(), Cmd: req.GetCmd(),
		AppConfig: req.GetAppConfig(), WorkingDir: req.GetWorkingDir(),
		RestartPolicy: req.RestartPolicy, UserArgs: req.GetUserArgs(), Env: req.GetEnv(),
	}
}

func deploymentOutcomeError(result *agentpb.DeploymentResult, requireReady bool) error {
	if result == nil {
		return fmt.Errorf("agent returned no deployment outcome")
	}
	switch result.GetState() {
	case agentpb.DeploymentState_READY:
		return nil
	case agentpb.DeploymentState_RUNNING:
		if requireReady {
			return fmt.Errorf("agent started the application without verifying its required readiness")
		}
		return nil
	default:
		return fmt.Errorf("deployment %s: %s", result.GetState(), result.GetMessage())
	}
}

// verifiedRequest copies req with verified set and app_config's readiness
// replaced by the exact probe the agent must run. The agent never infers one.
func verifiedRequest(cfg *appconfig.AppConfig, req *agentpb.RunContainerLayersRequest, opts runOptions) (*agentpb.RunContainerLayersRequest, error) {
	var sent appconfig.AppConfig
	if len(req.GetAppConfig()) > 0 {
		if err := json.Unmarshal(req.GetAppConfig(), &sent); err != nil {
			return nil, fmt.Errorf("decoding app config: %w", err)
		}
	}
	sent.Readiness = nil
	if probe := deploymentReadiness(cfg, opts); probe.HasProbe() {
		resolved := *probe
		if opts.readinessTimeout > 0 {
			resolved.TimeoutSeconds = int(opts.readinessTimeout / time.Second)
		}
		sent.Readiness = &resolved
	}
	data, err := json.Marshal(&sent)
	if err != nil {
		return nil, fmt.Errorf("encoding app config: %w", err)
	}
	out := proto.Clone(req).(*agentpb.RunContainerLayersRequest)
	out.AppConfig = data
	out.Verified = true
	return out, nil
}

func openVerifiedDeployment(ctx context.Context, conn *grpcclient.AgentConnection, cfg *appconfig.AppConfig, req *agentpb.RunContainerLayersRequest, opts runOptions) (containerOutputStream, error) {
	request, err := verifiedRequest(cfg, req, opts)
	if err != nil {
		return nil, err
	}
	rpcCtx := contextWithPostStartAgentHook(ctx, cfg)
	if !opts.detach && !opts.isWatch() && !opts.serviceDeployment {
		stream, err := conn.ContainerService.AttachContainer(rpcCtx)
		if err == nil {
			err = stream.Send(&agentpb.AttachContainerRequest{RequestType: &agentpb.AttachContainerRequest_Deploy{Deploy: request}})
		}
		if err != nil {
			return nil, &submittedDeploymentError{fmt.Errorf("submitting attached deployment: %w", err)}
		}
		go func() {
			defer stream.CloseSend()
			buf := make([]byte, 4096)
			for {
				n, readErr := os.Stdin.Read(buf)
				if n > 0 {
					if err := stream.Send(&agentpb.AttachContainerRequest{RequestType: &agentpb.AttachContainerRequest_StdinData{StdinData: buf[:n]}}); err != nil {
						return
					}
				}
				if readErr != nil {
					return
				}
			}
		}()
		return stream, nil
	}
	stream, err := conn.ContainerService.RunContainer(rpcCtx, request)
	if err != nil {
		return nil, &submittedDeploymentError{fmt.Errorf("submitting deployment: %w", err)}
	}
	return stream, nil
}

// prefetchedContainerStream returns an outcome already consumed while ordering
// a service group, then resumes the same RPC's output. It never starts again.
type prefetchedContainerStream struct {
	first *agentpb.RunContainerLayersResponse
	rest  containerOutputStream
}

func (s *prefetchedContainerStream) Recv() (*agentpb.RunContainerLayersResponse, error) {
	if s.first != nil {
		first := s.first
		s.first = nil
		return first, nil
	}
	return s.rest.Recv()
}

func awaitDeploymentOutcome(appName string, stream containerOutputStream, requireReady bool, opts runOptions) (*agentpb.RunContainerLayersResponse, error) {
	for {
		resp, err := stream.Recv()
		if err != nil {
			if err == io.EOF {
				err = fmt.Errorf("deployment stream ended without an outcome")
			}
			return nil, &submittedDeploymentError{err}
		}
		if result := resp.GetDeployment(); result != nil {
			if err := deploymentOutcomeError(result, requireReady); err != nil {
				emitDeploymentOutcome(appName, result, opts)
				return nil, &submittedDeploymentError{err}
			}
			return resp, nil
		}
		writeDeploymentOutput(resp, opts)
	}
}

func writeDeploymentOutput(resp *agentpb.RunContainerLayersResponse, opts runOptions) {
	if out := resp.GetStdoutOutput(); out != nil {
		if opts.deploymentStdout != nil {
			opts.deploymentStdout.Write(out.GetData())
		} else {
			outWriter := os.Stdout
			if jsonOutput {
				outWriter = os.Stderr
			}
			_, _ = outWriter.Write(out.GetData())
		}
	}
	if out := resp.GetStderrOutput(); out != nil {
		if opts.deploymentStderr != nil {
			opts.deploymentStderr.Write(out.GetData())
		} else {
			_, _ = os.Stderr.Write(out.GetData())
		}
	}
}

// Each service commits independently; failure stops the sequence and leaves
// already successful services running. No group-wide rollback is claimed.
func runVerifiedServiceGroup(ctx context.Context, conn *grpcclient.AgentConnection, ordered []string, cfgs, lifecycleCfgs map[string]*appconfig.AppConfig, requests map[string]*agentpb.CreateContainerRequest, appLevelCfg *appconfig.AppConfig, opts runOptions) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stdoutWriters, stderrWriters := newServiceLogWriters(ordered)
	for _, name := range ordered {
		if jsonOutput {
			stdoutWriters[name].dest = os.Stderr
		}
		defer stdoutWriters[name].Flush()
		defer stderrWriters[name].Flush()
	}
	optionsForService := func(name string) runOptions {
		serviceOpts := opts
		serviceOpts.verifiedLifecycleConfig = lifecycleCfgs[name]
		serviceOpts.deploymentStdout = stdoutWriters[name]
		serviceOpts.deploymentStderr = stderrWriters[name]
		return serviceOpts
	}
	streams := make(map[string]containerOutputStream, len(ordered))
	for _, name := range ordered {
		cfg := cfgs[name]
		stream, err := openVerifiedDeployment(runCtx, conn, cfg, layerRequestFromCreate(requests[name]), opts)
		if err != nil {
			return fmt.Errorf("service %s: %w", name, err)
		}
		outcome, err := awaitDeploymentOutcome(cfg.ContainerName(), stream, deploymentReadiness(cfg, opts).HasProbe() || opts.waitReady, optionsForService(name))
		if err != nil {
			return fmt.Errorf("service %s: %w", name, err)
		}
		emitDeploymentOutcome(cfg.ContainerName(), outcome.GetDeployment(), opts)
		streams[name] = &prefetchedContainerStream{first: outcome, rest: stream}
	}
	opts.deploymentOutcomeReported = true
	var appRunner *serviceHookRunner
	if !opts.detach && appLevelCfg != nil {
		hostOpts := opts
		hostOpts.verifiedDeployment = false
		appRunner = &serviceHookRunner{conn: conn, opts: hostOpts}
		appRunner.startAsync(runCtx, appLevelCfg)
		defer func() { cancel(); appRunner.reap() }()
	}
	if opts.detach || opts.isWatch() {
		for _, name := range ordered {
			serviceOpts := optionsForService(name)
			if err := streamRunContainer(runCtx, conn, streams[name], cfgs[name], serviceOpts); err != nil {
				return err
			}
		}
		if appRunner != nil {
			appRunner.reap()
		}
		return nil
	}
	defer func() {
		if ctx.Err() == nil {
			return
		}
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer stopCancel()
		for i := len(ordered) - 1; i >= 0; i-- {
			_, _ = conn.ContainerService.StopContainer(stopCtx, &agentpb.StopContainerRequest{AppName: cfgs[ordered[i]].ContainerName()})
		}
	}()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt)
	defer signal.Stop(signals)
	go func() {
		select {
		case <-runCtx.Done():
			return
		case <-signals:
		}
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer stopCancel()
		for i := len(ordered) - 1; i >= 0; i-- {
			_, _ = conn.ContainerService.StopContainer(stopCtx, &agentpb.StopContainerRequest{AppName: cfgs[ordered[i]].ContainerName()})
		}
		cancel()
	}()
	var wg sync.WaitGroup
	errs := make(chan error, len(ordered))
	for _, name := range ordered {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			serviceOpts := optionsForService(name)
			if err := streamRunContainer(runCtx, conn, streams[name], cfgs[name], serviceOpts); err != nil {
				errs <- err
				cancel()
			}
		}(name)
	}
	wg.Wait()
	close(errs)
	var failures []error
	for err := range errs {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

// Service configs keep inherited entitlements for runtime access, but probe only
// service-owned readiness. App-level HTTP cannot make a database listen on it.
func verifiedServiceConfig(full, lifecycle *appconfig.AppConfig) *appconfig.AppConfig {
	cfg := *full
	cfg.Readiness = appconfig.EffectiveReadiness(lifecycle)
	return &cfg
}

func deploymentReadiness(cfg *appconfig.AppConfig, opts runOptions) *appconfig.ReadinessConfig {
	if opts.serviceDeployment {
		return cfg.Readiness
	}
	return appconfig.EffectiveReadiness(cfg)
}

var deploymentOutputMu sync.Mutex

// JSON output is one object per service outcome (JSON Lines for groups). App
// output stays on stderr so callers can parse deployment results from stdout.
func emitDeploymentOutcome(appName string, result *agentpb.DeploymentResult, opts runOptions) {
	if result == nil {
		return
	}
	deploymentOutputMu.Lock()
	defer deploymentOutputMu.Unlock()
	if jsonOutput && !opts.suppressDeploymentJSON {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
			"app_name": appName, "state": result.GetState().String(), "message": result.GetMessage(),
		})
	} else {
		cliLogln("Deployment %s: %s. %s", appName, result.GetState(), result.GetMessage())
	}
}
