// Package container implements container health monitoring and restart policies.
package container

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/agent/services"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

const (
	// restartBackoffBase is the delay before the second restart of a container
	// that keeps failing; each subsequent restart doubles it.
	restartBackoffBase = 10 * time.Second
	// restartBackoffCap ceilings the doubling. The monitor never gives up on a
	// crash-looping container — unattended devices must self-heal when a
	// dependency (network, camera, USB peripheral) comes back — so it keeps
	// retrying at this interval indefinitely.
	restartBackoffCap = 5 * time.Minute
	// gpuMinRestartDelay floors the delay before restarting a container that
	// holds a gpu entitlement, including its first restart after a crash.
	//
	// A GPU app that dies hard — a segfault, an OOM kill, anything that skips
	// its CUDA teardown — leaves the driver holding the context it was using.
	// Restarting instantly asks the driver for a fresh context before it has
	// reaped the dead one, and a driver that refuses returns "no device": the
	// app crashes again, on a tighter loop, for a reason that has nothing to do
	// with the app. A few seconds of patience costs a GPU app very little (they
	// take longer than this to load their weights) and takes the retry out of
	// the window where the driver is still cleaning up.
	gpuMinRestartDelay = 10 * time.Second
	// restartStabilityWindow is how long a container must be observed RUNNING
	// continuously before its backoff resets. It is deliberately much longer
	// than the tick interval: a container that starts, is seen running once,
	// and dies is still crash-looping, and resetting on that sighting would
	// pin the delay at the base forever.
	restartStabilityWindow = 60 * time.Second
)

// restartDelay returns how long to wait before the next restart of a container
// that has already been restarted level times, doubling from restartBackoffBase
// and clamping at restartBackoffCap:
//
//	level: 0   1     2     3     4     5      6+
//	delay: 0   10s   20s   40s   80s   160s   5m
//
// Level 0 (the first restart after a crash) is immediate and level 1 is 10s,
// preserving the pre-backoff timing for the first two attempts so a transient
// crash still recovers promptly. GPU containers are the exception — see
// gpuMinRestartDelay.
func restartDelay(level int) time.Duration {
	if level <= 0 {
		return 0
	}
	// Doubling by repeated multiplication with an early exit rather than
	// `base << (level-1)`: level is unbounded (a container can loop for days),
	// and a shift that large silently wraps — on a 64-bit int the delay would
	// come back as 0 and restore the unthrottled every-tick restart this
	// exists to prevent. The loop cannot run more than a handful of times
	// because it returns the moment the cap is reached.
	d := restartBackoffBase
	for i := 0; i < level-1; i++ {
		if d >= restartBackoffCap {
			return restartBackoffCap
		}
		d *= 2
	}
	if d > restartBackoffCap {
		return restartBackoffCap
	}
	return d
}

// RestartPolicy determines the container restart behavior.
type RestartPolicy int

const (
	// RestartNo never restarts the container.
	RestartNo RestartPolicy = iota
	// RestartUnlessStopped restarts unless explicitly stopped.
	RestartUnlessStopped
	// RestartOnFailure restarts only on non-zero exit codes.
	RestartOnFailure
	// RestartAlways always restarts the container.
	RestartAlways
)

func (p RestartPolicy) String() string {
	switch p {
	case RestartNo:
		return "no"
	case RestartUnlessStopped:
		return "unless-stopped"
	case RestartOnFailure:
		return "on-failure"
	case RestartAlways:
		return "always"
	default:
		return fmt.Sprintf("unknown(%d)", int(p))
	}
}

// ParseRestartPolicy converts a string to a RestartPolicy.
func ParseRestartPolicy(s string) (RestartPolicy, error) {
	switch s {
	case "no", "":
		return RestartNo, nil
	case "unless-stopped":
		return RestartUnlessStopped, nil
	case "on-failure":
		return RestartOnFailure, nil
	case "always":
		return RestartAlways, nil
	default:
		return RestartNo, fmt.Errorf("unknown restart policy: %q", s)
	}
}

