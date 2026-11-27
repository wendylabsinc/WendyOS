package services

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"strings"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Paired WebRTC cameras have no V4L2 node. Reserve the high half of the uint32
// ID space so their identifiers cannot address a physical or loopback device.
const unitreeCameraIDBit uint32 = 1 << 31
const unitreeCameraPrefix = "unitree:"
const unitreeCameraDriver = "unitree-webrtc"

// SetUnitreeCameras attaches the retained robot connections before serving RPCs.
func (s *VideoService) SetUnitreeCameras(robots *UnitreeRobotService) {
	s.unitreeCameras.Store(robots)
}

func unitreeCameraIdentity(serial string) string { return unitreeCameraPrefix + serial + ":front" }

func unitreeCameraID(serial string) uint32 {
	sum := sha256.Sum256([]byte(unitreeCameraIdentity(serial)))
	return binary.BigEndian.Uint32(sum[:4]) | unitreeCameraIDBit
}

func (s *VideoService) listUnitreeCameras(ctx context.Context) ([]*agentpb.VideoDevice, error) {
	robots := s.unitreeCameras.Load()
	if robots == nil {
		return nil, nil
	}
	// Reuse the pairing owner's authorization, including on listeners where
	// ordinary local camera access is allowed without authentication.
	listed, err := robots.pairing.ListUnitreePairings(ctx, &pb.ListUnitreePairingsRequest{})
	if err != nil {
		return nil, err
	}
	var cameras []*agentpb.VideoDevice
	ids := make(map[uint32]string)
	for _, robot := range listed.GetRobots() {
		if !robot.GetPaired() {
			continue
		}
		serial := robot.GetSerial()
		id := unitreeCameraID(serial)
		if prior, ok := ids[id]; ok && prior != serial {
			// Never resolve a truncated-hash collision to whichever robot was first.
			return nil, status.Error(codes.FailedPrecondition, "paired Go2 camera IDs collide")
		}
		ids[id] = serial
		name := robot.GetName()
		if name == "" {
			name = "Unitree Go2"
		}
		cameras = append(cameras, &agentpb.VideoDevice{
			Id: id, StableId: unitreeCameraIdentity(serial), Name: name + " front camera",
			Transport: agentpb.VideoTransport_VIDEO_TRANSPORT_IP, Driver: unitreeCameraDriver,
			Address: robot.GetAddress(), HasCredentials: true, Online: robot.GetConnected(),
		})
	}
	return cameras, nil
}

func isUnitreeCameraRequest(req *agentpb.StreamVideoRequest) bool {
	stableID := strings.TrimSpace(req.GetStableId())
	// A stable identity always wins, even when a stale numeric ID names Go2.
	return strings.HasPrefix(stableID, unitreeCameraPrefix) || (stableID == "" && req.GetDeviceId()&unitreeCameraIDBit != 0)
}

func (s *VideoService) streamUnitreeCamera(req *agentpb.StreamVideoRequest, stream grpc.ServerStreamingServer[agentpb.VideoFrame]) error {
	cameras, err := s.listUnitreeCameras(stream.Context())
	if err != nil {
		return err
	}
	stableID := strings.TrimSpace(req.GetStableId())
	var selected *agentpb.VideoDevice
	for _, cam := range cameras {
		if (stableID != "" && cam.GetStableId() == stableID) || (stableID == "" && cam.GetId() == req.GetDeviceId()) {
			selected = cam
			break
		}
	}
	if selected == nil {
		return status.Error(codes.NotFound, "Go2 camera is not paired on this device")
	}
	switch req.GetCodec() {
	case agentpb.VideoCodec_VIDEO_CODEC_H264:
	case agentpb.VideoCodec_VIDEO_CODEC_RAW:
		return errRawUnavailable("Go2 cameras deliver encoded H.264 video only")
	default:
		return status.Error(codes.InvalidArgument, "Go2 cameras deliver H.264 video only")
	}
	if req.GetWidth() != 0 || req.GetHeight() != 0 || req.GetFramerate() != 0 {
		return status.Error(codes.InvalidArgument, "Go2 camera uses the robot's video mode; omit width, height and framerate")
	}
	serial := strings.TrimSuffix(strings.TrimPrefix(selected.GetStableId(), unitreeCameraPrefix), ":front")
	// Connection.Subscribe multiplexes the existing WebRTC track. Each viewer
	// gets a keyframe and releases only its own subscription when it exits.
	// Stream rechecks ownership and connection state before subscribing.
	return s.unitreeCameras.Load().Stream(&pb.UnitreeStreamRequest{Serial: serial, Video: true}, &unitreeCameraStream{ServerStreamingServer: stream})
}

type unitreeCameraStream struct {
	grpc.ServerStreamingServer[agentpb.VideoFrame]
}

func (s *unitreeCameraStream) Send(message *pb.UnitreeStreamMessage) error {
	frame := message.GetVideo()
	if frame == nil {
		return nil
	}
	return s.ServerStreamingServer.Send(&agentpb.VideoFrame{
		Data: frame.GetAnnexb(), TimestampNs: uint64(frame.GetReceivedUnixNanos()),
		Codec: agentpb.VideoCodec_VIDEO_CODEC_H264,
	})
}
