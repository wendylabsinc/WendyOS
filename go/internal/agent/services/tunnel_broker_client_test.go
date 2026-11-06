package services

import (
	"context"
	"errors"
	"io"
	"net"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

type testAgentTunnelStream struct {
	ctx      context.Context
	received chan *cloudpb.TunnelData
	sent     chan *cloudpb.TunnelData
}

func (s *testAgentTunnelStream) Send(message *cloudpb.TunnelData) error {
	messageCopy := &cloudpb.TunnelData{
		SessionId: message.SessionId,
		Payload:   append([]byte(nil), message.Payload...),
		HalfClose: message.HalfClose,
	}
	select {
	case s.sent <- messageCopy:
		return nil
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
}

func (s *testAgentTunnelStream) Recv() (*cloudpb.TunnelData, error) {
	select {
	case message, ok := <-s.received:
		if !ok {
			return nil, io.EOF
		}
		return message, nil
	case <-s.ctx.Done():
		return nil, s.ctx.Err()
	}
}

func (s *testAgentTunnelStream) CloseSend() error { return nil }

// failWriteConn models the hardware failure that motivated this regression:
// the registry-side write fails, but the opposite Read remains blocked until
// the relay explicitly closes the connection.
type failWriteConn struct {
	net.Conn
	err error
}

func (c *failWriteConn) Write([]byte) (int, error) { return 0, c.err }

func TestTunnelBrokerRelayDoesNotCancelAfterBackendEOF(t *testing.T) {
	ctx, cancelContext := context.WithCancel(context.Background())
	defer cancelContext()

	var cancelled atomic.Bool
	cancel := func() {
		cancelled.Store(true)
		cancelContext()
	}
	stream := &testAgentTunnelStream{
		ctx:      ctx,
		received: make(chan *cloudpb.TunnelData),
		sent:     make(chan *cloudpb.TunnelData, 4),
	}
	relayConn, backendConn := net.Pipe()
	defer relayConn.Close()

	client := &TunnelBrokerClient{}
	relayDone := make(chan struct{})
	go func() {
		client.relay(ctx, cancel, relayConn, stream)
		close(relayDone)
	}()

	const response = "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nOK"
	go func() {
		_, _ = backendConn.Write([]byte(response))
		_ = backendConn.Close()
	}()

	var payload []byte
	for {
		select {
		case message := <-stream.sent:
			payload = append(payload, message.Payload...)
			if message.HalfClose {
				goto receivedHalfClose
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for agent half-close")
		}
	}

receivedHalfClose:
	if string(payload) != response {
		t.Fatalf("relayed payload = %q, want %q", payload, response)
	}
	time.Sleep(25 * time.Millisecond)
	if cancelled.Load() {
		t.Fatal("relay cancelled the stream before the broker closed its direction")
	}

	close(stream.received)
	select {
	case <-relayDone:
	case <-time.After(time.Second):
		t.Fatal("relay did not finish after both directions closed")
	}
}

func TestTunnelBrokerRelayHardWriteFailureUnblocksOppositeDirection(t *testing.T) {
	ctx, cancelContext := context.WithCancel(context.Background())
	defer cancelContext()

	var cancelled atomic.Bool
	cancel := func() {
		cancelled.Store(true)
		cancelContext()
	}
	stream := &testAgentTunnelStream{
		ctx:      ctx,
		received: make(chan *cloudpb.TunnelData, 1),
		sent:     make(chan *cloudpb.TunnelData, 1),
	}
	stream.received <- &cloudpb.TunnelData{Payload: []byte("upload bytes")}

	relayPipe, blockedPeer := net.Pipe()
	defer blockedPeer.Close()
	relayConn := &failWriteConn{Conn: relayPipe, err: errors.New("registry reset")}

	client := &TunnelBrokerClient{}
	relayDone := make(chan struct{})
	go func() {
		client.relay(ctx, cancel, relayConn, stream)
		close(relayDone)
	}()

	select {
	case <-relayDone:
	case <-time.After(time.Second):
		t.Fatal("relay remained blocked after the registry-side write failed")
	}
	if !cancelled.Load() {
		t.Fatal("hard TCP failure did not cancel the broker stream")
	}
}

// runBrokerSessions drives Run through sessions that each live for the given
// time on a fake clock and then fail, returning the backoff before each redial.
func runBrokerSessions(t *testing.T, lifetimes ...time.Duration) []time.Duration {
	t.Helper()
	origRunOnce, origNow, origAfter := brokerRunOnceFn, brokerNowFn, brokerAfterFn
	t.Cleanup(func() { brokerRunOnceFn, brokerNowFn, brokerAfterFn = origRunOnce, origNow, origAfter })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	now := time.Unix(0, 0)
	var delays []time.Duration
	brokerNowFn = func() time.Time { return now }
	brokerAfterFn = func(d time.Duration) <-chan time.Time {
		delays = append(delays, d)
		now = now.Add(d)
		fired := make(chan time.Time, 1)
		fired <- now
		return fired
	}
	brokerRunOnceFn = func(*TunnelBrokerClient, context.Context) error {
		if len(lifetimes) == 0 {
			cancel()
			return ctx.Err()
		}
		now = now.Add(lifetimes[0])
		lifetimes = lifetimes[1:]
		return status.Error(codes.Unavailable, "stream terminated")
	}

	(&TunnelBrokerClient{logger: zap.NewNop()}).Run(ctx)
	return delays
}

func TestTunnelBrokerRunResetsBackoffAfterALongSessionFails(t *testing.T) {
	// Boot-time dial failures, then hour-long streams the platform ends with an error.
	got := runBrokerSessions(t, 0, 0, 0, time.Hour, time.Hour)
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, time.Second, time.Second}
	if !slices.Equal(got, want) {
		t.Fatalf("backoffs = %v, want %v", got, want)
	}
}

func TestTunnelBrokerRunKeepsBackingOffWhileSessionsFailFast(t *testing.T) {
	// The stream opens but the broker refuses it on the first receive.
	got := runBrokerSessions(t, slices.Repeat([]time.Duration{200 * time.Millisecond}, 9)...)
	want := []time.Duration{
		time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second,
		32 * time.Second, 64 * time.Second, brokerMaxBackoff, brokerMaxBackoff,
	}
	if !slices.Equal(got, want) {
		t.Fatalf("backoffs = %v, want %v", got, want)
	}
}

func TestTunnelBrokerRunResetsOnlyAfterOutlivingTheLongestBackoff(t *testing.T) {
	got := runBrokerSessions(t, 0, brokerMaxBackoff-time.Second, brokerMaxBackoff)
	want := []time.Duration{time.Second, 2 * time.Second, time.Second}
	if !slices.Equal(got, want) {
		t.Fatalf("backoffs = %v, want %v", got, want)
	}
}
