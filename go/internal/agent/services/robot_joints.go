package services

import (
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/agent/robotjoints"
	"github.com/wendylabsinc/wendy/go/internal/shared/streamreason"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// StreamJointPositions reports where the robot's joints are, for as long as the
// client listens.
//
// The listening happens here rather than in the CLI because DDS discovery is
// multicast and a robot's graph does not leave the robot's own network segment.
// The agent is already inside it, so the calibration wizard reads joints over
// the same connection it reads everything else and `wendy device robot
// calibrate` works from a laptop.
//
// This method owns nothing but the mapping between robotjoints' answers and
// gRPC's. What a joint source is, which slots are real and when a robot has gone
// quiet all live in robotjoints, where they can be tested without a robot.
func (s *RobotService) StreamJointPositions(
	req *agentpbv2.StreamJointPositionsRequest,
	stream agentpbv2.WendyRobotService_StreamJointPositionsServer,
) error {
	if s.joints == nil {
		// An agent built without a participant pool cannot join a DDS domain at
		// all. Named as such rather than reported as an absent robot: the
		// operator's next move is different.
		return status.Error(codes.FailedPrecondition,
			"this agent has no DDS participant pool, so it cannot read a robot's joints")
	}

	cfg := robotjoints.Config{
		Backend:     req.GetBackend(),
		Topic:       req.GetTopic(),
		DomainID:    int(req.GetDomainId()),
		Interface:   req.GetInterface(),
		MinInterval: time.Duration(req.GetMinIntervalMillis()) * time.Millisecond,
	}
	logger := s.logger.With(
		zap.String("backend", cfg.Backend),
		zap.String("topic", cfg.Topic),
		zap.Int("dds_domain_id", cfg.DomainID),
	)
	reader, err := robotjoints.NewReader(cfg, s.joints, func(format string, args ...any) {
		logger.Debug(fmt.Sprintf(format, args...))
	})
	if err != nil {
		if errors.Is(err, robotjoints.ErrUnknownBackend) {
			return status.Error(codes.InvalidArgument, err.Error())
		}
		return status.Error(codes.FailedPrecondition, err.Error())
	}

	err = reader.Stream(stream.Context(), func(reading robotjoints.Reading) error {
		return stream.Send(jointSampleProto(reading))
	})
	return jointStreamError(reader.Describe(), err)
}

// jointSampleProto renders one reading.
//
// Only reporting slots are carried, and the total is carried beside them. An
// idle slot is absent rather than zero: a zero angle reads as a joint sitting
// perfectly still, which is indistinguishable from a joint that was swept and
// did not move — the false calibration the wizard's rules exist to prevent.
func jointSampleProto(reading robotjoints.Reading) *agentpbv2.JointPositionSample {
	sample := &agentpbv2.JointPositionSample{
		ObservedAtUnixNanos: reading.At.UnixNano(),
		SlotCount:           uint32(reading.SlotCount),
		Unit:                reading.Unit,
		Slots:               make([]*agentpbv2.JointSlot, 0, len(reading.Slots)),
	}
	for _, slot := range reading.Slots {
		sample.Slots = append(sample.Slots, &agentpbv2.JointSlot{
			Index:    uint32(slot.Index),
			Position: slot.Position,
		})
	}
	return sample
}

// jointStreamError maps how a stream ended onto a code the operator can act on.
//
// The distinction that matters is between a robot that is silent and a device
// that could not listen, and it is carried as a machine-readable reason as well
// as a code — a cloud tunnel that has lost the device answers NOT_FOUND too, and
// a client matching on message text would eventually be told the robot is quiet
// when the truth is that nothing reached it.
func jointStreamError(describe string, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, robotjoints.ErrSourceAbsent):
		return streamreason.New(codes.NotFound, err.Error(),
			streamreason.RobotJointSourceAbsent, map[string]string{"source": describe})
	case errors.Is(err, robotjoints.ErrNoInterface):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, robotjoints.ErrUnknownBackend):
		return status.Error(codes.InvalidArgument, err.Error())
	case status.Code(err) != codes.Unknown:
		// Already a status — a Send that failed because the client went away,
		// most often. Passed through rather than relabelled.
		return err
	default:
		// Bytes arrived on the topic and did not decode, or the subscription
		// itself failed. Reported as the device's problem, never smoothed into
		// an absence: the topic is there and we could not read it.
		return status.Error(codes.Internal, err.Error())
	}
}
