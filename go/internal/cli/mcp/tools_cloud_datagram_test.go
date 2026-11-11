package mcp

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
)

// delayedReturnPingSession delivers the reply while sendEcho still holds the
// ping loop, as a synchronous agent RPC does with the loop's next send.
type delayedReturnPingSession struct {
	ctx      context.Context
	replies  chan *cloudpb.TunnelData
	received chan struct{}
	once     sync.Once
}

func (s *delayedReturnPingSession) sendEcho(req *cloudpb.IcmpEchoRequest) error {
	reply := &cloudpb.TunnelData{IcmpReply: &cloudpb.IcmpEchoReply{Identifier: req.GetIdentifier(), Sequence: req.GetSequence()}}
	select {
	case s.replies <- reply:
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
	select {
	case <-s.received:
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
	select {
	case <-time.After(300 * time.Millisecond):
		return nil
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
}

func (s *delayedReturnPingSession) recv() (*cloudpb.TunnelData, error) {
	select {
	case reply := <-s.replies:
		s.once.Do(func() { close(s.received) })
		return reply, nil
	case <-s.ctx.Done():
		return nil, s.ctx.Err()
	}
}

// Mirrors commands.TestPingRTTExcludesReplyQueueDelay.
func TestMCPPingRTTExcludesReplyQueueDelay(t *testing.T) {
	session := &delayedReturnPingSession{ctx: t.Context(), replies: make(chan *cloudpb.TunnelData, 1), received: make(chan struct{})}
	stats := mcpRunPingLoop(t.Context(), session, "device", 1, time.Second, io.Discard)
	if stats.Sent != 1 || stats.Received != 1 || stats.Err != nil {
		t.Fatalf("unexpected stats: %+v", stats)
	}
	if stats.Max >= 150*time.Millisecond {
		t.Fatalf("RTT includes time waiting to process an arrived reply: %v", stats.Max)
	}
}
