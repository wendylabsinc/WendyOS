package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
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

	topicsOnce sync.Once
	topics     []robotprobe.ROS2Topic
	topicsErr  error

	nodesOnce sync.Once
	nodes     []robotprobe.ROS2Node
	nodesErr  error

	// ddsInterface and ddsSettle are passed to every raw topic read, since the
	// robot's graph is often not on the interface a route would pick.
	ddsInterface string
	ddsSettle    time.Duration

	// lost holds the first transport failure, so a connection that dropped mid-run
	// can be reported once instead of once per probe.
	lostOnce sync.Once
	lost     error
}

func newAgentHostFacts(conn *grpcclient.AgentConnection, ddsInterface string, ddsSettle time.Duration) *agentHostFacts {
	return &agentHostFacts{conn: conn, ddsInterface: ddsInterface, ddsSettle: ddsSettle}
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
func (a *agentHostFacts) SampleCamera(ctx context.Context, stableID string, window time.Duration, maxFrames int, mode robotprobe.CameraMode) ([]robotprobe.CameraFrame, error) {
	streamCtx, stop := context.WithTimeout(ctx, window)
	defer stop()

	stream, err := a.conn.VideoService.StreamVideo(streamCtx, &agentpb.StreamVideoRequest{
		StableId: stableID,
		// Zero means the device default, which is what an inventory pass wants.
		Width:     mode.Width,
		Height:    mode.Height,
		Framerate: mode.Framerate,
	})
	if err != nil {
		return nil, a.classifyCameraError(err)
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
			return nil, a.classifyCameraError(err)
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
func (a *agentHostFacts) classifyCameraError(err error) error {
	if streamreason.Has(err, streamreason.CameraInUse) || status.Code(err) == codes.FailedPrecondition {
		return fmt.Errorf("%w: %s", robotprobe.ErrDeviceBusy, a.noteFailure(err))
	}
	return errors.New(a.noteFailure(err))
}

// noteFailure renders an agent error for a report and, on the way, remembers a connection
// that has gone away.
//
// Every RPC in this file formats its failure here, which makes it the one place that sees
// them all. That matters because a tunnel that drops mid-run fails every probe
// individually: the reader gets a wall of identical handshake errors and no statement that
// the connection itself is what broke, which reads as a robot with seven broken subsystems.
func (a *agentHostFacts) noteFailure(err error) string {
	if code := status.Code(err); code == codes.Unavailable || code == codes.DeadlineExceeded {
		a.lostOnce.Do(func() { a.lost = err })
	}
	return agentMessage(err)
}

// lostConnection reports the first transport failure seen, if any.
func (a *agentHostFacts) lostConnection() error {
	if a == nil {
		return nil
	}
	a.lostOnce.Do(func() {})
	return a.lost
}

// agentMessage strips the gRPC envelope so the report carries what the agent said rather
// than "rpc error: code = Internal desc = ...". The reason ends up inside a condition on
// a measurement, where the wrapper is noise that pushes the actual cause off the line.
func agentMessage(err error) string {
	if st, ok := status.FromError(err); ok {
		return st.Message()
	}
	return err.Error()
}

// transportName and codecName render the agent's enums without the wire prefixes, so the
// report reads "USB" rather than "VIDEO_TRANSPORT_USB". The core never interprets either.
func transportName(transport agentpb.VideoTransport) string {
	return strings.TrimPrefix(transport.String(), "VIDEO_TRANSPORT_")
}

func codecName(codec agentpb.VideoCodec) string {
	return strings.TrimPrefix(codec.String(), "VIDEO_CODEC_")
}

// ROS2Topics and ROS2Nodes read the robot's graph through the agent's ROS 2 service.
// The client is built here from the shared connection, the way the ros2 command does,
// rather than widening AgentConnection for one caller.
//
// which runs the ros2 CLI in a sidecar on the device. That is why this works over a
// cloud tunnel: DDS discovery is multicast and never leaves the robot's own network, but
// the agent is already standing inside it.
//
// Both are cached: the graph does not change between two probes of one pass, and listing
// it costs a round trip plus a process on the device.
func (a *agentHostFacts) ROS2Topics(ctx context.Context) ([]robotprobe.ROS2Topic, error) {
	a.topicsOnce.Do(func() {
		resp, err := agentpbv2.NewROS2ServiceClient(a.conn.Conn).ListTopics(ctx,
			&agentpbv2.ListROS2TopicsRequest{IncludeCounts: true})
		if err != nil {
			a.topicsErr = fmt.Errorf("listing ROS 2 topics: %s", a.noteFailure(err))
			return
		}
		for _, topic := range resp.GetTopics() {
			a.topics = append(a.topics, robotprobe.ROS2Topic{
				Name:            topic.GetName(),
				Types:           topic.GetTypes(),
				PublisherCount:  int(topic.GetPublisherCount()),
				SubscriberCount: int(topic.GetSubscriberCount()),
				RMW:             topic.GetRmw(),
			})
		}
	})
	return a.topics, a.topicsErr
}

func (a *agentHostFacts) ROS2Nodes(ctx context.Context) ([]robotprobe.ROS2Node, error) {
	a.nodesOnce.Do(func() {
		resp, err := agentpbv2.NewROS2ServiceClient(a.conn.Conn).ListNodes(ctx,
			&agentpbv2.ListROS2NodesRequest{})
		if err != nil {
			a.nodesErr = fmt.Errorf("listing ROS 2 nodes: %s", a.noteFailure(err))
			return
		}
		for _, node := range resp.GetNodes() {
			a.nodes = append(a.nodes, robotprobe.ROS2Node{
				Name:      node.GetName(),
				Namespace: node.GetNamespace(),
				RMW:       node.GetRmw(),
			})
		}
	})
	return a.nodes, a.nodesErr
}

// LiveHostStats reads the host's moment-to-moment state through the same resource-stats
// RPC `wendy device top` uses. Not cached: unlike an inventory, a temperature is only
// true when it is read.
func (a *agentHostFacts) LiveHostStats(ctx context.Context) (*robotprobe.LiveHostStats, error) {
	resp, err := a.conn.ContainerService.GetResourceStats(ctx, &agentpb.GetResourceStatsRequest{})
	if err != nil {
		return nil, fmt.Errorf("reading resource stats: %s", a.noteFailure(err))
	}
	host := resp.GetHost()
	if host == nil {
		return nil, fmt.Errorf("the agent returned no host stats")
	}

	stats := &robotprobe.LiveHostStats{MemoryAvailableBytes: host.GetMemAvailableBytes()}
	for _, zone := range host.GetThermalZones() {
		stats.ThermalZones = append(stats.ThermalZones, robotprobe.ThermalZone{
			Name: zone.GetName(), Celsius: zone.GetTempC(),
		})
	}
	for _, gpu := range host.GetGpus() {
		live := robotprobe.GPUStats{
			Index:           gpu.GetIndex(),
			Name:            gpu.GetName(),
			UtilPercent:     gpu.GetUtilPercent(),
			MemoryUsedBytes: gpu.GetMemUsedBytes(),
		}
		// A GPU reporting no temperature is different from one reporting zero, so the
		// optional field stays optional rather than collapsing to 0.
		if gpu.TempC != nil {
			celsius := gpu.GetTempC()
			live.Celsius = &celsius
		}
		stats.GPUs = append(stats.GPUs, live)
	}
	return stats, nil
}

// errRawTopicUnsupported marks an agent too old to serve raw topic samples. It is a
// sentinel rather than a message because the caller acts on it: it is the one failure that
// justifies falling back to a participant on this machine.
var errRawTopicUnsupported = errors.New("the agent does not serve raw topic samples")

// Sample reads a topic's raw payloads through the agent, which satisfies
// robotprobe.TopicReader. This is what lets the body probes — joints, the robot's own
// battery, its hands — run from a laptop: their data lives on the robot's internal
// network, which DDS discovery never leaves, and the agent is already standing inside it.
//
// The decoding stays on this side deliberately. One raw RPC serves every vendor message on
// every robot, where a decoded RPC would need extending, redeploying and version-matching
// for each new message a robot speaks.
func (a *agentHostFacts) Sample(ctx context.Context, topic, typeName string, window time.Duration, maxMessages int) ([][]byte, error) {
	stream, err := agentpbv2.NewROS2ServiceClient(a.conn.Conn).StreamRawTopic(ctx, &agentpbv2.StreamRawTopicRequest{
		Topic: topic,
		Type:  typeName,
		// The robot's graph is not always on the interface a route would choose, so
		// the caller's choice is passed through rather than inferred on the device.
		Interface:  a.ddsInterface,
		SettleMs:   uint32(a.ddsSettle.Milliseconds()),
		DurationMs: uint32(window.Milliseconds()),
		MaxSamples: uint32(maxMessages),
	})
	if err != nil {
		return nil, a.rawTopicError(topic, err)
	}

	var payloads [][]byte
	for {
		sample, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			if status.Code(err) == codes.NotFound {
				// Nobody publishes it. That is an answer about the robot, and the
				// probe turns it into a finding rather than an error.
				return payloads, nil
			}
			if len(payloads) > 0 {
				// A stream that carried samples and then broke has already
				// answered the question it was asked.
				break
			}
			return nil, a.rawTopicError(topic, err)
		}
		payloads = append(payloads, sample.GetPayload())
		if maxMessages > 0 && len(payloads) >= maxMessages {
			break
		}
	}
	return payloads, nil
}

// RawTopics lists the topics the agent's own participant can see, which is how the body
// probes find out whether a robot publishes anything worth reading.
//
// This deliberately does not go through ROS2Topics: that runs `ros2 topic list` in a
// sidecar and so needs a ROS 2 container deployed on the device. Gating the body probes on
// it would make them unavailable on exactly the robots they exist for — a G1 with no Wendy
// app running still publishes its whole body, and the agent can see it.
func (a *agentHostFacts) RawTopics(ctx context.Context) ([]robotprobe.RawTopic, error) {
	resp, err := agentpbv2.NewROS2ServiceClient(a.conn.Conn).ListRawTopics(ctx, &agentpbv2.ListRawTopicsRequest{
		Interface: a.ddsInterface,
		SettleMs:  uint32(a.ddsSettle.Milliseconds()),
	})
	if err != nil {
		if status.Code(err) == codes.Unimplemented {
			return nil, errRawTopicUnsupported
		}
		return nil, fmt.Errorf("listing raw topics: %s", a.noteFailure(err))
	}
	topics := make([]robotprobe.RawTopic, 0, len(resp.GetTopics()))
	for _, topic := range resp.GetTopics() {
		topics = append(topics, robotprobe.RawTopic{
			Name:        topic.GetName(),
			Type:        topic.GetType(),
			WriterCount: int(topic.GetWriterCount()),
		})
	}
	return topics, nil
}

// rawTopicError keeps an old agent distinguishable from a real failure. Everything else
// is flattened to a message, because by this point the gRPC status carries nothing the
// reader of a report can act on.
func (a *agentHostFacts) rawTopicError(topic string, err error) error {
	if status.Code(err) == codes.Unimplemented {
		return errRawTopicUnsupported
	}
	return fmt.Errorf("reading %s: %s", topic, a.noteFailure(err))
}
