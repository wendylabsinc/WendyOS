package services

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"go.uber.org/zap"
	"google.golang.org/grpc"

	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
	otelpb "github.com/wendylabsinc/wendy/proto/gen/otelpb"
)

const defaultMaxCachedLogs = 20

// TelemetryBroadcaster fans out received OTEL telemetry to multiple connected clients.
type TelemetryBroadcaster struct {
	mu            sync.RWMutex
	logSubs       map[string]chan *otelpb.ExportLogsServiceRequest
	metricSubs    map[string]chan *otelpb.ExportMetricsServiceRequest
	traceSubs     map[string]chan *otelpb.ExportTraceServiceRequest
	nextID        uint64
	recentLogs    []*otelpb.ExportLogsServiceRequest
	maxCachedLogs int
	latestMetrics map[string]*otelpb.ExportMetricsServiceRequest // keyed by "service:metric"
}

// NewTelemetryBroadcaster creates a new TelemetryBroadcaster.
func NewTelemetryBroadcaster() *TelemetryBroadcaster {
	return &TelemetryBroadcaster{
		logSubs:       make(map[string]chan *otelpb.ExportLogsServiceRequest),
		metricSubs:    make(map[string]chan *otelpb.ExportMetricsServiceRequest),
		traceSubs:     make(map[string]chan *otelpb.ExportTraceServiceRequest),
		maxCachedLogs: defaultMaxCachedLogs,
		latestMetrics: make(map[string]*otelpb.ExportMetricsServiceRequest),
	}
}

func (b *TelemetryBroadcaster) nextSubID() string {
	b.nextID++
	return fmt.Sprintf("sub-%d", b.nextID)
}

// SubscribeLogs adds a log subscriber and returns the channel and subscription ID.
// Cached recent logs are pre-filled into the channel asynchronously.
func (b *TelemetryBroadcaster) SubscribeLogs() (string, <-chan *otelpb.ExportLogsServiceRequest) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.nextSubID()
	ch := make(chan *otelpb.ExportLogsServiceRequest, 64)
	b.logSubs[id] = ch

	// Pre-fill cached logs into the channel in a goroutine.
	if len(b.recentLogs) > 0 {
		cached := make([]*otelpb.ExportLogsServiceRequest, len(b.recentLogs))
		copy(cached, b.recentLogs)
		go func() {
			for _, entry := range cached {
				select {
				case ch <- entry:
				default:
					return // channel full, stop pre-filling
				}
			}
		}()
	}

	return id, ch
}

// UnsubscribeLogs removes a log subscriber.
func (b *TelemetryBroadcaster) UnsubscribeLogs(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ch, ok := b.logSubs[id]; ok {
		close(ch)
		delete(b.logSubs, id)
	}
}

// PublishLogs sends a log export request to all log subscribers and caches the log.
func (b *TelemetryBroadcaster) PublishLogs(req *otelpb.ExportLogsServiceRequest) {
	b.mu.Lock()
	// Append to recent logs ring buffer.
	b.recentLogs = append(b.recentLogs, req)
	if len(b.recentLogs) > b.maxCachedLogs {
		b.recentLogs = b.recentLogs[len(b.recentLogs)-b.maxCachedLogs:]
	}
	b.mu.Unlock()

	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, ch := range b.logSubs {
		select {
		case ch <- req:
		default:
			// Drop if subscriber is slow.
		}
	}
}

// SubscribeMetrics adds a metrics subscriber.
// Cached latest metrics are pre-filled into the channel asynchronously.
func (b *TelemetryBroadcaster) SubscribeMetrics() (string, <-chan *otelpb.ExportMetricsServiceRequest) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.nextSubID()
	ch := make(chan *otelpb.ExportMetricsServiceRequest, 64)
	b.metricSubs[id] = ch

	// Pre-fill cached metrics into the channel in a goroutine.
	if len(b.latestMetrics) > 0 {
		cached := make([]*otelpb.ExportMetricsServiceRequest, 0, len(b.latestMetrics))
		for _, v := range b.latestMetrics {
			cached = append(cached, v)
		}
		go func() {
			for _, entry := range cached {
				select {
				case ch <- entry:
				default:
					return
				}
			}
		}()
	}

	return id, ch
}

// UnsubscribeMetrics removes a metrics subscriber.
func (b *TelemetryBroadcaster) UnsubscribeMetrics(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ch, ok := b.metricSubs[id]; ok {
		close(ch)
		delete(b.metricSubs, id)
	}
}

