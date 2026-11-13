package main

import (
	"context"
	"os/exec"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	agentcontainerd "github.com/wendylabsinc/wendy/go/internal/agent/containerd"
)

// Shutdown budget. systemd SIGKILLs a unit that is still running 90 s
// (DefaultTimeoutStopSec) after SIGTERM, and while the host powers off that
// whole wait is added to the shutdown. Every step below is bounded so the
// agent exits long before that.
const (
	// hostStateTimeout bounds asking systemd whether the host is going down.
	hostStateTimeout = 3 * time.Second
	// grpcDrainTimeout is how long in-flight RPCs get to finish before the
	// servers cancel them.
	grpcDrainTimeout = 5 * time.Second
	// appStopTimeout bounds one sweep of stopping apps when the host shuts
	// down: their SIGTERM grace, the wait after SIGKILL, and deleting their
	// tasks.
	appStopTimeout = agentcontainerd.HostShutdownTimeout
	// backgroundStopTimeout is how long background loops get to notice the
	// cancellation and return.
	backgroundStopTimeout = 5 * time.Second
	// shutdownDeadline is the backstop for everything else, including the
	// deferred cleanup in main: whatever is still running then, the agent
	// exits anyway.
	shutdownDeadline = 45 * time.Second
)

// grpcStopper is the part of *grpc.Server that shutdown uses.
type grpcStopper interface {
	GracefulStop()
	Stop()
}

type namedGRPCServer struct {
	name   string
	server grpcStopper
}

// agentShutdown is what the agent does between SIGTERM and returning from
// main. Every field but logger and cancel is optional.
type agentShutdown struct {
	logger *zap.Logger

	// hostShuttingDown reports whether the whole host is going down, as
	// opposed to only the agent restarting.
	hostShuttingDown func(context.Context) bool
	// cancel stops the restart monitor and the background loops.
	cancel context.CancelFunc
	// stopApps stops every running app and returns how many it stopped; nil
	// when containerd is unavailable. run calls it a second time for apps
	// started while the first call ran.
	stopApps func(context.Context) (int, error)
	servers  []namedGRPCServer
	// shutdownHTTP stops the OTLP/HTTP receiver.
	shutdownHTTP func(context.Context) error
	// background holds the agent's long-running goroutines.
	background *sync.WaitGroup

	// Zero means the package default; tests shrink them.
	hostStateTimeout      time.Duration
	grpcDrainTimeout      time.Duration
	appStopTimeout        time.Duration
	backgroundStopTimeout time.Duration
}

// run stops the agent, bounding every step.
//
// Apps are stopped only when the host itself is shutting down. They outlive an
// agent restart by design (an update, `systemctl restart`, a crash), and the
// next agent adopts them. On host shutdown, though, stopping them here is what
// lets the host power off promptly; see StopTasksForHostShutdown.
func (s *agentShutdown) run() {
	start := time.Now()

	hostDown := false
	if s.hostShuttingDown != nil {
		ctx, cancel := context.WithTimeout(context.Background(), orDefault(s.hostStateTimeout, hostStateTimeout))
		hostDown = s.hostShuttingDown(ctx)
		cancel()
	}

	// First, so the restart monitor cannot start an app again while it is
	// being stopped below.
	s.cancel()

	stoppingApps := hostDown && s.stopApps != nil
	var steps sync.WaitGroup
	if stoppingApps {
		s.logger.Info("Host is shutting down; stopping apps")
		steps.Add(1)
		go func() {
			defer steps.Done()
			s.stopAppsForHostShutdown("running")
		}()
	}
	drain := orDefault(s.grpcDrainTimeout, grpcDrainTimeout)
	for _, srv := range s.servers {
		steps.Add(1)
		go func() {
			defer steps.Done()
			if !stopGRPCServer(srv.server, drain) {
				s.logger.Info("RPCs still open after the drain period were cancelled",
					zap.String("server", srv.name), zap.Duration("drain", drain))
			}
		}()
	}
	if s.shutdownHTTP != nil {
		steps.Add(1)
		go func() {
			defer steps.Done()
			ctx, cancel := context.WithTimeout(context.Background(), drain)
			defer cancel()
			if err := s.shutdownHTTP(ctx); err != nil {
				s.logger.Error("OTEL HTTP server shutdown error", zap.Error(err))
			}
		}()
	}
	steps.Wait()

	// The servers have stopped now, but a start that was already past its
	// checks when the apps were listed, from an RPC that was still draining or
	// a restart under way, can have left a task the sweep above never saw.
	// Usually there is none and this is one listing. Should it find an app and
	// the first sweep was slow too, the watchdog still bounds the total.
	if stoppingApps {
		s.stopAppsForHostShutdown("started during shutdown")
	}

	if s.background != nil {
		limit := orDefault(s.backgroundStopTimeout, backgroundStopTimeout)
		if !waitTimeout(s.background, limit) {
			s.logger.Warn("Background tasks did not stop in time; exiting without them", zap.Duration("waited", limit))
		}
	}
	s.logger.Info("Agent shutdown finished",
		zap.Bool("host_shutdown", hostDown), zap.Duration("took", time.Since(start)))
}

