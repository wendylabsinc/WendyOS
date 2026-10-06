package commands

import (
	"context"
	"encoding/json"
	"errors"
	"os"
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
	// device with no active Wendy Mesh route whose LAN address does not answer
	// from here) before calling it running. 10 s is the agent restart
	// monitor's restartBackoffBase and two of its default 5 s ticks: an app
	// that dies at startup is by then either visibly not RUNNING (with the
	// exit the agent recorded) or already restarted, which raises its
	// failure_count.
	waitReadyStabilityWindow = 10 * time.Second
	// waitReadyStateTimeout bounds each ListContainers poll, like readinessState.
	waitReadyStateTimeout = 3 * time.Second
)

// waitReadyPollInterval paces --wait-ready's state polls and probe dials. A
// variable so tests can shorten it.
var waitReadyPollInterval = time.Second

// --wait-ready outcome vocabulary: the "status" and "readiness" values of the
// JSON object a detached JSON-mode run prints.
const (
	waitReadyStatusReady    = "ready"     // probe passed (readiness "passed")
	waitReadyStatusRunning  = "running"   // stayed up for the window (readiness "not_checked")
	waitReadyStatusCrashed  = "crashed"   // exited (even with code 0) or restarted while waiting
	waitReadyStatusNotReady = "not_ready" // probe deadline passed, or state unreadable
	waitReadyStatusFailed   = "failed"    // the run failed before the wait (readiness "not_checked")

	readinessPassed     = "passed"
	readinessNotChecked = "not_checked"
	readinessFailed     = "failed"
)

// waitReadyOutcome is --wait-ready's result. Detached runs print it as their
// single JSON object in JSON mode; err is what the run returns (nil for ready
// and running, errAppCrashed or errReadinessTimeout otherwise, the context's
// error when interrupted — Status is then empty). A run that fails before the
// wait prints a "failed" one instead (runReportingWaitReadyFailure), which
// omits the app and device when the run had not learned them yet.
type waitReadyOutcome struct {
	Status            string `json:"status"`
	App               string `json:"app,omitempty"`
	Device            string `json:"device,omitempty"`
	Readiness         string `json:"readiness"`
	ExitCode          *int32 `json:"exit_code,omitempty"`
	TerminationReason string `json:"termination_reason,omitempty"`
	URL               string `json:"url,omitempty"`
	Message           string `json:"message,omitempty"`

	err error
	// record is the app's state behind a crashed outcome — nil when the app
	// was no longer listed — and observed says the outcome has one, for the
	// attached gate's replacement check (gateAppReplaced).
	record   *agentpb.AppContainer
	observed bool
	// verdict is the wait's waitReadyChecks.foreign verdict when that ended
	// it: appWasReplaced — it saw another deployment's app and has no outcome
	// of its own — or appExitUnreported, a not_ready outcome
	// (unreportedExitOutcome) for an app it cannot tell from one.
	verdict appVerdict
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
	// baseline is the app's restart count (failure_count) when the wait
	// began; a poll showing more means the restart policy restarted it. It
	// is known before the first poll, which may already show a restart: 0
	// for an app this run started, since the agent resets the count on
	// start, or the count the run saw when it chose to follow an app it
	// found running (appBaseline.failures).
	baseline uint32
	// ended fires once an attached run's own output has ended — the task it
	// started exited — so the wait checks the app at once rather than probing
	// on; nil never fires.
	ended <-chan struct{}
	// foreign reports whether c, read by a state poll, is another
	// deployment's app (appWasReplaced) or one the wait cannot tell from it
	// (appExitUnreported); taskEnded says ended had fired before that poll.
	// nil: never (see gateTarget.foreign).
	foreign func(c *agentpb.AppContainer, taskEnded bool) appVerdict
}

