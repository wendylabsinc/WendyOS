package containerd

import (
	"context"
	"fmt"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/agent/services"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

// CameraLoopbackProvider is the narrow capability the client needs from the
// video service to keep host-side v4l2loopback camera nodes and their pump
// consumers in sync with which entitled containers are actually running.
// Declared here (mirroring AppSystemAPISocketProvider, client.go:61-65) so
// *services.VideoService satisfies it structurally without an adapter — see
// the compile-time assertion below.
type CameraLoopbackProvider interface {
	// EnsureCameraNodes creates a v4l2loopback device node for every
	// registered network camera that does not already have one.
	EnsureCameraNodes(ctx context.Context) error
	// SetCameraContainerConsumers replaces, wholesale, the set of running
	// containers entitled to camera access, so the loopback manager can
	// start or stop its pumps to match.
	SetCameraContainerConsumers(ctx context.Context, containerIDs []string)
	// SetTwoPlaneContainerConsumers replaces, wholesale, the set of running
	// containers entitled to the two-plane camera path. Since frame identity
	// moved in-band, that is the camera set; the call stays separate because
	// the two-plane pump has its own lifecycle.
	SetTwoPlaneContainerConsumers(ctx context.Context, containerIDs []string)
}

// Compile-time check that *services.VideoService satisfies CameraLoopbackProvider
// without an adapter (Task C6 wires it in directly via SetCameraLoopbackProvider).
var _ CameraLoopbackProvider = (*services.VideoService)(nil)

// SetCameraLoopbackProvider injects the manager for host-side camera loopback
// nodes and pump consumers. Called once from main.go at agent startup,
// mirroring SetAppSystemAPISocketProvider (client.go:194-197): not protected
// by c.mu, since it is set during single-threaded startup before the Client
// is exposed to concurrent callers. A nil provider (build without the ipcam
// module, or before main.go wires it in) leaves camera-loopback sync a
// complete no-op — see SyncCameraLoopbacks and the create-hook gate in
// CreateContainerWithProgress.
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
}

// cameraConsumerNames returns the names of containers that currently count
// as camera-loopback consumers: a Running task AND the camera entitlement or
// its deprecated alias, video (both map to applyCamera, entitlements.go:83).
// Labels are revalidated here rather than trusted outright — same
// defense-in-depth posture as appSystemAPIOwnersFromLabels (client.go:204-217)
// — so a container whose appID/serviceName labels fail validation is dropped
// rather than handed to the loopback manager as a consumer name.
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

// twoPlaneConsumerNames returns the names of containers entitled to the
// two-plane camera path: a Running task holding the camera entitlement (or its
// deprecated alias, video).
//
// # Why camera alone is the whole grant
//
// The path has two halves. The data plane is a v4l2loopback device node the app
// opens and reads, which is a device-node grant, and device-node grants are what
// the camera entitlement is.
//
// The control plane is frame identity: source id, sample id, canonical time and
// uncertainty. It used to travel as its own gRPC stream, which is why this gate
// once demanded a second entitlement to authorise it. It no longer does. Identity
// now rides in-band, in the v4l2 buffer timestamp the agent writes when it feeds
// the node, so the app that is entitled to the pixels receives the identity with
// them and there is no second channel to authorise.
//
// That in-band carriage is what makes one entitlement correct rather than merely
// convenient. The node is agent-fed, so the app is not an independent second
// reader: the frame it scores is provably the frame the episode recorded, which
// is the property the harness exists to deliver. An app holding camera without
// the agent-fed path keeps exactly the behaviour it has today.
func twoPlaneConsumerNames(infos []containerCameraInfo) []string {
	// Identical to the camera set, and deliberately still its own call. The two
	// gates collapsed into one when frame identity moved in-band and the second
	// entitlement went away; the two-plane pump keeps its own lifecycle, so the
	// caller still says which set it means rather than relying on them being the
	// same list forever.
	return cameraConsumerNames(infos)
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
// consumers from container truth: it lists Wendy-managed containers (the same
// label-filter pattern as RestoreAppSystemAPISockets, client.go:227), keeps
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
// Entirely best-effort: a nil provider is a silent no-op, and any error from
// the provider is logged, never returned — a camera loopback problem must
// never break container management (why callers invoke this via `go` and
// discard nothing, since there is nothing to discard).
func (c *Client) SyncCameraLoopbacks(ctx context.Context) {
	if c.cameraLoopbackProvider == nil {
		return
	}
	ctx = c.withNamespace(ctx)

	ctrs, err := c.client.Containers(ctx, fmt.Sprintf("labels.%q", labelKeyAppID))
	if err != nil {
		c.logger.Warn("camera loopback sync: listing containers failed", zap.Error(err))
		return
	}

	infos := make([]containerCameraInfo, 0, len(ctrs))
	for _, ctr := range ctrs {
		info, err := ctr.Info(ctx)
		if err != nil {
			c.logger.Warn("camera loopback sync: reading container failed", zap.String("id", ctr.ID()), zap.Error(err))
			continue
		}
		running := false
		if task, terr := ctr.Task(ctx, nil); terr == nil {
			if st, serr := task.Status(ctx); serr == nil && st.Status == containerd.Running {
				running = true
			}
		}
		infos = append(infos, containerCameraInfo{name: ctr.ID(), labels: info.Labels, running: running})
	}

	names := cameraConsumerNames(infos)

	if err := c.cameraLoopbackProvider.EnsureCameraNodes(ctx); err != nil {
		c.logger.Warn("camera loopback sync: ensuring camera nodes failed", zap.Error(err))
	}
	c.cameraLoopbackProvider.SetCameraContainerConsumers(ctx, names)
	c.cameraLoopbackProvider.SetTwoPlaneContainerConsumers(ctx, twoPlaneConsumerNames(infos))
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