// PublishMetrics sends a metrics export request to all metrics subscribers and updates the cache.
func (b *TelemetryBroadcaster) PublishMetrics(req *otelpb.ExportMetricsServiceRequest) {
	b.mu.Lock()
	// Update latestMetrics cache keyed by "serviceName:metricName".
	for _, rm := range req.GetResourceMetrics() {
		serviceName := resourceServiceName(rm.GetResource())
		for _, sm := range rm.GetScopeMetrics() {
			for _, m := range sm.GetMetrics() {
				key := serviceName + ":" + m.GetName()
				b.latestMetrics[key] = req
			}
		}
	}
	b.mu.Unlock()

	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, ch := range b.metricSubs {
		select {
		case ch <- req:
		default:
		}
	}
}

// SubscribeTraces adds a traces subscriber.
func (b *TelemetryBroadcaster) SubscribeTraces() (string, <-chan *otelpb.ExportTraceServiceRequest) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.nextSubID()
	ch := make(chan *otelpb.ExportTraceServiceRequest, 64)
	b.traceSubs[id] = ch
	return id, ch
}

// UnsubscribeTraces removes a traces subscriber.
func (b *TelemetryBroadcaster) UnsubscribeTraces(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ch, ok := b.traceSubs[id]; ok {
		close(ch)
		delete(b.traceSubs, id)
	}
}

// PublishTraces sends a trace export request to all trace subscribers.
func (b *TelemetryBroadcaster) PublishTraces(req *otelpb.ExportTraceServiceRequest) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, ch := range b.traceSubs {
		select {
		case ch <- req:
		default:
		}
	}
}

// TelemetryService implements agentpb.WendyTelemetryServiceServer.
type TelemetryService struct {
	agentpb.UnimplementedWendyTelemetryServiceServer
	logger      *zap.Logger
	broadcaster *TelemetryBroadcaster
}

// NewTelemetryService creates a new TelemetryService.
func NewTelemetryService(logger *zap.Logger, broadcaster *TelemetryBroadcaster) *TelemetryService {
	return &TelemetryService{
		logger:      logger,
		broadcaster: broadcaster,
	}
}

// Broadcaster returns the underlying broadcaster for publishing telemetry data.
func (s *TelemetryService) Broadcaster() *TelemetryBroadcaster {
	return s.broadcaster
}

// StreamLogs streams filtered log records to the client.
func (s *TelemetryService) StreamLogs(req *agentpb.StreamLogsRequest, stream grpc.ServerStreamingServer[agentpb.StreamLogsResponse]) error {
	ctx := stream.Context()

	id, ch := s.broadcaster.SubscribeLogs()
	defer s.broadcaster.UnsubscribeLogs(id)

	s.logger.Info("StreamLogs client connected", zap.String("sub_id", id))

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case logReq, ok := <-ch:
			if !ok {
				return nil
			}

			// Apply filters if requested.
			if req.AppName != nil || req.ServiceName != nil || req.MinSeverity != nil {
				logReq = filterLogs(logReq, req)
				if logReq == nil {
					continue
				}
			}

			if err := stream.Send(&agentpb.StreamLogsResponse{
				Logs: logReq,
			}); err != nil {
				return err
			}
		}
	}
}

// StreamMetrics streams filtered metrics to the client.
func (s *TelemetryService) StreamMetrics(req *agentpb.StreamMetricsRequest, stream grpc.ServerStreamingServer[agentpb.StreamMetricsResponse]) error {
	ctx := stream.Context()

	id, ch := s.broadcaster.SubscribeMetrics()
	defer s.broadcaster.UnsubscribeMetrics(id)

	s.logger.Info("StreamMetrics client connected", zap.String("sub_id", id))

	hasFilter := req.ServiceName != nil || req.AppName != nil || req.MetricNamePrefix != nil

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case metricsReq, ok := <-ch:
			if !ok {
				return nil
			}

			if hasFilter {
				metricsReq = filterMetrics(metricsReq, req)
				if metricsReq == nil {
					continue
				}
			}

			if err := stream.Send(&agentpb.StreamMetricsResponse{
				Metrics: metricsReq,
			}); err != nil {
				return err
			}
		}
	}
}

// StreamTraces streams filtered traces to the client.
func (s *TelemetryService) StreamTraces(req *agentpb.StreamTracesRequest, stream grpc.ServerStreamingServer[agentpb.StreamTracesResponse]) error {
	ctx := stream.Context()

	id, ch := s.broadcaster.SubscribeTraces()
	defer s.broadcaster.UnsubscribeTraces(id)

	s.logger.Info("StreamTraces client connected", zap.String("sub_id", id))

	hasFilter := req.ServiceName != nil || req.AppName != nil || req.SpanNamePrefix != nil

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case traceReq, ok := <-ch:
			if !ok {
				return nil
			}

			if hasFilter {
				traceReq = filterTraces(traceReq, req)
				if traceReq == nil {
					continue
				}
			}

			if err := stream.Send(&agentpb.StreamTracesResponse{
				Traces: traceReq,
			}); err != nil {
				return err
			}
		}
	}
}