// containerState tracks the runtime state of a monitored container.
type containerState struct {
	FailureCount  int
	LastRestart   time.Time
	ExplicitStop  bool
	RestartPolicy RestartPolicy
	MaxRetries    int
	// BackoffLevel is the number of restarts performed since the last reset,
	// driving the delay before the next one (see restartDelay). It is distinct
	// from FailureCount, which stays cumulative because it is user-visible
	// through RestartStatuses.
	BackoffLevel int
	// RunningSince is when the container was first observed RUNNING since its
	// last restart, or zero while it is not running. Once it has been running
	// for restartStabilityWindow the backoff resets.
	RunningSince time.Time
	// DownSince is when the container was first observed not running since it
	// was last up, or zero while it is running.
	//
	// Distinct from LastRestart, which the backoff ladder measures from: an app
	// that ran happily for hours and then crashed has a LastRestart hours in
	// the past, so every level-based delay is already satisfied and the restart
	// is immediate no matter what the ladder says. Only a clock that starts at
	// the death can hold a restart back for a fixed interval after it.
	DownSince time.Time
}

// ContainerMonitor monitors container health and implements restart policies.
type ContainerMonitor struct {
	logger     *zap.Logger
	containerd services.ContainerdClient
	logManager *services.ContainerLogManager
	states     map[string]*containerState
	// groupRestarting tracks shared-namespace app groups with an in-flight group
	// restart, keyed by appID. A group restart stops every member, so a later
	// tick would otherwise see the siblings stopped and launch a second,
	// overlapping restart that races on the primary PID. Guarded by mu.
	groupRestarting map[string]bool
	// suppressed counts in-flight Suppress handles per container name (see
	// Suppress). While a name's count is > 0, planRestarts will not schedule a
	// restart for it. A counter rather than a bool so two independent
	// operations racing on the same name (e.g. a stop overlapping a replace of
	// the same service) don't have the first resume() re-enable restarts while
	// the second is still tearing the task down. Guarded by mu.
	suppressed map[string]int
	// gpuEntitled caches, per container name, whether it holds a gpu
	// entitlement. Entitlements are fixed for a container's lifetime (they are
	// baked into its labels at create time), so this is resolved once per name
	// rather than on every tick. Guarded by mu.
	gpuEntitled map[string]bool
	mu          sync.Mutex
	interval    time.Duration
	// now is the monitor's clock, defaulting to time.Now. Restart backoff is
	// measured in minutes, so tests override this to drive the curve
	// deterministically instead of sleeping.
	now func() time.Time
}

func NewContainerMonitor(logger *zap.Logger, client services.ContainerdClient, logManager *services.ContainerLogManager, interval time.Duration) *ContainerMonitor {
	if interval == 0 {
		interval = 5 * time.Second
	}
	return &ContainerMonitor{
		logger:          logger,
		containerd:      client,
		logManager:      logManager,
		states:          make(map[string]*containerState),
		groupRestarting: make(map[string]bool),
		suppressed:      make(map[string]int),
		gpuEntitled:     make(map[string]bool),
		interval:        interval,
		now:             time.Now,
	}
}

// Register registers a container for monitoring with a given restart policy.
func (m *ContainerMonitor) Register(appName string, policy RestartPolicy, maxRetries int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.gpuEntitled, appName)
	m.states[appName] = &containerState{
		RestartPolicy: policy,
		MaxRetries:    maxRetries,
	}
	m.logger.Info("Container registered for monitoring",
		zap.String("app_name", appName),
		zap.Int("policy", int(policy)),
	)
}

// Unregister removes a container from monitoring.
func (m *ContainerMonitor) Unregister(appName string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.states, appName)
	// A redeploy can change an app's entitlements, and the name is reused, so
	// the cached answer must not outlive the registration it was resolved for.
	delete(m.gpuEntitled, appName)
}

