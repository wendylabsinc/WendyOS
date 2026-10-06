package services

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

func TestBroadcaster_SubscribeUnsubscribe(t *testing.T) {
	b := NewTelemetryBroadcaster()

	id, ch := b.SubscribeLogs()
	if id == "" {
		t.Fatal("expected non-empty subscription ID")
	}
	if ch == nil {
		t.Fatal("expected non-nil channel")
	}

	b.UnsubscribeLogs(id)

	// Channel should be closed after unsubscribe.
	_, ok := <-ch
	if ok {
		t.Error("expected channel to be closed after unsubscribe")
	}
}

func TestBroadcaster_PublishLogs(t *testing.T) {
	b := NewTelemetryBroadcaster()

	id1, ch1 := b.SubscribeLogs()
	id2, ch2 := b.SubscribeLogs()
	defer b.UnsubscribeLogs(id1)
	defer b.UnsubscribeLogs(id2)

	req := &collogspb.ExportLogsServiceRequest{}
	b.PublishLogs(req)

	select {
	case got := <-ch1:
		if got != req {
			t.Error("subscriber 1 received wrong message")
		}
	case <-time.After(time.Second):
		t.Error("subscriber 1 did not receive message")
	}

	select {
	case got := <-ch2:
		if got != req {
			t.Error("subscriber 2 received wrong message")
		}
	case <-time.After(time.Second):
		t.Error("subscriber 2 did not receive message")
	}
}

func TestBroadcaster_PublishMetrics(t *testing.T) {
	b := NewTelemetryBroadcaster()

	id1, ch1 := b.SubscribeMetrics()
	id2, ch2 := b.SubscribeMetrics()
	defer b.UnsubscribeMetrics(id1)
	defer b.UnsubscribeMetrics(id2)

	req := &colmetricspb.ExportMetricsServiceRequest{}
	b.PublishMetrics(req)

	for i, ch := range []<-chan *colmetricspb.ExportMetricsServiceRequest{ch1, ch2} {
		select {
		case got := <-ch:
			if got != req {
				t.Errorf("subscriber %d received wrong message", i+1)
			}
		case <-time.After(time.Second):
			t.Errorf("subscriber %d did not receive message", i+1)
		}
	}
}

func TestBroadcaster_PublishTraces(t *testing.T) {
	b := NewTelemetryBroadcaster()

	id1, ch1 := b.SubscribeTraces()
	id2, ch2 := b.SubscribeTraces()
	defer b.UnsubscribeTraces(id1)
	defer b.UnsubscribeTraces(id2)

	req := &coltracepb.ExportTraceServiceRequest{}
	b.PublishTraces(req)

	for i, ch := range []<-chan *coltracepb.ExportTraceServiceRequest{ch1, ch2} {
		select {
		case got := <-ch:
			if got != req {
				t.Errorf("subscriber %d received wrong message", i+1)
			}
		case <-time.After(time.Second):
			t.Errorf("subscriber %d did not receive message", i+1)
		}
	}
}

func TestBroadcaster_SlowSubscriber(t *testing.T) {
	b := NewTelemetryBroadcaster()

	// Subscribe with default buffer of 64.
	id, ch := b.SubscribeLogs()
	defer b.UnsubscribeLogs(id)

	// Fill the channel buffer.
	for i := 0; i < 64; i++ {
		b.PublishLogs(&collogspb.ExportLogsServiceRequest{})
	}

	// The 65th publish should be dropped (not block).
	done := make(chan struct{})
	go func() {
		b.PublishLogs(&collogspb.ExportLogsServiceRequest{})
		close(done)
	}()

	select {
	case <-done:
		// Good: publish did not block.
	case <-time.After(time.Second):
		t.Error("PublishLogs blocked on slow subscriber")
	}

	// Drain to verify we have 64 messages.
	count := 0
	for {
		select {
		case <-ch:
			count++
		default:
			goto drained
		}
	}
drained:
	if count != 64 {
		t.Errorf("drained %d messages; want 64", count)
	}
}

