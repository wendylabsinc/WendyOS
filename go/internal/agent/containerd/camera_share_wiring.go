package containerd

import (
	"context"

	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/agent/services"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

// Camera sharing's container lifecycle. A container created with the camera
// entitlement gets its shares when it is created, has them covered over its
// cameras' paths between NewTask and Start at every start, lets go of its
// mount namespace when its task ends, and has its shares removed when it is
// deleted or replaced. The share manager is the video service
// (services/camera_share.go); everything here only says when.

// CameraShareProvider is the share manager the client drives. Declared here,
// like CameraLoopbackProvider, so *services.VideoService satisfies it
// structurally and tests can record the calls.
type CameraShareProvider interface {
	// CreateCameraShares gives a newly created container its shares, within
	// its own budget; it logs and never fails.
	CreateCameraShares(ctx context.Context, containerID, appID string)
	// CoverCameraShares covers the container's cameras in the new task pid,
	// between NewTask and Start; it never returns later than its own budget,
	// and its errors are only for the caller's log.
	CoverCameraShares(ctx context.Context, containerID string, pid uint32) []error
	// ReleaseCameraShares lets go of the container's mount namespace, for a
	// task that ended.
	ReleaseCameraShares(containerID string)
	// RemoveCameraShares removes the container's shares, for a container that
	// was deleted or replaced. It does not wait for the nodes to go.
	RemoveCameraShares(containerID string)
	// SyncCameraShares squares the shares with the containers that exist.
	SyncCameraShares(ctx context.Context, containers []services.CameraShareContainer)
}

var _ CameraShareProvider = (*services.VideoService)(nil)

// labelKeyCameraShare records, at create, how a camera app gets its camera:
// appconfig.CameraShareShared or appconfig.CameraShareDirect. It is sharing's
// marker for R4: a container made before sharing existed has none, so it is
// never passed to the share manager and keeps the real cameras.
const labelKeyCameraShare = "sh.wendy/camera.share"

// SetCameraShareProvider injects the share manager. Called once from main.go
// during startup, before the Client is used concurrently, like
// SetCameraLoopbackProvider. Without one (no video service, other platforms)
// every share call is a no-op and camera apps get the real cameras.
func (c *Client) SetCameraShareProvider(p CameraShareProvider) {
	if v, ok := p.(*services.VideoService); ok && v == nil {
		// A nil pointer in a non-nil interface would pass every nil check
		// and then panic (WDY-3835's class of bug).
		p = nil
	}
	c.cameraShareProvider = p
}

// addCameraShareLabel sets labelKeyCameraShare for a camera app, from its
// entitlements; an app without a camera gets no label.
func addCameraShareLabel(labels map[string]string, entitlements []appconfig.Entitlement) {
	if choice := appconfig.CameraShareChoice(entitlements); choice != "" {
		labels[labelKeyCameraShare] = choice
	}
}

// sharesCamera reports whether a container's labels mark it as sharing its
// cameras. Only such containers are ever passed to the share manager.
func sharesCamera(labels map[string]string) bool {
	return labels[labelKeyCameraShare] == appconfig.CameraShareShared
}

// cameraShareContainers is what SyncCameraShares is given: every container
// labelled "shared", running or not, with a running one's task pid.
func cameraShareContainers(infos []containerCameraInfo) []services.CameraShareContainer {
	var out []services.CameraShareContainer
	for _, info := range infos {
		if !sharesCamera(info.labels) {
			continue
		}
		sc := services.CameraShareContainer{ID: info.name, Running: info.running}
		if info.running {
			sc.PID = info.pid
		}
		out = append(out, sc)
	}
	return out
}

func (c *Client) createCameraShares(ctx context.Context, containerID, appID string) {
	if c.cameraShareProvider == nil {
		return
	}
	c.cameraShareProvider.CreateCameraShares(ctx, containerID, appID)
}

// coverCameraShares covers a sharing container's cameras in its new task,
// whose entrypoint the runtime still holds. Each error is logged and dropped:
// sharing never stops or delays a start (R2), and a camera not covered is the
// real camera. It reports whether a cover was attempted, which the start then
// releases if it fails.
func (c *Client) coverCameraShares(ctx context.Context, containerID, appName string, pid uint32) bool {
	if c.cameraShareProvider == nil {
		return false
	}
	for _, err := range c.cameraShareProvider.CoverCameraShares(ctx, containerID, pid) {
		c.logger.Warn("camera share not applied; the app starts with the real camera",
			zap.String("app_name", appName), zap.Error(err))
	}
	return true
}

func (c *Client) releaseCameraShares(containerID string) {
	if c.cameraShareProvider == nil {
		return
	}
	c.cameraShareProvider.ReleaseCameraShares(containerID)
}

func (c *Client) removeCameraShares(containerID string) {
	if c.cameraShareProvider == nil {
		return
	}
	c.cameraShareProvider.RemoveCameraShares(containerID)
}

// syncCameraShares passes the listed sharing containers to the share manager.
// A listing that could not read every container is not passed on: the share
// manager takes a container missing from the list for deleted and removes its
// shares, so a passing read error must not cost a running app its cameras.
// The next sync tries again.
func (c *Client) syncCameraShares(ctx context.Context, infos []containerCameraInfo, complete bool) {
	if c.cameraShareProvider == nil {
		return
	}
	if !complete {
		c.logger.Warn("camera share sync skipped: not every container could be read")
		return
	}
	c.cameraShareProvider.SyncCameraShares(ctx, cameraShareContainers(infos))
}
