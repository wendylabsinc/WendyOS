package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/internal/shared/unitree"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

func (s *mcpServer) registerRobotCameraTools(srv *server.MCPServer) {
	opts := []mcpgo.ToolOption{
		mcpgo.WithDescription("Capture one image from the paired Go2's onboard camera. Use this for seeing or finding objects around the robot, even when camera_list reports no cameras on the Wendy gateway. Returns an actual JPEG and gateway receipt freshness metadata. Requires camera approval. Does not connect, pair, move, or deploy anything. The robot must already be connected. Local GStreamer with H.264 decoding and JPEG encoding is required."),
		mcpgo.WithString("serial", mcpgo.Description("Saved paired robot serial. May be omitted when exactly one paired robot is saved.")),
		mcpgo.WithInteger("timeout_seconds", mcpgo.Min(1), mcpgo.Max(30), mcpgo.Description("Total snapshot deadline, including robot selection and fresh frame wait; default 15 seconds.")),
	}
	opts = append(opts, mutating()...)
	opts = append(opts, openWorld()...)
	srv.AddTool(mcpgo.NewTool("robot_camera_snapshot", opts...), s.handleRobotCameraSnapshot)
}

func (s *mcpServer) handleRobotCameraSnapshot(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	conn := s.GetConn()
	if conn == nil {
		return errNotConnected(), nil
	}
	return robotCameraSnapshotResult(ctx, req, s.captureGo2Snapshot)
}

// captureGo2Snapshot shares the onboard camera path with local perception.
// Callers must supply a deadline that includes pairing lookup and decoding.
func (s *mcpServer) captureGo2Snapshot(ctx context.Context, serial string) (string, cameraSnapshot, int64, error) {
	return s.captureGo2SnapshotMode(ctx, serial, false)
}

// captureGo2CalibratedSnapshot requires the raw mode used by the community
// lens preset. This establishes mode compatibility, not measured calibration
// of the connected robot or synchronized sensor exposure timestamps.
func (s *mcpServer) captureGo2CalibratedSnapshot(ctx context.Context, serial string) (string, cameraSnapshot, int64, error) {
	return s.captureGo2SnapshotMode(ctx, serial, true)
}

func (s *mcpServer) captureGo2SnapshotMode(ctx context.Context, serial string, native720 bool) (string, cameraSnapshot, int64, error) {
	conn := s.GetConn()
	if conn == nil || conn.UnitreeService == nil || conn.TimeSyncService == nil || conn.Conn == nil {
		return "", cameraSnapshot{}, 0, errors.New("selected agent does not provide the Unitree camera and clock services")
	}
	listed, err := conn.UnitreeService.ListUnitreePairings(ctx, &pb.ListUnitreePairingsRequest{})
	if err != nil {
		return "", cameraSnapshot{}, 0, err
	}
	serial, err = selectRobotCameraSerial(serial, listed.GetRobots())
	if err != nil {
		return "", cameraSnapshot{}, 0, err
	}
	path, err := resolveSnapshotGStreamer()
	if err != nil {
		return serial, cameraSnapshot{}, 0, err
	}
	shot, cutoff, err := captureRobotCameraSnapshot(ctx, pb.NewWendyUnitreeRobotServiceClient(conn.Conn), conn.TimeSyncService, serial, func(ctx context.Context, codec agentpb.VideoCodec) (*exec.Cmd, error) {
		args, err := robotCameraSnapshotPipeline(codec, native720)
		if err != nil {
			return nil, err
		}
		return exec.CommandContext(ctx, path, args...), nil
	})
	return serial, shot, cutoff, err
}

func robotCameraSnapshotPipeline(codec agentpb.VideoCodec, native720 bool) ([]string, error) {
	args, err := cameraSnapshotPipeline(codec)
	if err != nil || !native720 {
		return args, err
	}
	if codec != agentpb.VideoCodec_VIDEO_CODEC_H264 {
		return nil, errors.New("Go2 lens preset requires raw H.264 1280x720 video")
	}
	// Require the encoded source dimensions before decoding or resizing. A
	// differently cropped/scaled source must not acquire a known lens profile
	// merely because its final JPEG happens to have the expected aspect ratio.
	for i, arg := range args {
		if strings.HasPrefix(arg, "video/x-h264,width=") {
			args[i] = "video/x-h264,width=1280,height=720"
			return args, nil
		}
	}
	return nil, errors.New("Go2 snapshot decoder is missing the native video mode check")
}

func selectRobotCameraSerial(serial string, robots []*pb.UnitreeRobot) (string, error) {
	serial = strings.TrimSpace(serial)
	var paired []*pb.UnitreeRobot
	for _, robot := range robots {
		if robot.GetPaired() && robot.GetSerial() != "" {
			paired = append(paired, robot)
			if serial != "" && robot.GetSerial() == serial {
				return serial, nil
			}
		}
	}
	if serial == "" && len(paired) == 1 {
		return paired[0].GetSerial(), nil
	}
	return "", errors.New("choose a saved paired robot serial from unitree_pairings")
}