// OTELLogsReceiver implements otelpb.LogsServiceServer so the agent can receive
// OTEL logs from containers and broadcast them to CLI clients.
type OTELLogsReceiver struct {
	otelpb.UnimplementedLogsServiceServer
	broadcaster *TelemetryBroadcaster
}

// NewOTELLogsReceiver creates a new OTELLogsReceiver.
func NewOTELLogsReceiver(b *TelemetryBroadcaster) *OTELLogsReceiver {
	return &OTELLogsReceiver{broadcaster: b}
}

// Export receives OTEL logs and fans them out to subscribers.
func (r *OTELLogsReceiver) Export(_ context.Context, req *otelpb.ExportLogsServiceRequest) (*otelpb.ExportLogsServiceResponse, error) {
	r.broadcaster.PublishLogs(req)
	return &otelpb.ExportLogsServiceResponse{}, nil
}

// OTELMetricsReceiver implements otelpb.MetricsServiceServer.
type OTELMetricsReceiver struct {
	otelpb.UnimplementedMetricsServiceServer
	broadcaster *TelemetryBroadcaster
}

// NewOTELMetricsReceiver creates a new OTELMetricsReceiver.
func NewOTELMetricsReceiver(b *TelemetryBroadcaster) *OTELMetricsReceiver {
	return &OTELMetricsReceiver{broadcaster: b}
}

// Export receives OTEL metrics and fans them out to subscribers.
func (r *OTELMetricsReceiver) Export(_ context.Context, req *otelpb.ExportMetricsServiceRequest) (*otelpb.ExportMetricsServiceResponse, error) {
	r.broadcaster.PublishMetrics(req)
	return &otelpb.ExportMetricsServiceResponse{}, nil
}

// OTELTraceReceiver implements otelpb.TraceServiceServer.
type OTELTraceReceiver struct {
	otelpb.UnimplementedTraceServiceServer
	broadcaster *TelemetryBroadcaster
}

// NewOTELTraceReceiver creates a new OTELTraceReceiver.
func NewOTELTraceReceiver(b *TelemetryBroadcaster) *OTELTraceReceiver {
	return &OTELTraceReceiver{broadcaster: b}
}

// Export receives OTEL traces and fans them out to subscribers.
func (r *OTELTraceReceiver) Export(_ context.Context, req *otelpb.ExportTraceServiceRequest) (*otelpb.ExportTraceServiceResponse, error) {
	r.broadcaster.PublishTraces(req)
	return &otelpb.ExportTraceServiceResponse{}, nil
}

// matchResourceAttributes checks whether a resource's attributes match the given
// service name filter. Returns true if all specified filters match.
func matchResourceAttributes(resource *otelpb.Resource, serviceName *string, appName *string) bool {
	if serviceName == nil && appName == nil {
		return true
	}
	for _, attr := range resource.GetAttributes() {
		if attr.GetKey() == "service.name" {
			val := attr.GetValue().GetStringValue()
			if serviceName != nil && val == *serviceName {
				return true
			}
			if appName != nil && val == *appName {
				return true
			}
			return false
		}
	}
	return false
}

// resourceServiceName extracts the service.name attribute from a resource, if present.
func resourceServiceName(resource *otelpb.Resource) string {
	for _, attr := range resource.GetAttributes() {
		if attr.GetKey() == "service.name" {
			return attr.GetValue().GetStringValue()
		}
	}
	return ""
}

