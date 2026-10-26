package commands

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/shared/cloudrelay"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
)

// udpFlowIdleTimeout is the client-edge flow lifetime: a local UDP peer that
// stays silent this long is forgotten (the agent side keeps a 2min safety net).
const udpFlowIdleTimeout = 60 * time.Second

// datagramSession is one multiplexed DATAGRAM tunnel to a device, carrying
// all UDP flows and ICMP echoes for that (client, device) pair.
type datagramSession struct {
	stream cloudpb.TunnelBrokerService_ClientTunnelClient
	sendMu sync.Mutex
}

// datagramSender is the subset used by serveUDPForward, split out for tests.
type datagramSender interface {
	sendDatagram(flowID, port uint32, payload []byte) error
	recv() (*cloudpb.TunnelData, error)
}

func openDatagramSession(ctx context.Context, brokerConn *grpc.ClientConn, auth *config.AuthConfig, assetID int32) (*datagramSession, error) {
	client := cloudpb.NewTunnelBrokerServiceClient(brokerConn)
	cloudCtx, err := cloudContext(ctx, auth)
	if err != nil {
		return nil, err
	}
	stream, err := client.ClientTunnel(cloudCtx)
	if err != nil {
		return nil, fmt.Errorf("opening datagram session: %w", err)
	}
	if err := stream.Send(&cloudpb.ClientTunnelMessage{
		Content: &cloudpb.ClientTunnelMessage_Open{
			Open: &cloudpb.ClientTunnelOpen{
				AssetId:  assetID,
				Host:     "localhost",
				Protocol: cloudpb.TunnelProtocol_TUNNEL_PROTOCOL_DATAGRAM,
			},
		},
	}); err != nil {
		return nil, fmt.Errorf("sending datagram open: %w", err)
	}
	return &datagramSession{stream: stream}, nil
}

func (s *datagramSession) send(d *cloudpb.TunnelData) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	return s.stream.Send(&cloudpb.ClientTunnelMessage{
		Content: &cloudpb.ClientTunnelMessage_Data{Data: d},
	})
}

func (s *datagramSession) sendDatagram(flowID, port uint32, payload []byte) error {
	return s.send(&cloudpb.TunnelData{Datagram: &cloudpb.TunnelDatagram{
		FlowId: flowID, Port: port, Payload: payload,
	}})
}

func (s *datagramSession) sendEcho(req *cloudpb.IcmpEchoRequest) error {
	return s.send(&cloudpb.TunnelData{IcmpRequest: req})
}

func (s *datagramSession) recv() (*cloudpb.TunnelData, error) { return s.stream.Recv() }

func (s *datagramSession) close() { _ = s.stream.CloseSend() }

// datagramOpenError maps the classic old-agent failure (the agent ignores the
// protocol field, dials TCP, and never claims the session) to a helpful hint.
func datagramOpenError(err error, device string) error {
	if s, ok := status.FromError(err); ok &&
		(s.Code() == codes.DeadlineExceeded || s.Code() == codes.Unavailable) {
		return fmt.Errorf("%s did not answer the datagram session (%v); the device may be offline or need a WendyOS update for UDP/ping tunnel support", device, err)
	}
	return err
}