// MarkExplicitStop marks a container as explicitly stopped, preventing restart.
// An unknown key leaves the restart policy live on a container the user
// believes is stopped, so it is logged rather than ignored.
func (m *ContainerMonitor) MarkExplicitStop(appName string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state, ok := m.states[appName]
	if !ok {
		m.logger.Warn("MarkExplicitStop: container not registered for monitoring; restart policy unchanged",
			zap.String("app_name", appName))
		return
	}
	state.ExplicitStop = true
}

// ClearExplicitStop reverts a prior MarkExplicitStop, re-enabling automatic
// restarts for the container. It is a no-op if appName is not registered.
func (m *ContainerMonitor) ClearExplicitStop(appName string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if state, ok := m.states[appName]; ok {
		state.ExplicitStop = false
	}
}

// Suppress pauses automatic restarts for containerName until the returned
// resume func is called. The containerd client holds this for the duration of
// a replace or stop operation's kill+delete sequence so the monitor's
// periodic tick cannot launch a competing restartSingle while the task is
// being torn down — observed live as a crash-looping app's restart racing a
// replace/stop ("cannot delete running task: failed precondition"), and a
// half-dead task left behind wedging the follow-up delete entirely.
//
// The returned func is idempotent: calling it more than once only decrements
// the count on the first call. containerName uses the same keying as
// Register/states — bare appID for single-container apps, "{appID}_{service}"
// for services-map apps.
func (m *ContainerMonitor) Suppress(containerName string) func() {
	m.mu.Lock()
	m.suppressed[containerName]++
	m.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			if m.suppressed[containerName] > 0 {
				m.suppressed[containerName]--
			}
			if m.suppressed[containerName] <= 0 {
				delete(m.suppressed, containerName)
			}
			m.mu.Unlock()
		})
	}
}

// restartBlocked reports whether containerName currently has a Suppress
// handle held on it, or is marked as explicitly stopped, and so must not be
// (re)started. Used by restartSingle to
// re-check right before it would call StartContainer — see the comment there
// for why this differs from the planRestarts-time check.
//
// Both arms are unconditional. Restart policies govern spontaneous exits;
// none of them overrides an explicit user stop. In particular, allowing
// RestartAlways through here makes a multi-service group's first stopped
// member schedule a whole-group restart while its siblings are still being
// stopped.
func (m *ContainerMonitor) restartBlocked(containerName string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.suppressed[containerName] > 0 {
		return true
	}
	if state, ok := m.states[containerName]; ok && state.ExplicitStop {
		return true
	}
	return false
}

// RestartStatuses returns the monitor's per-container restart bookkeeping,
// keyed by the monitored container name (bare appID, or "{appID}_{serviceName}"
// for services-map apps). It implements services.RestartStatusProvider so the
// container list can report a crash-looping app truthfully instead of STOPPED.
func (m *ContainerMonitor) RestartStatuses() map[string]services.RestartStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]services.RestartStatus, len(m.states))
	for name, state := range m.states {
		out[name] = services.RestartStatus{
			FailureCount: state.FailureCount,
			WillRestart:  m.shouldRestart(state),
		}
	}
	return out
}

