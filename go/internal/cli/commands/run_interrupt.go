package commands

import (
	"context"
	"sync"
)

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