// serveUDPForward pumps datagrams between a local UDP listener and a datagram
// session. Flows are keyed by local source address; the first packet from a
// new source allocates a flow_id, replies route back by source, and entries
// expire after `idle` without traffic.
//
// It returns nil when `ctx` is what ended the loop (normal shutdown, e.g.
// Ctrl+C), or the error that killed the session/listener otherwise — callers
// use this to tell "user stopped it" from "the tunnel died underneath us"
// (see cloudTunnelCommand, which folds a non-nil result through
// datagramOpenError for the WendyOS-update hint).
func serveUDPForward(ctx context.Context, pc *net.UDPConn, session datagramSender, remotePort uint32, idle time.Duration) error {
	type flowEntry struct {
		addr       *net.UDPAddr
		lastActive time.Time
	}
	var (
		mu      sync.Mutex
		nextID  uint32 = 1
		byAddr         = map[string]uint32{}
		byID           = map[uint32]*flowEntry{}
		lifeErr error  // first error that ended the session, guarded by mu
	)
	setLifeErr := func(err error) {
		mu.Lock()
		if lifeErr == nil {
			lifeErr = err
		}
		mu.Unlock()
	}

	// done ties the idle-sweep goroutine's lifetime to this call, not just to
	// ctx: without it, a session death (not a ctx cancellation) would leave
	// the sweep goroutine parked on ctx.Done() until process exit.
	done := make(chan struct{})
	defer close(done)

	// Session → local peers.
	go func() {
		for {
			msg, err := session.recv()
			if err != nil {
				setLifeErr(err)
				pc.Close() // unblocks the read loop below
				return
			}
			d := msg.GetDatagram()
			if d == nil {
				continue
			}
			mu.Lock()
			entry := byID[d.GetFlowId()]
			if entry != nil {
				entry.lastActive = time.Now()
			}
			mu.Unlock()
			if entry != nil {
				_, _ = pc.WriteToUDP(d.GetPayload(), entry.addr)
			}
		}
	}()

	// Idle sweep.
	go func() {
		t := time.NewTicker(idle / 4)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				mu.Lock()
				for id, e := range byID {
					if time.Since(e.lastActive) > idle {
						delete(byAddr, e.addr.String())
						delete(byID, id)
					}
				}
				mu.Unlock()
			case <-ctx.Done():
				return
			case <-done:
				return
			}
		}
	}()

	// Local peers → session.
	buf := make([]byte, 65535)
	for {
		n, addr, err := pc.ReadFromUDP(buf)
		if err != nil {
			break // listener closed (ctx cancel or session death)
		}
		key := addr.String()
		mu.Lock()
		id, ok := byAddr[key]
		if !ok {
			id = nextID
			nextID++
			byAddr[key] = id
			byID[id] = &flowEntry{addr: addr, lastActive: time.Now()}
		} else {
			byID[id].lastActive = time.Now()
		}
		mu.Unlock()
		payload := make([]byte, n)
		copy(payload, buf[:n])
		if err := session.sendDatagram(id, remotePort, payload); err != nil {
			setLifeErr(err)
			break
		}
	}

	if ctx.Err() != nil {
		return nil
	}
	mu.Lock()
	err := lifeErr
	mu.Unlock()
	if err != nil {
		return fmt.Errorf("datagram session closed unexpectedly: %w", err)
	}
	return nil
}

// openDatagram selects the authorized v2 UDP path or the legacy broker path.
func (d cloudDiscoveryDevice) openDatagram(ctx context.Context, brokerConn *grpc.ClientConn, auth *config.AuthConfig, remotePort uint32) (interface {
	datagramSender
	close()
}, error) {
	if d.legacy != nil {
		return openDatagramSession(ctx, brokerConn, auth, d.legacy.GetId())
	}
	signer, err := tunnelPrincipalSigner(auth)
	if err != nil {
		return nil, err
	}
	cloudCtx, err := cloudContext(ctx, auth)
	if err != nil {
		return nil, err
	}
	conn, err := dialCloudGRPC(auth)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	issuer, err := cloudrelay.Issuer(auth.CloudGRPC, os.Getenv("WENDY_CLOUD_GRANT_ISSUER"))
	if err != nil {
		return nil, err
	}
	session, err := cloudrelay.OpenUDP(ctx, cloudCtx, conn, &cloudrelay.Verifier{Issuer: issuer}, d.key, remotePort, signer)
	if err != nil {
		return nil, err
	}
	return &v2DatagramSession{session: session, port: remotePort}, nil
}

type v2DatagramSession struct {
	session *cloudrelay.DatagramSession
	port    uint32
}

func (s *v2DatagramSession) close() { _ = s.session.Close() }
func (s *v2DatagramSession) sendDatagram(flowID, port uint32, payload []byte) error {
	if port != s.port {
		return fmt.Errorf("UDP port differs from authorized service")
	}
	return s.session.Send(flowID, payload)
}
func (s *v2DatagramSession) recv() (*cloudpb.TunnelData, error) {
	d, err := s.session.Recv()
	if err != nil {
		return nil, err
	}
	return &cloudpb.TunnelData{Datagram: &cloudpb.TunnelDatagram{FlowId: d.FlowId, Port: s.port, Payload: d.Payload}}, nil
}
