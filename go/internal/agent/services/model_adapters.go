package services

import (
	"context"
	"fmt"
	"runtime"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/agent/gpudiscovery"
	"github.com/wendylabsinc/wendy/go/internal/agent/models"
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

func (c ModelCameras) List(ctx context.Context) []models.Camera {
	var out []models.Camera
	for _, src := range c.Data.Sources(ctx) {
		if src.Kind == "camera" && src.Healthy && strings.HasPrefix(src.ID, "v4l2:") {
			out = append(out, models.Camera{SourceID: src.ID, Name: src.Detail})
		}
	}
	return out
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
