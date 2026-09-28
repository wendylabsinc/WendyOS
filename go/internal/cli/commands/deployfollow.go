package commands

import (
	"context"
	"errors"
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
// base is the followed app's baseline (observedAppBaseline of the lookup that
// chose to follow it), against which a stop is told from a replacement.
func followExistingContainer(ctx context.Context, conn *grpcclient.AgentConnection, appCfg *appconfig.AppConfig, opts runOptions, base appBaseline) error {
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
	runner := &serviceHookRunner{conn: conn, opts: hookRunnerOptions(opts)}
	var gate *readinessGate
	defer func() {
		cancel()
		gate.wait()
		runner.reap()
		if logDone != nil {
			<-logDone
		}
	}()
	if opts.waitReady {
		// Following never started the app, so a failed --wait-ready fails the
		// run without stopping it.
		gate = startReadinessGate(runCtx, conn, appCfg, opts, gateTarget{base: base}, func() { runner.startAsync(runCtx, appCfg) })
	} else {
		runner.startAsync(runCtx, appCfg)
	}

	// Following never started the app, so an interrupt leaves it running —
	// unchanged Ctrl-C behavior; SIGTERM's error says so. Report
	// ErrUserCancelled so a SIGINT still exits 0; runWithInterruptChannel
	// turns it into errTerminated on SIGTERM.
	interrupted := func() error {
		noteInterruptedApp(ctx, appCfg.ContainerName(), interruptedAppLeftRunning, nil)
		return ErrUserCancelled
	}
	// notReady ends the run on a --wait-ready failure the gate saw first. The
	// app was not stopped; a readiness timeout last saw it running (a crash
	// already says what happened to it).
	notReady := func(err error) error {
		if errors.Is(err, errReadinessTimeout) {
			cliLogln("Not stopping %s: this run did not start it.", containerDisplayName(appCfg))
			noteInterruptedApp(ctx, appCfg.ContainerName(), interruptedAppLeftRunning, nil)
		}
		return err
	}
	gateDone := gate.finished()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		var logErr error
		select {
		case <-ctx.Done():
			return interrupted()
		case <-gateDone:
			// --wait-ready decided: a failure ends the run now, since nothing
			// stops the app; a pass keeps following.
			gateDone = nil
			if ctx.Err() != nil {
				return interrupted()
			}
			if gate.Replaced() {
				noteAppReplaced(appCfg)
				return nil
			}
			if err := gate.Err(); err != nil {
				return notReady(err)
			}
			continue
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
			// The app stopped on its own. Let the gate finish first, so its
			// next poll records whether the app became ready before exiting.
			gate.wait()
			if ctx.Err() != nil {
				return interrupted() // Ctrl-C or SIGTERM during that wait
			}
			if gate.Replaced() {
				noteAppReplaced(appCfg)
				return nil
			}
			if err := gate.Err(); err != nil {
				return err
			}
			replaced, err := appReplacedAfterKill(ctx, conn, appCfg, base, container)
			if err != nil {
				return interrupted() // Ctrl-C or SIGTERM while confirming a SIGKILL
			}
			if replaced {
				noteAppReplaced(appCfg)
				return nil
			}
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
