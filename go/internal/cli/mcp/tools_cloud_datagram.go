package mcp

import (
	"context"
	"fmt"
	"io"
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

// This file mirrors the UDP/ICMP datagram-session plumbing in
// go/internal/cli/commands/cloud_datagram.go and cloud_ping.go
// (mcpDatagramSession ~ commands.datagramSession, mcpServeUDPForward ~
// commands.serveUDPForward, mcpRunPingLoop ~ commands.runPingLoop, etc).
//
// The commands package keeps all of this unexported, so it cannot be called
// from package mcp directly (an interface with unexported methods, like
// pingSession/datagramSender, can only be satisfied by types defined in the
// same package as the interface). tools_cloud.go already established the
// convention for this situation with mcpOpenBrokerTunnel/mcpServeTunnelConn/
// mcpTunnelDialer: rather than exporting the CLI helpers or introducing a
// shared internal package, it keeps a package-private MCP mirror of the logic
// it needs. (The broker dial itself is the exception: both packages now share
// clouddefaults.DialBroker, WDY-2434.) This file does the same for the
// datagram session and ping loop needed by cloud_tunnel's /udp path and
// cloud_ping.

// mcpUDPFlowIdleTimeout is the client-edge flow lifetime: a local UDP peer
// that stays silent this long is forgotten (the agent side keeps a 2min
// safety net). Mirrors commands.udpFlowIdleTimeout.
const mcpUDPFlowIdleTimeout = 60 * time.Second

// mcpDatagramSession is one multiplexed DATAGRAM tunnel to a device, carrying
// all UDP flows and ICMP echoes for that (client, device) pair. Mirrors
// commands.datagramSession.
type mcpDatagramSession struct {
	stream cloudpb.TunnelBrokerService_ClientTunnelClient
	sendMu sync.Mutex
}

// mcpDatagramSender is the subset used by mcpServeUDPForward, split out for
// tests. Mirrors commands.datagramSender.
type mcpDatagramSender interface {
	sendDatagram(flowID, port uint32, payload []byte) error
	recv() (*cloudpb.TunnelData, error)
}

func mcpOpenDatagramSession(ctx context.Context, brokerConn *grpc.ClientConn, auth *config.AuthConfig, assetID int32) (*mcpDatagramSession, error) {
	client := cloudpb.NewTunnelBrokerServiceClient(brokerConn)
	cloudCtx, err := mcpCloudContext(ctx, auth)
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
	return &mcpDatagramSession{stream: stream}, nil
}

func (s *mcpDatagramSession) send(d *cloudpb.TunnelData) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	return s.stream.Send(&cloudpb.ClientTunnelMessage{
		Content: &cloudpb.ClientTunnelMessage_Data{Data: d},
	})
}

func (s *mcpDatagramSession) sendDatagram(flowID, port uint32, payload []byte) error {
	return s.send(&cloudpb.TunnelData{Datagram: &cloudpb.TunnelDatagram{
		FlowId: flowID, Port: port, Payload: payload,
	}})
}

func (s *mcpDatagramSession) sendEcho(req *cloudpb.IcmpEchoRequest) error {
	return s.send(&cloudpb.TunnelData{IcmpRequest: req})
}

func (s *mcpDatagramSession) recv() (*cloudpb.TunnelData, error) { return s.stream.Recv() }

func (s *mcpDatagramSession) close() { _ = s.stream.CloseSend() }

// mcpDatagramOpenError maps the classic old-agent failure (the agent ignores
// the protocol field, dials TCP, and never claims the session) to a helpful
// hint. Mirrors commands.datagramOpenError.
func mcpDatagramOpenError(err error, device string) error {
	if s, ok := status.FromError(err); ok &&
		(s.Code() == codes.DeadlineExceeded || s.Code() == codes.Unavailable) {
		return fmt.Errorf("%s did not answer the datagram session (%v); the device may be offline or need a WendyOS update for UDP/ping tunnel support", device, err)
	}
	return err
}

