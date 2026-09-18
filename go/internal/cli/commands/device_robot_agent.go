package commands

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/robotprobe"
	"github.com/wendylabsinc/wendy/go/internal/shared/streamreason"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// agentHostFacts adapts the agent's device-info RPC to robotprobe.HostFactsSource. It is
// the only place that knows the agent's wire format, which is what keeps the probes
// themselves free of proto types and testable with a literal.
//
// The fetch is cached: four probes read these facts, and a report should interrogate the
// device once rather than once per section.
type agentHostFacts struct {
	conn *grpcclient.AgentConnection

	once  sync.Once
	facts *robotprobe.HostFacts
	err   error

	hardwareOnce sync.Once
	hardware     []robotprobe.HardwareDevice
	hardwareErr  error

	camerasOnce sync.Once
	cameras     []robotprobe.CameraDevice
	camerasErr  error
}

func newAgentHostFacts(conn *grpcclient.AgentConnection) *agentHostFacts {
	return &agentHostFacts{conn: conn}
}

func (a *agentHostFacts) HostFacts(ctx context.Context) (*robotprobe.HostFacts, error) {
	a.once.Do(func() {
		resp, err := a.conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
		if err != nil {
			a.err = fmt.Errorf("reading device info: %w", err)
			return
		}
		a.facts = hostFactsFromAgent(resp)
	})
	return a.facts, a.err
}

// hostFactsFromAgent flattens the agent's response. Optional fields arrive as pointers,
// and an absent one stays absent rather than becoming a zero that reads as a measurement.
func hostFactsFromAgent(resp *agentpb.GetAgentVersionResponse) *robotprobe.HostFacts {
	facts := &robotprobe.HostFacts{
		Hostname:         resp.GetHostname(),
		DeviceType:       resp.GetDeviceType(),
		CPUArchitecture:  resp.GetCpuArchitecture(),
		CPUCount:         int(resp.GetCpuCount()),
		MemoryTotalBytes: resp.GetMemTotalBytes(),
		OS:               resp.GetOs(),
		OSVersion:        resp.GetOsVersion(),
		AgentVersion:     resp.GetVersion(),
		HasGPU:           resp.GetHasGpu(),
		GPUVendor:        resp.GetGpuVendor(),
		GPUArch:          resp.GetGpuArch(),
		CUDAVersion:      resp.GetCudaVersion(),
		JetpackVersion:   resp.GetJetpackVersion(),
		HasNPU:           resp.GetHasNpu(),
		NPUVendor:        resp.GetNpuVendor(),
		DiskTotalBytes:   resp.GetDiskTotalBytes(),
		DiskUsedBytes:    resp.GetDiskUsedBytes(),
		StorageMedium:    resp.GetStorageMedium(),
	}

	for _, capability := range resp.GetGpuCapabilities() {
		facts.ComputeBackends = append(facts.ComputeBackends, capability.GetComputeBackends()...)
	}
	if cs := resp.GetContainerStorage(); cs != nil {
		facts.ContainerStorage = &robotprobe.Partition{
			Mountpoint: cs.GetMountpoint(),
			TotalBytes: cs.GetTotalBytes(),
			UsedBytes:  cs.GetUsedBytes(),
		}
	}
	for _, iface := range resp.GetNetworkInterfaces() {
		facts.Interfaces = append(facts.Interfaces, robotprobe.HostInterface{
			Name:      iface.GetName(),
			Addresses: iface.GetIpAddresses(),
		})
	}
	if battery := resp.GetBattery(); battery != nil {
		facts.Battery = &robotprobe.BatteryFacts{
			Percent: battery.GetPercent(),
			State:   battery.GetState().String(),
		}
	}
	return facts
}

// Hardware enumerates attached devices, caching for the same reason as HostFacts.
func (a *agentHostFacts) Hardware(ctx context.Context) ([]robotprobe.HardwareDevice, error) {
	a.hardwareOnce.Do(func() {
		resp, err := a.conn.AgentService.ListHardwareCapabilities(ctx,
			&agentpb.ListHardwareCapabilitiesRequest{})
		if err != nil {
			a.hardwareErr = fmt.Errorf("enumerating hardware: %w", err)
			return
		}
		for _, capability := range resp.GetCapabilities() {
			a.hardware = append(a.hardware, robotprobe.HardwareDevice{
				Category:    capability.GetCategory(),
				DevicePath:  capability.GetDevicePath(),
				Description: capability.GetDescription(),
			})
		}
	})
	return a.hardware, a.hardwareErr
}

