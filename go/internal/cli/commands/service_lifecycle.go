package commands

import (
	"context"
	"os/exec"
	"sync"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

// serviceHookRunner runs the per-service "wait for readiness → announce URL →
// fire postStart" sequence for multi-service runs (compose + services map).
// A readiness failure warns and still fires explicitly configured hooks, but
// suppresses the success announcement and HTTP-entitlement-synthesized browser
// open. Context cancellation suppresses every side effect, as does a watch
// session that has already completed the sequence for this container.
//
// Zero-value-ready except conn: construct with
// &serviceHookRunner{conn: conn, opts: opts}.
type serviceHookRunner struct {
	conn *grpcclient.AgentConnection
	// opts carries the watch session state, if any. Under `wendy run --watch`
	// each service completes this sequence after its first successful readiness
	// check only.
	opts runOptions
	wg   sync.WaitGroup
	mu   sync.Mutex
	cmds []*exec.Cmd // cli-hook children to reap in attached mode
}

// runOne runs the readiness→announce→postStart sequence for a single service
// and blocks until it completes. ctx gates the readiness wait and the
// reachable-URL announcement; canceling it aborts both steps silently (no
// warning, no hook), matching Ctrl+C behavior on the single-container paths.
// hookCtx is the context the postStart CLI hook is spawned under: attached
// callers (via startAsync) pass the same runCtx as ctx, so canceling it after
// the run ends kills the hook too — mirroring run.go's
// `runCancel(); postStartCmd.Wait()`. Detached multi-service runs do not call
// runOne because they skip all host-side lifecycle work.
//
// A nil cfg, or a cfg that declares neither Readiness/Hooks nor an `http`
// entitlement, is a no-op: most services in a multi-service app don't opt
// into per-service lifecycle hooks, so runOne must not dial, warn, or print
// anything for them. Readiness and the postStart hook are both computed via
// effectiveReadiness/synthesizedOpenURLHook (the same helpers run.go's
// single-container path uses) rather than reading cfg.Readiness/cfg.Hooks
// directly: a service that declares only an `http` entitlement — no explicit
// readiness.tcpSocket — must still get an actual readiness wait and an
// auto-opened browser tab, not just the announceReachableURL text that
// already (via effectiveReadiness) assumes readiness was probed.
//
// The dial target for both readiness and the hook is resolveHookHost's
// result, not r.conn.Host directly — same reasoning as run.go's
// single-container path (see resolveHookHost): a cloud connection's Host is
// the tunnel's unresolvable asset name, while an IPv6-literal Host may be a
// rotating temporary address. IPv6 can prefer an agent-reported IP; cloud
// requires a verified mesh route because a reported LAN address is not
// necessarily reachable. resolveHookHost's announceReachableURL call replaces
// the old readiness-gated announce call below — it now runs before the
// readiness wait (not after a successful one), which mirrors run.go's
// documented tradeoff: the "App reachable at" line prints regardless of
// whether this service's probe later fails, in exchange for a probe that can
// actually reach a device at all. An active desktop VPN supplies the stable
// mesh hostname for cloud; without it, readiness and the hook are skipped
// instead of dialing a dead asset name or private device LAN address.
func (r *serviceHookRunner) runOne(ctx, hookCtx context.Context, cfg *appconfig.AppConfig) {
	if cfg == nil {
		return
	}
	readiness := effectiveReadiness(cfg)
	hooks := synthesizedOpenURLHook(cfg)
	if readiness == nil && hooks == nil {
		return
	}
	containerName := cfg.ContainerName()
	if !r.opts.beginHostLifecycle(containerName) {
		return
	}
	completed := false
	defer func() {
		if !completed {
			r.opts.abandonHostLifecycle(containerName)
		}
	}()

	hookHost, hostOK := resolveHookHost(ctx, r.conn, cfg)
	if !hostOK {
		// cfg.ServiceName is "" for the app-level fallback config (see
		// appLevelLifecycleConfig); containerDisplayName falls back to the
		// bare AppID in that case rather than printing a dangling "for :".
		cliNotice("Skipping postStart hook for %s: Wendy Mesh is not active; connect the desktop VPN to reach this cloud device.", containerDisplayName(cfg))
		return
	}

	readinessSucceeded := true
	if err := waitForReadiness(ctx, readiness, hookHost, cloudHTTPReadinessPort(r.conn, cfg, readiness)); err != nil {
		if ctx.Err() != nil {
			// Canceled (e.g. Ctrl+C, or the run ending) — stay silent and skip
			// the hook entirely; this is not a readiness failure to report.
			return
		}
		// containerExitDetail (invoked by warnReadiness) matches on the GROUP
		// appID: the agent's ListContainers groups per-service containers under
		// the group app-ID label, reports AppContainer.AppName as the bare
		// group appID, and aggregates exit code/reason onto that group entry —
		// so pass cfg.AppID, never cfg.ContainerName().
		warnReadiness(ctx, r.conn, cfg.AppID, err)
		readinessSucceeded = false
	}
	if ctx.Err() != nil {
		return
	}
	// Watch claims host-side actions only after readiness succeeds. A failed
	// attempt releases the claim so a later deploy can try again.
	if r.opts.isWatch() && !readinessSucceeded {
		return
	}

	effectiveCfg := cfg
	// A failed probe must not synthesize an automatic browser open from an HTTP
	// entitlement. Explicit hooks still run after a non-cancellation timeout.
	if readinessSucceeded && hooks != cfg.Hooks {
		clone := *cfg
		clone.Hooks = hooks
		effectiveCfg = &clone
	}
	if ctx.Err() != nil {
		return
	}
	r.opts.completeHostLifecycle(containerName)
	completed = true

	cmd := startPostStartHook(hookCtx, effectiveCfg, hookHost, cfg.ServiceName)
	if cmd != nil {
		if r.opts.isWatch() {
			r.opts.watchState.reapCommand(cmd)
			return
		}
		r.mu.Lock()
		r.cmds = append(r.cmds, cmd)
		r.mu.Unlock()
	}
}

// startAsync runs runOne on a goroutine tracked by r.wg, using runCtx as both
// the readiness/announce context and the postStart hook's context. Attached
// multi-service paths use this so a slow or failing readiness probe on one
// service never delays starting the next; canceling runCtx once the overall
// run ends also terminates any still-running cli hook, so callers should
// follow up with reap() the same way run.go cancels runCtx before waiting on
// postStartCmd.
func (r *serviceHookRunner) startAsync(runCtx context.Context, cfg *appconfig.AppConfig) {
	hookCtx := runCtx
	if r.opts.isWatch() {
		hookCtx = r.opts.watchState.hookContext(runCtx)
	}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.runOne(runCtx, hookCtx, cfg)
	}()
}

