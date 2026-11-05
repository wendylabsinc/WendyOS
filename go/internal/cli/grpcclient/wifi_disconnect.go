package grpcclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

const wifiDisconnectPreparationTimeout = 30 * time.Second
const wifiDisconnectObservationTimeout = 5 * time.Second

// WiFiDisconnectResult separates acknowledgement from confirmed completion.
// A requested result must not be presented as proof that WiFi disconnected.
type WiFiDisconnectResult struct {
	State               string `json:"state"`
	Acknowledged        bool   `json:"acknowledged"`
	CompletionConfirmed bool   `json:"completion_confirmed"`
	Success             *bool  `json:"success"`
	Legacy              bool   `json:"legacy,omitempty"`
	Message             string `json:"message"`
	Warning             string `json:"warning,omitempty"`
	Error               string `json:"error,omitempty"`
}

// DisconnectWiFi acknowledges receipt of the device's commitment before it
// removes its transport. It observes completion briefly, rather than waiting for a dead
// WiFi TCP connection's retransmission timeout. An old agent keeps the legacy
// unary behavior and is never inferred successful from a lost reply.
func (c *AgentConnection) DisconnectWiFi(ctx context.Context) (result WiFiDisconnectResult, err error) {
	defer func() {
		if err != nil {
			result.Error = err.Error()
		}
	}()
	if c == nil || c.Conn == nil && c.AgentService == nil {
		return unknownWiFiDisconnectResult(), errors.New("WiFi disconnect requires a connection to a WendyOS device")
	}
	var acknowledged agentpbv2.WendyWiFiServiceClient
	if c.Conn != nil {
		acknowledged = agentpbv2.NewWendyWiFiServiceClient(c.Conn)
	}
	return disconnectWiFi(ctx, acknowledged, c.AgentService, wifiDisconnectObservationTimeout)
}

func unknownWiFiDisconnectResult() WiFiDisconnectResult {
	return WiFiDisconnectResult{State: "unknown", Message: "Could not confirm whether the device disconnected from WiFi. Check WiFi status using another working connection."}
}

func completedWiFiDisconnectResult(acknowledged bool, response *agentpbv2.DisconnectWiFiResponse) (WiFiDisconnectResult, error) {
	result := WiFiDisconnectResult{State: "completed", Acknowledged: acknowledged, CompletionConfirmed: true}
	if response == nil {
		unknown := unknownWiFiDisconnectResult()
		unknown.Acknowledged = acknowledged
		return unknown, errors.New("the device did not return a WiFi disconnect result; check WiFi status using another connection")
	}
	success := response.GetSuccess()
	result.Success = &success
	if !success {
		result.Message = "The device reported that it could not disconnect from WiFi."
		return result, fmt.Errorf("failed to disconnect WiFi: %s", response.GetErrorMessage())
	}
	result.Message = "Disconnected from WiFi."
	return result, nil
}

