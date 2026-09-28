package commands

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// followExistingContainer observes a running app without calling StartContainer
// or AttachContainer: both RPCs replace the agent's existing task. Telemetry
// carries stdout/stderr as well as native logs; state polling ends the foreground
// session when the app stops. Watch sessions already own their log subscription.
func followExistingContainer(ctx context.Context, conn *grpcclient.AgentConnection, appCfg *appconfig.AppConfig, opts runOptions) error {
	if conn.TelemetryService == nil {
		return fmt.Errorf("cannot follow existing app: device telemetry is unavailable")
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	lastN := int32(0)
	stream, err := conn.TelemetryService.StreamLogs(runCtx, &agentpb.StreamLogsRequest{
		AppName: &appCfg.AppID,
		LastN:   &lastN,
	})
	if err != nil {
		return fmt.Errorf("following existing app logs: %w", err)
	}
	logDone := make(chan error, 1)
	go func() {
		for {
			resp, err := stream.Recv()
			if err != nil {
				logDone <- err
				return
			}
			for _, rl := range watchLiveLogs(resp).GetResourceLogs() {
				service := resourceServiceName(rl.GetResource())
				for _, sl := range rl.GetScopeLogs() {
					for _, record := range sl.GetLogRecords() {
						printLogRecord(service, record)
					}
				}
			}
		}
	}()
	runner := &serviceHookRunner{conn: conn, opts: opts}
	defer func() {
		cancel()
		runner.reap()
		if logDone != nil {
			<-logDone
		}
	}()
	runner.startAsync(runCtx, appCfg)

	// Following never started the app, so an interrupt leaves it running —
	// unchanged Ctrl-C behavior; SIGTERM's error says so. Report
	// ErrUserCancelled so a SIGINT still exits 0; runWithInterruptChannel
	// turns it into errTerminated on SIGTERM.
	interrupted := func() error {
		noteInterruptedApp(ctx, appCfg.ContainerName(), interruptedAppLeftRunning, nil)
		return ErrUserCancelled
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		var logErr error
		select {
		case <-ctx.Done():
			return interrupted()
		case logErr = <-logDone:
			logDone = nil
			if ctx.Err() != nil {
				return interrupted()
			}
		case <-ticker.C:
		}
		probeCtx, probeCancel := context.WithTimeout(runCtx, 5*time.Second)
		container, err := lookupAppContainer(probeCtx, conn, appCfg.AppID)
		probeCancel()
		if ctx.Err() != nil {
			return interrupted()
		}
		if err != nil {
			return fmt.Errorf("checking existing app state: %w", err)
		}
		if container == nil || container.GetRunningState() != agentpb.AppRunningState_RUNNING {
			if failure := appExitFailure(appCfg.AppID, container); failure != nil {
				return failure
			}
			cliLogln("\nApplication %s stopped.", containerDisplayName(appCfg))
			return nil
		}
		if logErr != nil {
			if logErr == io.EOF {
				return fmt.Errorf("log stream ended while the existing app is still running")
			}
			return fmt.Errorf("following existing app logs: %w", logErr)
		}
	}
}
