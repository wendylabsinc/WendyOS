package commands

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

const (
	// waitReadyStabilityWindow is how long --wait-ready watches an app it
	// cannot probe from this machine (no readiness probe, or a cloud-tunnel
	// device whose LAN address does not answer from here) before calling it
	// running. 10 s is the agent restart monitor's restartBackoffBase and two
	// of its default 5 s ticks: an app that dies at startup is by then either
	// visibly not RUNNING (with the exit the agent recorded) or already
	// restarted, which raises its failure_count.
	waitReadyStabilityWindow = 10 * time.Second
	// waitReadyPollInterval paces --wait-ready's state polls and probe dials.
	waitReadyPollInterval = time.Second
	// waitReadyStateTimeout bounds each ListContainers poll, like readinessState.
	waitReadyStateTimeout = 3 * time.Second
	// waitReadyDialTimeout matches waitForReadiness's per-attempt dial timeout.
	waitReadyDialTimeout = 2 * time.Second
)

// --wait-ready outcome vocabulary: the "status" and "readiness" values of the
// JSON object a detached JSON-mode run prints.
const (
	waitReadyStatusReady    = "ready"     // probe passed (readiness "passed")
	waitReadyStatusRunning  = "running"   // stayed up for the window (readiness "not_checked")
	waitReadyStatusCrashed  = "crashed"   // exited (even with code 0) or restarted while waiting
	waitReadyStatusNotReady = "not_ready" // probe deadline passed, or state unreadable

	readinessPassed     = "passed"
	readinessNotChecked = "not_checked"
	readinessFailed     = "failed"
)

// waitReadyOutcome is --wait-ready's result. Detached runs print it as their
// single JSON object in JSON mode; err is what the run returns (nil for ready
// and running, errAppCrashed or errReadinessTimeout otherwise, the context's
// error when interrupted — Status is then empty).
type waitReadyOutcome struct {
	Status            string `json:"status"`
	App               string `json:"app"`
	Device            string `json:"device,omitempty"`
	Readiness         string `json:"readiness"`
	ExitCode          *int32 `json:"exit_code,omitempty"`
	TerminationReason string `json:"termination_reason,omitempty"`
	URL               string `json:"url,omitempty"`
	Message           string `json:"message,omitempty"`

	err error
}

// waitReadyChecks are awaitAppReady's inputs, injectable so the probe and
// stability logic runs against scripted state and channels in tests.
type waitReadyChecks struct {
	appID    string
	limit    time.Duration // probe deadline, or the stability window (for messages)
	tick     <-chan time.Time
	deadline <-chan time.Time
	probe    func(context.Context) bool // nil: readiness is not checked from this machine
	state    func(context.Context) (*agentpb.AppContainer, error)
}

// awaitAppReady waits for a started app. With a probe it succeeds once the
// probe passes and the app is still running; without one it succeeds when the
// deadline (the stability window) finds the app still running and not
// restarted. It fails as soon as a poll shows the app exited, crash-looping,
// gone, or restarted (failure_count above the first poll's). Transient
// ListContainers errors are tolerated until the deadline.
func awaitAppReady(ctx context.Context, c waitReadyChecks) waitReadyOutcome {
	checked := c.probe != nil
	onFailure := readinessNotChecked
	if checked {
		onFailure = readinessFailed
	}
	var baseline uint32
	if first, err := c.state(ctx); err == nil && first != nil {
		baseline = first.GetFailureCount()
	}
	for {
		select {
		case <-ctx.Done():
			return waitReadyOutcome{err: ctx.Err()}
		case <-c.deadline:
			if checked {
				return notReadyOutcome(readinessFailed, "app %s did not pass its readiness probe within %s", c.appID, c.limit)
			}
			container, err := c.state(ctx)
			if ctx.Err() != nil {
				return waitReadyOutcome{err: ctx.Err()}
			}
			if err != nil {
				return notReadyOutcome(readinessNotChecked, "could not confirm app %s is still running: %v", c.appID, err)
			}
			if out, crashed := crashedOutcome(c.appID, container, baseline, onFailure); crashed {
				return out
			}
			return waitReadyOutcome{Status: waitReadyStatusRunning, Readiness: readinessNotChecked}
		case <-c.tick:
		}
		container, err := c.state(ctx)
		if ctx.Err() != nil {
			return waitReadyOutcome{err: ctx.Err()}
		}
		if err == nil {
			if out, crashed := crashedOutcome(c.appID, container, baseline, onFailure); crashed {
				return out
			}
		}
		if checked && c.probe(ctx) {
			if ctx.Err() != nil {
				return waitReadyOutcome{err: ctx.Err()}
			}
			// The port answered; make sure it is still this run's process.
			if container, err := c.state(ctx); err == nil {
				if out, crashed := crashedOutcome(c.appID, container, baseline, onFailure); crashed {
					return out
				}
			}
			return waitReadyOutcome{Status: waitReadyStatusReady, Readiness: readinessPassed}
		}
	}
}

func notReadyOutcome(readiness, format string, args ...any) waitReadyOutcome {
	err := commandErrorf(errReadinessTimeout, format, args...)
	return waitReadyOutcome{Status: waitReadyStatusNotReady, Readiness: readiness, Message: err.Error(), err: err}
}