// ReconcileBootContainers brings apps back after a device boot. containerd
// keeps container definitions across a reboot but loses their tasks, so without
// this every app sits stopped until manually started. It registers each
// container whose restart policy keeps it running (and that the user didn't
// explicitly stop) under that policy, then runs one immediate reconcile so the
// stopped ones are (re)launched without waiting for the next tick. Intended to
// be called once at agent startup.
//
// Apps deployed with the default policy (unless-stopped) come back; apps
// deployed with --no-restart, and apps the user explicitly stopped, stay down.
func (m *ContainerMonitor) ReconcileBootContainers(ctx context.Context) {
	// Warm the isolation/service caches from persisted labels before anything
	// starts a container: after a reboot these in-memory caches are empty, and
	// StartContainer would otherwise skip CNI networking + mesh egress for
	// isolated apps. Optional capability, mirroring GroupRestarter.
	if r, ok := m.containerd.(services.AppStateRebuilder); ok {
		r.RebuildAppStateCaches(ctx)
	}

	// One-time upgrade back-fill: apps stopped under an older agent carry no
	// stopped-by-user mark, so without this the first post-upgrade boot would
	// resurrect them. Runs once (persistent marker); must precede the listing
	// below so the marks are in place before we decide what to start.
	if err := m.containerd.MigrateStoppedByUserOnce(ctx); err != nil {
		m.logger.Warn("Boot reconcile migration failed; proceeding without it", zap.Error(err))
	}

	bcs, err := m.containerd.ListBootContainers(ctx)
	if err != nil {
		m.logger.Error("Failed to list boot containers", zap.Error(err))
		return
	}
	if len(bcs) == 0 {
		return
	}
	for _, bc := range bcs {
		// Empty policy means "default keep-running"; map it to unless-stopped.
		policy := RestartUnlessStopped
		if bc.RestartPolicy != "" {
			p, parseErr := ParseRestartPolicy(bc.RestartPolicy)
			if parseErr != nil {
				m.logger.Warn("Skipping boot container with unparseable restart policy",
					zap.String("app_name", bc.Name), zap.String("policy", bc.RestartPolicy), zap.Error(parseErr))
				continue
			}
			policy = p
		}
		m.Register(bc.Name, policy, bc.MaxRetries)
	}
	m.logger.Info("Reconciling apps on boot", zap.Int("count", len(bcs)))
	// Immediate pass: start the ones that aren't running yet (the common
	// post-reboot case) instead of waiting up to one tick interval.
	m.checkContainers(ctx)
}

// Run is the main monitoring loop that checks container health and restarts as needed.
func (m *ContainerMonitor) Run(ctx context.Context) {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.checkContainers(ctx)
			m.probeExposedPorts(ctx)
		}
	}
}

// probeExposedPorts asks the containerd client (if it supports it) to warn
// about publicly-bound ports on running host-network apps. Optional capability,
// mirroring the AppStateRebuilder hook.
func (m *ContainerMonitor) probeExposedPorts(ctx context.Context) {
	if p, ok := m.containerd.(services.PortExposureProber); ok {
		p.WarnPubliclyExposedPorts(ctx)
	}
}

// checkContainers queries containerd for running containers and restarts any that
// have exited according to their restart policy.
func (m *ContainerMonitor) checkContainers(ctx context.Context) {
	containers, err := m.containerd.ListContainers(ctx)
	if err != nil {
		m.logger.Error("Failed to list containers for health check", zap.Error(err))
		return
	}

	m.resolveGPUEntitlements(ctx)

	toRestart := m.planRestarts(containers)

	for _, act := range m.planRestartActions(ctx, toRestart) {
		if act.groupAppID != "" {
			go m.restartGroup(ctx, act.groupAppID)
		} else {
			go m.restartSingle(ctx, act.single)
		}
	}
}

// restartAction is one unit of restart work: either a single container, or an
// entire shared-namespace app group identified by appID.
type restartAction struct {
	single     string // restart this container on its own
	groupAppID string // restart this shared-namespace group as a unit
}

