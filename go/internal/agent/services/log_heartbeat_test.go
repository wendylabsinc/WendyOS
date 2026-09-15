package services

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	v2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	"go.uber.org/zap"
	"google.golang.org/grpc"
)

type heartbeatTestStream[T any] struct {
	grpc.ServerStream
	ctx       context.Context
	sent      chan *T
	sendError error
}

func (s *heartbeatTestStream[T]) Context() context.Context { return s.ctx }
func (s *heartbeatTestStream[T]) Send(value *T) error {
	if s.sendError != nil {
		return s.sendError
	}
	s.sent <- value
	return nil
}

func testLogHeartbeats[T any](t *testing.T, serve func(*TelemetryBroadcaster, *heartbeatTestStream[T]) error, isHeartbeat func(*T) bool) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		b := NewTelemetryBroadcaster()
		s := &heartbeatTestStream[T]{ctx: ctx, sent: make(chan *T, 8)}
		done := make(chan error, 1)
		go func() { done <- serve(b, s) }()
		synctest.Wait()
		time.Sleep(15 * time.Second)
		synctest.Wait()
		if len(s.sent) != 1 || !isHeartbeat(<-s.sent) {
			t.Fatal("quiet stream did not send an empty heartbeat at 15 seconds")
		}
		b.PublishLogs(&collogspb.ExportLogsServiceRequest{})
		synctest.Wait()
		if len(s.sent) != 1 || isHeartbeat(<-s.sent) {
			t.Fatal("data after silence was lost")
		}
		time.Sleep(14 * time.Second)
		synctest.Wait()
		if len(s.sent) != 0 {
			t.Fatal("heartbeat sent before the quiet deadline")
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if len(s.sent) != 1 || !isHeartbeat(<-s.sent) {
			t.Fatal("second quiet heartbeat missing")
		}
		cancel()
		synctest.Wait()
		if !errors.Is(<-done, context.Canceled) || len(b.logSubs) != 0 {
			t.Fatal("cancellation leaked the stream subscription")
		}
	})
	synctest.Test(t, func(t *testing.T) {
		b := NewTelemetryBroadcaster()
		failure := errors.New("connection lost")
		s := &heartbeatTestStream[T]{ctx: context.Background(), sendError: failure}
		done := make(chan error, 1)
		go func() { done <- serve(b, s) }()
		time.Sleep(15 * time.Second)
		synctest.Wait()
		if !errors.Is(<-done, failure) || len(b.logSubs) != 0 {
			t.Fatal("heartbeat send failure was hidden or leaked subscription")
		}
	})
}

func TestLogHeartbeatsV1(t *testing.T) {
	testLogHeartbeats(t, func(b *TelemetryBroadcaster, s *heartbeatTestStream[agentpb.StreamLogsResponse]) error {
		return NewTelemetryService(zap.NewNop(), b, nil).StreamLogs(&agentpb.StreamLogsRequest{}, s)
	}, func(r *agentpb.StreamLogsResponse) bool { return r.Logs == nil && !r.IsHistory })
}
func TestLogHeartbeatsV2(t *testing.T) {
	testLogHeartbeats(t, func(b *TelemetryBroadcaster, s *heartbeatTestStream[v2.StreamLogsResponse]) error {
		return NewTelemetryServiceV2(zap.NewNop(), b, nil).StreamLogs(&v2.StreamLogsRequest{}, s)
	}, func(r *v2.StreamLogsResponse) bool { return r.Logs == nil && !r.IsHistory })
}