// awaitAppReady waits for a started app. With a probe it succeeds once the
// probe passes and the app is still running; without one it succeeds when the
// deadline (the stability window) finds the app still running and not
// restarted. It fails as soon as a poll shows the app exited, crash-looping,
// gone, or restarted (failure_count above c.baseline), or once ended fires
// and a poll finds the app running again. It ends without an outcome
// of its own (appWasReplaced) as soon as a poll shows another deployment's
// app, and as not_ready (unreportedExitOutcome) when a poll shows one it
// cannot tell from that. Transient ListContainers errors are tolerated until
// the deadline, but a passed probe counts only once a poll right after it
// confirms the app is still running.
func awaitAppReady(ctx context.Context, c waitReadyChecks) waitReadyOutcome {
	checked := c.probe != nil
	onFailure := readinessNotChecked
	if checked {
		onFailure = readinessFailed
	}
	// poll reads the app's state and whether it is another deployment's app.
	// It samples ended before the read: a record read before this run's
	// output ended may be of the run's own task, still running.
	poll := func() (container *agentpb.AppContainer, foreign appVerdict, err error) {
		taskEnded := false
		select {
		case <-c.ended:
			taskEnded = true
		default:
		}
		container, err = c.state(ctx)
		if err != nil || c.foreign == nil {
			return container, appOwn, err
		}
		return container, c.foreign(container, taskEnded), nil
	}
	interrupted := func() waitReadyOutcome { return waitReadyOutcome{err: ctx.Err()} }
	// foreignOutcome ends the wait on a poll's verdict other than appOwn.
	foreignOutcome := func(foreign appVerdict) waitReadyOutcome {
		if foreign == appExitUnreported {
			return unreportedExitOutcome(c.appID, onFailure)
		}
		return waitReadyOutcome{verdict: appWasReplaced}
	}
	_, foreign, _ := poll()
	switch {
	case ctx.Err() != nil:
		return interrupted()
	case foreign != appOwn:
		return foreignOutcome(foreign)
	}
	// unconfirmed is the error of the latest poll that failed to confirm a
	// passed probe; nil once one succeeds.
	var unconfirmed error
	ended := c.ended
	for {
		taskEnded := false
		select {
		case <-ctx.Done():
			return interrupted()
		case <-c.deadline:
			if checked && unconfirmed != nil {
				return notReadyOutcome(readinessFailed, "app %s accepted connections within %s, but its state could not be read to confirm it is still running: %v", c.appID, c.limit, unconfirmed)
			}
			if checked {
				return notReadyOutcome(readinessFailed, "app %s did not pass its readiness probe within %s", c.appID, c.limit)
			}
			container, foreign, err := poll()
			switch {
			case ctx.Err() != nil:
				return interrupted()
			case foreign != appOwn:
				return foreignOutcome(foreign)
			case err != nil:
				return notReadyOutcome(readinessNotChecked, "could not confirm app %s is still running: %v", c.appID, err)
			}
			if out, crashed := crashedOutcome(c.appID, container, c.baseline, onFailure); crashed {
				return out
			}
			return waitReadyOutcome{Status: waitReadyStatusRunning, Readiness: readinessNotChecked}
		case <-ended:
			ended, taskEnded = nil, true
		case <-c.tick:
		}
		container, foreign, err := poll()
		switch {
		case ctx.Err() != nil:
			return interrupted()
		case foreign != appOwn:
			return foreignOutcome(foreign)
		}
		if err == nil {
			if out, crashed := crashedOutcome(c.appID, container, c.baseline, onFailure); crashed {
				return out
			}
			if taskEnded {
				// The run's task exited before the app became ready, and the
				// app runs again with no start resetting its count.
				err := commandErrorf(errAppCrashed, "app %s exited before becoming ready and was restarted by its restart policy; see its logs with `wendy device logs --app %s`", c.appID, c.appID)
				return waitReadyOutcome{Status: waitReadyStatusCrashed, Readiness: onFailure, Message: err.Error(), err: err, record: container, observed: true}
			}
		}
		if checked && c.probe(ctx) {
			if ctx.Err() != nil {
				return interrupted()
			}
			// The port answered; make sure it is still this run's process.
			// Without a state to judge, the probe proves nothing yet: probe
			// and confirm again on the next tick, until the deadline.
			container, foreign, err := poll()
			switch {
			case foreign != appOwn:
				return foreignOutcome(foreign)
			case err != nil:
				unconfirmed = err
				continue
			}
			if out, crashed := crashedOutcome(c.appID, container, c.baseline, onFailure); crashed {
				return out
			}
			return waitReadyOutcome{Status: waitReadyStatusReady, Readiness: readinessPassed}
		}
	}
}

