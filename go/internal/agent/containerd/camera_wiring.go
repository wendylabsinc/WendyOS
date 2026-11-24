package containerd

import (
	"context"
	"fmt"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/errdefs"
	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/agent/services"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

// CameraLoopbackProvider is the narrow capability the client needs from the
// video service to keep the host-side v4l2loopback nodes of network cameras
// and their pump consumers in sync with which entitled containers are running.
type CameraLoopbackProvider interface {
	// EnsureCameraNodes creates a v4l2loopback device node for every
	// registered network camera that does not already have one.
	EnsureCameraNodes(ctx context.Context) error
	// SetCameraContainerConsumers replaces, wholesale, the set of running
	// containers entitled to camera access, so the loopback manager can
	// start or stop its pumps to match.
	SetCameraContainerConsumers(ctx context.Context, containerIDs []string)
}

var _ CameraLoopbackProvider = (*services.VideoService)(nil)

// SetCameraLoopbackProvider injects the manager for host-side camera loopback
// nodes and pump consumers. It is not protected by c.mu: it must be called
// during single-threaded startup, before the Client is exposed to concurrent
// callers. With a nil provider, camera-loopback sync does nothing.
func (c *Client) SetCameraLoopbackProvider(p CameraLoopbackProvider) {
	c.cameraLoopbackProvider = p
}

// containerCameraInfo is the minimal container truth cameraConsumerNames
// needs: enough to decide whether a container currently counts as a camera
// consumer, without requiring a live containerd connection to test.
type containerCameraInfo struct {
	name    string
	labels  map[string]string
	running bool
	pid     uint32 // the task's pid, for a running task only
}

// cameraConsumerNames returns the names of containers that currently count
// as camera-loopback consumers: a Running task AND the camera entitlement or
// its deprecated alias, video. Labels are revalidated here rather than trusted
// outright, so a container whose appID/serviceName labels fail validation is
// dropped rather than handed to the loopback manager as a consumer name.
func cameraConsumerNames(infos []containerCameraInfo) []string {
	var names []string
	for _, info := range infos {
		if !info.running {
			continue
		}
		appID := info.labels[labelKeyAppID]
		serviceName := info.labels[labelKeyServiceName]
		if appconfig.ValidateAppID(appID) != nil || (serviceName != "" && appconfig.ValidateServiceName(serviceName) != nil) {
			continue
		}
		entitlements := parseEntitlementsFromAnnotations(info.labels)
		if entitlementsContain(entitlements, appconfig.EntitlementCamera) || entitlementsContain(entitlements, appconfig.EntitlementVideo) {
			names = append(names, info.name)
		}
	}
	return names
}

// shouldEnsureCameraNodes reports whether appCfg's entitlements include the
// camera entitlement or its deprecated alias, video — the create-hook gate in
// CreateContainerWithProgress that decides whether pre-creating v4l2loopback
// nodes is worth attempting for this app at all.
func shouldEnsureCameraNodes(appCfg *appconfig.AppConfig) bool {
	if appCfg == nil {
		return false
	}
	return appCfg.HasEntitlement(appconfig.EntitlementCamera) || appCfg.HasEntitlement(appconfig.EntitlementVideo)
}

// SyncCameraLoopbacks recomputes v4l2loopback node existence and pump
// consumers from container truth: it lists Wendy-managed containers, keeps
// the ones with a Running task and a camera/video entitlement via
// cameraConsumerNames, then calls EnsureCameraNodes and
// SetCameraContainerConsumers on the registered provider.
//
// This is the ONE truth-driven path: callers never reason about incremental
// add/remove, they just call this after anything that could change the
// entitled-and-running set (see the lifecycle nudges in StartContainer,
// StartContainerWithStdin, stopOne, and DeleteContainer) or on a fixed
// interval (RunCameraLoopbackSync), which also catches crash-exits the
// lifecycle nudges cannot see.
//
// The same listing feeds the camera share manager (syncCameraShares): every
// container labelled "shared", running or not, with a running task's pid.
//
// Entirely best-effort: with neither provider it is a silent no-op, and any
// error from a provider is logged, never returned — a camera loopback problem
// must never break container management (why callers invoke this via `go` and
// discard nothing, since there is nothing to discard).
func (c *Client) SyncCameraLoopbacks(ctx context.Context) {
	if c.cameraLoopbackProvider == nil && c.cameraShareProvider == nil {
		return
	}
	ctx = c.withNamespace(ctx)

	ctrs, err := c.client.Containers(ctx, fmt.Sprintf("labels.%q", labelKeyAppID))
	if err != nil {
		c.logger.Warn("camera loopback sync: listing containers failed", zap.Error(err))
		return
	}

	infos, complete := c.cameraInfos(ctx, ctrs)

	if c.cameraLoopbackProvider != nil {
		if err := c.cameraLoopbackProvider.EnsureCameraNodes(ctx); err != nil {
			c.logger.Warn("camera loopback sync: ensuring camera nodes failed", zap.Error(err))
		}
		c.cameraLoopbackProvider.SetCameraContainerConsumers(ctx, cameraConsumerNames(infos))
	}
	c.syncCameraShares(ctx, infos, complete)
}

// cameraInfos reads each listed container's labels and task state. complete
// is false when a container that still exists could not be read; one deleted
// since the listing is simply left out, since it is gone either way.
func (c *Client) cameraInfos(ctx context.Context, ctrs []containerd.Container) (infos []containerCameraInfo, complete bool) {
	infos = make([]containerCameraInfo, 0, len(ctrs))
	complete = true
	for _, ctr := range ctrs {
		info, err := ctr.Info(ctx)
		if errdefs.IsNotFound(err) {
			continue
		}
		if err != nil {
			c.logger.Warn("camera loopback sync: reading container failed", zap.String("id", ctr.ID()), zap.Error(err))
			complete = false
			continue
		}
		running := false
		var pid uint32
		if task, terr := ctr.Task(ctx, nil); terr == nil {
			if st, serr := task.Status(ctx); serr == nil && st.Status == containerd.Running {
				running = true
				pid = task.Pid()
			}
		}
		infos = append(infos, containerCameraInfo{name: ctr.ID(), labels: info.Labels, running: running, pid: pid})
	}
	return infos, complete
}

// RunCameraLoopbackSync runs SyncCameraLoopbacks on a fixed interval until ctx
// is done. It does not sync immediately on entry — main.go issues the
// agent-restart reconcile sync itself before launching this loop via
// `go RunCameraLoopbackSync(ctx, time.Minute)`, so an immediate sync here
// would be redundant. The ticker's job is purely to catch drift the lifecycle
// nudges miss, e.g. a container's task crash-exiting on its own between
// nudges.
func (c *Client) RunCameraLoopbackSync(ctx context.Context, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.SyncCameraLoopbacks(ctx)
		}
	}
}
