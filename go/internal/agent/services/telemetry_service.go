package services

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

const (
	defaultMaxCachedLogs = 20
	// maxCachedResourcesPerService caps how many distinct ResourceMetrics entries
	// (e.g. individual pods / containers) are retained in the per-service metrics
	// cache. Without this cap, high-churn resource attributes (like container IDs
	// that change on every restart) would grow the cache without bound.
	// Oldest entries are evicted first (FIFO) when the cap is reached.
	maxCachedResourcesPerService = 100
)

type TelemetryBroadcaster struct {
	mu            sync.RWMutex
	logSubs       map[string]chan *collogspb.ExportLogsServiceRequest
	metricSubs    map[string]chan *colmetricspb.ExportMetricsServiceRequest
	traceSubs     map[string]chan *coltracepb.ExportTraceServiceRequest
	nextID        uint64
	recentLogs    [defaultMaxCachedLogs]*collogspb.ExportLogsServiceRequest
	logHead       int                                                  // next write index (0..defaultMaxCachedLogs-1)
	logCount      int                                                  // number of valid entries (0..defaultMaxCachedLogs)
	latestMetrics map[string]*colmetricspb.ExportMetricsServiceRequest // keyed by "service"
}

func NewTelemetryBroadcaster() *TelemetryBroadcaster {
	return &TelemetryBroadcaster{
		logSubs:       make(map[string]chan *collogspb.ExportLogsServiceRequest),
		metricSubs:    make(map[string]chan *colmetricspb.ExportMetricsServiceRequest),
		traceSubs:     make(map[string]chan *coltracepb.ExportTraceServiceRequest),
		latestMetrics: make(map[string]*colmetricspb.ExportMetricsServiceRequest),
	}
}

func (b *TelemetryBroadcaster) nextSubID() string {
	b.nextID++
	return fmt.Sprintf("sub-%d", b.nextID)
}

// SubscribeLogs adds a log subscriber and returns the channel and subscription ID.
// Cached recent logs are pre-filled into the channel synchronously before returning.
func (b *TelemetryBroadcaster) SubscribeLogs() (string, <-chan *collogspb.ExportLogsServiceRequest) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.nextSubID()
	ch := make(chan *collogspb.ExportLogsServiceRequest, 64)
	b.logSubs[id] = ch

	// Pre-fill cached logs synchronously while the lock is held. The 64-slot
	// buffer is always larger than defaultMaxCachedLogs (20), so these sends
	// never block and eliminate the data race between a background goroutine
	// sending on ch and UnsubscribeLogs closing it.
	if b.logCount > 0 {
		start := (b.logHead - b.logCount + defaultMaxCachedLogs) % defaultMaxCachedLogs
		for i := 0; i < b.logCount; i++ {
			ch <- b.recentLogs[(start+i)%defaultMaxCachedLogs]
		}
	}

	return id, ch
}

// SubscribeLogsWithHistory registers a log subscriber and returns cached
// batches separately from batches published after registration. Registration
// and the cache snapshot happen under the same lock, so no batch can fall
// between the two results.
func (b *TelemetryBroadcaster) SubscribeLogsWithHistory() (string, []*collogspb.ExportLogsServiceRequest, <-chan *collogspb.ExportLogsServiceRequest) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.nextSubID()
	ch := make(chan *collogspb.ExportLogsServiceRequest, 64)
	b.logSubs[id] = ch

	recent := make([]*collogspb.ExportLogsServiceRequest, 0, b.logCount)
	start := (b.logHead - b.logCount + defaultMaxCachedLogs) % defaultMaxCachedLogs
	for i := 0; i < b.logCount; i++ {
		recent = append(recent, b.recentLogs[(start+i)%defaultMaxCachedLogs])
	}
	return id, recent, ch
}

