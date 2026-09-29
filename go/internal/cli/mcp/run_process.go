package mcp

import (
	"context"
	"os/exec"
	"time"
)

// runStopGrace is how long a cancelled run's process group gets after SIGINT
// before SIGTERM, and after SIGTERM before SIGKILL. On SIGINT the CLI cancels
// the build or stops the application it started, which takes a device round
// trip; SIGTERM is also handled gracefully by the CLI.
var runStopGrace = [...]time.Duration{10 * time.Second, 5 * time.Second}

// stopRunOnCancel escalates stop signals to the run's process group once ctx
// is done, as Ctrl-C then kill would in a terminal. The returned function stops
// the escalation and must be called after the CLI has exited.
//
// exec.CommandContext is not used: its WaitDelay would bound both this grace
// period and the pipe drain after a successful exit with one timer.
func stopRunOnCancel(ctx context.Context, cmd *exec.Cmd) func() {
	exited, finished := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		select {
		case <-exited:
			return
		case <-ctx.Done():
		}
		for stage := 0; ; stage++ {
			_ = signalRunProcess(cmd, stage)
			if stage == len(runStopGrace) {
				return
			}
			select {
			case <-exited:
				return
			case <-time.After(runStopGrace[stage]):
			}
		}
	}()
	return func() {
		close(exited)
		<-finished
	}
}
