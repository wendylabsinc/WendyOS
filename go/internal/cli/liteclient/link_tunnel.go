package liteclient

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/clouddefaults"
	"github.com/wendylabsinc/wendy/go/internal/shared/cloudrelay"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/proto/gen/wcomrelaypb"
	"google.golang.org/grpc"
)

// RelayOptions supplies the operator signer and authenticated broker routing.
type RelayOptions struct {
	// OperatorIdentity is the exact certificate and resolved private key used
	// by SignRequest. The provider must use this identity for inner mTLS.
	OperatorIdentity config.CertificateInfo
	// DeviceTrustChainPEM comes from the selected Cloud session. Lite operator
	// leaves use a separate authority and do not carry the device's CA chain.
	DeviceTrustChainPEM string
	// DeviceTrustFallback loads the tenant's Lite Device CA chain as untrusted
	// intermediates; a Lite leaf must still chain to DeviceTrustChainPEM.
	DeviceTrustFallback LiteDeviceCAChain
	SignRequest         func([]byte) ([]byte, error)
	Redial              func(context.Context, string) (*grpc.ClientConn, context.Context, error)
}

type relayConn struct {
	*clouddefaults.BrokerTunnelConn
	remote net.Conn
	cc     *grpc.ClientConn
	cancel context.CancelFunc
	once   sync.Once
	sendMu sync.Mutex
	stream wcomrelaypb.WendyComRelayService_WendyComRelayClient
	grant  []byte
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
func openRelayConn(ctx context.Context, cc *grpc.ClientConn, assetID string, options RelayOptions) (*relayConn, error) {
	principal, err := cloudrelay.LitePrincipalRequest(assetID, options.OperatorIdentity, options.SignRequest)
	if err != nil {
		cc.Close()
		return nil, err
	}
	var stream wcomrelaypb.WendyComRelayService_WendyComRelayClient
	var cancel context.CancelFunc
	var grant []byte
	for attempt := 0; attempt < 3; attempt++ {
		streamCtx, stop := context.WithCancel(ctx)
		cancel = stop
		admissionTimer := time.AfterFunc(30*time.Second, stop)
		stream, err = wcomrelaypb.NewWendyComRelayServiceClient(cc).WendyComRelay(streamCtx)
		if err == nil {
			err = stream.Send(&wcomrelaypb.WendyComRelayMessage{Msg: &wcomrelaypb.WendyComRelayMessage_Open{
				Open: &wcomrelaypb.WendyComRelayOpen{RequestBody: principal.RequestBody, PrincipalRequest: principal.PrincipalRequest},
			}})
		}
		var first *wcomrelaypb.WendyComRelayMessage
		if err == nil {
			first, err = stream.Recv()
		}
		admissionTimer.Stop()
		if err == nil && first.GetRoute() != nil && options.Redial != nil && attempt < 2 {
			stop()
			cc.Close()
			cc, ctx, err = options.Redial(ctx, first.GetRoute().Endpoint)
			if err != nil {
				return nil, err
			}
			continue
		}
		if err == nil {
			grant = first.GetGrant().GetRecord()
			if !validLiteGrantRecord(grant) {
				err = fmt.Errorf("broker returned no valid compact Lite grant")
			}
		}
		if err != nil {
			stop()
			cc.Close()
			return nil, fmt.Errorf("open relay: %w", err)
		}
		break
	}
	local, remote := net.Pipe()
	conn := &relayConn{BrokerTunnelConn: clouddefaults.NewBrokerTunnelConn(local), remote: remote, cc: cc, cancel: cancel, stream: stream, grant: grant}
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
		buffer := make([]byte, 16384)
		for {
			n, err := remote.Read(buffer)
			if err != nil {
				conn.sendMu.Lock()
				if err == io.EOF {
					_ = stream.CloseSend()
				} else {
					conn.Close()
				}
				conn.sendMu.Unlock()
				return
			}
			conn.sendMu.Lock()
			err = stream.Send(&wcomrelaypb.WendyComRelayMessage{Msg: &wcomrelaypb.WendyComRelayMessage_Payload{
				Payload: &wcomrelaypb.WendyComRelayPayload{Bytes: buffer[:n]},
			}})
			conn.sendMu.Unlock()
			if err != nil {
				conn.Fail(err)
				conn.Close()
				return
			}
		}
	}()
	return conn, nil
}

// A Cloud WLG3 record carries timestamps, three UUIDs, five SHA-256 hashes
// and a raw P-256 signature after its key ID. WLG2 is no longer accepted. This
// checks framing only; the device verifies the signature and bindings before
// acknowledging WLA2.
func validLiteGrantRecord(grant []byte) bool {
	const fixedSize = 4 + 1 + 2*8 + 3*16 + 5*32 + 64
	if string(grant[:min(4, len(grant))]) != "WLG3" || len(grant) < fixedSize+1 || len(grant) > fixedSize+128 {
		return false
	}
	kidLen := int(grant[4])
	if kidLen < 1 || kidLen > 128 || len(grant) != fixedSize+kidLen {
		return false
	}
	for _, b := range grant[5 : 5+kidLen] {
		if !(b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '-' || b == '.' || b == '_') {
			return false
		}
	}
	return true
}

func (c *relayConn) established() error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	return c.stream.Send(&wcomrelaypb.WendyComRelayMessage{Msg: &wcomrelaypb.WendyComRelayMessage_Established{
		Established: &wcomrelaypb.WendyComRelayEstablished{},
	}})
}