// stopAppsForHostShutdown runs one sweep of stopApps; which names it in the log.
func (s *agentShutdown) stopAppsForHostShutdown(which string) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), orDefault(s.appStopTimeout, appStopTimeout))
	defer cancel()
	n, err := s.stopApps(ctx)
	if err != nil {
		s.logger.Warn("Some apps did not stop cleanly for host shutdown",
			zap.String("apps", which), zap.Int("tasks", n), zap.Error(err))
		return
	}
	s.logger.Info("Stopped apps for host shutdown",
		zap.String("apps", which), zap.Int("tasks", n), zap.Duration("took", time.Since(start)))
}

// stopGRPCServer lets in-flight RPCs finish for up to timeout, then cancels
// the ones still open. It reports whether they all finished on their own.
//
// GracefulStop alone waits for every open stream to end, and a followed log or
// a watch only ends when its client goes away. With any client connected it
// never returned, and systemd killed the agent 90 s later.
func stopGRPCServer(srv grpcStopper, timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		srv.GracefulStop()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		// Closes every connection and cancels the RPCs on them. Not waiting
		// for GracefulStop after this: it also waits for each handler to
		// return, and a handler that ignores its context would hold it forever.
		srv.Stop()
		return false
	}
}

// waitTimeout waits for wg for up to timeout and reports whether it finished.
func waitTimeout(wg *sync.WaitGroup, timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

func orDefault(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// armShutdownWatchdog makes sure the agent exits within deadline of being
// asked to, whatever is still running. It returns the func that disarms it.
//
// Every step of the shutdown is bounded on its own, but the deferred cleanup in
// main is not, and neither is anything a later change adds. Without the
// backstop a single stuck step holds the host's power-off for systemd's full
// stop timeout again.
func armShutdownWatchdog(logger *zap.Logger, deadline time.Duration, exit func(int)) func() {
	t := time.AfterFunc(deadline, func() {
		logger.Error("Shutdown did not finish in time; exiting anyway", zap.Duration("deadline", deadline))
		_ = logger.Sync()
		exit(1)
	})
	return func() { t.Stop() }
}

// systemctlOutput runs systemctl and returns what it printed. A non-zero exit
// is normal for the queries below, so the output is returned with the error.
func systemctlOutput(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "systemctl", args...)
	// Bound output-pipe cleanup too, in case a child outlives systemctl.
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	return string(out), err
}

// hostShuttingDown reports whether the host is powering off or rebooting,
// according to systemd. It is false where systemd is not running the show (a
// development host, a container) and whenever systemd cannot be asked, which
// keeps the agent-restart behaviour: apps keep running.
func hostShuttingDown(ctx context.Context, systemctl func(ctx context.Context, args ...string) (string, error)) bool {
	// "stopping" once shutdown.target is queued or active. systemctl exits
	// non-zero for every state but "running", so the output is the answer.
	state, _ := systemctl(ctx, "is-system-running")
	if strings.TrimSpace(state) == "stopping" {
		return true
	}
	// Older systemd reports a host whose boot never finished as "starting",
	// even while it shuts down. The queued start job for shutdown.target is
	// there either way: it waits for the agent to stop.
	jobs, err := systemctl(ctx, "list-jobs", "--no-legend", "shutdown.target")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(jobs, "\n") {
		// JOB UNIT TYPE STATE
		f := strings.Fields(line)
		if len(f) >= 3 && f[1] == "shutdown.target" && f[2] == "start" {
			return true
		}
	}
	return false
}
