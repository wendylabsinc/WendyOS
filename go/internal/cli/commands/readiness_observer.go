package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// readinessState reads the relevant service, never the group's any-running
// aggregate. App-level probes require every service to remain running.
func readinessState(ctx context.Context, conn *grpcclient.AgentConnection, cfg *appconfig.AppConfig) (bool, error) {
	if conn == nil || conn.ContainerService == nil {
		return false, fmt.Errorf("container status unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	stream, err := conn.ContainerService.ListContainers(ctx, &agentpb.ListContainersRequest{})
	if err != nil {
		return false, fmt.Errorf("container status unavailable: %w", err)
	}
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			return false, fmt.Errorf("container status unavailable: %s was not reported", cfg.ContainerName())
		}
		if err != nil {
			return false, fmt.Errorf("container status unavailable: %w", err)
		}
		app := resp.GetContainer()
		if app == nil || !strings.EqualFold(app.GetAppName(), cfg.AppID) {
			continue
		}
		if len(app.GetServices()) == 0 {
			if cfg.ServiceName != "" {
				return false, fmt.Errorf("service status unavailable: %s was not reported", cfg.ContainerName())
			}
			return app.GetRunningState() == agentpb.AppRunningState_RUNNING, nil
		}
		for _, service := range app.GetServices() {
			if cfg.ServiceName != "" && service.GetName() != cfg.ServiceName {
				continue
			}
			if service.GetRunningState() != agentpb.AppRunningState_RUNNING {
				return false, nil
			}
			if cfg.ServiceName != "" {
				return true, nil
			}
		}
		if cfg.ServiceName != "" {
			return false, fmt.Errorf("service status unavailable: %s was not reported", cfg.ContainerName())
		}
		return true, nil
	}
}

// readinessObserveFactor bounds how long an attached run keeps observing a
// slow-starting app after its first readiness timeout: this many probe
// timeouts in total — 5 minutes at the default 30 s (the agent's
// restartBackoffCap), 30 minutes for an app that declares 180 s. Observation
// used to continue for as long as the app ran, polling forever for an app that
// never listens on its port (or a port this machine cannot reach).
const readinessObserveFactor = 10

// errReadinessObserveLimit is continueReadiness's result when its deadline
// fires before the probe passes.
var errReadinessObserveLimit = errors.New("readiness observation limit reached")

// continueReadiness is clock/probe/status injectable so slow-start and
// cancellation races can be tested without sleeping through real deadlines.
// deadline may be nil (never fires).
func continueReadiness(ctx context.Context, tick, deadline <-chan time.Time, probe func(context.Context) bool, running func(context.Context) (bool, error), unavailable func(error)) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return errReadinessObserveLimit
		case <-tick:
		}
		alive, err := running(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			unavailable(err)
			continue
		}
		if !alive {
			return fmt.Errorf("service stopped before readiness succeeded")
		}
		if probe(ctx) {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return nil
		}
	}
}

// waitForAttachedReadiness waits for an attached run's app to pass its
// readiness probe. timeoutOverride (--readiness-timeout) is the whole deadline
// when set; otherwise a probe that times out while the app is still running
// keeps being observed for up to readinessObserveFactor probe timeouts.
func waitForAttachedReadiness(ctx context.Context, conn *grpcclient.AgentConnection, cfg *appconfig.AppConfig, hostname string, timeoutOverride time.Duration) error {
	started := time.Now()
	readiness := effectiveReadiness(cfg)
	httpPort := cloudHTTPReadinessPort(conn, cfg, readiness, hostname)
	timeout := readinessProbeTimeout(readiness, timeoutOverride)
	err := waitForReadinessWithin(ctx, readiness, hostname, httpPort, timeout)
	if err == nil || ctx.Err() != nil || timeoutOverride > 0 {
		return err
	}
	running := func(ctx context.Context) (bool, error) { return readinessState(ctx, conn, cfg) }
	alive, stateErr := running(ctx)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if stateErr != nil {
		return fmt.Errorf("%w; %v", err, stateErr)
	}
	if !alive {
		return fmt.Errorf("%w; %s stopped before readiness succeeded", err, cfg.ContainerName())
	}
	limit := time.Duration(readinessObserveFactor) * timeout
	cliLogln("Application %s is still starting after %s; continuing readiness checks every 5 seconds for up to %s.", cfg.ContainerName(), time.Since(started).Round(time.Second), limit)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	deadline := time.NewTimer(limit - time.Since(started))
	defer deadline.Stop()
	_, probeOnce, closeProbe := makeReadinessProbe(
		hostname,
		readiness.TCPSocket.Port,
		httpPort != 0 && httpPort == readiness.TCPSocket.Port,
	)
	defer closeProbe()
	probe := func(ctx context.Context) bool { return probeOnce(ctx) == nil }
	warned := false
	err = continueReadiness(ctx, ticker.C, deadline.C, probe, running, func(err error) {
		if !warned {
			cliLogln("Warning: %v; continuing to observe startup.", err)
			warned = true
		}
	})
	if errors.Is(err, errReadinessObserveLimit) {
		return commandErrorf(errReadinessTimeout, "%s did not become ready within %s", cfg.ContainerName(), limit)
	}
	if err == nil {
		cliLogln("Application %s ready after %s.", cfg.ContainerName(), time.Since(started).Round(time.Second))
	}
	return err
}