// spawn runs fn on a goroutine tracked by r.wg, so a later reap() waits for it
// and any cli-hook children it registers via runOne. Unlike startAsync (which
// wraps a single runOne), spawn lets the caller gate runOne behind custom
// preconditions while keeping it part of the run's teardown — the compose
// app-level fallback uses it to wait for every service's Started ack, then
// re-check runCtx, before firing. Because reap() runs only after those Started
// acks have all been released and runCtx has been canceled, the spawned
// goroutine can never block reap indefinitely.
func (r *serviceHookRunner) spawn(fn func()) {
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		fn()
	}()
}

// reap waits for every startAsync'd runOne to finish, then waits on each
// tracked cli-hook child so its exit status is collected and no zombie is
// left behind. Callers invoke it after canceling runCtx, mirroring run.go's
// `runCancel(); postStartCmd.Wait()`. Wait errors are ignored — the caller
// only needs the process reaped, not its exit status.
func (r *serviceHookRunner) reap() {
	r.wg.Wait()
	r.mu.Lock()
	cmds := r.cmds
	r.mu.Unlock()
	for _, cmd := range cmds {
		_ = cmd.Wait()
	}
}

// lifecycleConfig returns a private synthetic AppConfig for CLI-side lifecycle
// execution. Only HTTP entitlements declared at this exact scope are copied;
// all other entitlements belong in the container create payload, not in the
// readiness/browser pipeline.
func lifecycleConfig(appID, serviceName string, readiness *appconfig.ReadinessConfig, hooks *appconfig.HooksConfig, entitlements []appconfig.Entitlement) *appconfig.AppConfig {
	httpEntitlements := make([]appconfig.Entitlement, 0, 1)
	for _, entitlement := range entitlements {
		if entitlement.Type == appconfig.EntitlementHTTP {
			httpEntitlements = append(httpEntitlements, entitlement)
		}
	}
	if readiness == nil && hooks == nil && len(httpEntitlements) == 0 {
		return nil
	}
	return &appconfig.AppConfig{
		AppID:        appID,
		ServiceName:  serviceName,
		Entitlements: httpEntitlements,
		Readiness:    readiness,
		Hooks:        hooks,
	}
}

// appLevelLifecycleConfig returns a private config carrying the top-level HTTP
// entitlement, readiness (including timeoutSeconds), and hooks. It fires once
// after ALL services have started. Its hooks.postStart.agent is never sent to
// the agent because there is no app-level container; validation/callers warn.
func appLevelLifecycleConfig(appID string, top *appconfig.AppConfig) *appconfig.AppConfig {
	if top == nil {
		return nil
	}
	return lifecycleConfig(appID, "", top.Readiness, top.Hooks, top.Entitlements)
}
