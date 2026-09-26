package liteclient

import (
	"context"
	"testing"
	"time"

	wendypb "github.com/wendylabsinc/wendy/go/proto/gen/litepb"
	"github.com/wendylabsinc/wendy/go/proto/gen/tunnelpb"
	"google.golang.org/protobuf/proto"
)

// floodStream is a tunnel stream whose Recv always has another payload ready,
// as a chatty device would. Only Recv is used by recvLoop.
type floodStream struct {
	tunnelpb.WendyComTunnelBrokerService_WendyComTunnelClient
	payload []byte
}

func (s *floodStream) Recv() (*tunnelpb.WendyComTunnelMessage, error) {
	return &tunnelpb.WendyComTunnelMessage{
		Msg: &tunnelpb.WendyComTunnelMessage_Payload{
			Payload: &tunnelpb.WendyComTunnelPayload{Bytes: s.payload},
		},
	}, nil
}

func TestTunnelRecvLoopExitsWhenCancelledWithNoReader(t *testing.T) {
	body, err := proto.Marshal(&wendypb.WendyComMessage{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	l := &tunnelLink{
		stream: &floodStream{payload: body},
		ctx:    ctx,
		cancel: cancel,
		msgs:   make(chan *wendypb.WendyComMessage, 16),
	}
	exited := make(chan struct{})
	go func() {
		l.recvLoop()
		close(exited)
	}()

	// Let the channel fill with nobody reading, as after a failed handshake.
	deadline := time.Now().Add(time.Second)
	for len(l.msgs) < cap(l.msgs) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(l.msgs) < cap(l.msgs) {
		t.Fatalf("msgs holds %d of %d, want it full", len(l.msgs), cap(l.msgs))
	}

	cancel()
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("recvLoop still blocked on a full channel after cancel")
	}
}