// SubscribeLogsNoPrefill adds a log subscriber without pre-filling cached logs.
// Use this when the caller will replay disk history itself (via ReadLastN) to
// avoid sending the same recent batches twice — once from disk and again from
// the in-memory cache. Live telemetry published after this call is buffered in
// the returned channel and will be delivered after the caller's history replay.
func (b *TelemetryBroadcaster) SubscribeLogsNoPrefill() (string, <-chan *collogspb.ExportLogsServiceRequest) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.nextSubID()
	ch := make(chan *collogspb.ExportLogsServiceRequest, 64)
	b.logSubs[id] = ch
	return id, ch
}

// SubscribeMetricsNoPrefill adds a metrics subscriber without pre-filling cached snapshots.
// Use when the caller replays disk history to avoid duplicate metric deliveries.
func (b *TelemetryBroadcaster) SubscribeMetricsNoPrefill() (string, <-chan *colmetricspb.ExportMetricsServiceRequest) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.nextSubID()
	ch := make(chan *colmetricspb.ExportMetricsServiceRequest, 64)
	b.metricSubs[id] = ch
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

func (b *TelemetryBroadcaster) PublishLogs(req *collogspb.ExportLogsServiceRequest) {
	b.mu.Lock()
	b.recentLogs[b.logHead] = req
	b.logHead = (b.logHead + 1) % defaultMaxCachedLogs
	if b.logCount < defaultMaxCachedLogs {
		b.logCount++
	}
	for _, ch := range b.logSubs {
		select {
		case ch <- req:
		default:
			// Drop if subscriber is slow.
		}
	}
	b.mu.Unlock()
}

// SubscribeMetrics adds a metrics subscriber.
// Cached latest metrics are pre-filled into the channel synchronously before returning.
func (b *TelemetryBroadcaster) SubscribeMetrics() (string, <-chan *colmetricspb.ExportMetricsServiceRequest) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.nextSubID()
	ch := make(chan *colmetricspb.ExportMetricsServiceRequest, 64)
	b.metricSubs[id] = ch

	// Pre-fill one snapshot per service synchronously. Clones are made while
	// the lock is held so concurrent PublishMetrics cannot mutate the cached
	// object after it is enqueued. Non-blocking sends guard against the
	// unlikely case where the number of distinct service names exceeds 64.
	if len(b.latestMetrics) > 0 {
		seen := make(map[*colmetricspb.ExportMetricsServiceRequest]bool, len(b.latestMetrics))
		for _, v := range b.latestMetrics {
			if !seen[v] {
				seen[v] = true
				select {
				case ch <- proto.Clone(v).(*colmetricspb.ExportMetricsServiceRequest):
				default:
				}
			}
		}
	}

	return id, ch
}

func (b *TelemetryBroadcaster) UnsubscribeMetrics(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ch, ok := b.metricSubs[id]; ok {
		close(ch)
		delete(b.metricSubs, id)
	}
}

func (b *TelemetryBroadcaster) PublishMetrics(req *colmetricspb.ExportMetricsServiceRequest) {
	b.mu.Lock()
	for _, rm := range req.GetResourceMetrics() {
		serviceName := resourceServiceName(rm.GetResource())
		b.latestMetrics[serviceName] = mergeServiceMetrics(b.latestMetrics[serviceName], rm)
	}
	for _, ch := range b.metricSubs {
		select {
		case ch <- req:
		default:
		}
	}
	b.mu.Unlock()
}