func TestStreamLogs_Integration(t *testing.T) {
	broadcaster := NewTelemetryBroadcaster()
	logger := zap.NewNop()
	svc := NewTelemetryService(logger, broadcaster, nil)

	lis := bufconn.Listen(bufSize)
	srv := grpc.NewServer()
	agentpb.RegisterWendyTelemetryServiceServer(srv, svc)
	go func() { _ = srv.Serve(lis) }()

	dialer := func(context.Context, string) (net.Conn, error) {
		return lis.Dial()
	}
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	defer func() {
		conn.Close()
		srv.Stop()
		lis.Close()
	}()

	client := agentpb.NewWendyTelemetryServiceClient(conn)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := client.StreamLogs(ctx, &agentpb.StreamLogsRequest{})
	if err != nil {
		t.Fatalf("StreamLogs: %v", err)
	}

	// Give the server a moment to register the subscriber.
	time.Sleep(50 * time.Millisecond)

	// Publish a log.
	broadcaster.PublishLogs(&collogspb.ExportLogsServiceRequest{})

	// Receive on client.
	resp, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	if resp.Logs == nil {
		t.Error("expected non-nil logs in response")
	}

	// Cancel to end the stream.
	cancel()
	_, err = stream.Recv()
	if err == nil || err == io.EOF {
		// Either is acceptable upon cancellation.
	}
}

func TestOTELLogsReceiver(t *testing.T) {
	broadcaster := NewTelemetryBroadcaster()
	receiver := NewOTELLogsReceiver(broadcaster)

	id, ch := broadcaster.SubscribeLogs()
	defer broadcaster.UnsubscribeLogs(id)

	req := &collogspb.ExportLogsServiceRequest{}
	resp, err := receiver.Export(context.Background(), req)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}

	select {
	case got := <-ch:
		if got != req {
			t.Error("received wrong message from broadcaster")
		}
	case <-time.After(time.Second):
		t.Error("did not receive published log")
	}
}

func TestOTELMetricsReceiver(t *testing.T) {
	broadcaster := NewTelemetryBroadcaster()
	receiver := NewOTELMetricsReceiver(broadcaster)

	id, ch := broadcaster.SubscribeMetrics()
	defer broadcaster.UnsubscribeMetrics(id)

	req := &colmetricspb.ExportMetricsServiceRequest{}
	_, err := receiver.Export(context.Background(), req)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}

	select {
	case got := <-ch:
		if got != req {
			t.Error("received wrong message")
		}
	case <-time.After(time.Second):
		t.Error("did not receive published metrics")
	}
}

func TestOTELTraceReceiver(t *testing.T) {
	broadcaster := NewTelemetryBroadcaster()
	receiver := NewOTELTraceReceiver(broadcaster)

	id, ch := broadcaster.SubscribeTraces()
	defer broadcaster.UnsubscribeTraces(id)

	req := &coltracepb.ExportTraceServiceRequest{}
	_, err := receiver.Export(context.Background(), req)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}

	select {
	case got := <-ch:
		if got != req {
			t.Error("received wrong message")
		}
	case <-time.After(time.Second):
		t.Error("did not receive published trace")
	}
}

func TestBroadcaster_RingBufferWrapAround(t *testing.T) {
	b := NewTelemetryBroadcaster()

	// Publish more than defaultMaxCachedLogs entries so the ring buffer wraps.
	total := defaultMaxCachedLogs + 5 // 25 entries
	reqs := make([]*collogspb.ExportLogsServiceRequest, total)
	for i := 0; i < total; i++ {
		reqs[i] = &collogspb.ExportLogsServiceRequest{}
		b.PublishLogs(reqs[i])
	}

	// The ring buffer should hold exactly defaultMaxCachedLogs entries.
	if b.logCount != defaultMaxCachedLogs {
		t.Errorf("logCount = %d; want %d", b.logCount, defaultMaxCachedLogs)
	}

	// Subscribe now; the pre-fill goroutine should deliver the last
	// defaultMaxCachedLogs entries in chronological order.
	_, ch := b.SubscribeLogs()

	// Collect pre-filled entries (allow a brief window for the goroutine).
	var got []*collogspb.ExportLogsServiceRequest
	timeout := time.After(time.Second)
	for len(got) < defaultMaxCachedLogs {
		select {
		case entry := <-ch:
			got = append(got, entry)
		case <-timeout:
			t.Fatalf("timed out waiting for pre-filled logs; got %d, want %d", len(got), defaultMaxCachedLogs)
		}
	}

	// Verify order: should be reqs[5..24] in order.
	expected := reqs[total-defaultMaxCachedLogs:]
	for i, want := range expected {
		if got[i] != want {
			t.Errorf("pre-filled entry %d: got %p, want %p", i, got[i], want)
		}
	}
}

