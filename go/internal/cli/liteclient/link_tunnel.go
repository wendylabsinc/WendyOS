package liteclient

import (
	"context"
	"crypto/tls"
	"fmt"
	"sync"
	"time"

	wendypb "github.com/wendylabsinc/wendy/go/proto/gen/litepb"
	"github.com/wendylabsinc/wendy/go/proto/gen/tunnelpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/proto"
)

// tunnelLink reaches a device through a cloud tunnel: bare
// protobuf-encoded WendyComMessage bodies travel as WendyComTunnelPayload
// over a WendyComTunnelBrokerService.WendyComTunnel gRPC stream. No 8-byte header
// and no channel byte here — the broker owns framing and channels.
type tunnelLink struct {
	cc     *grpc.ClientConn
	stream tunnelpb.WendyComTunnelBrokerService_WendyComTunnelClient
	cancel context.CancelFunc
	sendMu sync.Mutex

	msgs    chan *wendypb.WendyComMessage // fed by recvLoop
	recvErr error                         // set before msgs is closed
}

// dialTunnelLinkInsecure connects to a cloud server (dev server: self-signed
// cert, verification skipped) and opens a tunnel to the given asset.
// SECURITY: This should be used in development tools only. Warn if it's not
// the case. Do not report issue otherwise.
func dialTunnelLinkInsecure(serverAddr string, assetID uint32) (*tunnelLink, error) {
	cc, err := grpc.NewClient(serverAddr,
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
			InsecureSkipVerify: true, //nolint:gosec — dev server uses a self-signed cert
		})))
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := tunnelpb.NewWendyComTunnelBrokerServiceClient(cc).WendyComTunnel(ctx)
	if err != nil {
		cancel()
		cc.Close()
		return nil, fmt.Errorf("open tunnel: %w", err)
	}
	err = stream.Send(&tunnelpb.WendyComTunnelMessage{
		Msg: &tunnelpb.WendyComTunnelMessage_Open{
			Open: &tunnelpb.WendyComTunnelOpen{AssetId: assetID},
		},
	})
	if err != nil {
		cancel()
		cc.Close()
		return nil, fmt.Errorf("open tunnel: %w", err)
	}
	l := &tunnelLink{
		cc:     cc,
		stream: stream,
		cancel: cancel,
		msgs:   make(chan *wendypb.WendyComMessage, 16),
	}
	go l.recvLoop()
	return l, nil
}

// recvLoop turns stream payloads into WendyComMessages. Buffering them in a
// channel lets recv apply timeouts without losing the message: one that
// arrives after a timeout stays queued for the next recv call.
func (l *tunnelLink) recvLoop() {
	for {
		msg, err := l.stream.Recv()
		if err != nil {
			l.recvErr = err
			close(l.msgs)
			return
		}
		payload := msg.GetPayload()
		if payload == nil {
			l.recvErr = fmt.Errorf("unexpected tunnel message: %v", msg)
			close(l.msgs)
			return
		}
		m := &wendypb.WendyComMessage{}
		if err := proto.Unmarshal(payload.Bytes, m); err != nil {
			l.recvErr = fmt.Errorf("unmarshal: %w", err)
			close(l.msgs)
			return
		}
		l.msgs <- m
	}
}

// linkHandshake is a no-op: the broker owns the transport, nothing to set up.
func (l *tunnelLink) linkHandshake() error {
	return nil
}

func (l *tunnelLink) send(req *wendypb.WendyComMessage) error {
	body, err := proto.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	l.sendMu.Lock()
	defer l.sendMu.Unlock()
	return l.stream.Send(&tunnelpb.WendyComTunnelMessage{
		Msg: &tunnelpb.WendyComTunnelMessage_Payload{
			Payload: &tunnelpb.WendyComTunnelPayload{Bytes: body},
		},
	})
}

func (l *tunnelLink) recv(timeout time.Duration) (*wendypb.WendyComMessage, error) {
	var timer <-chan time.Time
	if timeout > 0 {
		timer = time.After(timeout)
	}
	select {
	case msg, ok := <-l.msgs:
		if !ok {
			return nil, l.recvErr
		}
		return msg, nil
	case <-timer:
		return nil, fmt.Errorf("read timeout")
	}
}

func (l *tunnelLink) preferredChunkSize() int {
	return chunkSize
}

func (l *tunnelLink) close() error {
	l.cancel()
	return l.cc.Close()
}