func notReadyOutcome(readiness, format string, args ...any) waitReadyOutcome {
	err := commandErrorf(errReadinessTimeout, format, args...)
	return waitReadyOutcome{Status: waitReadyStatusNotReady, Readiness: readiness, Message: err.Error(), err: err}
}

// unreportedExitOutcome is --wait-ready's outcome for an app it cannot tell
// from another deployment's because the device did not report how the app
// exited (appExitUnreported): not_ready, since the run can prove neither a
// crash nor a replacement, and never a reason to stop the app.
func unreportedExitOutcome(appID, readiness string) waitReadyOutcome {
	out := notReadyOutcome(readiness, "could not confirm app %s became ready: it stopped, and the device did not report how it exited, so it may have crashed and been restarted by its restart policy, or another deployment may have replaced it; see its logs with `wendy device logs --app %s`", appID, appID)
	out.verdict = appExitUnreported
	return out
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
	out := waitReadyOutcome{Status: waitReadyStatusCrashed, Readiness: readiness, Message: err.Error(), err: err, record: c, observed: true}
	if c != nil && c.GetTerminationReason() != "" {
		code := c.GetExitCode()
		out.ExitCode, out.TerminationReason = &code, c.GetTerminationReason()
	}
	return out, true
}

// waitForAppReady is --wait-ready for an app the agent just confirmed started.
// It probes the readiness port (explicit, or the http entitlement's) from this
// machine when it can reach the device, within readinessProbeTimeout. Without
// a probe, through the cloud tunnel when neither Wendy Mesh nor the device's
// LAN address reaches it from here (see hostSideAppAddress), or over the
// on-device agent socket, it instead requires the app to stay running for
// waitReadyStabilityWindow (shortened by a smaller --readiness-timeout),
// polled via ListContainers.
//
// base is the app's baseline: startedAppBaseline for an app the run just
// started, observedAppBaseline for one it found running; a restart counted
// above base.failures fails the wait.
func waitForAppReady(ctx context.Context, conn *grpcclient.AgentConnection, appCfg *appconfig.AppConfig, opts runOptions, base appBaseline) waitReadyOutcome {
	return waitForGatedAppReady(ctx, conn, appCfg, opts, base, nil)
}