func TestBroadcaster_SubscribeLogs_ChronologicalOrder(t *testing.T) {
	b := NewTelemetryBroadcaster()

	// Publish exactly defaultMaxCachedLogs entries (no wrap yet).
	reqs := make([]*collogspb.ExportLogsServiceRequest, defaultMaxCachedLogs)
	for i := 0; i < defaultMaxCachedLogs; i++ {
		reqs[i] = &collogspb.ExportLogsServiceRequest{}
		b.PublishLogs(reqs[i])
	}

	_, ch := b.SubscribeLogs()

	var got []*collogspb.ExportLogsServiceRequest
	timeout := time.After(time.Second)
	for len(got) < defaultMaxCachedLogs {
		select {
		case entry := <-ch:
			got = append(got, entry)
		case <-timeout:
			t.Fatalf("timed out; got %d entries, want %d", len(got), defaultMaxCachedLogs)
		}
	}

	for i, want := range reqs {
		if got[i] != want {
			t.Errorf("entry %d: got %p, want %p", i, got[i], want)
		}
	}
}

func TestBroadcaster_PublishMetrics_PerServiceMergeRetainsMetrics(t *testing.T) {
	b := NewTelemetryBroadcaster()

	makeAttr := func(key, val string) *commonpb.KeyValue {
		return &commonpb.KeyValue{
			Key:   key,
			Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: val}},
		}
	}
	makeResource := func(svc string) *resourcepb.Resource {
		return &resourcepb.Resource{Attributes: []*commonpb.KeyValue{makeAttr("service.name", svc)}}
	}
	makeReq := func(svc string, metrics ...string) *colmetricspb.ExportMetricsServiceRequest {
		ms := make([]*metricspb.Metric, len(metrics))
		for i, n := range metrics {
			ms[i] = &metricspb.Metric{Name: n}
		}
		return &colmetricspb.ExportMetricsServiceRequest{
			ResourceMetrics: []*metricspb.ResourceMetrics{
				{
					Resource:     makeResource(svc),
					ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: ms}},
				},
			},
		}
	}

	// svc-a first reports {one, two}, then a partial batch with only {one}.
	b.PublishMetrics(makeReq("svc-a", "metric.one", "metric.two"))
	b.PublishMetrics(makeReq("svc-a", "metric.one"))
	// svc-b is independent.
	b.PublishMetrics(makeReq("svc-b", "metric.three"))

	b.mu.RLock()
	mapLen := len(b.latestMetrics)
	gotA := b.latestMetrics["svc-a"]
	gotB := b.latestMetrics["svc-b"]
	b.mu.RUnlock()

	if mapLen != 2 {
		t.Errorf("latestMetrics has %d entries; want 2", mapLen)
	}

	names := func(req *colmetricspb.ExportMetricsServiceRequest) map[string]bool {
		out := map[string]bool{}
		for _, rm := range req.GetResourceMetrics() {
			for _, sm := range rm.GetScopeMetrics() {
				for _, m := range sm.GetMetrics() {
					out[m.GetName()] = true
				}
			}
		}
		return out
	}

	gotNames := names(gotA)
	// The partial batch must NOT drop metric.two reported earlier.
	if !gotNames["metric.one"] || !gotNames["metric.two"] {
		t.Errorf("svc-a cached metrics = %v; want metric.one and metric.two retained", gotNames)
	}
	if !names(gotB)["metric.three"] {
		t.Errorf("svc-b cached metrics = %v; want metric.three", names(gotB))
	}
}

