package services

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
	otelpb "github.com/wendylabsinc/wendy/proto/gen/otelpb"
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

	req := &otelpb.ExportLogsServiceRequest{}
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

	req := &otelpb.ExportMetricsServiceRequest{}
	b.PublishMetrics(req)

	for i, ch := range []<-chan *otelpb.ExportMetricsServiceRequest{ch1, ch2} {
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

	req := &otelpb.ExportTraceServiceRequest{}
	b.PublishTraces(req)

	for i, ch := range []<-chan *otelpb.ExportTraceServiceRequest{ch1, ch2} {
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
		b.PublishLogs(&otelpb.ExportLogsServiceRequest{})
	}

	// The 65th publish should be dropped (not block).
	done := make(chan struct{})
	go func() {
		b.PublishLogs(&otelpb.ExportLogsServiceRequest{})
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
	svc := NewTelemetryService(logger, broadcaster)

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
	broadcaster.PublishLogs(&otelpb.ExportLogsServiceRequest{})

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

	req := &otelpb.ExportLogsServiceRequest{}
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

	req := &otelpb.ExportMetricsServiceRequest{}
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

	req := &otelpb.ExportTraceServiceRequest{}
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
			b.PublishLogs(&otelpb.ExportLogsServiceRequest{})
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