// planRestartActions maps the flat list of containers due for restart into
// restart units, collapsing members of the same shared-namespace group into a
// single group restart. Members of a shared-namespace group must restart
// together: a secondary's namespace join is resolved against the primary's live
// task, so restarting members independently would leave a secondary attached to
// a dead namespace.
func (m *ContainerMonitor) planRestartActions(ctx context.Context, toRestart []string) []restartAction {
	gr, _ := m.containerd.(services.GroupRestarter)
	seenGroup := make(map[string]bool)
	var actions []restartAction
	for _, name := range toRestart {
		if gr != nil {
			if appID, grouped := gr.GroupRestartAppID(ctx, name); grouped {
				if seenGroup[appID] {
					continue
				}
				seenGroup[appID] = true
				if m.groupHasBlockedMember(ctx, gr, appID) {
					// A sibling in this group is mid-replace/stop or was
					// explicitly stopped, even though it didn't itself qualify
					// for toRestart — e.g. it still reports RUNNING because
					// the caller hasn't killed its task yet.
					// RestartGroup stops/recreates every member, which would
					// stomp on whatever that operation is doing to the
					// suppressed one. Defer the whole group restart; the next
					// tick retries once the handle is resumed.
					continue
				}
				actions = append(actions, restartAction{groupAppID: appID})
				continue
			}
		}
		actions = append(actions, restartAction{single: name})
	}
	return actions
}

// groupHasBlockedMember reports whether any suppressed or explicitly-stopped
// container belongs to the same shared-namespace group as appID. Blocked names
// are normally few, so this scans them directly rather than maintaining a
// second appID-to-members index.
func (m *ContainerMonitor) groupHasBlockedMember(ctx context.Context, gr services.GroupRestarter, appID string) bool {
	m.mu.Lock()
	blocked := make(map[string]struct{}, len(m.suppressed))
	for name := range m.suppressed {
		blocked[name] = struct{}{}
	}
	for name, state := range m.states {
		if state.ExplicitStop {
			blocked[name] = struct{}{}
		}
	}
	m.mu.Unlock()

	for name := range blocked {
		if memberAppID, grouped := gr.GroupRestartAppID(ctx, name); grouped && memberAppID == appID {
			return true
		}
	}
	return false
}

// restartSingle restarts one container and drains its output to the log manager.
func (m *ContainerMonitor) restartSingle(ctx context.Context, name string) {
	if m.restartBlocked(name) {
		// This goroutine was scheduled by a tick that observed name as down
		// and eligible, but a Suppress or MarkExplicitStop landed in the gap
		// between that decision and this goroutine actually running (there is
		// no blocking call in between to close the window at the scheduling
		// point — see planRestarts/Suppress). Re-checking here, immediately
		// before the call that would resurrect the container, is what
		// actually closes it: a replace/stop that starts suppression after
		// the tick fired but before this line runs still wins.
		m.logger.Debug("Skipping restart: suppressed or explicitly stopped since being scheduled",
			zap.String("app_name", name))
		return
	}
	outputCh, err := m.containerd.StartContainer(ctx, name, "", nil)
	if err != nil {
		m.logger.Error("Failed to restart container",
			zap.String("app_name", name),
			zap.Error(err),
		)
		return
	}
	m.drainOutput(name, outputCh)
}

// restartGroup restarts an entire shared-namespace app group as a unit, draining
// each member's output to the log manager.
func (m *ContainerMonitor) restartGroup(ctx context.Context, appID string) {
	gr, ok := m.containerd.(services.GroupRestarter)
	if !ok {
		// Should not happen: only reachable when planRestartActions produced a
		// group action, which requires the client to be a GroupRestarter.
		m.logger.Error("group restart requested but client is not a GroupRestarter",
			zap.String("app_id", appID))
		return
	}

	// Guard against overlapping restarts of the same group: a group restart
	// stops every member, so a later tick can see the siblings stopped and try
	// to restart the group again while this one is still in flight.
	m.mu.Lock()
	if m.groupRestarting[appID] {
		m.mu.Unlock()
		return
	}
	m.groupRestarting[appID] = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.groupRestarting, appID)
		m.mu.Unlock()
	}()

	// Re-check after claiming the in-flight slot. A stop can mark/suppress the
	// group after planRestartActions chose it but before this goroutine runs.
	// Without this execution-time guard the stale action can resurrect every
	// member after the user-visible stop has already completed.
	if m.groupHasBlockedMember(ctx, gr, appID) {
		m.logger.Debug("Skipping app group restart: member stopped or suppressed since being scheduled",
			zap.String("app_id", appID))
		return
	}
	channels, err := gr.RestartGroup(ctx, appID)
	// RestartGroup can return partially-started services together with an
	// error (e.g. the primary started but a secondary failed). Drain every
	// returned channel even on error: an abandoned channel back-pressures
	// through the agent's pipes into the service's stdout FIFO and freezes
	// the process in pipe_write once the buffers fill (WDY-1822).
	for name, ch := range channels {
		go m.drainOutput(name, ch)
	}
	if err != nil {
		m.logger.Error("Failed to restart app group",
			zap.String("app_id", appID),
			zap.Error(err),
		)
	}
}

