package services

import (
	"context"
	"fmt"
	"runtime"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/agent/gpudiscovery"
	"github.com/wendylabsinc/wendy/go/internal/agent/models"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// ModelDeviceProfile describes this device for model variant selection,
// using the same detection device info reports.
func ModelDeviceProfile() models.DeviceProfile {
	gpu := detectGPUInfo()
	npu := detectNPUInfo()
	_, backends := gpudiscovery.Summary(gpu.devices)
	return models.DeviceProfile{Arch: runtime.GOARCH, GPUVendor: gpu.vendor, GPUArch: gpu.gpuArch,
		ComputeBackends: backends, NPUBackends: npu.backends}
}

// ModelCameras serves models.Cameras: local V4L2 cameras from the data
// manager's sources, and their two-plane nodes from the video service.
type ModelCameras struct {
	Video *VideoService
	Data  *data.Manager
}

var _ models.Cameras = ModelCameras{}

// List returns the healthy local cameras, less those whose stream the
// two-plane path has refused: they cannot stream to a model.
func (c ModelCameras) List(ctx context.Context) []models.Camera {
	names := c.localCameraNames(ctx)
	var out []models.Camera
	for _, src := range c.Data.Sources(ctx) {
		if src.Kind == "camera" && src.Healthy && strings.HasPrefix(src.ID, "v4l2:") && !c.Video.twoPlaneSourceRefused(src.ID) {
			name := names[src.ID]
			if name == "" {
				name = src.Detail
			}
			out = append(out, models.Camera{SourceID: src.ID, Name: name})
		}
	}
	return out
}

// localCameraNames maps local source IDs to the names their devices report.
// A data source's Detail also carries the transport, which a catalog does not
// need to show.
func (c ModelCameras) localCameraNames(ctx context.Context) map[string]string {
	devices, err := c.Video.listCameras(ctx)
	if err != nil {
		return nil
	}
	names := make(map[string]string, len(devices))
	for _, dev := range devices {
		if dev.GetTransport() != agentpb.VideoTransport_VIDEO_TRANSPORT_IP {
			names["v4l2:"+dev.GetPath()] = dev.GetName()
		}
	}
	return names
}

func (c ModelCameras) Acquire(ctx context.Context, owner, sourceID string) (string, error) {
	node, err := c.Video.AcquireTwoPlaneNode(ctx, owner, sourceID)
	if err != nil {
		return "", fmt.Errorf("%w: %v", models.ErrCameraNotStreamable, err)
	}
	return node, nil
}

func (c ModelCameras) Release(ctx context.Context, owner string) {
	c.Video.ReleaseTwoPlaneNode(ctx, owner)
}
