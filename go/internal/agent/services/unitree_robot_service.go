package services

import (
	"context"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/unitree"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

const teleopRobotTimeout = 200 * time.Millisecond
const teleopMoveLease = 500 * time.Millisecond

type unitreeMotionLink interface {
	unitreeLink
	Move(context.Context, float64, float64, float64) error
	Stop(context.Context) error
	StandUp(context.Context) error
	LieDown(context.Context) error
	Hello(context.Context) error
}

// UnitreeRobotService never connects a robot or sees its key. It streams robot
// data and sends a small fixed set of commands over the retained paired
// connection. A move expires unless refreshed, so a crashed caller cannot
// leave the robot walking.
type UnitreeRobotService struct {
	pb.UnimplementedWendyUnitreeRobotServiceServer
	pairing *UnitreeService
	lease   time.Duration
	mu      sync.Mutex
	gen     map[string]uint64
	timers  map[string]*time.Timer
	logger  *zap.Logger
}

func NewUnitreeRobotService(pairing *UnitreeService, loggers ...*zap.Logger) *UnitreeRobotService {
	logger := zap.NewNop()
	if len(loggers) > 0 && loggers[0] != nil {
		logger = loggers[0]
	}
	return &UnitreeRobotService{pairing: pairing, lease: teleopMoveLease,
		gen: make(map[string]uint64), timers: make(map[string]*time.Timer), logger: logger}
}

func (s *UnitreeRobotService) Command(ctx context.Context, req *pb.UnitreeCommandRequest) (*pb.UnitreeCommandResponse, error) {
	if err := requireUnitreeTransport(ctx); err != nil {
		return nil, err
	}
	if !unitree.ValidSerial(req.GetSerial()) {
		return nil, status.Error(codes.InvalidArgument, "a paired Go2 serial is required")
	}
	if req.GetCommand() == pb.UnitreeCommand_UNITREE_COMMAND_MOVE && !unitree.ValidMotion(req.GetForward(), req.GetLateral(), req.GetYaw()) {
		return nil, status.Error(codes.InvalidArgument, "Go2 velocity outside limits")
	}
	link, err := s.link(ctx, req.GetSerial())
	if err != nil {
		return nil, err
	}
	started := time.Now()
	transport := "unknown"
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		transport = p.Addr.Network()
	}
	defer func() {
		s.logger.Debug("Go2 command", zap.Time("started", started),
			zap.String("command", req.GetCommand().String()), zap.String("transport", transport),
			zap.Float64("forward", req.GetForward()), zap.Float64("lateral", req.GetLateral()),
			zap.Float64("yaw", req.GetYaw()), zap.Duration("duration", time.Since(started)), zap.Error(err))
	}()

	// Cancel any pending move expiry first, so it cannot fire after this command.
	gen := s.cancelLease(req.GetSerial())
	timeout := teleopRobotTimeout
	switch req.GetCommand() {
	case pb.UnitreeCommand_UNITREE_COMMAND_STAND_UP, pb.UnitreeCommand_UNITREE_COMMAND_LIE_DOWN, pb.UnitreeCommand_UNITREE_COMMAND_HELLO, pb.UnitreeCommand_UNITREE_COMMAND_SET_OBSTACLE_AVOIDANCE:
		timeout = unitree.PostureAckTimeout
	}
	commandCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	switch req.GetCommand() {
	case pb.UnitreeCommand_UNITREE_COMMAND_MOVE:
		err = link.Move(commandCtx, req.GetForward(), req.GetLateral(), req.GetYaw())
		if err == nil && (req.GetForward() != 0 || req.GetLateral() != 0 || req.GetYaw() != 0) {
			s.armLease(req.GetSerial(), gen, link)
		}
	case pb.UnitreeCommand_UNITREE_COMMAND_STOP:
		err = link.Stop(commandCtx)
	case pb.UnitreeCommand_UNITREE_COMMAND_STAND_UP:
		err = link.StandUp(commandCtx)
	case pb.UnitreeCommand_UNITREE_COMMAND_LIE_DOWN:
		err = link.LieDown(commandCtx)
	case pb.UnitreeCommand_UNITREE_COMMAND_HELLO:
		err = link.Hello(commandCtx)
	case pb.UnitreeCommand_UNITREE_COMMAND_SET_OBSTACLE_AVOIDANCE:
		// A failed stop must never be followed by a mode change.
		err = link.Stop(commandCtx)
		if err == nil {
			if configurable, ok := link.(interface {
				SetObstacleAvoidance(context.Context, bool) error
			}); ok {
				err = configurable.SetObstacleAvoidance(commandCtx, req.GetObstacleAvoidanceEnabled())
			} else {
				return nil, status.Error(codes.Unimplemented, "this robot connection cannot configure obstacle avoidance")
			}
		}
	default:
		return nil, status.Error(codes.InvalidArgument, "unknown Go2 command")
	}
	if err != nil {
		if req.GetCommand() == pb.UnitreeCommand_UNITREE_COMMAND_MOVE {
			stopRobot(link)
		}
		return nil, status.Error(codes.Unavailable, "robot did not acknowledge the command")
	}
	return &pb.UnitreeCommandResponse{}, nil
}