// crashedOutcome reports whether c shows appID failing while --wait-ready
// watched it, and the outcome if so. Unlike appExitFailure, any stop counts —
// a clean exit included — because the run promised a running app.
func crashedOutcome(appID string, c *agentpb.AppContainer, baseline uint32, readiness string) (waitReadyOutcome, bool) {
	var err error
	switch {
	case c == nil:
		err = commandErrorf(errAppCrashed, "app %s is no longer reported by the device", appID)
	case c.GetRunningState() != agentpb.AppRunningState_RUNNING && c.GetTerminationReason() == "exited":
		// "exited" is the agent's word for exit code 0.
		err = commandErrorf(errAppCrashed, "app %s exited before becoming ready (exit code 0); see its logs with `wendy device logs --app %s`", appID, appID)
	case c.GetRunningState() != agentpb.AppRunningState_RUNNING:
		err = appCrashedError(appID, c)
	case c.GetFailureCount() > baseline:
		err = commandErrorf(errAppCrashed, "app %s exited and was restarted %d time(s) by its restart policy while starting; see its logs with `wendy device logs --app %s`", appID, c.GetFailureCount()-baseline, appID)
	default:
		return waitReadyOutcome{}, false
	}
	out := waitReadyOutcome{Status: waitReadyStatusCrashed, Readiness: readiness, Message: err.Error(), err: err}
	if c != nil && c.GetTerminationReason() != "" {
		code := c.GetExitCode()
		out.ExitCode, out.TerminationReason = &code, c.GetTerminationReason()
	}
	return out, true
}

// waitForAppReady is --wait-ready for an app the agent just confirmed started.
// It probes the readiness port (explicit, or the http entitlement's) from this
// machine when it can reach the device, within readinessProbeTimeout. Without
// a probe, through the cloud tunnel when the device's LAN address does not
// answer from here (see hostSideAppAddress), or over the on-device agent
// socket, it instead requires the app to stay running for
// waitReadyStabilityWindow (shortened by a smaller --readiness-timeout),
// polled via ListContainers.
func waitForAppReady(ctx context.Context, conn *grpcclient.AgentConnection, appCfg *appconfig.AppConfig, opts runOptions) waitReadyOutcome {
	checks := waitReadyChecks{
		appID: appCfg.AppID,
		limit: waitReadyStabilityWindow,
		state: func(ctx context.Context) (*agentpb.AppContainer, error) {
			stateCtx, cancel := context.WithTimeout(ctx, waitReadyStateTimeout)
			defer cancel()
			return lookupAppContainer(stateCtx, conn, appCfg.AppID)
		},
	}
	if opts.readinessTimeout > 0 && opts.readinessTimeout < checks.limit {
		checks.limit = opts.readinessTimeout
	}
	readiness := effectiveReadiness(appCfg)
	hasProbe := readiness != nil && readiness.TCPSocket != nil && readiness.TCPSocket.Port > 0
	// On the device, WENDY_AGENT_SOCKET connects through grpcclient.ConnectUnix,
	// which sets Host to "unix:<socket path>" (the MCP server's
	// directCommandTarget checks it the same way). That is no network address:
	// there is nothing to probe the app at and no device name to report.
	agentSocket := strings.HasPrefix(conn.Host, "unix:")
	var host, url string
	hostOK := false
	if hasProbe && !agentSocket {
		host, url, hostOK = hostSideAppAddress(ctx, conn, appCfg)
	}
	switch {
	case hasProbe && hostOK:
		addr := net.JoinHostPort(host, strconv.Itoa(readiness.TCPSocket.Port))
		checks.limit = readinessProbeTimeout(readiness, opts.readinessTimeout)
		checks.probe = func(ctx context.Context) bool {
			dialer := net.Dialer{Timeout: waitReadyDialTimeout}
			connection, err := dialer.DialContext(ctx, "tcp", addr)
			if err != nil {
				return false
			}
			_ = connection.Close()
			return true
		}
		cliLogln("Waiting up to %s for %s to accept connections on %s...", checks.limit, containerDisplayName(appCfg), tui.Value(addr))
	case hasProbe && agentSocket:
		cliLogln("Not checking readiness from here: wendy reaches the device agent through its local socket (WENDY_AGENT_SOCKET) and has no address to probe the app at. Confirming %s stays running for %s instead...", containerDisplayName(appCfg), checks.limit)
	case hasProbe:
		cliLogln("Not checking readiness from this machine: the device is connected through Wendy Cloud and its LAN address is not reachable from here. Confirming %s stays running for %s instead...", containerDisplayName(appCfg), checks.limit)
	default:
		cliLogln("No readiness probe configured; confirming %s stays running for %s...", containerDisplayName(appCfg), checks.limit)
	}
	ticker := time.NewTicker(waitReadyPollInterval)
	defer ticker.Stop()
	deadline := time.NewTimer(checks.limit)
	defer deadline.Stop()
	checks.tick, checks.deadline = ticker.C, deadline.C

	out := awaitAppReady(ctx, checks)
	out.App = appCfg.AppID
	if !agentSocket {
		out.Device = conn.Host
	}
	if out.Status == waitReadyStatusReady {
		out.URL = url
	}
	return out
}