// mergeServiceMetrics upserts the metrics in rm into the cached per-service
// request, keyed by resource identity, scope identity, and metric name.
// Metrics absent from the new batch are retained so partial batches do not
// drop previously reported metrics for late subscribers. Multiple resource
// instances sharing the same service.name (e.g. different pods) are kept as
// separate ResourceMetrics entries distinguished by their resource attributes.
func mergeServiceMetrics(cached *colmetricspb.ExportMetricsServiceRequest, rm *metricspb.ResourceMetrics) *colmetricspb.ExportMetricsServiceRequest {
	// Clone rm so the cache never holds references to live-broadcast request objects.
	// Without this, a subscriber that has queued a broadcast req could observe mutations
	// to its ResourceMetrics objects the next time the same service publishes a batch.
	rm = proto.Clone(rm).(*metricspb.ResourceMetrics)
	if cached == nil || len(cached.GetResourceMetrics()) == 0 {
		return &colmetricspb.ExportMetricsServiceRequest{
			ResourceMetrics: []*metricspb.ResourceMetrics{rm},
		}
	}

	// Find the cached ResourceMetrics with matching resource identity. Different
	// instances of the same service (e.g. pods) share service.name but differ in
	// other attributes (e.g. service.instance.id); keying by full resource identity
	// preserves each instance's metrics as a separate entry.
	rmKey := resourceKey(rm.GetResource())
	var dst *metricspb.ResourceMetrics
	for _, existing := range cached.GetResourceMetrics() {
		if resourceKey(existing.GetResource()) == rmKey {
			dst = existing
			break
		}
	}
	if dst == nil {
		cached.ResourceMetrics = append(cached.ResourceMetrics, rm)
		// Evict oldest entries once the per-service cap is reached so that
		// high-churn resource attributes (e.g. unique container IDs per restart)
		// cannot grow the cache without bound.
		if len(cached.ResourceMetrics) > maxCachedResourcesPerService {
			cached.ResourceMetrics = cached.ResourceMetrics[len(cached.ResourceMetrics)-maxCachedResourcesPerService:]
		}
		return cached
	}

	dst.Resource = rm.GetResource()
	dst.SchemaUrl = rm.GetSchemaUrl()

	// Index by full scope identity (name + version + schema_url) to avoid
	// conflating scopes that share a name but differ in version or schema.
	scopeIdx := make(map[string]*metricspb.ScopeMetrics, len(dst.GetScopeMetrics()))
	for _, sm := range dst.GetScopeMetrics() {
		scopeIdx[scopeKey(sm)] = sm
	}

	for _, sm := range rm.GetScopeMetrics() {
		key := scopeKey(sm)
		existing, ok := scopeIdx[key]
		if !ok {
			dst.ScopeMetrics = append(dst.ScopeMetrics, sm)
			scopeIdx[key] = sm
			continue
		}
		metricIdx := make(map[string]int, len(existing.GetMetrics()))
		for i, m := range existing.GetMetrics() {
			metricIdx[m.GetName()] = i
		}
		for _, m := range sm.GetMetrics() {
			if i, ok := metricIdx[m.GetName()]; ok {
				existing.Metrics[i] = m
			} else {
				existing.Metrics = append(existing.Metrics, m)
				metricIdx[m.GetName()] = len(existing.Metrics) - 1
			}
		}
	}
	return cached
}

func resourceKey(r *resourcepb.Resource) string {
	attrs := r.GetAttributes()
	if len(attrs) == 0 {
		return ""
	}
	parts := make([]string, len(attrs))
	for i, kv := range attrs {
		parts[i] = kv.GetKey() + "=" + kv.GetValue().String()
	}
	sort.Strings(parts)
	return strings.Join(parts, "\x00")
}

func scopeKey(sm *metricspb.ScopeMetrics) string {
	return sm.GetScope().GetName() + "\x00" + sm.GetScope().GetVersion() + "\x00" + sm.GetSchemaUrl()
}

func (b *TelemetryBroadcaster) SubscribeTraces() (string, <-chan *coltracepb.ExportTraceServiceRequest) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.nextSubID()
	ch := make(chan *coltracepb.ExportTraceServiceRequest, 64)
	b.traceSubs[id] = ch
	return id, ch
}

func (b *TelemetryBroadcaster) UnsubscribeTraces(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ch, ok := b.traceSubs[id]; ok {
		close(ch)
		delete(b.traceSubs, id)
	}
}

func (b *TelemetryBroadcaster) PublishTraces(req *coltracepb.ExportTraceServiceRequest) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, ch := range b.traceSubs {
		select {
		case ch <- req:
		default:
		}
	}
}