// drainOutput consumes a container's output channel so the containerd pipe never
// blocks, publishing through the log manager when available so stdout/stderr
// from restarted containers reaches OTel (and therefore `wendy device logs`).
func (m *ContainerMonitor) drainOutput(name string, outputCh <-chan services.ContainerOutput) {
	for output := range outputCh {
		if m.logManager != nil {
			m.logManager.Publish(name, output)
		}
	}
	if m.logManager != nil {
		m.logManager.Publish(name, services.ContainerOutput{Done: true})
	}
}

// planRestarts reconciles the registered container states against the current
// container list and returns the names of containers that should be restarted,
// advancing their FailureCount/LastRestart as a side effect.
func (m *ContainerMonitor) planRestarts(containers []*agentpb.AppContainer) []string {
	// Build the set of running container identities, keyed the same way the
	// monitor registers state. Services-map apps are monitored per service under
	// the "{appID}_{serviceName}" container name (see containerd.ContainerName /
	// AppConfig.ContainerName), so key each service by that name using its own
	// running state. Apps with no services (legacy single-container apps) are
	// monitored under the bare appID. Keying only by bare appID — as before —
	// meant running["{appID}_{serviceName}"] was never true, so the monitor
	// force-restarted healthy services-map apps every tick (WDY-1552).
	running := make(map[string]bool)
	for _, c := range containers {
		svcs := c.GetServices()
		if len(svcs) == 0 {
			if c.GetRunningState() == agentpb.AppRunningState_RUNNING {
				running[c.GetAppName()] = true
			}
			continue
		}
		for _, s := range svcs {
			if s.GetRunningState() != agentpb.AppRunningState_RUNNING {
				continue
			}
			if s.GetName() == "" {
				// Defensive: a serviceless entry maps to the bare appID.
				running[c.GetAppName()] = true
				continue
			}
			// Keep in sync with containerd.ContainerName.
			running[c.GetAppName()+"_"+s.GetName()] = true
			// Single-service apps deploy as a bare-named container (see
			// AppConfig.ContainerName: ServiceName == "" → bare appID) while
			// still reporting a services list here, so their monitor state is
			// registered under the bare appID. Without the bare-key mark the
			// monitor believes the app is down and force-restarts a healthy
			// container every tick — the same failure mode as WDY-1552.
			running[c.GetAppName()] = true
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	// One clock reading for the whole pass so every container is judged
	// against the same instant.
	now := m.now()
	var toRestart []string
	for appName, state := range m.states {
		if running[appName] {
			// Track how long it has been up. A container that comes back and
			// stays up is healthy, and its next failure should restart
			// promptly rather than inherit the accumulated delay — but only
			// sustained health counts. The monitor ticks every few seconds, so
			// resetting on the first RUNNING sighting would hand a free reset
			// to a container that starts, is seen once, and dies; its backoff
			// would stay pinned at the base forever.
			state.DownSince = time.Time{}
			if state.RunningSince.IsZero() {
				state.RunningSince = now
			} else if now.Sub(state.RunningSince) >= restartStabilityWindow {
				// FailureCount is deliberately not reset: it is user-visible
				// through RestartStatuses as the app's cumulative failure
				// tally.
				state.BackoffLevel = 0
			}
			continue
		}
		// Down: any partial stability streak is void.
		state.RunningSince = time.Time{}
		if state.DownSince.IsZero() {
			state.DownSince = now
		}
		if m.suppressed[appName] > 0 {
			// A replace/stop operation is mid-teardown of this container; see
			// Suppress. Skip it this tick — the caller will resume suppression
			// once it is done, and the container's next observed state (running
			// again, or still down) drives the following tick normally.
			continue
		}
		if !m.shouldRestart(state) {
			continue
		}
		// Hold a GPU container back for a fixed interval after its death,
		// however long it had been up (see gpuMinRestartDelay). This gate is
		// measured from DownSince rather than LastRestart precisely because the
		// ladder below cannot see the death of a long-lived container.
		if m.gpuEntitled[appName] && now.Sub(state.DownSince) < gpuMinRestartDelay {
			continue
		}
		delay := restartDelay(state.BackoffLevel)
		if now.Sub(state.LastRestart) < delay {
			continue
		}
		m.logger.Info("Restarting container",
			zap.String("app_name", appName),
			zap.Int("failure_count", state.FailureCount),
			zap.Duration("backoff", delay),
			zap.Duration("down_for", now.Sub(state.DownSince)),
		)
		state.FailureCount++
		state.LastRestart = now
		// Stop counting at the ceiling: past it the delay no longer changes,
		// and an ever-growing level on a container that loops for days is just
		// an overflow waiting to happen.
		if delay < restartBackoffCap {
			state.BackoffLevel++
		}
		toRestart = append(toRestart, appName)
	}
	return toRestart
}

// resolveGPUEntitlements fills the gpuEntitled cache for any registered
// container it has not answered for yet.
//
// It runs before planRestarts rather than inside it because the lookup reaches
// containerd, and planRestarts holds mu for its whole pass — the same reason
// planRestartActions does its group lookups outside the lock.
func (m *ContainerMonitor) resolveGPUEntitlements(ctx context.Context) {
	reporter, ok := m.containerd.(services.GPUDeviceReporter)
	if !ok {
		return
	}

	m.mu.Lock()
	unresolved := make(map[string]*containerState)
	for name, state := range m.states {
		if _, known := m.gpuEntitled[name]; !known {
			unresolved[name] = state
		}
	}
	m.mu.Unlock()

	for name, registration := range unresolved {
		hasGPU, err := reporter.HasGPUEntitlement(ctx, name)
		if err != nil {
			continue // A transient lookup failure is not a permanent non-GPU answer.
		}
		m.mu.Lock()
		// Register replaces the state pointer. Never commit a lookup from a
		// previous registration, even if its name has already been reused.
		if m.states[name] == registration {
			m.gpuEntitled[name] = hasGPU
		}
		m.mu.Unlock()
	}
}

// shouldRestart determines whether a container should be restarted based on its policy.
func (m *ContainerMonitor) shouldRestart(state *containerState) bool {
	// A direct user stop always wins. RestartAlways means "restart after every
	// spontaneous exit", not "make the stop command ineffective".
	if state.ExplicitStop {
		return false
	}
	switch state.RestartPolicy {
	case RestartNo:
		return false
	case RestartUnlessStopped:
		return true
	case RestartOnFailure:
		// The monitor detects only whether a container has stopped; it has no
		// exit-code signal from containerd. Until exit-code detection is added,
		// ON_FAILURE behaves like UNLESS_STOPPED: it restarts on any exit, not
		// only non-zero ones. MaxRetries is still enforced. The ExplicitStop
		// ExplicitStop check above already ran, so reaching here means it wasn't
		// set.
		if state.MaxRetries > 0 && state.FailureCount >= state.MaxRetries {
			return false
		}
		return true
	case RestartAlways:
		return true
	default:
		return false
	}
}