// DeviceClock reads the device's wall clock and times the round trip. It is deliberately
// not cached: unlike an inventory, a clock reading is only true at the moment it is taken.
func (a *agentHostFacts) DeviceClock(ctx context.Context) (time.Time, time.Duration, error) {
	started := time.Now()
	resp, err := a.conn.TimeSyncService.GetClock(ctx, &agentpbv2.GetClockRequest{})
	roundTrip := time.Since(started)
	if err != nil {
		return time.Time{}, roundTrip, fmt.Errorf("reading the device clock: %w", err)
	}
	return time.Unix(0, resp.GetUnixNanos()).UTC(), roundTrip, nil
}

// Cameras enumerates every camera the agent knows about, whatever transport it arrives
// on. Cached, like the other inventories.
func (a *agentHostFacts) Cameras(ctx context.Context) ([]robotprobe.CameraDevice, error) {
	a.camerasOnce.Do(func() {
		resp, err := a.conn.VideoService.ListVideoDevices(ctx, &agentpb.ListVideoDevicesRequest{})
		if err != nil {
			a.camerasErr = fmt.Errorf("listing cameras: %w", err)
			return
		}
		for _, device := range resp.GetDevices() {
			a.cameras = append(a.cameras, robotprobe.CameraDevice{
				StableID:  device.GetStableId(),
				Name:      device.GetName(),
				Path:      device.GetPath(),
				Model:     device.GetModel(),
				Driver:    device.GetDriver(),
				Transport: transportName(device.GetTransport()),
				Topic:     device.GetTopic(),
				Online:    device.GetOnline(),
			})
		}
	})
	return a.cameras, a.camerasErr
}

// SampleCamera streams frames for at most window, stopping at maxFrames. The frame count
// is capped because this runs over a cloud tunnel as readily as on a LAN, and an
// uncompressed stream is expensive to move.
func (a *agentHostFacts) SampleCamera(ctx context.Context, stableID string, window time.Duration, maxFrames int) ([]robotprobe.CameraFrame, error) {
	streamCtx, stop := context.WithTimeout(ctx, window)
	defer stop()

	stream, err := a.conn.VideoService.StreamVideo(streamCtx, &agentpb.StreamVideoRequest{
		StableId: stableID,
	})
	if err != nil {
		return nil, classifyCameraError(err)
	}

	var frames []robotprobe.CameraFrame
	for maxFrames <= 0 || len(frames) < maxFrames {
		frame, err := stream.Recv()
		if err != nil {
			// The window closing is the normal way this ends, so whatever arrived is
			// the answer. A failure with nothing to show is reported.
			if len(frames) > 0 || errors.Is(streamCtx.Err(), context.DeadlineExceeded) {
				break
			}
			return nil, classifyCameraError(err)
		}
		captured := robotprobe.CameraFrame{
			Codec:       codecName(frame.GetCodec()),
			TimestampNs: frame.GetTimestampNs(),
			ReceivedAt:  time.Now(),
		}
		if raw := frame.GetRawFormat(); raw != nil {
			captured.Width = raw.GetWidth()
			captured.Height = raw.GetHeight()
			captured.Fourcc = raw.GetFourcc()
		}
		frames = append(frames, captured)
	}
	return frames, nil
}

// classifyCameraError marks the one case the report can name a cause for. The agent
// already publishes a machine-readable reason for a camera in use, which is exactly what
// shared/streamreason exists for, so this reads that rather than matching on message
// text; a FailedPrecondition is the fallback for an older agent.
func classifyCameraError(err error) error {
	if streamreason.Has(err, streamreason.CameraInUse) || status.Code(err) == codes.FailedPrecondition {
		return fmt.Errorf("%w: %v", robotprobe.ErrDeviceBusy, err)
	}
	return err
}

// transportName and codecName render the agent's enums without the wire prefixes, so the
// report reads "USB" rather than "VIDEO_TRANSPORT_USB". The core never interprets either.
func transportName(transport agentpb.VideoTransport) string {
	return strings.TrimPrefix(transport.String(), "VIDEO_TRANSPORT_")
}

func codecName(codec agentpb.VideoCodec) string {
	return strings.TrimPrefix(codec.String(), "VIDEO_CODEC_")
}