// waitForGatedAppReady is waitForAppReady for an attached run's gate, whose
// target also makes the wait watch for the run's task ending and for another
// deployment's app (waitReadyChecks.ended and foreign); nil is a plain wait.
// The gate passes target.base as base.
func waitForGatedAppReady(ctx context.Context, conn *grpcclient.AgentConnection, appCfg *appconfig.AppConfig, opts runOptions, base appBaseline, target *gateTarget) waitReadyOutcome {
	checks := waitReadyChecks{
		appID:    appCfg.AppID,
		limit:    waitReadyStabilityWindow,
		baseline: base.failures,
		state: func(ctx context.Context) (*agentpb.AppContainer, error) {
			stateCtx, cancel := context.WithTimeout(ctx, waitReadyStateTimeout)
			defer cancel()
			return lookupAppContainer(stateCtx, conn, appCfg.AppID)
		},
	}
	if target != nil {
		// The gate's polls also tell whether the agent reports exits
		// (appBaseline.seeing), which target.foreign and gateAppReplaced
		// judge by. Only the gate's goroutine touches target.
		lookup := checks.state
		checks.state = func(ctx context.Context) (*agentpb.AppContainer, error) {
			c, err := lookup(ctx)
			if err == nil {
				target.base = target.base.seeing(c)
			}
			return c, err
		}
		checks.ended = target.taskEnded
		checks.foreign = func(c *agentpb.AppContainer, taskEnded bool) appVerdict { return target.foreign(c, taskEnded) }
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
		addr, probeOnce, closeProbe := makeReadinessProbe(
			host,
			readiness.TCPSocket.Port,
			cloudHTTPReadinessPort(conn, appCfg, readiness, host) != 0,
		)
		defer closeProbe()
		checks.limit = readinessProbeTimeout(readiness, opts.readinessTimeout)
		checks.probe = func(ctx context.Context) bool { return probeOnce(ctx) == nil }
		cliLogln("Waiting up to %s for %s to accept connections on %s...", checks.limit, containerDisplayName(appCfg), tui.Value(addr))
	case hasProbe && agentSocket:
		cliLogln("Not checking readiness from here: wendy reaches the device agent through its local socket (WENDY_AGENT_SOCKET) and has no address to probe the app at. Confirming %s stays running for %s instead...", containerDisplayName(appCfg), checks.limit)
	case hasProbe:
		cliLogln("Not checking readiness from this machine: the device is connected through Wendy Cloud, Wendy Mesh is not active, and its LAN address is not reachable from here. Confirming %s stays running for %s instead...", containerDisplayName(appCfg), checks.limit)
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
	out.Device = waitReadyDevice(conn)
	if out.Status == waitReadyStatusReady {
		out.URL = url
	}
	return out
}

// waitReadyDevice is the device a --wait-ready outcome names: the agent
// connection's host, or "" on the device itself, where the agent socket
// (Host "unix:<socket path>", see waitForAppReady) names no device.
func waitReadyDevice(conn *grpcclient.AgentConnection) string {
	if strings.HasPrefix(conn.Host, "unix:") {
		return ""
	}
	return conn.Host
}

// reportWaitReadyOutcome prints a successful outcome's status line on stderr
// (failures are the run's returned error). A detached run also prints the app
// URL and, in JSON mode, the outcome as exactly one JSON object on stdout —
// failures included — and records that in ctx (markWaitReadyOutcomePrinted).
func reportWaitReadyOutcome(ctx context.Context, appCfg *appconfig.AppConfig, out waitReadyOutcome, detached bool) {
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
		markWaitReadyOutcomePrinted(ctx)
	}
}

// waitReadyAfterDetachedStart runs --wait-ready for a detached run whose
// container the agent just confirmed started (base: startedAppBaseline), or
// that the no-change fast path found already running (base:
// observedAppBaseline of the record it found), reports the outcome, and
// returns the run's error. A caller holding the start stream drains it meanwhile
// (drainDetachedStartOutput). An interrupted wait prints no outcome and leaves
// the app running; runWithInterruptChannel turns Ctrl-C into exit 0 and
// SIGTERM into errTerminated.
func waitReadyAfterDetachedStart(ctx context.Context, conn *grpcclient.AgentConnection, appCfg *appconfig.AppConfig, opts runOptions, base appBaseline) error {
	out := waitForAppReady(ctx, conn, appCfg, opts, base)
	if out.Status == "" {
		noteInterruptedApp(ctx, appCfg.ContainerName(), interruptedAppLeftRunning, nil)
		return out.err
	}
	reportWaitReadyOutcome(ctx, appCfg, out, true)
	return out.err
}

// drainDetachedStartOutput reads and discards a detached run's start stream
// (RunContainer or StartContainer) in the background until it ends (io.EOF or
// an error). A --wait-ready wait outlives the Started acknowledgement by up to
// the probe deadline while the app keeps printing into that stream; an agent
// that applies backpressure to an unread stream could otherwise block a
// chatty app into a false readiness_timeout. It does not cancel the stream:
// that is the run's context, so, as without the wait, the stream ends when
// the app exits or the process does. A variable so tests can see whether a
// run drains.
var drainDetachedStartOutput = func(stream containerOutputStream) {
	go func() {
		for {
			if _, err := stream.Recv(); err != nil {
				return
			}
		}
	}()
}