func TestBroadcaster_PublishMetrics_ResourceCap(t *testing.T) {
	b := NewTelemetryBroadcaster()

	makeAttr := func(key, val string) *commonpb.KeyValue {
		return &commonpb.KeyValue{
			Key:   key,
			Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: val}},
		}
	}

	// Publish maxCachedResourcesPerService+10 distinct resource instances for the same service.
	for i := 0; i < maxCachedResourcesPerService+10; i++ {
		req := &colmetricspb.ExportMetricsServiceRequest{
			ResourceMetrics: []*metricspb.ResourceMetrics{
				{
					Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
						makeAttr("service.name", "svc"),
						makeAttr("instance.id", fmt.Sprintf("pod-%d", i)),
					}},
					ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: []*metricspb.Metric{{Name: "m"}}}},
				},
			},
		}
		b.PublishMetrics(req)
	}

	b.mu.RLock()
	cached := b.latestMetrics["svc"]
	b.mu.RUnlock()

	if cached == nil {
		t.Fatal("expected cache entry for svc")
	}
	if n := len(cached.GetResourceMetrics()); n > maxCachedResourcesPerService {
		t.Errorf("cache has %d ResourceMetrics entries; want at most %d", n, maxCachedResourcesPerService)
	}
}

func TestBroadcaster_ConcurrentPublish(t *testing.T) {
	b := NewTelemetryBroadcaster()

	id, ch := b.SubscribeLogs()
	defer b.UnsubscribeLogs(id)

	const n = 100
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			b.PublishLogs(&collogspb.ExportLogsServiceRequest{})
		}()
	}

	// Drain in parallel.
	received := 0
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range n {
			select {
			case <-ch:
				received++
			case <-time.After(2 * time.Second):
				return
			}
		}
	}()

	wg.Wait()
	<-done

	// We might have received fewer than n if the buffer filled up.
	if received == 0 {
		t.Error("expected at least some messages to be received")
	}
}

func TestStreamLogs_LastN(t *testing.T) {
	dir := t.TempDir()
	broadcaster := NewTelemetryBroadcaster()
	buf := NewTelemetryBuffer(TelemetryBufferConfig{
		Dir:           dir,
		MaxTotalBytes: 10 * 1024 * 1024,
		SegmentBytes:  1 * 1024 * 1024,
	}, broadcaster, zap.NewNop())

	// Write 5 log entries to disk before any client connects.
	for i := 0; i < 5; i++ {
		buf.PublishLogs(makeLogReq(fmt.Sprintf("msg-%d", i)))
	}

	svc := NewTelemetryService(zap.NewNop(), broadcaster, buf)

	lis := bufconn.Listen(1024 * 1024)
	srv := grpc.NewServer()
	agentpb.RegisterWendyTelemetryServiceServer(srv, svc)
	go srv.Serve(lis) //nolint:errcheck
	defer srv.Stop()

	conn, err := grpc.NewClient("passthrough://bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	defer conn.Close()

	client := agentpb.NewWendyTelemetryServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	lastN := int32(3)
	stream, err := client.StreamLogs(ctx, &agentpb.StreamLogsRequest{LastN: &lastN})
	if err != nil {
		t.Fatalf("StreamLogs: %v", err)
	}

	var historyCount int
	for i := 0; i < 3; i++ {
		resp, err := stream.Recv()
		if err != nil {
			t.Fatalf("Recv[%d]: %v", i, err)
		}
		if resp.IsHistory {
			historyCount++
		}
	}
	if historyCount != 3 {
		t.Errorf("want 3 history records, got %d", historyCount)
	}
}

func makeMetricReq(name string) *colmetricspb.ExportMetricsServiceRequest {
	return &colmetricspb.ExportMetricsServiceRequest{
		ResourceMetrics: []*metricspb.ResourceMetrics{{
			ScopeMetrics: []*metricspb.ScopeMetrics{{
				Metrics: []*metricspb.Metric{{Name: name}},
			}},
		}},
	}
}

func makeTraceReq(name string) *coltracepb.ExportTraceServiceRequest {
	return &coltracepb.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{{
			ScopeSpans: []*tracepb.ScopeSpans{{
				Spans: []*tracepb.Span{{Name: name}},
			}},
		}},
	}
}

