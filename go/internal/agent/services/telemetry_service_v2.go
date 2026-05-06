package services

import (
	"go.uber.org/zap"
	"google.golang.org/grpc"

	agentpbv2 "github.com/wendylabsinc/wendy/proto/gen/agentpb/v2"
)

// TelemetryServiceV2 implements agentpbv2.WendyTelemetryServiceServer by
// forwarding telemetry events from the shared TelemetryBroadcaster.
type TelemetryServiceV2 struct {
	agentpbv2.UnimplementedWendyTelemetryServiceServer
	logger      *zap.Logger
	broadcaster *TelemetryBroadcaster
}

// NewTelemetryServiceV2 creates a new TelemetryServiceV2.
func NewTelemetryServiceV2(logger *zap.Logger, broadcaster *TelemetryBroadcaster) *TelemetryServiceV2 {
	return &TelemetryServiceV2{logger: logger, broadcaster: broadcaster}
}

func (s *TelemetryServiceV2) StreamLogs(_ *agentpbv2.StreamLogsRequest, stream grpc.ServerStreamingServer[agentpbv2.StreamLogsResponse]) error {
	subID, ch := s.broadcaster.SubscribeLogs()
	defer s.broadcaster.UnsubscribeLogs(subID)

	for {
		select {
		case <-stream.Context().Done():
			return stream.Context().Err()
		case item, ok := <-ch:
			if !ok {
				return nil
			}
			if err := stream.Send(&agentpbv2.StreamLogsResponse{Logs: item}); err != nil {
				return err
			}
		}
	}
}

func (s *TelemetryServiceV2) StreamMetrics(_ *agentpbv2.StreamMetricsRequest, stream grpc.ServerStreamingServer[agentpbv2.StreamMetricsResponse]) error {
	subID, ch := s.broadcaster.SubscribeMetrics()
	defer s.broadcaster.UnsubscribeMetrics(subID)

	for {
		select {
		case <-stream.Context().Done():
			return stream.Context().Err()
		case item, ok := <-ch:
			if !ok {
				return nil
			}
			if err := stream.Send(&agentpbv2.StreamMetricsResponse{Metrics: item}); err != nil {
				return err
			}
		}
	}
}

func (s *TelemetryServiceV2) StreamTraces(_ *agentpbv2.StreamTracesRequest, stream grpc.ServerStreamingServer[agentpbv2.StreamTracesResponse]) error {
	subID, ch := s.broadcaster.SubscribeTraces()
	defer s.broadcaster.UnsubscribeTraces(subID)

	for {
		select {
		case <-stream.Context().Done():
			return stream.Context().Err()
		case item, ok := <-ch:
			if !ok {
				return nil
			}
			if err := stream.Send(&agentpbv2.StreamTracesResponse{Traces: item}); err != nil {
				return err
			}
		}
	}
}
