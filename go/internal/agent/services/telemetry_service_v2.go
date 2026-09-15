package services

import (
	"strings"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// TelemetryServiceV2 implements agentpbv2.WendyTelemetryServiceServer by
// forwarding telemetry events from the shared TelemetryBroadcaster.
type TelemetryServiceV2 struct {
	agentpbv2.UnimplementedWendyTelemetryServiceServer
	logger      *zap.Logger
	broadcaster *TelemetryBroadcaster
	buffer      *TelemetryBuffer
}

// NewTelemetryServiceV2 creates a new TelemetryServiceV2.
func NewTelemetryServiceV2(logger *zap.Logger, broadcaster *TelemetryBroadcaster, buffer *TelemetryBuffer) *TelemetryServiceV2 {
	return &TelemetryServiceV2{logger: logger, broadcaster: broadcaster, buffer: buffer}
}

func (s *TelemetryServiceV2) StreamLogs(req *agentpbv2.StreamLogsRequest, stream grpc.ServerStreamingServer[agentpbv2.StreamLogsResponse]) error {
	// Subscribe first (without cache prefill when replaying history) so live
	// telemetry is buffered during replay and not lost, and to avoid duplicate
	// deliveries from both the disk history and the in-memory ring buffer.
	var subID string
	var recent []*collogspb.ExportLogsServiceRequest
	var ch <-chan *collogspb.ExportLogsServiceRequest
	if req.LastN != nil && *req.LastN > 0 && s.buffer != nil && s.buffer.DiskEnabled() {
		subID, ch = s.broadcaster.SubscribeLogsNoPrefill()
	} else {
		subID, recent, ch = s.broadcaster.SubscribeLogsWithHistory()
	}
	defer s.broadcaster.UnsubscribeLogs(subID)

	// Replay history after subscribing. Count the tail window against batches
	// that survive the filter: the last N batches device-wide may all belong
	// to a chatty co-tenant, which would replay nothing for the requested app.
	if req.LastN != nil && *req.LastN > 0 && s.buffer != nil && s.buffer.DiskEnabled() {
		entries := s.buffer.ReadLastNMatching(SignalLogs, int(*req.LastN), func(m proto.Message) proto.Message {
			logs, ok := m.(*collogspb.ExportLogsServiceRequest)
			if !ok {
				return nil
			}
			if req.AppName != nil || req.ServiceName != nil || req.MinSeverity != nil {
				if logs = filterLogsV2(logs, req); logs == nil {
					return nil
				}
			}
			return logs
		})
		for _, e := range entries {
			logs, ok := e.(*collogspb.ExportLogsServiceRequest)
			if !ok {
				continue
			}
			if err := stream.Send(&agentpbv2.StreamLogsResponse{Logs: logs, IsHistory: true}); err != nil {
				return err
			}
		}
	}

	for _, logs := range recent {
		if req.ServiceName != nil || req.MinSeverity != nil || req.AppName != nil {
			logs = filterLogsV2(logs, req)
			if logs == nil {
				continue
			}
		}
		if err := stream.Send(&agentpbv2.StreamLogsResponse{Logs: logs, IsHistory: true}); err != nil {
			return err
		}
	}

	// A single sender owns both logs and empty application heartbeats. Reset
	// after each successful write so busy streams don't emit extra messages.
	heartbeat := time.NewTimer(15 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-heartbeat.C:
			if err := stream.Send(&agentpbv2.StreamLogsResponse{}); err != nil {
				return err
			}
			heartbeat.Reset(15 * time.Second)
		case <-stream.Context().Done():
			return stream.Context().Err()
		case item, ok := <-ch:
			if !ok {
				return nil
			}
			// Apply filters for live streaming too.
			if req.AppName != nil || req.ServiceName != nil || req.MinSeverity != nil {
				item = filterLogsV2(item, req)
				if item == nil {
					continue
				}
			}
			if err := stream.Send(&agentpbv2.StreamLogsResponse{Logs: item}); err != nil {
				return err
			}
			heartbeat.Reset(15 * time.Second)
		}
	}
}