func disconnectWiFi(ctx context.Context, acknowledged agentpbv2.WendyWiFiServiceClient, legacy agentpb.WendyAgentServiceClient, observation time.Duration) (WiFiDisconnectResult, error) {
	if err := ctx.Err(); err != nil {
		return unknownWiFiDisconnectResult(), err
	}
	if acknowledged == nil {
		return disconnectWiFiLegacy(ctx, legacy)
	}
	preparation, cancel := context.WithTimeout(ctx, wifiDisconnectPreparationTimeout)
	defer cancel()
	stream, err := acknowledged.DisconnectWiFiAcknowledged(preparation)
	if err == nil {
		err = stream.Send(&agentpbv2.DisconnectWiFiControl{Control: &agentpbv2.DisconnectWiFiControl_Begin{Begin: &emptypb.Empty{}}})
		if errors.Is(err, io.EOF) {
			// A peer can reject an unknown method before the first Send. Recv
			// carries the actual gRPC status needed for legacy negotiation.
			if _, receiveErr := stream.Recv(); receiveErr != nil {
				err = receiveErr
			}
		}
	}
	if err != nil {
		if cause := ctx.Err(); cause != nil {
			return unknownWiFiDisconnectResult(), cause
		}
		if status.Code(err) == codes.Unimplemented {
			return disconnectWiFiLegacy(ctx, legacy)
		}
		return unknownWiFiDisconnectResult(), fmt.Errorf("the device did not confirm receiving the WiFi disconnect request: %w", err)
	}

	var ready bool
	for skipped := 0; skipped < 8; skipped++ {
		event, receiveErr := stream.Recv()
		if receiveErr != nil {
			if cause := ctx.Err(); cause != nil {
				return unknownWiFiDisconnectResult(), cause
			}
			if status.Code(receiveErr) == codes.Unimplemented {
				return disconnectWiFiLegacy(ctx, legacy)
			}
			return unknownWiFiDisconnectResult(), fmt.Errorf("the device did not confirm receiving the WiFi disconnect request: %w", receiveErr)
		}
		if event == nil {
			return unknownWiFiDisconnectResult(), errors.New("the device returned no confirmation of the WiFi disconnect request")
		}
		switch message := event.Event.(type) {
		case *agentpbv2.DisconnectWiFiEvent_Ready:
			ready = message.Ready != nil
		case *agentpbv2.DisconnectWiFiEvent_Completed:
			// Ready must identify the accepted request before its result.
			return unknownWiFiDisconnectResult(), errors.New("the device returned a WiFi disconnect result before acknowledging the request (protocol error; check CLI and agent versions)")
		case nil:
			if len(event.ProtoReflect().GetUnknown()) != 0 {
				continue
			}
		}
		if !ready {
			return unknownWiFiDisconnectResult(), errors.New("the device returned an invalid WiFi disconnect confirmation (protocol error; check CLI and agent versions)")
		}
		break
	}
	if !ready {
		return unknownWiFiDisconnectResult(), errors.New("the device returned an unsupported WiFi disconnect confirmation; use compatible CLI and agent versions")
	}
	result := WiFiDisconnectResult{
		State:        "requested",
		Acknowledged: true,
		Message:      "The device accepted the WiFi disconnect request and will attempt it shortly. Completion is unconfirmed; check WiFi status using another working connection.",
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := stream.Send(&agentpbv2.DisconnectWiFiControl{Control: &agentpbv2.DisconnectWiFiControl_Confirm{Confirm: &emptypb.Empty{}}}); err != nil {
		return finishUnconfirmedWiFiDisconnect(ctx, result, err)
	}
	// Keep the stream alive while the server consumes confirmation. Canceling
	// immediately after Send could discard the confirmation and force the target
	// to use its grace period instead of disconnecting promptly.
	if err := stream.CloseSend(); err != nil {
		return finishUnconfirmedWiFiDisconnect(ctx, result, err)
	}
	type received struct {
		event *agentpbv2.DisconnectWiFiEvent
		err   error
	}
	replies := make(chan received, 1)
	go func() {
		for skipped := 0; skipped < 8; skipped++ {
			event, err := stream.Recv()
			if err == nil && event != nil && event.Event == nil && len(event.ProtoReflect().GetUnknown()) != 0 {
				continue
			}
			replies <- received{event, err}
			return
		}
		replies <- received{err: status.Error(codes.Unimplemented, "unsupported WiFi disconnect events")}
	}()
	defer cancel() // Recv is also released if the observation window ends.
	timer := time.NewTimer(observation)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return result, ctx.Err()
	case <-timer.C:
		if err := ctx.Err(); err != nil {
			return result, err
		}
		return result, nil
	case reply := <-replies:
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if reply.err != nil {
			return finishUnconfirmedWiFiDisconnect(ctx, result, reply.err)
		}
		if reply.event == nil {
			return result, errors.New("the device did not return a WiFi disconnect result; check WiFi status using another connection")
		}
		if completed, ok := reply.event.Event.(*agentpbv2.DisconnectWiFiEvent_Completed); ok {
			return completedWiFiDisconnectResult(true, completed.Completed)
		}
		return result, errors.New("the device returned an invalid WiFi disconnect result (protocol error; check CLI and agent versions)")
	}
}

func finishUnconfirmedWiFiDisconnect(ctx context.Context, result WiFiDisconnectResult, err error) (WiFiDisconnectResult, error) {
	if cause := ctx.Err(); cause != nil {
		return result, cause
	}
	if errors.Is(err, io.EOF) || status.Code(err) == codes.Unavailable || status.Code(err) == codes.DeadlineExceeded || status.Code(err) == codes.Canceled {
		return result, nil
	}
	return result, fmt.Errorf("could not confirm the WiFi disconnect request: %w", err)
}

func disconnectWiFiLegacy(ctx context.Context, client agentpb.WendyAgentServiceClient) (WiFiDisconnectResult, error) {
	result := unknownWiFiDisconnectResult()
	result.Legacy = true
	result.Warning = "This older agent cannot confirm receiving the disconnect request before dropping WiFi. If contact is lost, check WiFi status using another working connection."
	if client == nil {
		return result, errors.New("this device connection does not support WiFi disconnect")
	}
	bounded, cancel := context.WithTimeout(ctx, wifiDisconnectPreparationTimeout)
	defer cancel()
	response, err := client.DisconnectWiFi(bounded, &agentpb.DisconnectWiFiRequest{})
	if err != nil {
		if cause := ctx.Err(); cause != nil {
			return result, cause
		}
		if status.Code(err) == codes.Unimplemented {
			return result, fmt.Errorf("agent does not support WiFi disconnect: %w", err)
		}
		if status.Code(err) != codes.Unavailable && status.Code(err) != codes.DeadlineExceeded && status.Code(err) != codes.Canceled && !errors.Is(err, io.EOF) {
			return result, fmt.Errorf("disconnecting WiFi: %w", err)
		}
		return result, fmt.Errorf("could not confirm WiFi disconnect; this older agent may have disconnected before replying. Check WiFi status using another connection: %w", err)
	}
	if response == nil {
		return result, errors.New("the older agent returned no WiFi disconnect result; check WiFi status using another connection")
	}
	completed, err := completedWiFiDisconnectResult(false, &agentpbv2.DisconnectWiFiResponse{Success: response.GetSuccess(), ErrorMessage: response.ErrorMessage})
	completed.Legacy, completed.Warning = true, result.Warning
	return completed, err
}