type unitreeStreamLink interface {
	unitreeLink
	Subscribe(topics []string, video bool) (<-chan unitree.TopicMessage, <-chan unitree.VideoFrame, func(), error)
}

func (s *UnitreeRobotService) Stream(req *pb.UnitreeStreamRequest, stream grpc.ServerStreamingServer[pb.UnitreeStreamMessage]) error {
	ctx := stream.Context()
	if err := requireUnitreeTransport(ctx); err != nil {
		return err
	}
	if !unitree.ValidSerial(req.GetSerial()) {
		return status.Error(codes.InvalidArgument, "a paired Go2 serial is required")
	}
	for _, topic := range req.GetTopics() {
		if !unitree.ValidTopic(topic) {
			return status.Error(codes.InvalidArgument, "invalid Go2 topic name")
		}
	}
	paired, err := s.pairedLink(ctx, req.GetSerial())
	if err != nil {
		return err
	}
	link, ok := paired.(unitreeStreamLink)
	if !ok {
		return status.Error(codes.Unavailable, "paired Go2 cannot stream")
	}
	messages, video, cancel, err := link.Subscribe(req.GetTopics(), req.GetVideo())
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	defer cancel()
	for {
		var out *pb.UnitreeStreamMessage
		select {
		case <-ctx.Done():
			return nil
		case <-link.Done():
			return status.Error(codes.Unavailable, "Go2 disconnected")
		case m := <-messages:
			out = &pb.UnitreeStreamMessage{Kind: &pb.UnitreeStreamMessage_Topic{Topic: &pb.UnitreeTopicMessage{
				Topic: m.Topic, Data: m.Data, Binary: m.Binary, ReceivedUnixNanos: m.Received.UnixNano()}}}
		case f := <-video:
			out = &pb.UnitreeStreamMessage{Kind: &pb.UnitreeStreamMessage_Video{Video: &pb.UnitreeVideoFrame{
				Annexb: f.AnnexB, RtpTimestamp: f.RTPTimestamp, ReceivedUnixNanos: f.Received.UnixNano()}}}
		}
		if err := stream.Send(out); err != nil {
			return err
		}
	}
}

func (s *UnitreeRobotService) link(ctx context.Context, serial string) (unitreeMotionLink, error) {
	paired, err := s.pairedLink(ctx, serial)
	if err != nil {
		return nil, err
	}
	link, ok := paired.(unitreeMotionLink)
	if !ok {
		return nil, status.Error(codes.Unavailable, "paired Go2 is not connected")
	}
	return link, nil
}

// pairedLink returns the retained connection after the pairing-owner check,
// which applies even when the common listener's scope enforcement is in
// grace/off mode.
func (s *UnitreeRobotService) pairedLink(ctx context.Context, serial string) (unitreeLink, error) {
	if !s.pairing.opMu.TryLock() {
		return nil, status.Error(codes.Unavailable, "pairing is changing; try again")
	}
	defer s.pairing.opMu.Unlock()
	if err := s.pairing.authorize(ctx); err != nil {
		return nil, err
	}
	s.pairing.mu.RLock()
	defer s.pairing.mu.RUnlock()
	var link unitreeLink
	if session := s.pairing.sessions[serial]; session != nil {
		link = session.link
	}
	if link == nil || !link.Connected() {
		return nil, status.Error(codes.Unavailable, "paired Go2 is not connected")
	}
	return link, nil
}

func (s *UnitreeRobotService) cancelLease(serial string) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gen[serial]++
	if t := s.timers[serial]; t != nil {
		t.Stop()
		delete(s.timers, serial)
	}
	return s.gen[serial]
}

func (s *UnitreeRobotService) armLease(serial string, gen uint64, link unitreeMotionLink) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gen[serial] != gen {
		return // a newer command already arrived
	}
	s.timers[serial] = time.AfterFunc(s.lease, func() {
		s.mu.Lock()
		current := s.gen[serial] == gen
		if current {
			delete(s.timers, serial)
		}
		s.mu.Unlock()
		if current {
			stopRobot(link)
			s.logger.Debug("Go2 move lease expired", zap.Uint64("generation", gen))
		}
	})
}

func stopRobot(link unitreeMotionLink) {
	ctx, cancel := context.WithTimeout(context.Background(), teleopRobotTimeout)
	defer cancel()
	_ = link.Stop(ctx)
}