func TestStreamMetrics_LastN(t *testing.T) {
	dir := t.TempDir()
	broadcaster := NewTelemetryBroadcaster()
	buf := NewTelemetryBuffer(TelemetryBufferConfig{
		Dir:           dir,
		MaxTotalBytes: 10 * 1024 * 1024,
		SegmentBytes:  1 * 1024 * 1024,
	}, broadcaster, zap.NewNop())

	for i := 0; i < 5; i++ {
		buf.PublishMetrics(makeMetricReq(fmt.Sprintf("metric-%d", i)))
	}

	svc := NewTelemetryService(zap.NewNop(), broadcaster, buf)

	lis := bufconn.Listen(1024 * 1024)
	srv := grpc.NewServer()
	agentpb.RegisterWendyTelemetryServiceServer(srv, svc)
	go srv.Serve(lis) //nolint:errcheck
	defer srv.Stop()

	conn, err := grpc.NewClient("passthrough://bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	defer conn.Close()

	client := agentpb.NewWendyTelemetryServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	lastN := int32(3)
	stream, err := client.StreamMetrics(ctx, &agentpb.StreamMetricsRequest{LastN: &lastN})
	if err != nil {
		t.Fatalf("StreamMetrics: %v", err)
	}

	var historyCount int
	for i := 0; i < 3; i++ {
		resp, err := stream.Recv()
		if err != nil {
			t.Fatalf("Recv[%d]: %v", i, err)
		}
		if resp.IsHistory {
			historyCount++
		}
	}
	if historyCount != 3 {
		t.Errorf("want 3 history records, got %d", historyCount)
	}
}

func TestStreamTraces_LastN(t *testing.T) {
	dir := t.TempDir()
	broadcaster := NewTelemetryBroadcaster()
	buf := NewTelemetryBuffer(TelemetryBufferConfig{
		Dir:           dir,
		MaxTotalBytes: 10 * 1024 * 1024,
		SegmentBytes:  1 * 1024 * 1024,
	}, broadcaster, zap.NewNop())

	for i := 0; i < 5; i++ {
		buf.PublishTraces(makeTraceReq(fmt.Sprintf("span-%d", i)))
	}

	svc := NewTelemetryService(zap.NewNop(), broadcaster, buf)

	lis := bufconn.Listen(1024 * 1024)
	srv := grpc.NewServer()
	agentpb.RegisterWendyTelemetryServiceServer(srv, svc)
	go srv.Serve(lis) //nolint:errcheck
	defer srv.Stop()

	conn, err := grpc.NewClient("passthrough://bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	defer conn.Close()

	client := agentpb.NewWendyTelemetryServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	lastN := int32(3)
	stream, err := client.StreamTraces(ctx, &agentpb.StreamTracesRequest{LastN: &lastN})
	if err != nil {
		t.Fatalf("StreamTraces: %v", err)
	}

	var historyCount int
	for i := 0; i < 3; i++ {
		resp, err := stream.Recv()
		if err != nil {
			t.Fatalf("Recv[%d]: %v", i, err)
		}
		if resp.IsHistory {
			historyCount++
		}
	}
	if historyCount != 3 {
		t.Errorf("want 3 history records, got %d", historyCount)
	}
}

// makeLogReqForService builds a single-record log batch attributed to the
// given service.name, which is what --app/--service filtering matches on.
func makeLogReqForService(svc, body string) *collogspb.ExportLogsServiceRequest {
	return &collogspb.ExportLogsServiceRequest{
		ResourceLogs: []*logspb.ResourceLogs{{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{{
				Key:   "service.name",
				Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: svc}},
			}}},
			ScopeLogs: []*logspb.ScopeLogs{{
				LogRecords: []*logspb.LogRecord{{
					Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: body}},
				}},
			}},
		}},
	}
}

func firstLogBody(req *collogspb.ExportLogsServiceRequest) string {
	return req.GetResourceLogs()[0].GetScopeLogs()[0].GetLogRecords()[0].GetBody().GetStringValue()
}