func robotCameraSnapshotResult(ctx context.Context, req mcpgo.CallToolRequest, capture func(context.Context, string) (string, cameraSnapshot, int64, error)) (*mcpgo.CallToolResult, error) {
	seconds, err := snapshotInteger(req, "timeout_seconds", 15, 1, 30)
	if err != nil {
		return errResult(errCodeInvalidArgument, err.Error()), nil
	}
	serial := ""
	if value, exists := req.GetArguments()["serial"]; exists {
		var ok bool
		serial, ok = value.(string)
		if !ok || strings.TrimSpace(serial) == "" {
			return errResult(errCodeInvalidArgument, "serial must be a nonempty saved robot serial"), nil
		}
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
	defer cancel()
	serial, shot, cutoff, err := capture(ctx, serial)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return errResult(errCodeTimeout, "Go2 camera snapshot timed out waiting for a fresh image. The temporary video subscription was canceled. Check the robot connection and video stream."), nil
		}
		return errResult(codeFromGRPC(err), fmt.Sprintf("Go2 camera snapshot failed: %v", err)), nil
	}
	metadata := map[string]any{
		"kind": "robot_camera_snapshot", "source": "go2_onboard_camera", "serial": serial, "live": false,
		"width": shot.width, "height": shot.height, "mime_type": "image/jpeg",
		"received_at":                 time.Now().UTC().Format(time.RFC3339Nano),
		"gateway_received_unix_nanos": shot.timestampNS, "gateway_cutoff_unix_nanos": cutoff,
		"freshness_basis": "gateway_receipt_after_request",
		"note":            "Single still image from a fresh H.264 keyframe received by the gateway after its clock check. Gateway receipt time is not sensor exposure time. The temporary video subscription has closed.",
	}
	encoded, _ := json.Marshal(metadata)
	result := mcpgo.NewToolResultImage(string(encoded), base64.StdEncoding.EncodeToString(shot.jpeg), "image/jpeg")
	result.StructuredContent = metadata
	return result, nil
}

type robotCameraStreamer interface {
	Stream(context.Context, *pb.UnitreeStreamRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[pb.UnitreeStreamMessage], error)
}

type robotCameraClock interface {
	GetClock(context.Context, *pb.GetClockRequest, ...grpc.CallOption) (*pb.GetClockResponse, error)
}

func captureRobotCameraSnapshot(ctx context.Context, client robotCameraStreamer, clock robotCameraClock, serial string, decoder func(context.Context, agentpb.VideoCodec) (*exec.Cmd, error)) (cameraSnapshot, int64, error) {
	clockReply, err := clock.GetClock(ctx, &pb.GetClockRequest{})
	if err != nil {
		return cameraSnapshot{}, 0, fmt.Errorf("checking gateway clock for video freshness: %w", err)
	}
	cutoff := clockReply.GetUnixNanos()
	if cutoff <= 0 {
		return cameraSnapshot{}, 0, errors.New("gateway did not report a valid clock; cannot verify video freshness")
	}
	shot, err := captureCameraSnapshot(ctx, &robotCameraAdapter{client: client, serial: serial, cutoff: cutoff}, &agentpb.StreamVideoRequest{Codec: agentpb.VideoCodec_VIDEO_CODEC_H264}, decoder)
	return shot, cutoff, err
}

// The adapter shares the generic snapshot decoder's byte limits, process
// cleanup and subscriber cancellation without discovering gateway cameras.
type robotCameraAdapter struct {
	client robotCameraStreamer
	serial string
	cutoff int64
}

func (a *robotCameraAdapter) StreamVideo(ctx context.Context, _ *agentpb.StreamVideoRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[agentpb.VideoFrame], error) {
	stream, err := a.client.Stream(ctx, &pb.UnitreeStreamRequest{Serial: a.serial, Video: true}, opts...)
	if err != nil {
		return nil, err
	}
	return &robotCameraStream{ClientStream: stream, stream: stream, ctx: ctx, cutoff: a.cutoff}, nil
}

type robotCameraStream struct {
	grpc.ClientStream
	stream  grpc.ServerStreamingClient[pb.UnitreeStreamMessage]
	ctx     context.Context
	cutoff  int64
	started bool
	bytes   int
}

var (
	errRobotCameraByteLimit       = errors.New("Go2 snapshot exceeded the 32 MiB encoded-video limit")
	errRobotCameraClockRegression = errors.New("Go2 video receipt time regressed during snapshot")
)

func (s *robotCameraStream) Recv() (*agentpb.VideoFrame, error) {
	for {
		if err := s.ctx.Err(); err != nil {
			return nil, err
		}
		message, err := s.stream.Recv()
		if err != nil {
			return nil, err
		}
		// Count skipped cached frames too, not only bytes sent to the decoder.
		s.bytes += proto.Size(message)
		if s.bytes > snapshotMaxVideoBytes {
			return nil, errRobotCameraByteLimit
		}
		frame := message.GetVideo()
		if frame == nil || len(frame.GetAnnexb()) == 0 {
			continue
		}
		if frame.GetReceivedUnixNanos() <= s.cutoff {
			if s.started {
				return nil, errRobotCameraClockRegression
			}
			continue
		}
		if !s.started && !unitree.IsH264Keyframe(frame.GetAnnexb()) {
			continue
		}
		s.started = true
		s.cutoff = frame.GetReceivedUnixNanos()
		return &agentpb.VideoFrame{Data: frame.GetAnnexb(), Codec: agentpb.VideoCodec_VIDEO_CODEC_H264, TimestampNs: uint64(frame.GetReceivedUnixNanos())}, nil
	}
}
