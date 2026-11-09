package services

import (
	"context"
	"errors"
	"io"
	"time"

	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

const wifiDisconnectPhaseTimeout = 30 * time.Second
const wifiDisconnectConfirmationTimeout = 5 * time.Second
const wifiDisconnectExecutionTimeout = 30 * time.Second

// DisconnectWiFiAcknowledged leaves the legacy unary operation unchanged. The
// valid Begin commits the device to attempting the disconnect. Ready reports
// that commitment; Confirm permits it immediately, while a missing Confirm or
// lost stream only limits the acknowledgement grace period, not the operation.
func (s *WiFiService) DisconnectWiFiAcknowledged(stream grpc.BidiStreamingServer[agentpbv2.DisconnectWiFiControl, agentpbv2.DisconnectWiFiEvent]) error {
	if s.networkManager == nil {
		return status.Error(codes.Unavailable, "WiFi management is not available (nmcli not found)")
	}
	phaseTimeout := s.disconnectPhaseTimeout
	if phaseTimeout <= 0 {
		phaseTimeout = wifiDisconnectPhaseTimeout
	}
	phase, cancelPhase := context.WithTimeout(stream.Context(), phaseTimeout)
	defer cancelPhase()
	if err := awaitWiFiDisconnectControl(phase, stream, false); err != nil {
		return err
	}
	// Begin is the commit point. Do not gate the operation on another status
	// read or on successful delivery of Ready/Confirm: either can lose the
	// requesting path after the device has already accepted the command.
	cancelPhase()
	confirmationTimeout := s.disconnectConfirmationTimeout
	if confirmationTimeout <= 0 {
		confirmationTimeout = wifiDisconnectConfirmationTimeout
	}
	confirmation, cancelConfirmation := context.WithTimeout(stream.Context(), confirmationTimeout)
	readyErr := sendWiFiDisconnectEvent(confirmation, stream, &agentpbv2.DisconnectWiFiEvent{
		Event: &agentpbv2.DisconnectWiFiEvent_Ready{Ready: &emptypb.Empty{}},
	})
	if readyErr == nil {
		if err := awaitWiFiDisconnectControl(confirmation, stream, true); err != nil && s.logger != nil {
			s.logger.Debug("WiFi disconnect proceeding without client confirmation", zap.Error(err))
		}
	}
	cancelConfirmation()

	executionTimeout := s.disconnectExecutionTimeout
	if executionTimeout <= 0 {
		executionTimeout = wifiDisconnectExecutionTimeout
	}
	// An accepted disconnect must not be aborted just because it removes the
	// requesting stream. Keep identity/trace values but impose a separate bound.
	execution, cancelExecution := context.WithTimeout(context.WithoutCancel(stream.Context()), executionTimeout)
	defer cancelExecution()
	response := &agentpbv2.DisconnectWiFiResponse{Success: true}
	if err := s.networkManager.DisconnectWiFi(execution); err != nil {
		response.Success = false
		message := err.Error()
		response.ErrorMessage = &message
		if s.logger != nil {
			s.logger.Warn("WiFi disconnect failed after accepting the request", zap.Error(err))
		}
	}
	// A failed Ready send can still own gRPC's send side. Do not attempt a
	// concurrent completion send, and never let that failure cancel the action.
	if readyErr != nil {
		return readyErr
	}
	completion, cancelCompletion := context.WithTimeout(stream.Context(), phaseTimeout)
	defer cancelCompletion()
	if err := sendWiFiDisconnectEvent(completion, stream, &agentpbv2.DisconnectWiFiEvent{
		Event: &agentpbv2.DisconnectWiFiEvent_Completed{Completed: response},
	}); err != nil {
		if s.logger != nil {
			s.logger.Info("WiFi disconnect completion could not be delivered", zap.Bool("success", response.Success), zap.Error(err))
		}
		return err
	}
	return nil
}

func awaitWiFiDisconnectControl(ctx context.Context, stream grpc.BidiStreamingServer[agentpbv2.DisconnectWiFiControl, agentpbv2.DisconnectWiFiEvent], confirm bool) error {
	// Future optional controls may be skipped, but an abandoned or continually
	// incompatible stream cannot retain the prepared operation indefinitely.
	for skipped := 0; skipped < 8; skipped++ {
		type received struct {
			message *agentpbv2.DisconnectWiFiControl
			err     error
		}
		result := make(chan received, 1)
		go func() {
			message, err := stream.Recv()
			result <- received{message, err}
		}()
		// Returning from the RPC cancels gRPC's stream context and unblocks an
		// outstanding Recv. The buffered result also cannot strand its sender.
		var got received
		select {
		case <-ctx.Done():
			return status.FromContextError(ctx.Err()).Err()
		case got = <-result:
		}
		if got.err != nil {
			if errors.Is(got.err, io.EOF) {
				return status.Error(codes.InvalidArgument, "WiFi disconnect requires begin followed by confirm")
			}
			return got.err
		}
		if err := ctx.Err(); err != nil {
			return status.FromContextError(err).Err()
		}
		if got.message == nil {
			return status.Error(codes.InvalidArgument, "missing WiFi disconnect control")
		}
		switch control := got.message.Control.(type) {
		case *agentpbv2.DisconnectWiFiControl_Begin:
			if !confirm && control.Begin != nil {
				return nil
			}
		case *agentpbv2.DisconnectWiFiControl_Confirm:
			if confirm && control.Confirm != nil {
				return nil
			}
		case nil:
			if len(got.message.ProtoReflect().GetUnknown()) != 0 {
				continue
			}
		}
		return status.Error(codes.InvalidArgument, "WiFi disconnect requires begin followed by confirm")
	}
	return status.Error(codes.Unimplemented, "unsupported WiFi disconnect controls")
}

func sendWiFiDisconnectEvent(ctx context.Context, stream grpc.BidiStreamingServer[agentpbv2.DisconnectWiFiControl, agentpbv2.DisconnectWiFiEvent], event *agentpbv2.DisconnectWiFiEvent) error {
	result := make(chan error, 1)
	go func() { result <- stream.Send(event) }()
	select {
	case <-ctx.Done():
		return status.FromContextError(ctx.Err()).Err()
	case err := <-result:
		return err
	}
}
