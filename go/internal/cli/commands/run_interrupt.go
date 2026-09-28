package commands

import (
	"context"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// attachedStopTimeout bounds the StopContainer call an interrupted attached run
// makes. The agent gives the app 10 s to exit on SIGTERM before killing it, so
// 30 s leaves room for that and a slow link — the same bound multi-service runs
// use for their Ctrl-C stop (startAndStreamServices).
const attachedStopTimeout = 30 * time.Second

// interruptedAppOutcome is what an attached or waiting run did with its app
// when a signal ended the run.
type interruptedAppOutcome int

const (
	interruptedAppUnknown interruptedAppOutcome = iota
	interruptedAppStopped
	interruptedAppLeftRunning
	interruptedAppStopFailed
)

// runInterruptNotes lets a run path tell runWithInterruptChannel what happened
// to the app when a signal ended the run, so the SIGTERM error can say whether
// the app was stopped. It travels in the run context because the signal
// wrapper sits above every run path and cannot see the app otherwise.
type runInterruptNotes struct {
	mu      sync.Mutex
	app     string
	outcome interruptedAppOutcome
	stopErr error
}

type runInterruptNotesKey struct{}

// noteInterruptedApp records app's fate for runWithInterruptChannel. It is a
// no-op when ctx does not come from runWithInterruptChannel (tests, callers
// outside `wendy run`).
func noteInterruptedApp(ctx context.Context, app string, outcome interruptedAppOutcome, stopErr error) {
	notes, _ := ctx.Value(runInterruptNotesKey{}).(*runInterruptNotes)
	if notes == nil {
		return
	}
	notes.mu.Lock()
	defer notes.mu.Unlock()
	notes.app, notes.outcome, notes.stopErr = app, outcome, stopErr
}

// terminatedError is the error a SIGTERM'd run returns. It is classified
// errTerminated and deliberately wraps no context error: main's errorClass
// checks context.Canceled first and would otherwise report the run as an
// ordinary cancellation.
func (n *runInterruptNotes) terminatedError() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	switch n.outcome {
	case interruptedAppStopped:
		return commandErrorf(errTerminated, "wendy run was terminated; app %s was stopped", n.app)
	case interruptedAppLeftRunning:
		return commandErrorf(errTerminated, "wendy run was terminated; app %s is still running on the device", n.app)
	case interruptedAppStopFailed:
		return commandErrorf(errTerminated, "wendy run was terminated; stopping app %s failed: %v", n.app, n.stopErr)
	default:
		return commandErrorf(errTerminated, "wendy run was terminated")
	}
}

// stopInterruptedApp stops appCfg's container after Ctrl-C or SIGTERM ended an
// attached run, so an interrupted attached run never leaves its app running.
// ctx is already cancelled, so the RPC runs on a fresh context; ctx only
// carries the outcome back to runWithInterruptChannel.
func stopInterruptedApp(ctx context.Context, conn *grpcclient.AgentConnection, appCfg *appconfig.AppConfig) {
	name := appCfg.ContainerName()
	if conn == nil || conn.ContainerService == nil {
		noteInterruptedApp(ctx, name, interruptedAppLeftRunning, nil)
		return
	}
	cliLogln("\nStopping container...")
	stopCtx, cancel := context.WithTimeout(context.Background(), attachedStopTimeout)
	defer cancel()
	if _, err := conn.ContainerService.StopContainer(stopCtx, &agentpb.StopContainerRequest{AppName: name}); err != nil {
		cliNotice("Could not stop %s: %v", containerDisplayName(appCfg), err)
		noteInterruptedApp(ctx, name, interruptedAppStopFailed, err)
		return
	}
	noteInterruptedApp(ctx, name, interruptedAppStopped, nil)
	cliLogln("\nApplication %s stopped.", containerDisplayName(appCfg))
}
