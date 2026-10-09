package liteclient

import (
	"context"
	"fmt"
	"net"
	"sync"

	"github.com/wendylabsinc/wendy/go/internal/cli/clouddefaults"
	"github.com/wendylabsinc/wendy/go/proto/gen/wcomrelaypb"
	"google.golang.org/grpc"
)

// relayConn is a byte stream. The inner TLS session and WendyCom framing belong
// to the CLI and device; the broker cannot interpret either protocol.
type relayConn struct {
	*clouddefaults.BrokerTunnelConn
	remote net.Conn
	cc     *grpc.ClientConn
	cancel context.CancelFunc
	once   sync.Once
}

func (c *relayConn) Close() error {
	c.once.Do(func() {
		c.cancel()
		c.BrokerTunnelConn.Close()
		c.remote.Close()
		c.cc.Close()
	})
	return nil
}

// openRelayConn owns cc on every path. The pipe provides bounded buffering and
// real read/write deadlines to crypto/tls and the existing directLink codec.
func openRelayConn(ctx context.Context, cc *grpc.ClientConn, assetID string) (*relayConn, error) {
	ctx, cancel := context.WithCancel(ctx)
	stream, err := wcomrelaypb.NewWendyComRelayServiceClient(cc).WendyComRelay(ctx)
	if err == nil {
		err = stream.Send(&wcomrelaypb.WendyComRelayMessage{Msg: &wcomrelaypb.WendyComRelayMessage_Open{
			Open: &wcomrelaypb.WendyComRelayOpen{AssetId: assetID},
		}})
	}
	if err != nil {
		cancel()
		cc.Close()
		return nil, fmt.Errorf("open relay: %w", err)
	}
	local, remote := net.Pipe()
	conn := &relayConn{BrokerTunnelConn: clouddefaults.NewBrokerTunnelConn(local), remote: remote, cc: cc, cancel: cancel}
	context.AfterFunc(ctx, func() { conn.Close() })
	go func() {
		defer conn.Close()
		for {
			message, err := stream.Recv()
			if err != nil {
				conn.Fail(err)
				return
			}
			payload := message.GetPayload()
			if payload == nil || len(payload.Bytes) == 0 || len(payload.Bytes) > 65536 {
				conn.Fail(fmt.Errorf("invalid relay byte payload"))
				return
			}
			if _, err := remote.Write(payload.Bytes); err != nil {
				return
			}
		}
	}()
	go func() {
		defer conn.Close()
		buffer := make([]byte, 16384)
		for {
			n, err := remote.Read(buffer)
			if err != nil {
				return
			}
			if err = stream.Send(&wcomrelaypb.WendyComRelayMessage{Msg: &wcomrelaypb.WendyComRelayMessage_Payload{
				Payload: &wcomrelaypb.WendyComRelayPayload{Bytes: buffer[:n]},
			}}); err != nil {
				conn.Fail(err)
				return
			}
		}
	}()
	return conn, nil
}
