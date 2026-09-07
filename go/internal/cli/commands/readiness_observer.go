package commands

import (
	"context"
	"fmt"
	"io"
	"net"
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

// continueReadiness is clock/probe/status injectable so slow-start and
// cancellation races can be tested without sleeping through real deadlines.
func continueReadiness(ctx context.Context, tick <-chan time.Time, probe func(context.Context) bool, running func(context.Context) (bool, error), unavailable func(error)) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
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

func waitForAttachedReadiness(ctx context.Context, conn *grpcclient.AgentConnection, cfg *appconfig.AppConfig, hostname string) error {
	started := time.Now()
	readiness := effectiveReadiness(cfg)
	err := waitForReadiness(ctx, readiness, hostname)
	if err == nil || ctx.Err() != nil {
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
	cliLogln("Application %s is still starting after %s; continuing readiness checks every 5 seconds.", cfg.ContainerName(), time.Since(started).Round(time.Second))
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	addr := net.JoinHostPort(hostname, fmt.Sprint(readiness.TCPSocket.Port))
	dialer := net.Dialer{Timeout: 2 * time.Second}
	probe := func(ctx context.Context) bool {
		connection, err := dialer.DialContext(ctx, "tcp", addr)
		if err != nil {
			return false
		}
		_ = connection.Close()
		return true
	}
	warned := false
	err = continueReadiness(ctx, ticker.C, probe, running, func(err error) {
		if !warned {
			cliLogln("Warning: %v; continuing to observe startup.", err)
			warned = true
		}
	})
	if err == nil {
		cliLogln("Application %s ready after %s.", cfg.ContainerName(), time.Since(started).Round(time.Second))
	}
	return err
}