// newTelemetryTestConn serves the given registration over bufconn and returns
// a client connection to it.
func newTelemetryTestConn(t *testing.T, register func(*grpc.Server)) *grpc.ClientConn {
	t.Helper()
	lis := bufconn.Listen(1024 * 1024)
	srv := grpc.NewServer()
	register(srv)
	go srv.Serve(lis) //nolint:errcheck
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough://bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// newChattyQuietBuffer seeds a buffer where a quiet app's 3 batches are
// followed by 50 batches from a chatty app, so the last N batches
// device-wide never include the quiet app.
func newChattyQuietBuffer(t *testing.T) (*TelemetryBroadcaster, *TelemetryBuffer) {
	t.Helper()
	broadcaster := NewTelemetryBroadcaster()
	buf := NewTelemetryBuffer(TelemetryBufferConfig{
		Dir:           t.TempDir(),
		MaxTotalBytes: 10 * 1024 * 1024,
		SegmentBytes:  1 * 1024 * 1024,
	}, broadcaster, zap.NewNop())

	for i := 0; i < 3; i++ {
		buf.PublishLogs(makeLogReqForService("quiet", fmt.Sprintf("quiet-%d", i)))
	}
	for i := 0; i < 50; i++ {
		buf.PublishLogs(makeLogReqForService("chatty", fmt.Sprintf("chatty-%d", i)))
	}
	return broadcaster, buf
}

func TestStreamLogs_LastN_FilterBeforeWindow(t *testing.T) {
	broadcaster, buf := newChattyQuietBuffer(t)
	svc := NewTelemetryService(zap.NewNop(), broadcaster, buf)
	conn := newTelemetryTestConn(t, func(srv *grpc.Server) {
		agentpb.RegisterWendyTelemetryServiceServer(srv, svc)
	})
	client := agentpb.NewWendyTelemetryServiceClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	appName := "quiet"
	lastN := int32(2)
	stream, err := client.StreamLogs(ctx, &agentpb.StreamLogsRequest{LastN: &lastN, AppName: &appName})
	if err != nil {
		t.Fatalf("StreamLogs: %v", err)
	}

	for _, want := range []string{"quiet-1", "quiet-2"} {
		resp, err := stream.Recv()
		if err != nil {
			t.Fatalf("Recv(%s): %v", want, err)
		}
		if !resp.IsHistory {
			t.Errorf("want history record %s, got live", want)
		}
		if got := firstLogBody(resp.GetLogs()); got != want {
			t.Errorf("want body %s, got %s", want, got)
		}
	}

	// History is fully replayed before live delivery starts, so a live batch
	// arriving next proves no extra history records were sent.
	broadcaster.PublishLogs(makeLogReqForService("quiet", "live-sentinel"))
	resp, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv(sentinel): %v", err)
	}
	if resp.IsHistory || firstLogBody(resp.GetLogs()) != "live-sentinel" {
		t.Errorf("want live sentinel after 2 history records, got history=%v body=%s",
			resp.IsHistory, firstLogBody(resp.GetLogs()))
	}
}

func TestStreamLogs_NoFollowReturnsFilteredHistoryAndEOF(t *testing.T) {
	broadcaster, buf := newChattyQuietBuffer(t)
	svc := NewTelemetryService(zap.NewNop(), broadcaster, buf)
	conn := newTelemetryTestConn(t, func(srv *grpc.Server) {
		agentpb.RegisterWendyTelemetryServiceServer(srv, svc)
	})
	client := agentpb.NewWendyTelemetryServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	appName := "quiet"
	lastN := int32(2)
	stream, err := client.StreamLogs(ctx, &agentpb.StreamLogsRequest{LastN: &lastN, AppName: &appName, NoFollow: true})
	if err != nil {
		t.Fatalf("StreamLogs: %v", err)
	}
	for _, want := range []string{"quiet-1", "quiet-2"} {
		resp, err := stream.Recv()
		if err != nil {
			t.Fatalf("Recv(%s): %v", want, err)
		}
		if !resp.IsHistory || firstLogBody(resp.GetLogs()) != want {
			t.Errorf("got history=%v body=%q, want %q", resp.IsHistory, firstLogBody(resp.GetLogs()), want)
		}
	}
	if _, err := stream.Recv(); err != io.EOF {
		t.Errorf("after history Recv() = %v, want clean EOF", err)
	}
}

func TestStreamLogs_NoFollowLimitsInMemoryHistoryAfterFiltering(t *testing.T) {
	broadcaster := NewTelemetryBroadcaster()
	broadcaster.PublishLogs(makeLogReqForService("quiet", "quiet-0"))
	broadcaster.PublishLogs(makeLogReqForService("chatty", "chatty-0"))
	broadcaster.PublishLogs(makeLogReqForService("quiet", "quiet-1"))
	broadcaster.PublishLogs(makeLogReqForService("quiet", "quiet-2"))
	svc := NewTelemetryService(zap.NewNop(), broadcaster, nil)
	conn := newTelemetryTestConn(t, func(srv *grpc.Server) {
		agentpb.RegisterWendyTelemetryServiceServer(srv, svc)
	})
	client := agentpb.NewWendyTelemetryServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	appName := "quiet"
	lastN := int32(2)
	stream, err := client.StreamLogs(ctx, &agentpb.StreamLogsRequest{LastN: &lastN, AppName: &appName, NoFollow: true})
	if err != nil {
		t.Fatalf("StreamLogs: %v", err)
	}
	for _, want := range []string{"quiet-1", "quiet-2"} {
		resp, err := stream.Recv()
		if err != nil {
			t.Fatalf("Recv(%s): %v", want, err)
		}
		if !resp.IsHistory || firstLogBody(resp.GetLogs()) != want {
			t.Errorf("got history=%v body=%q, want %q", resp.IsHistory, firstLogBody(resp.GetLogs()), want)
		}
	}
	if _, err := stream.Recv(); err != io.EOF {
		t.Errorf("after memory history Recv() = %v, want clean EOF", err)
	}
}

