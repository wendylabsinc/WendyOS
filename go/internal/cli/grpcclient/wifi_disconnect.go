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

// DisconnectWiFi acknowledges the request before allowing the agent to remove
// its transport. It observes completion briefly, rather than waiting for a dead
// WiFi TCP connection's retransmission timeout. An old agent keeps the legacy
// unary behavior and is never inferred successful from a lost reply.
func (c *AgentConnection) DisconnectWiFi(ctx context.Context) (result WiFiDisconnectResult, err error) {
	defer func() {
		if err != nil {
			result.Error = err.Error()
		}
	}()
	if c == nil || c.Conn == nil && c.AgentService == nil {
		return unknownWiFiDisconnectResult(), errors.New("WiFi disconnect requires an agent connection")
	}
	var acknowledged agentpbv2.WendyWiFiServiceClient
	if c.Conn != nil {
		acknowledged = agentpbv2.NewWendyWiFiServiceClient(c.Conn)
	}
	return disconnectWiFi(ctx, acknowledged, c.AgentService, wifiDisconnectObservationTimeout)
}

func unknownWiFiDisconnectResult() WiFiDisconnectResult {
	return WiFiDisconnectResult{State: "unknown", Message: "WiFi disconnect completion is unknown."}
}

func completedWiFiDisconnectResult(acknowledged bool, response *agentpbv2.DisconnectWiFiResponse) (WiFiDisconnectResult, error) {
	result := WiFiDisconnectResult{State: "completed", Acknowledged: acknowledged, CompletionConfirmed: true}
	if response == nil {
		unknown := unknownWiFiDisconnectResult()
		unknown.Acknowledged = acknowledged
		return unknown, errors.New("missing WiFi disconnect completion")
	}
	success := response.GetSuccess()
	result.Success = &success
	if !success {
		result.Message = "The agent reported a WiFi disconnect error."
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
		return unknownWiFiDisconnectResult(), fmt.Errorf("WiFi disconnect was not acknowledged: %w", err)
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
			return unknownWiFiDisconnectResult(), fmt.Errorf("WiFi disconnect was not acknowledged: %w", receiveErr)
		}
		if event == nil {
			return unknownWiFiDisconnectResult(), errors.New("missing WiFi disconnect acknowledgement")
		}
		switch message := event.Event.(type) {
		case *agentpbv2.DisconnectWiFiEvent_Ready:
			ready = message.Ready != nil
		case *agentpbv2.DisconnectWiFiEvent_Completed:
			return completedWiFiDisconnectResult(false, message.Completed)
		case nil:
			if len(event.ProtoReflect().GetUnknown()) != 0 {
				continue
			}
		}
		if !ready {
			return unknownWiFiDisconnectResult(), errors.New("invalid WiFi disconnect acknowledgement")
		}
		break
	}
	if !ready {
		return unknownWiFiDisconnectResult(), errors.New("unsupported WiFi disconnect acknowledgement")
	}
	result := WiFiDisconnectResult{
		State:        "requested",
		Acknowledged: true,
		Message:      "WiFi disconnect requested; completion is unconfirmed. Check WiFi status using another connection.",
	}
	if err := ctx.Err(); err != nil {
		result.State = "unknown"
		return result, err
	}
	if err := stream.Send(&agentpbv2.DisconnectWiFiControl{Control: &agentpbv2.DisconnectWiFiControl_Confirm{Confirm: &emptypb.Empty{}}}); err != nil {
		return finishUnconfirmedWiFiDisconnect(ctx, result, err)
	}
	// Keep the stream alive while the server consumes confirmation. Canceling
	// immediately after Send could discard the confirmation before it arrives.
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
		result.State = "unknown"
		return result, ctx.Err()
	case <-timer.C:
		if err := ctx.Err(); err != nil {
			result.State = "unknown"
			return result, err
		}
		return result, nil
	case reply := <-replies:
		if err := ctx.Err(); err != nil {
			result.State = "unknown"
			return result, err
		}
		if reply.err != nil {
			return finishUnconfirmedWiFiDisconnect(ctx, result, reply.err)
		}
		if reply.event == nil {
			return result, errors.New("missing WiFi disconnect completion")
		}
		if completed, ok := reply.event.Event.(*agentpbv2.DisconnectWiFiEvent_Completed); ok {
			return completedWiFiDisconnectResult(true, completed.Completed)
		}
		return result, errors.New("invalid WiFi disconnect completion")
	}
}

func finishUnconfirmedWiFiDisconnect(ctx context.Context, result WiFiDisconnectResult, err error) (WiFiDisconnectResult, error) {
	if cause := ctx.Err(); cause != nil {
		result.State = "unknown"
		return result, cause
	}
	if errors.Is(err, io.EOF) || status.Code(err) == codes.Unavailable || status.Code(err) == codes.DeadlineExceeded || status.Code(err) == codes.Canceled {
		return result, nil
	}
	return result, fmt.Errorf("WiFi disconnect confirmation failed: %w", err)
}

func disconnectWiFiLegacy(ctx context.Context, client agentpb.WendyAgentServiceClient) (WiFiDisconnectResult, error) {
	result := unknownWiFiDisconnectResult()
	result.Legacy = true
	result.Warning = "This agent uses legacy WiFi disconnect without receipt acknowledgement; use another connection to verify an interrupted result."
	if client == nil {
		return result, errors.New("legacy WiFi disconnect is unavailable")
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
		return result, fmt.Errorf("legacy WiFi disconnect completion is unknown; the network path may have closed: %w", err)
	}
	if response == nil {
		return result, errors.New("legacy WiFi disconnect returned no completion response")
	}
	completed, err := completedWiFiDisconnectResult(false, &agentpbv2.DisconnectWiFiResponse{Success: response.GetSuccess(), ErrorMessage: response.ErrorMessage})
	completed.Legacy, completed.Warning = true, result.Warning
	return completed, err
}
