package services

import (
	"context"
	"os"
	"sync"
	"time"

	"go.uber.org/zap"
)

// Restart budget. A client that sent an update waits for the agent to answer
// again. The CLI gives it 20 s on a LAN, counted from one second after the
// reply (defaultAgentRestartTimeout in cli/commands/device.go), and 60 s
// through Cloud. Into that go the two steps below, the 2 s systemd waits
// before it starts the agent again (RestartSec) and the new agent's start-up.
// The steps take 5.5 s at most, which leaves the new agent 13.5 s on a LAN.
const (
	// agentRestartAckDelay is how long the reply to the update gets to reach
	// the client before anything is stopped.
	agentRestartAckDelay = 500 * time.Millisecond
	// agentRestartStopTimeout bounds AgentRestart.Stop.
	agentRestartStopTimeout = 5 * time.Second
)

// AgentRestart restarts the agent after it committed something only a new
// process picks up: an updated binary, a renewed certificate. The agent exits
// and whatever runs it (systemd, with Restart=always) starts it again.
//
// That exit skips the shutdown at the end of main, so Stop is for state the
// next agent must not inherit. Apps keep running across an agent restart; Stop
// must leave them alone.
type AgentRestart struct {
	Logger *zap.Logger
	// Stop runs after the reply delay and before the exit; nil skips the step.
	// It gets StopTimeout. The agent exits when that is over even if Stop has
	// not returned: a stuck Stop must not keep the old binary running.
	Stop func(context.Context) error

	// Zero means the package default; tests shrink them.
	AckDelay    time.Duration
	StopTimeout time.Duration
	// Exit ends the process; nil means os.Exit.
	Exit func(code int)

	// sleep waits out AckDelay; nil means time.Sleep.
	sleep func(time.Duration)
	once  sync.Once
}

// Schedule starts the restart and returns at once, so the caller can still
// send its reply. A second call joins the restart already under way.
func (r *AgentRestart) Schedule() {
	r.once.Do(func() { go r.run() })
}

func (r *AgentRestart) run() {
	sleep := r.sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	sleep(durationOr(r.AckDelay, agentRestartAckDelay))
	if r.Stop != nil {
		r.stop()
	}
	exit := r.Exit
	if exit == nil {
		exit = os.Exit
	}
	// Always 0, whatever became of Stop: the restart was asked for, and
	// whatever runs the agent would take a failed exit for a crash.
	exit(0)
}

// stop runs Stop for at most StopTimeout.
func (r *AgentRestart) stop() {
	logger := r.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	timeout := durationOr(r.StopTimeout, agentRestartStopTimeout)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Stop(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			logger.Warn("Stopping before the agent restart failed; restarting anyway", zap.Error(err))
		}
	case <-ctx.Done():
		logger.Warn("Stopping before the agent restart did not finish; restarting anyway", zap.Duration("waited", timeout))
	}
}

func durationOr(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// scheduleAgentRestartExit is the restart of a service that was given no
// other: the reply delay and the exit, with nothing stopped in between.
func scheduleAgentRestartExit() {
	(&AgentRestart{}).Schedule()
}

// SetRestartFunc replaces what the service calls once an update is committed.
// Call it before the service is served.
func (s *AgentService) SetRestartFunc(restart func()) { s.restartFn = restart }

// SetRestartFunc replaces what the service calls once an update is committed.
// Call it before the service is served.
func (s *AgentUpdateService) SetRestartFunc(restart func()) { s.restartFn = restart }
