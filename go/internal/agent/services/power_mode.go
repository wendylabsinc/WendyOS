package services

import (
	"context"
	"errors"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/agent/board"
	"github.com/wendylabsinc/wendy/go/internal/agent/powermode"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// powerModeController is the slice of powermode.Controller the service uses,
// so tests can substitute it.
type powerModeController interface {
	State(ctx context.Context) (powermode.State, error)
	Set(ctx context.Context, mode string, reboot bool) (powermode.SetResult, error)
}

// powerModeRebootDelay lets the SetPowerMode response reach the caller before
// the device goes down.
const powerModeRebootDelay = 2 * time.Second

const powerModeUnsupported = "power modes are only supported on NVIDIA Jetson devices"

func modeToProto(m powermode.Mode) *agentpb.PowerMode {
	return &agentpb.PowerMode{Id: m.ID, Name: m.Name}
}

// powerModeError maps controller errors onto gRPC codes.
func powerModeError(err error) error {
	switch {
	case errors.Is(err, powermode.ErrUnavailable):
		return status.Error(codes.Unavailable, err.Error())
	case errors.Is(err, powermode.ErrUnknownMode):
		return status.Error(codes.InvalidArgument, err.Error())
	default:
		return status.Error(codes.Internal, err.Error())
	}
}

// GetPowerMode reports the Jetson power mode and the modes the device offers.
func (s *AgentService) GetPowerMode(ctx context.Context, _ *agentpb.GetPowerModeRequest) (*agentpb.GetPowerModeResponse, error) {
	if !s.onJetson() {
		return nil, status.Error(codes.Unavailable, powerModeUnsupported)
	}
	st, err := s.powerModes.State(ctx)
	if err != nil {
		s.logger.Warn("GetPowerMode failed", zap.Error(err))
		return nil, powerModeError(err)
	}
	resp := &agentpb.GetPowerModeResponse{Current: modeToProto(st.Current)}
	for _, m := range st.Available {
		resp.Available = append(resp.Available, modeToProto(m))
	}
	if st.BootDefault != nil {
		resp.BootDefault = modeToProto(*st.BootDefault)
	}
	return resp, nil
}

// SetPowerMode switches the Jetson power mode. A mode that needs a reboot is
// neither recorded nor rebooted into unless the request asks for the reboot;
// without it the call only reports that a reboot is required.
func (s *AgentService) SetPowerMode(ctx context.Context, req *agentpb.SetPowerModeRequest) (*agentpb.SetPowerModeResponse, error) {
	if !s.onJetson() {
		return nil, status.Error(codes.Unavailable, powerModeUnsupported)
	}
	s.logger.Info("SetPowerMode requested", zap.String("mode", req.GetMode()), zap.Bool("reboot", req.GetReboot()))
	res, err := s.powerModes.Set(ctx, req.GetMode(), req.GetReboot())
	if err != nil {
		s.logger.Warn("SetPowerMode failed", zap.String("mode", req.GetMode()), zap.Error(err))
		return nil, powerModeError(err)
	}
	resp := &agentpb.SetPowerModeResponse{
		Mode:           modeToProto(res.Mode),
		RebootRequired: res.RebootRequired,
	}
	if res.RebootRequired && req.GetReboot() {
		resp.RebootScheduled = true
		s.logger.Warn("Rebooting to finish the power mode switch", zap.String("mode", res.Mode.Name))
		go func() {
			time.Sleep(powerModeRebootDelay)
			if err := s.rebootFn(); err != nil {
				s.logger.Error("Reboot after power mode switch failed", zap.Error(err))
			}
		}()
	}
	return resp, nil
}

// currentPowerMode is the best-effort mode for GetAgentVersion: nil off Jetson
// and whenever it cannot be read.
func (s *AgentService) currentPowerMode(ctx context.Context) *agentpb.PowerMode {
	if !s.onJetson() {
		return nil
	}
	st, err := s.powerModes.State(ctx)
	if err != nil {
		return nil
	}
	return modeToProto(st.Current)
}

// onJetson is false for a service built without the power-mode hooks (bare
// test fixtures) as well as for non-Jetson hosts.
func (s *AgentService) onJetson() bool {
	return s.isJetson != nil && s.powerModes != nil && s.isJetson()
}

func defaultIsJetson() bool { return board.Detect().IsJetson() }

func defaultReboot() error { return newRebooter(nil).rebootClean() }