// readinessGate is --wait-ready for an attached run: the check runs beside the
// session while logs stream. A failure is recorded and, with stopOnFailure
// (the run started the app), the app stopped, which ends the agent's output
// stream so the attached loop returns Err(). A run that only follows an app it
// did not start leaves the app alone and watches finished() instead: a
// host-side failure can be the network's (a firewall or VPN), and a stop
// would keep the app down across reboots. A stop is also skipped when the
// device shows no task of the app (appStoppable). Before failing, the gate
// makes sure another deployment did not replace the app meanwhile
// (gateAppReplaced): if it did, the gate records Replaced() instead — no
// failure and no stop, since the app on the device is the other deployment's
// — and the caller reports it (noteAppReplaced) and succeeds. When the gate
// cannot tell because the device did not report how the app exited
// (appExitUnreported), it fails as not_ready without a stop. For a follow,
// Replaced() also covers a crash verdict whose recorded exit does not stand
// (followedExitStands) and appExitUnreported; the follow reports any of them
// with noteFollowedAppEnded.
// A pass launches the host-side postStart work via onReady, whose hook
// runner skips its own probe (hostReadinessConfirmed); when the run's own
// task has ended by then, the gate first makes sure the app that passed is
// not a replacement. The nil gate (no --wait-ready) is valid: Err returns
// nil, Replaced false, wait returns at once, and finished never fires.
//
// When the app's output ends on its own (it exited), callers wait for the gate
// before reading Err and before cancelling its context: the gate checks the
// app at once (gateTarget.taskEnded) and records whether it became ready
// before it exited — a clean exit inside the wait is a failure too — or was
// replaced. Interrupts and broken streams cancel it first; it then stands
// down without an outcome or a stop.
type readinessGate struct {
	mu       sync.Mutex
	err      error
	replaced bool
	done     chan struct{}
}

// gateTarget is what a readiness gate knows about the app it checks.
type gateTarget struct {
	// stopOnFailure: the run started the app, so a failure stops it.
	stopOnFailure bool
	// base is the app's baseline: startedAppBaseline for a run that started
	// it, the follow's baseline otherwise.
	base appBaseline
	// taskEnded is closed once the run's own output stream has ended
	// (io.EOF), which the agent does when the task this run started exits.
	// nil for a run with no such stream (a follow).
	taskEnded <-chan struct{}
}

func startReadinessGate(ctx context.Context, conn *grpcclient.AgentConnection, appCfg *appconfig.AppConfig, opts runOptions, target gateTarget, onReady func()) *readinessGate {
	g := &readinessGate{done: make(chan struct{})}
	go func() {
		defer close(g.done)
		out := waitForGatedAppReady(ctx, conn, appCfg, opts, target.base, &target)
		if ctx.Err() != nil {
			return // the session ended first; its own teardown decides the outcome
		}
		verdict, stoppable := out.verdict, true
		if verdict == appOwn && (out.err != nil || target.ended()) {
			// Fail and stop — or, once the run's task has ended, run the
			// host-side hooks — only for an app that is still this run's.
			var evidence, last *agentpb.AppContainer
			var err error
			verdict, stoppable, evidence, last, err = gateAppReplaced(ctx, conn, appCfg, target, out)
			if err != nil {
				return // the session ended first, as above
			}
			if !target.stopOnFailure && errors.Is(out.err, errAppCrashed) && !followedExitStands(verdict != appOwn, last) {
				// A follow reports the end of an app it did not start neutrally
				// unless the recorded exit stands; Replaced() carries that.
				verdict = appWasReplaced
			}
			if verdict == appOwn && out.err != nil && out.observed && evidence.GetTerminationReason() != "" &&
				(evidence.GetExitCode() != out.record.GetExitCode() || evidence.GetTerminationReason() != out.record.GetTerminationReason()) {
				// The wait's poll caught the app before the agent recorded
				// its exit: report the exit recorded since.
				if fresh, crashed := crashedOutcome(appCfg.AppID, evidence, 0, out.Readiness); crashed {
					out.err = fresh.err
				}
			}
		}
		if verdict == appExitUnreported && !target.stopOnFailure {
			verdict = appWasReplaced // a follow ends neutrally either way
		}
		switch verdict {
		case appWasReplaced:
			g.mu.Lock()
			g.replaced = true
			g.mu.Unlock()
			return
		case appExitUnreported:
			if out.verdict != appExitUnreported { // gateAppReplaced's verdict, not the wait's
				readiness := out.Readiness
				if readiness == readinessPassed {
					readiness = readinessFailed
				}
				out = unreportedExitOutcome(appCfg.AppID, readiness)
			}
		}
		reportWaitReadyOutcome(ctx, appCfg, out, false)
		if out.err != nil {
			g.mu.Lock()
			g.err = out.err
			g.mu.Unlock()
			switch {
			case !target.stopOnFailure:
			case verdict == appExitUnreported:
				cliLogln("Not stopping %s: the app on the device may be another deployment's.", containerDisplayName(appCfg))
			case stoppable:
				stopUnreadyApp(conn, appCfg)
			default:
				cliLogln("Not stopping %s: the device shows no task of it to stop.", containerDisplayName(appCfg))
			}
			return
		}
		onReady()
	}()
	return g
}