// reportWaitReadyOutcome prints a successful outcome's status line on stderr
// (failures are the run's returned error). A detached run also prints the app
// URL and, in JSON mode, the outcome as exactly one JSON object on stdout —
// failures included.
func reportWaitReadyOutcome(appCfg *appconfig.AppConfig, out waitReadyOutcome, detached bool) {
	switch out.Status {
	case waitReadyStatusReady:
		cliSuccess("Application %s is ready.", containerDisplayName(appCfg))
		if detached && out.URL != "" {
			cliLogln("App reachable at %s", tui.Value(out.URL))
		}
	case waitReadyStatusRunning:
		cliSuccess("Application %s is running (readiness not checked from this machine).", containerDisplayName(appCfg))
	}
	if detached && jsonOutput {
		_ = json.NewEncoder(os.Stdout).Encode(out)
	}
}

// waitReadyAfterDetachedStart runs --wait-ready for a detached run whose
// container the agent just confirmed started, reports the outcome, and returns
// the run's error. An interrupted wait prints no outcome and leaves the app
// running; runWithInterruptChannel turns Ctrl-C into exit 0 and SIGTERM into
// errTerminated.
func waitReadyAfterDetachedStart(ctx context.Context, conn *grpcclient.AgentConnection, appCfg *appconfig.AppConfig, opts runOptions) error {
	out := waitForAppReady(ctx, conn, appCfg, opts)
	if out.Status == "" {
		noteInterruptedApp(ctx, appCfg.ContainerName(), interruptedAppLeftRunning, nil)
		return out.err
	}
	reportWaitReadyOutcome(appCfg, out, true)
	return out.err
}

// readinessGate is --wait-ready for an attached run: the check runs beside the
// session while logs stream. A failure is recorded and, with stopOnFailure
// (the run started the app), the app stopped, which ends the agent's output
// stream so the attached loop returns Err(). A run that only follows an app it
// did not start leaves the app alone and watches finished() instead: a
// host-side failure can be the network's (a firewall or VPN), and a stop
// would keep the app down across reboots. A pass launches the host-side
// postStart work via onReady, whose hook runner skips its own probe
// (hostReadinessConfirmed). The nil gate (no --wait-ready) is valid: Err
// returns nil, wait returns at once, and finished never fires.
//
// When the app's output ends on its own (it exited), callers wait for the gate
// before reading Err and before cancelling its context: the gate's next poll
// records whether the app became ready before it exited — a clean exit inside
// the wait is a failure too. Interrupts and broken streams cancel it first; it
// then stands down without an outcome or a stop.
type readinessGate struct {
	mu   sync.Mutex
	err  error
	done chan struct{}
}

func startReadinessGate(ctx context.Context, conn *grpcclient.AgentConnection, appCfg *appconfig.AppConfig, opts runOptions, stopOnFailure bool, onReady func()) *readinessGate {
	g := &readinessGate{done: make(chan struct{})}
	go func() {
		defer close(g.done)
		out := waitForAppReady(ctx, conn, appCfg, opts)
		if ctx.Err() != nil {
			return // the session ended first; its own teardown decides the outcome
		}
		reportWaitReadyOutcome(appCfg, out, false)
		if out.err != nil {
			g.mu.Lock()
			g.err = out.err
			g.mu.Unlock()
			if stopOnFailure {
				stopUnreadyApp(conn, appCfg)
			}
			return
		}
		onReady()
	}()
	return g
}

// Err returns the gate's failure, or nil.
func (g *readinessGate) Err() error {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.err
}

// wait blocks until the gate's goroutine has finished, so a caller can reap
// the hook runner it may have started.
func (g *readinessGate) wait() {
	if g != nil {
		<-g.done
	}
}

// finished is closed once the gate's goroutine has finished; for the nil gate
// it is nil, which never fires in a select.
func (g *readinessGate) finished() <-chan struct{} {
	if g == nil {
		return nil
	}
	return g.done
}

// stopUnreadyApp stops an attached run's app that failed --wait-ready, which
// also ends the run's output stream.
func stopUnreadyApp(conn *grpcclient.AgentConnection, appCfg *appconfig.AppConfig) {
	cliLogln("Stopping %s: it did not become ready.", containerDisplayName(appCfg))
	stopCtx, cancel := context.WithTimeout(context.Background(), attachedStopTimeout)
	defer cancel()
	if _, err := conn.ContainerService.StopContainer(stopCtx, &agentpb.StopContainerRequest{AppName: appCfg.ContainerName()}); err != nil {
		cliNotice("Could not stop %s: %v", containerDisplayName(appCfg), err)
	}
}

// hookRunnerOptions returns the options an attached run's serviceHookRunner
// gets: with --wait-ready the gate has already probed readiness by the time
// the runner starts, so the runner must not probe a second time.
func hookRunnerOptions(opts runOptions) runOptions {
	if opts.waitReady {
		opts.hostReadinessConfirmed = true
	}
	return opts
}