// mcpServeUDPForward pumps datagrams between a local UDP listener and a
// datagram session. Flows are keyed by local source address; the first
// packet from a new source allocates a flow_id, replies route back by
// source, and entries expire after `idle` without traffic.
//
// It returns nil when `ctx` is what ended the loop (normal shutdown), or the
// error that killed the session/listener otherwise. Mirrors
// commands.serveUDPForward.
func mcpServeUDPForward(ctx context.Context, pc *net.UDPConn, session mcpDatagramSender, remotePort uint32, idle time.Duration) error {
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

// mcpPingSession is the slice of mcpDatagramSession that mcpRunPingLoop
// needs. Mirrors commands.pingSession.
type mcpPingSession interface {
	sendEcho(req *cloudpb.IcmpEchoRequest) error
	recv() (*cloudpb.TunnelData, error)
}

type mcpPingStats struct {
	Sent, Received int
	Min, Avg, Max  time.Duration
	// Err is the transport error (if any) that ended the recv loop. Nil means
	// the recv loop never errored (silence, or a normal finish). Mirrors
	// commands.pingStats.Err.
	Err error
}

// mcpRunPingLoop sends one echo per interval (count of 0 = unbounded), writes
// each matched reply to out, and returns aggregate stats once every echo has
// either been answered or the ctx ends. Mirrors commands.runPingLoop.
func mcpRunPingLoop(ctx context.Context, session mcpPingSession, target string, count int, interval time.Duration, out io.Writer) mcpPingStats {
	identifier := uint32(os.Getpid() & 0xFFFF)
	type sentEcho struct{ originate time.Time }
	var (
		stats     mcpPingStats
		pending   = map[uint32]sentEcho{} // keyed by sequence
		total     time.Duration
		done      = make(chan struct{})
		replies   = make(chan *cloudpb.IcmpEchoReply, 16)
		lifeErrMu sync.Mutex
		lifeErr   error // first transport error out of the recv loop, guarded by lifeErrMu
	)
	setLifeErr := func(err error) {
		lifeErrMu.Lock()
		if lifeErr == nil {
			lifeErr = err
		}
		lifeErrMu.Unlock()
	}

	go func() {
		defer close(done)
		for {
			msg, err := session.recv()
			if err != nil {
				setLifeErr(err)
				return
			}
			if r := msg.GetIcmpReply(); r != nil && r.GetIdentifier() == identifier {
				select {
				case replies <- r:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	seq := uint32(0)
	// sendOne returns false once the requested count has been dispatched (or
	// on a transport error), which the caller uses to stop the send phase.
	sendOne := func() bool {
		if count > 0 && int(seq) >= count {
			return false
		}
		seq++
		now := time.Now()
		req := &cloudpb.IcmpEchoRequest{
			Identifier:      identifier,
			Sequence:        seq,
			Payload:         []byte("wendy-ping"),
			OriginateUnixNs: uint64(now.UnixNano()),
		}
		if err := session.sendEcho(req); err != nil {
			setLifeErr(err)
			return false
		}
		pending[seq] = sentEcho{originate: now}
		stats.Sent++
		return true
	}
	if !sendOne() {
		lifeErrMu.Lock()
		stats.Err = lifeErr
		lifeErrMu.Unlock()
		return stats
	}

	// Grace window after the last send so the final reply(ies) can still
	// arrive; armed exactly once, when the send phase ends.
	graceTimer := time.NewTimer(24 * time.Hour)
	defer graceTimer.Stop()
	sendingDone := false
	armGrace := func() {
		if sendingDone {
			return // already armed — a re-fired ticker.C must not push the deadline out again
		}
		sendingDone = true
		ticker.Stop()
		grace := interval
		if grace < 500*time.Millisecond {
			grace = 500 * time.Millisecond
		}
		graceTimer.Reset(grace)
	}

	finish := func() mcpPingStats {
		stats.Avg = mcpPingAvg(total, stats.Received)
		lifeErrMu.Lock()
		stats.Err = lifeErr
		lifeErrMu.Unlock()
		return stats
	}

	for {
		select {
		case r := <-replies:
			if s, ok := pending[r.GetSequence()]; ok {
				delete(pending, r.GetSequence())
				rtt := time.Since(s.originate)
				stats.Received++
				total += rtt
				if stats.Min == 0 || rtt < stats.Min {
					stats.Min = rtt
				}
				if rtt > stats.Max {
					stats.Max = rtt
				}
				fmt.Fprintf(out, "reply from %s: seq=%d time=%s\n", target, r.GetSequence(), rtt.Round(time.Microsecond))
			}
			if count > 0 && len(pending) == 0 && stats.Sent >= count {
				return finish()
			}
		case <-ticker.C:
			if !sendOne() {
				armGrace()
			}
		case <-graceTimer.C:
			return finish()
		case <-ctx.Done():
			return finish()
		case <-done:
			return finish()
		}
	}
}

func mcpPingAvg(total time.Duration, n int) time.Duration {
	if n == 0 {
		return 0
	}
	return total / time.Duration(n)
}

// openDatagram selects the authorized v2 UDP path or the legacy broker path.
func (d mcpCloudDevice) openDatagram(ctx context.Context, brokerConn *grpc.ClientConn, auth *config.AuthConfig, remotePort uint32) (interface {
	mcpDatagramSender
	close()
}, error) {
	if !d.isV2 {
		return mcpOpenDatagramSession(ctx, brokerConn, auth, d.legacyID)
	}
	signer, err := mcpTunnelPrincipalSigner(auth)
	if err != nil {
		return nil, err
	}
	cloudCtx, err := mcpCloudContext(ctx, auth)
	if err != nil {
		return nil, err
	}
	conn, err := mcpDialCloudGRPC(auth)
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
	return &mcpV2DatagramSession{session: session, port: remotePort}, nil
}

type mcpV2DatagramSession struct {
	session *cloudrelay.DatagramSession
	port    uint32
}

func (s *mcpV2DatagramSession) close() { _ = s.session.Close() }
func (s *mcpV2DatagramSession) sendDatagram(flowID, port uint32, payload []byte) error {
	if port != s.port {
		return fmt.Errorf("UDP port differs from authorized service")
	}
	return s.session.Send(flowID, payload)
}
func (s *mcpV2DatagramSession) recv() (*cloudpb.TunnelData, error) {
	d, err := s.session.Recv()
	if err != nil {
		return nil, err
	}
	return &cloudpb.TunnelData{Datagram: &cloudpb.TunnelDatagram{FlowId: d.FlowId, Port: s.port, Payload: d.Payload}}, nil
}