// gateAppReplaced reports whether a --wait-ready check that failed, or that
// passed after the run's own task ended, saw another deployment's doing: the
// app was replaced while the gate checked it (appWasReplaced), so the outcome
// is not this run's app's, and the app running now must not be stopped or get
// this run's hooks — nor may one the gate cannot tell from a replacement
// (appExitUnreported). ListContainers reports no container ID, so it judges
// the record behind a crash verdict or, otherwise, a fresh lookup
// (target.foreign first for the latter; the wait already applied it to the
// former), and confirms it with appReplaced — a clean exit too, since
// --wait-ready fails on one. stoppable says whether a failure may stop the
// app: not when the latest record judged shows no task of it (appStoppable);
// evidence and last are appReplaced's.
// An unreadable lookup keeps the outcome and today's stop. It returns ctx's
// error if ctx ends first.
func gateAppReplaced(ctx context.Context, conn *grpcclient.AgentConnection, appCfg *appconfig.AppConfig, target gateTarget, out waitReadyOutcome) (verdict appVerdict, stoppable bool, evidence, last *agentpb.AppContainer, err error) {
	c := out.record
	if !out.observed {
		taskEnded := target.ended() // sampled before the read, as in awaitAppReady
		lookupCtx, cancel := context.WithTimeout(ctx, waitReadyStateTimeout)
		now, err := lookupAppContainer(lookupCtx, conn, appCfg.AppID)
		cancel()
		if ctx.Err() != nil {
			return appOwn, false, nil, nil, ctx.Err()
		}
		if err != nil {
			return appOwn, true, nil, nil, nil
		}
		if foreign := target.foreign(now, taskEnded); foreign != appOwn {
			return foreign, false, now, now, nil
		}
		c = now
	}
	verdict, evidence, last, err = appReplaced(ctx, conn, appCfg, target.base, c, target.ended)
	if err != nil {
		return appOwn, false, nil, nil, err
	}
	return verdict, appStoppable(evidence), evidence, last, nil
}

// ended reports whether the run's own output stream has ended.
func (target gateTarget) ended() bool {
	select {
	case <-target.taskEnded:
		return true
	default:
		return false // still open, or no such stream (nil)
	}
}

// foreign reports whether c is another deployment's app for certain
// (appWasReplaced): on another app_version than the baseline's, or running
// with a failure_count below the baseline's (a start reset it; the restart
// policy only raises it), or — when the run's own task had already exited
// (taskEnded) — at or below it: the run's task is gone, and a restart by the
// restart policy would have raised the count. That last sign counts only
// once a record the gate saw showed that the agent reports exits
// (appBaseline.seeing); until then it is appExitUnreported, since an agent
// that reports no restart count lists a restarted app that way too.
func (target gateTarget) foreign(c *agentpb.AppContainer, taskEnded bool) appVerdict {
	base := target.base.seeing(c)
	switch {
	case base.versionChanged(c):
		return appWasReplaced
	case c.GetRunningState() != agentpb.AppRunningState_RUNNING:
		return appOwn
	case c.GetFailureCount() < base.failures:
		return appWasReplaced
	case taskEnded && c.GetFailureCount() <= base.failures:
		return base.replacement()
	default:
		return appOwn
	}
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

// Replaced reports whether the gate found that another deployment replaced
// the app while it checked it (for a follow, also that it cannot tell; see
// readinessGate).
func (g *readinessGate) Replaced() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.replaced
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