func TestStreamLogsV2_NoFollowReturnsEOFWithNoHistory(t *testing.T) {
	svc := NewTelemetryServiceV2(zap.NewNop(), NewTelemetryBroadcaster(), nil)
	conn := newTelemetryTestConn(t, func(srv *grpc.Server) {
		agentpbv2.RegisterWendyTelemetryServiceServer(srv, svc)
	})
	client := agentpbv2.NewWendyTelemetryServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	stream, err := client.StreamLogs(ctx, &agentpbv2.StreamLogsRequest{NoFollow: true})
	if err != nil {
		t.Fatalf("StreamLogs: %v", err)
	}
	if _, err := stream.Recv(); err != io.EOF {
		t.Errorf("empty history Recv() = %v, want clean EOF", err)
	}
}

func TestStreamLogs_LastN_FilterLargerThanHistory(t *testing.T) {
	broadcaster, buf := newChattyQuietBuffer(t)
	svc := NewTelemetryService(zap.NewNop(), broadcaster, buf)
	conn := newTelemetryTestConn(t, func(srv *grpc.Server) {
		agentpb.RegisterWendyTelemetryServiceServer(srv, svc)
	})
	client := agentpb.NewWendyTelemetryServiceClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	appName := "quiet"
	lastN := int32(10)
	stream, err := client.StreamLogs(ctx, &agentpb.StreamLogsRequest{LastN: &lastN, AppName: &appName})
	if err != nil {
		t.Fatalf("StreamLogs: %v", err)
	}

	for _, want := range []string{"quiet-0", "quiet-1", "quiet-2"} {
		resp, err := stream.Recv()
		if err != nil {
			t.Fatalf("Recv(%s): %v", want, err)
		}
		if !resp.IsHistory {
			t.Errorf("want history record %s, got live", want)
		}
		if got := firstLogBody(resp.GetLogs()); got != want {
			t.Errorf("want body %s, got %s", want, got)
		}
	}

	broadcaster.PublishLogs(makeLogReqForService("quiet", "live-sentinel"))
	resp, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv(sentinel): %v", err)
	}
	if resp.IsHistory || firstLogBody(resp.GetLogs()) != "live-sentinel" {
		t.Errorf("want live sentinel after 3 history records, got history=%v body=%s",
			resp.IsHistory, firstLogBody(resp.GetLogs()))
	}
}

func TestStreamLogs_LastN_FilterWithNoHistory(t *testing.T) {
	broadcaster, buf := newChattyQuietBuffer(t)
	svc := NewTelemetryService(zap.NewNop(), broadcaster, buf)
	conn := newTelemetryTestConn(t, func(srv *grpc.Server) {
		agentpb.RegisterWendyTelemetryServiceServer(srv, svc)
	})
	client := agentpb.NewWendyTelemetryServiceClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	appName := "ghost"
	lastN := int32(5)
	stream, err := client.StreamLogs(ctx, &agentpb.StreamLogsRequest{LastN: &lastN, AppName: &appName})
	if err != nil {
		t.Fatalf("StreamLogs: %v", err)
	}

	// Publish live batches until the stream delivers one; the subscription
	// may not be registered yet when the first publish happens.
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case <-done:
				return
			case <-time.After(10 * time.Millisecond):
				broadcaster.PublishLogs(makeLogReqForService("ghost", "live-sentinel"))
			}
		}
	}()

	resp, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if resp.IsHistory {
		t.Errorf("app with no logged history must replay nothing, got history record %s",
			firstLogBody(resp.GetLogs()))
	}
}