type TelemetryService struct {
	agentpb.UnimplementedWendyTelemetryServiceServer
	logger      *zap.Logger
	broadcaster *TelemetryBroadcaster
	buffer      *TelemetryBuffer // nil if disk buffering is unavailable
}

// NewTelemetryService creates a new TelemetryService.
func NewTelemetryService(logger *zap.Logger, broadcaster *TelemetryBroadcaster, buffer *TelemetryBuffer) *TelemetryService {
	return &TelemetryService{
		logger:      logger,
		broadcaster: broadcaster,
		buffer:      buffer,
	}
}

func (s *TelemetryService) Broadcaster() *TelemetryBroadcaster {
	return s.broadcaster
}

func (s *TelemetryService) StreamLogs(req *agentpb.StreamLogsRequest, stream grpc.ServerStreamingServer[agentpb.StreamLogsResponse]) error {
	ctx := stream.Context()

	// When replaying history, subscribe without cache prefill first so that
	// live telemetry published during replay is buffered rather than lost,
	// and to avoid sending the same recent batches twice (once from disk and
	// once from the broadcaster's in-memory ring buffer).
	var id string
	var recent []*collogspb.ExportLogsServiceRequest
	var ch <-chan *collogspb.ExportLogsServiceRequest
	if req.LastN != nil && *req.LastN > 0 && s.buffer != nil && s.buffer.DiskEnabled() {
		id, ch = s.broadcaster.SubscribeLogsNoPrefill()
	} else {
		id, recent, ch = s.broadcaster.SubscribeLogsWithHistory()
	}
	defer s.broadcaster.UnsubscribeLogs(id)

	// Replay history if requested (after subscribing so no live items are missed).
	if req.LastN != nil && *req.LastN > 0 && s.buffer != nil && s.buffer.DiskEnabled() {
		// Count the tail window against batches that survive the filter: the
		// last N batches device-wide may all belong to a chatty co-tenant,
		// which would replay nothing for the requested app.
		entries := s.buffer.ReadLastNMatching(SignalLogs, int(*req.LastN), func(m proto.Message) proto.Message {
			logs, ok := m.(*collogspb.ExportLogsServiceRequest)
			if !ok {
				return nil
			}
			if req.AppName != nil || req.ServiceName != nil || req.MinSeverity != nil {
				if logs = filterLogs(logs, req); logs == nil {
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
			if err := stream.Send(&agentpb.StreamLogsResponse{Logs: logs, IsHistory: true}); err != nil {
				return err
			}
		}
	}

	// Cached batches predate the subscription, so send them before the live
	// channel with IsHistory set. Apply the request filters to both paths.
	for _, logs := range recent {
		if req.AppName != nil || req.ServiceName != nil || req.MinSeverity != nil {
			logs = filterLogs(logs, req)
			if logs == nil {
				continue
			}
		}
		if err := stream.Send(&agentpb.StreamLogsResponse{Logs: logs, IsHistory: true}); err != nil {
			return err
		}
	}

	s.logger.Info("StreamLogs client connected", zap.String("sub_id", id))

	// A single sender owns both logs and empty application heartbeats. Reset
	// after each successful write so busy streams don't emit extra messages.
	heartbeat := time.NewTimer(15 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-heartbeat.C:
			if err := stream.Send(&agentpb.StreamLogsResponse{}); err != nil {
				return err
			}
			heartbeat.Reset(15 * time.Second)
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
			heartbeat.Reset(15 * time.Second)
		}
	}
}

func (s *TelemetryService) StreamMetrics(req *agentpb.StreamMetricsRequest, stream grpc.ServerStreamingServer[agentpb.StreamMetricsResponse]) error {
	ctx := stream.Context()

	// Subscribe first (without cache prefill when replaying history) so that
	// live telemetry is buffered during the replay window and not lost.
	var id string
	var ch <-chan *colmetricspb.ExportMetricsServiceRequest
	if req.LastN != nil && *req.LastN > 0 && s.buffer != nil && s.buffer.DiskEnabled() {
		id, ch = s.broadcaster.SubscribeMetricsNoPrefill()
	} else {
		id, ch = s.broadcaster.SubscribeMetrics()
	}
	defer s.broadcaster.UnsubscribeMetrics(id)

	// Replay history after subscribing. As with logs, the tail window counts
	// batches that survive the filter, not the last N device-wide.
	if req.LastN != nil && *req.LastN > 0 && s.buffer != nil && s.buffer.DiskEnabled() {
		entries := s.buffer.ReadLastNMatching(SignalMetrics, int(*req.LastN), func(m proto.Message) proto.Message {
			metrics, ok := m.(*colmetricspb.ExportMetricsServiceRequest)
			if !ok {
				return nil
			}
			if req.ServiceName != nil || req.AppName != nil || req.MetricNamePrefix != nil {
				if metrics = filterMetrics(metrics, req); metrics == nil {
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
			if err := stream.Send(&agentpb.StreamMetricsResponse{Metrics: metrics, IsHistory: true}); err != nil {
				return err
			}
		}
	}

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

func (s *TelemetryService) StreamTraces(req *agentpb.StreamTracesRequest, stream grpc.ServerStreamingServer[agentpb.StreamTracesResponse]) error {
	ctx := stream.Context()

	// Subscribe first so live traces published during history replay are buffered.
	// Traces have no in-memory cache prefill so SubscribeTraces is always correct here.
	id, ch := s.broadcaster.SubscribeTraces()
	defer s.broadcaster.UnsubscribeTraces(id)

	// Replay history after subscribing. As with logs, the tail window counts
	// batches that survive the filter, not the last N device-wide.
	if req.LastN != nil && *req.LastN > 0 && s.buffer != nil && s.buffer.DiskEnabled() {
		entries := s.buffer.ReadLastNMatching(SignalTraces, int(*req.LastN), func(m proto.Message) proto.Message {
			traces, ok := m.(*coltracepb.ExportTraceServiceRequest)
			if !ok {
				return nil
			}
			if req.ServiceName != nil || req.AppName != nil || req.SpanNamePrefix != nil {
				if traces = filterTraces(traces, req); traces == nil {
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
			if err := stream.Send(&agentpb.StreamTracesResponse{Traces: traces, IsHistory: true}); err != nil {
				return err
			}
		}
	}

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

type OTELLogsReceiver struct {
	collogspb.UnimplementedLogsServiceServer
	broadcaster TelemetryPublisher
}

// NewOTELLogsReceiver creates a new OTELLogsReceiver.
func NewOTELLogsReceiver(b TelemetryPublisher) *OTELLogsReceiver {
	return &OTELLogsReceiver{broadcaster: b}
}

func (r *OTELLogsReceiver) Export(_ context.Context, req *collogspb.ExportLogsServiceRequest) (*collogspb.ExportLogsServiceResponse, error) {
	r.broadcaster.PublishLogs(req)
	return &collogspb.ExportLogsServiceResponse{}, nil
}

type OTELMetricsReceiver struct {
	colmetricspb.UnimplementedMetricsServiceServer
	broadcaster TelemetryPublisher
}

// NewOTELMetricsReceiver creates a new OTELMetricsReceiver.
func NewOTELMetricsReceiver(b TelemetryPublisher) *OTELMetricsReceiver {
	return &OTELMetricsReceiver{broadcaster: b}
}

func (r *OTELMetricsReceiver) Export(_ context.Context, req *colmetricspb.ExportMetricsServiceRequest) (*colmetricspb.ExportMetricsServiceResponse, error) {
	r.broadcaster.PublishMetrics(req)
	return &colmetricspb.ExportMetricsServiceResponse{}, nil
}

type OTELTraceReceiver struct {
	coltracepb.UnimplementedTraceServiceServer
	broadcaster TelemetryPublisher
}

// NewOTELTraceReceiver creates a new OTELTraceReceiver.
func NewOTELTraceReceiver(b TelemetryPublisher) *OTELTraceReceiver {
	return &OTELTraceReceiver{broadcaster: b}
}

func (r *OTELTraceReceiver) Export(_ context.Context, req *coltracepb.ExportTraceServiceRequest) (*coltracepb.ExportTraceServiceResponse, error) {
	r.broadcaster.PublishTraces(req)
	return &coltracepb.ExportTraceServiceResponse{}, nil
}

func matchResourceAttributes(resource *resourcepb.Resource, serviceName *string, appName *string) bool {
	if serviceName == nil && appName == nil {
		return true
	}
	for _, attr := range resource.GetAttributes() {
		if attr.GetKey() == "service.name" {
			val := attr.GetValue().GetStringValue()
			if serviceName != nil && val == *serviceName {
				return true
			}
			if appName != nil && resourceBelongsToApp(val, *appName) {
				return true
			}
			return false
		}
	}
	return false
}

// resourceBelongsToApp reports whether a telemetry resource's service.name
// belongs to the given app: either the bare appID, or a per-service container
// name "{appID}_{serviceName}" (see containerd.ContainerName). Output the
// container monitor captures while restart-looping a services-map app is
// published under the container name, so an --app filter that only matched the
// bare appID made crash output unreachable (WDY-1826). The suffix must be a
// valid service name so an unrelated app that merely shares the prefix (e.g.
// "myapp_V2" for --app myapp) is not swept in.
func resourceBelongsToApp(resourceService, appName string) bool {
	if resourceService == appName {
		return true
	}
	if !strings.HasPrefix(resourceService, appName+"_") {
		return false
	}
	return appconfig.ValidateServiceName(resourceService[len(appName)+1:]) == nil
}

func resourceServiceName(resource *resourcepb.Resource) string {
	for _, attr := range resource.GetAttributes() {
		if attr.GetKey() == "service.name" {
			return attr.GetValue().GetStringValue()
		}
	}
	return ""
}

// filterLogs filters log records based on the stream request filters.
// Returns nil if all records are filtered out.
func filterLogs(req *collogspb.ExportLogsServiceRequest, filter *agentpb.StreamLogsRequest) *collogspb.ExportLogsServiceRequest {
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

	var filteredResourceLogs []*logspb.ResourceLogs
	for _, rl := range req.GetResourceLogs() {
		// Check resource attributes for service.name.
		if !matchResourceAttributes(rl.GetResource(), serviceName, appName) {
			continue
		}

		// Filter by severity if specified.
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
					filtered := &logspb.ScopeLogs{
						Scope:      sl.GetScope(),
						LogRecords: filteredRecords,
						SchemaUrl:  sl.GetSchemaUrl(),
					}
					filteredScopeLogs = append(filteredScopeLogs, filtered)
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

// filterMetrics filters metrics based on the stream request filters.
// Returns nil if all metrics are filtered out.
func filterMetrics(req *colmetricspb.ExportMetricsServiceRequest, filter *agentpb.StreamMetricsRequest) *colmetricspb.ExportMetricsServiceRequest {
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

// filterTraces filters traces based on the stream request filters.
// Returns nil if all spans are filtered out.
func filterTraces(req *coltracepb.ExportTraceServiceRequest, filter *agentpb.StreamTracesRequest) *coltracepb.ExportTraceServiceRequest {
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

// Ensure compile-time interface compliance.
var (
	_ agentpb.WendyTelemetryServiceServer = (*TelemetryService)(nil)
	_ collogspb.LogsServiceServer         = (*OTELLogsReceiver)(nil)
	_ colmetricspb.MetricsServiceServer   = (*OTELMetricsReceiver)(nil)
	_ coltracepb.TraceServiceServer       = (*OTELTraceReceiver)(nil)
)