// filterLogs filters log records based on the stream request filters.
// Returns nil if all records are filtered out.
func filterLogs(req *otelpb.ExportLogsServiceRequest, filter *agentpb.StreamLogsRequest) *otelpb.ExportLogsServiceRequest {
	if filter == nil {
		return req
	}

	serviceName := filter.ServiceName
	appName := filter.AppName
	var minSeverity int32
	if filter.MinSeverity != nil {
		minSeverity = *filter.MinSeverity
	}

	// If no filters, pass through.
	if serviceName == nil && appName == nil && minSeverity == 0 {
		return req
	}

	var filteredResourceLogs []*otelpb.ResourceLogs
	for _, rl := range req.GetResourceLogs() {
		// Check resource attributes for service.name.
		if !matchResourceAttributes(rl.GetResource(), serviceName, appName) {
			continue
		}

		// Filter by severity if specified.
		if minSeverity > 0 {
			var filteredScopeLogs []*otelpb.ScopeLogs
			for _, sl := range rl.GetScopeLogs() {
				var filteredRecords []*otelpb.LogRecord
				for _, lr := range sl.GetLogRecords() {
					if int32(lr.GetSeverityNumber()) >= minSeverity {
						filteredRecords = append(filteredRecords, lr)
					}
				}
				if len(filteredRecords) > 0 {
					filtered := &otelpb.ScopeLogs{
						Scope:      sl.GetScope(),
						LogRecords: filteredRecords,
						SchemaUrl:  sl.GetSchemaUrl(),
					}
					filteredScopeLogs = append(filteredScopeLogs, filtered)
				}
			}
			if len(filteredScopeLogs) > 0 {
				filteredResourceLogs = append(filteredResourceLogs, &otelpb.ResourceLogs{
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
	return &otelpb.ExportLogsServiceRequest{ResourceLogs: filteredResourceLogs}
}

// filterMetrics filters metrics based on the stream request filters.
// Returns nil if all metrics are filtered out.
func filterMetrics(req *otelpb.ExportMetricsServiceRequest, filter *agentpb.StreamMetricsRequest) *otelpb.ExportMetricsServiceRequest {
	if filter == nil {
		return req
	}

	serviceName := filter.ServiceName
	appName := filter.AppName
	metricNamePrefix := filter.MetricNamePrefix

	if serviceName == nil && appName == nil && metricNamePrefix == nil {
		return req
	}

	var filteredResourceMetrics []*otelpb.ResourceMetrics
	for _, rm := range req.GetResourceMetrics() {
		if !matchResourceAttributes(rm.GetResource(), serviceName, appName) {
			continue
		}

		if metricNamePrefix != nil {
			prefix := *metricNamePrefix
			var filteredScopeMetrics []*otelpb.ScopeMetrics
			for _, sm := range rm.GetScopeMetrics() {
				var filteredMetrics []*otelpb.Metric
				for _, m := range sm.GetMetrics() {
					if strings.HasPrefix(m.GetName(), prefix) {
						filteredMetrics = append(filteredMetrics, m)
					}
				}
				if len(filteredMetrics) > 0 {
					filteredScopeMetrics = append(filteredScopeMetrics, &otelpb.ScopeMetrics{
						Scope:     sm.GetScope(),
						Metrics:   filteredMetrics,
						SchemaUrl: sm.GetSchemaUrl(),
					})
				}
			}
			if len(filteredScopeMetrics) > 0 {
				filteredResourceMetrics = append(filteredResourceMetrics, &otelpb.ResourceMetrics{
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
	return &otelpb.ExportMetricsServiceRequest{ResourceMetrics: filteredResourceMetrics}
}

// filterTraces filters traces based on the stream request filters.
// Returns nil if all spans are filtered out.
func filterTraces(req *otelpb.ExportTraceServiceRequest, filter *agentpb.StreamTracesRequest) *otelpb.ExportTraceServiceRequest {
	if filter == nil {
		return req
	}

	serviceName := filter.ServiceName
	spanNamePrefix := filter.SpanNamePrefix

	if serviceName == nil && spanNamePrefix == nil {
		return req
	}

	var filteredResourceSpans []*otelpb.ResourceSpans
	for _, rs := range req.GetResourceSpans() {
		if !matchResourceAttributes(rs.GetResource(), serviceName) {
			continue
		}

		if spanNamePrefix != nil {
			prefix := *spanNamePrefix
			var filteredScopeSpans []*otelpb.ScopeSpans
			for _, ss := range rs.GetScopeSpans() {
				var filteredSpans []*otelpb.Span
				for _, s := range ss.GetSpans() {
					if strings.HasPrefix(s.GetName(), prefix) {
						filteredSpans = append(filteredSpans, s)
					}
				}
				if len(filteredSpans) > 0 {
					filteredScopeSpans = append(filteredScopeSpans, &otelpb.ScopeSpans{
						Scope:     ss.GetScope(),
						Spans:     filteredSpans,
						SchemaUrl: ss.GetSchemaUrl(),
					})
				}
			}
			if len(filteredScopeSpans) > 0 {
				filteredResourceSpans = append(filteredResourceSpans, &otelpb.ResourceSpans{
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
	return &otelpb.ExportTraceServiceRequest{ResourceSpans: filteredResourceSpans}
}

// Ensure compile-time interface compliance.
var (
	_ agentpb.WendyTelemetryServiceServer = (*TelemetryService)(nil)
	_ otelpb.LogsServiceServer            = (*OTELLogsReceiver)(nil)
	_ otelpb.MetricsServiceServer         = (*OTELMetricsReceiver)(nil)
	_ otelpb.TraceServiceServer           = (*OTELTraceReceiver)(nil)
)