func TestStreamLogsV2_LastN_FilterBeforeWindow(t *testing.T) {
	broadcaster, buf := newChattyQuietBuffer(t)
	svc := NewTelemetryServiceV2(zap.NewNop(), broadcaster, buf)
	conn := newTelemetryTestConn(t, func(srv *grpc.Server) {
		agentpbv2.RegisterWendyTelemetryServiceServer(srv, svc)
	})
	client := agentpbv2.NewWendyTelemetryServiceClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	appName := "quiet"
	lastN := int32(2)
	stream, err := client.StreamLogs(ctx, &agentpbv2.StreamLogsRequest{LastN: &lastN, AppName: &appName})
	if err != nil {
		t.Fatalf("StreamLogs: %v", err)
	}

	for _, want := range []string{"quiet-1", "quiet-2"} {
		resp, err := stream.Recv()
		if err != nil {
			t.Fatalf("Recv(%s): %v", want, err)
		}
		if !resp.IsHistory {
			t.Errorf("want history record %s, got live", want)
		}
		if got := firstLogBody(resp.GetLogs()); got != want {
			t.Errorf("want body %s, got %s", want, got)
		}
	}
}

func TestStreamLogs_RecentPrefillIsMarkedAsHistory(t *testing.T) {
	broadcaster := NewTelemetryBroadcaster()
	broadcaster.PublishLogs(makeLogReqForService("watch-app", "stale-before-watch"))
	svc := NewTelemetryService(zap.NewNop(), broadcaster, nil)
	conn := newTelemetryTestConn(t, func(srv *grpc.Server) {
		agentpb.RegisterWendyTelemetryServiceServer(srv, svc)
	})
	client := agentpb.NewWendyTelemetryServiceClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	appName := "watch-app"
	stream, err := client.StreamLogs(ctx, &agentpb.StreamLogsRequest{AppName: &appName})
	if err != nil {
		t.Fatalf("StreamLogs: %v", err)
	}

	resp, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if got := firstLogBody(resp.GetLogs()); got != "stale-before-watch" {
		t.Fatalf("first body = %q, want cached pre-subscription record", got)
	}
	if !resp.GetIsHistory() {
		t.Error("cached pre-subscription record was not marked as history")
	}

	broadcaster.PublishLogs(makeLogReqForService("watch-app", "live-after-watch"))
	resp, err = stream.Recv()
	if err != nil {
		t.Fatalf("Recv live: %v", err)
	}
	if got := firstLogBody(resp.GetLogs()); got != "live-after-watch" || resp.GetIsHistory() {
		t.Errorf("live response = history:%v body:%q, want history:false body:%q",
			resp.GetIsHistory(), got, "live-after-watch")
	}
}

func TestStreamLogsV2_RecentPrefillIsMarkedAsHistory(t *testing.T) {
	broadcaster := NewTelemetryBroadcaster()
	broadcaster.PublishLogs(makeLogReqForService("watch-app", "stale-before-watch"))
	svc := NewTelemetryServiceV2(zap.NewNop(), broadcaster, nil)
	conn := newTelemetryTestConn(t, func(srv *grpc.Server) {
		agentpbv2.RegisterWendyTelemetryServiceServer(srv, svc)
	})
	client := agentpbv2.NewWendyTelemetryServiceClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	appName := "watch-app"
	stream, err := client.StreamLogs(ctx, &agentpbv2.StreamLogsRequest{AppName: &appName})
	if err != nil {
		t.Fatalf("StreamLogs: %v", err)
	}

	resp, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if got := firstLogBody(resp.GetLogs()); got != "stale-before-watch" {
		t.Fatalf("first body = %q, want cached pre-subscription record", got)
	}
	if !resp.GetIsHistory() {
		t.Error("cached pre-subscription record was not marked as history")
	}

	broadcaster.PublishLogs(makeLogReqForService("watch-app", "live-after-watch"))
	resp, err = stream.Recv()
	if err != nil {
		t.Fatalf("Recv live: %v", err)
	}
	if got := firstLogBody(resp.GetLogs()); got != "live-after-watch" || resp.GetIsHistory() {
		t.Errorf("live response = history:%v body:%q, want history:false body:%q",
			resp.GetIsHistory(), got, "live-after-watch")
	}
}
