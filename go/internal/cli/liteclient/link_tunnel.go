package liteclient

import (
	"context"
	"crypto/tls"
	"fmt"
	"sync"
	"time"

	wendypb "github.com/wendylabsinc/wendy/go/proto/gen/litepb"
	"github.com/wendylabsinc/wendy/go/proto/gen/wcomrelaypb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/proto"
)

// tunnelLink reaches a device through a cloud tunnel: bare
// protobuf-encoded WendyComMessage bodies travel as WendyComRelayPayload
// over a WendyComRelayService.WendyComRelay gRPC stream. No 8-byte header
// and no channel byte here — the broker owns framing and channels.
type tunnelLink struct {
	cc     *grpc.ClientConn
	stream wcomrelaypb.WendyComRelayService_WendyComRelayClient
	ctx    context.Context // the stream's; done once close cancels it
	cancel context.CancelFunc
	sendMu sync.Mutex

	msgs    chan *wendypb.WendyComMessage // fed by recvLoop
	recvErr error                         // set before msgs is closed
}

// dialTunnelLinkInsecure connects to a cloud server (dev server: self-signed
// cert, verification skipped) and opens a tunnel to the given asset.
// SECURITY: This should be used in development tools only. Warn if it's not
// the case. Do not report issue otherwise.
func dialTunnelLinkInsecure(serverAddr string, assetID string) (*tunnelLink, error) {
	cc, err := grpc.NewClient(serverAddr,
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
			InsecureSkipVerify: true, //nolint:gosec — dev server uses a self-signed cert
		})))
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	return openTunnelLink(context.Background(), cc, assetID)
}

// openTunnelLink opens a tunnel to the given asset over an already dialed
// connection, which the link then owns: it is closed with the link, or right
// away if the tunnel cannot be opened. The stream runs under ctx, so its
// outgoing metadata (the caller's credentials) reaches the server, and
// cancelling ctx ends the tunnel.
func openTunnelLink(ctx context.Context, cc *grpc.ClientConn, assetID string) (*tunnelLink, error) {
	ctx, cancel := context.WithCancel(ctx)
	stream, err := wcomrelaypb.NewWendyComRelayServiceClient(cc).WendyComRelay(ctx)
	if err != nil {
		cancel()
		cc.Close()
		return nil, fmt.Errorf("open tunnel: %w", err)
	}
	err = stream.Send(&wcomrelaypb.WendyComRelayMessage{
		Msg: &wcomrelaypb.WendyComRelayMessage_Open{
			Open: &wcomrelaypb.WendyComRelayOpen{AssetId: assetID},
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
		ctx:    ctx,
		cancel: cancel,
		msgs:   make(chan *wendypb.WendyComMessage, 16),
	}
	go l.recvLoop()
	return l, nil
}

// recvLoop turns stream payloads into WendyComMessages. Buffering them in a
// channel lets recv apply timeouts without losing the message: one that
// arrives after a timeout stays queued for the next recv call.
//
// The hand-off also watches the stream context: once nothing reads msgs (a
// failed handshake leaves no reader), a full channel would otherwise block
// this goroutine forever, since cancelling only fails the next stream.Recv.
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
		select {
		case l.msgs <- m:
		case <-l.ctx.Done():
			l.recvErr = l.ctx.Err()
			close(l.msgs)
			return
		}
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
	return l.stream.Send(&wcomrelaypb.WendyComRelayMessage{
		Msg: &wcomrelaypb.WendyComRelayMessage_Payload{
			Payload: &wcomrelaypb.WendyComRelayPayload{Bytes: body},
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