func (s *TelemetryServiceV2) StreamMetrics(req *agentpbv2.StreamMetricsRequest, stream grpc.ServerStreamingServer[agentpbv2.StreamMetricsResponse]) error {
	// Subscribe first to buffer live items during replay; skip cache prefill
	// when replaying history to avoid duplicate metric deliveries.
	var subID string
	var ch <-chan *colmetricspb.ExportMetricsServiceRequest
	if req.LastN != nil && *req.LastN > 0 && s.buffer != nil && s.buffer.DiskEnabled() {
		subID, ch = s.broadcaster.SubscribeMetricsNoPrefill()
	} else {
		subID, ch = s.broadcaster.SubscribeMetrics()
	}
	defer s.broadcaster.UnsubscribeMetrics(subID)

	// Replay history after subscribing. As with logs, the tail window counts
	// batches that survive the filter, not the last N device-wide.
	if req.LastN != nil && *req.LastN > 0 && s.buffer != nil && s.buffer.DiskEnabled() {
		entries := s.buffer.ReadLastNMatching(SignalMetrics, int(*req.LastN), func(m proto.Message) proto.Message {
			metrics, ok := m.(*colmetricspb.ExportMetricsServiceRequest)
			if !ok {
				return nil
			}
			if req.ServiceName != nil || req.AppName != nil || req.MetricNamePrefix != nil {
				if metrics = filterMetricsV2(metrics, req); metrics == nil {
					return nil
				}
			}
			return metrics
		})
		for _, e := range entries {
			metrics, ok := e.(*colmetricspb.ExportMetricsServiceRequest)
			if !ok {
				continue
			}
			if err := stream.Send(&agentpbv2.StreamMetricsResponse{Metrics: metrics, IsHistory: true}); err != nil {
				return err
			}
		}
	}

	for {
		select {
		case <-stream.Context().Done():
			return stream.Context().Err()
		case item, ok := <-ch:
			if !ok {
				return nil
			}
			if req.ServiceName != nil || req.AppName != nil || req.MetricNamePrefix != nil {
				item = filterMetricsV2(item, req)
				if item == nil {
					continue
				}
			}
			if err := stream.Send(&agentpbv2.StreamMetricsResponse{Metrics: item}); err != nil {
				return err
			}
		}
	}
}

func (s *TelemetryServiceV2) StreamTraces(req *agentpbv2.StreamTracesRequest, stream grpc.ServerStreamingServer[agentpbv2.StreamTracesResponse]) error {
	// Subscribe first so live traces are buffered during history replay.
	// Traces have no in-memory cache prefill, so SubscribeTraces is always correct.
	subID, ch := s.broadcaster.SubscribeTraces()
	defer s.broadcaster.UnsubscribeTraces(subID)

	// Replay history after subscribing. As with logs, the tail window counts
	// batches that survive the filter, not the last N device-wide.
	if req.LastN != nil && *req.LastN > 0 && s.buffer != nil && s.buffer.DiskEnabled() {
		entries := s.buffer.ReadLastNMatching(SignalTraces, int(*req.LastN), func(m proto.Message) proto.Message {
			traces, ok := m.(*coltracepb.ExportTraceServiceRequest)
			if !ok {
				return nil
			}
			if req.ServiceName != nil || req.AppName != nil || req.SpanNamePrefix != nil {
				if traces = filterTracesV2(traces, req); traces == nil {
					return nil
				}
			}
			return traces
		})
		for _, e := range entries {
			traces, ok := e.(*coltracepb.ExportTraceServiceRequest)
			if !ok {
				continue
			}
			if err := stream.Send(&agentpbv2.StreamTracesResponse{Traces: traces, IsHistory: true}); err != nil {
				return err
			}
		}
	}

	for {
		select {
		case <-stream.Context().Done():
			return stream.Context().Err()
		case item, ok := <-ch:
			if !ok {
				return nil
			}
			if req.ServiceName != nil || req.AppName != nil || req.SpanNamePrefix != nil {
				item = filterTracesV2(item, req)
				if item == nil {
					continue
				}
			}
			if err := stream.Send(&agentpbv2.StreamTracesResponse{Traces: item}); err != nil {
				return err
			}
		}
	}
}

// filterLogsV2 filters log records based on the v2 stream request filters.
// Returns nil if all records are filtered out.
func filterLogsV2(req *collogspb.ExportLogsServiceRequest, filter *agentpbv2.StreamLogsRequest) *collogspb.ExportLogsServiceRequest {
	if filter == nil {
		return req
	}

	serviceName := filter.ServiceName
	appName := filter.AppName
	var minSeverity int32
	if filter.MinSeverity != nil {
		minSeverity = *filter.MinSeverity
	}

	if serviceName == nil && appName == nil && minSeverity == 0 {
		return req
	}

	var filteredResourceLogs []*logspb.ResourceLogs
	for _, rl := range req.GetResourceLogs() {
		if !matchResourceAttributes(rl.GetResource(), serviceName, appName) {
			continue
		}

		if minSeverity > 0 {
			var filteredScopeLogs []*logspb.ScopeLogs
			for _, sl := range rl.GetScopeLogs() {
				var filteredRecords []*logspb.LogRecord
				for _, lr := range sl.GetLogRecords() {
					if int32(lr.GetSeverityNumber()) >= minSeverity {
						filteredRecords = append(filteredRecords, lr)
					}
				}
				if len(filteredRecords) > 0 {
					filteredScopeLogs = append(filteredScopeLogs, &logspb.ScopeLogs{
						Scope:      sl.GetScope(),
						LogRecords: filteredRecords,
						SchemaUrl:  sl.GetSchemaUrl(),
					})
				}
			}
			if len(filteredScopeLogs) > 0 {
				filteredResourceLogs = append(filteredResourceLogs, &logspb.ResourceLogs{
					Resource:  rl.GetResource(),
					ScopeLogs: filteredScopeLogs,
					SchemaUrl: rl.GetSchemaUrl(),
				})
			}
		} else {
			filteredResourceLogs = append(filteredResourceLogs, rl)
		}
	}

	if len(filteredResourceLogs) == 0 {
		return nil
	}
	return &collogspb.ExportLogsServiceRequest{ResourceLogs: filteredResourceLogs}
}

// filterMetricsV2 filters metrics based on the v2 stream request filters.
// Returns nil if all metrics are filtered out.
func filterMetricsV2(req *colmetricspb.ExportMetricsServiceRequest, filter *agentpbv2.StreamMetricsRequest) *colmetricspb.ExportMetricsServiceRequest {
	if filter == nil {
		return req
	}

	serviceName := filter.ServiceName
	appName := filter.AppName
	metricNamePrefix := filter.MetricNamePrefix

	if serviceName == nil && appName == nil && metricNamePrefix == nil {
		return req
	}

	var filteredResourceMetrics []*metricspb.ResourceMetrics
	for _, rm := range req.GetResourceMetrics() {
		if !matchResourceAttributes(rm.GetResource(), serviceName, appName) {
			continue
		}

		if metricNamePrefix != nil {
			prefix := *metricNamePrefix
			var filteredScopeMetrics []*metricspb.ScopeMetrics
			for _, sm := range rm.GetScopeMetrics() {
				var filteredMetrics []*metricspb.Metric
				for _, m := range sm.GetMetrics() {
					if strings.HasPrefix(m.GetName(), prefix) {
						filteredMetrics = append(filteredMetrics, m)
					}
				}
				if len(filteredMetrics) > 0 {
					filteredScopeMetrics = append(filteredScopeMetrics, &metricspb.ScopeMetrics{
						Scope:     sm.GetScope(),
						Metrics:   filteredMetrics,
						SchemaUrl: sm.GetSchemaUrl(),
					})
				}
			}
			if len(filteredScopeMetrics) > 0 {
				filteredResourceMetrics = append(filteredResourceMetrics, &metricspb.ResourceMetrics{
					Resource:     rm.GetResource(),
					ScopeMetrics: filteredScopeMetrics,
					SchemaUrl:    rm.GetSchemaUrl(),
				})
			}
		} else {
			filteredResourceMetrics = append(filteredResourceMetrics, rm)
		}
	}

	if len(filteredResourceMetrics) == 0 {
		return nil
	}
	return &colmetricspb.ExportMetricsServiceRequest{ResourceMetrics: filteredResourceMetrics}
}

// filterTracesV2 filters traces based on the v2 stream request filters.
// Returns nil if all spans are filtered out.
func filterTracesV2(req *coltracepb.ExportTraceServiceRequest, filter *agentpbv2.StreamTracesRequest) *coltracepb.ExportTraceServiceRequest {
	if filter == nil {
		return req
	}

	serviceName := filter.ServiceName
	appName := filter.AppName
	spanNamePrefix := filter.SpanNamePrefix

	if serviceName == nil && appName == nil && spanNamePrefix == nil {
		return req
	}

	var filteredResourceSpans []*tracepb.ResourceSpans
	for _, rs := range req.GetResourceSpans() {
		if !matchResourceAttributes(rs.GetResource(), serviceName, appName) {
			continue
		}

		if spanNamePrefix != nil {
			prefix := *spanNamePrefix
			var filteredScopeSpans []*tracepb.ScopeSpans
			for _, ss := range rs.GetScopeSpans() {
				var filteredSpans []*tracepb.Span
				for _, s := range ss.GetSpans() {
					if strings.HasPrefix(s.GetName(), prefix) {
						filteredSpans = append(filteredSpans, s)
					}
				}
				if len(filteredSpans) > 0 {
					filteredScopeSpans = append(filteredScopeSpans, &tracepb.ScopeSpans{
						Scope:     ss.GetScope(),
						Spans:     filteredSpans,
						SchemaUrl: ss.GetSchemaUrl(),
					})
				}
			}
			if len(filteredScopeSpans) > 0 {
				filteredResourceSpans = append(filteredResourceSpans, &tracepb.ResourceSpans{
					Resource:   rs.GetResource(),
					ScopeSpans: filteredScopeSpans,
					SchemaUrl:  rs.GetSchemaUrl(),
				})
			}
		} else {
			filteredResourceSpans = append(filteredResourceSpans, rs)
		}
	}

	if len(filteredResourceSpans) == 0 {
		return nil
	}
	return &coltracepb.ExportTraceServiceRequest{ResourceSpans: filteredResourceSpans}
}
