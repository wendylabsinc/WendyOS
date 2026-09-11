package commands

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/wendylabsinc/wendy/go/internal/cli/clouddefaults"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"

	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
)

// pingSession is the slice of datagramSession that runPingLoop needs.
type pingSession interface {
	sendEcho(req *cloudpb.IcmpEchoRequest) error
	recv() (*cloudpb.TunnelData, error)
}

type pingStats struct {
	Sent, Received int
	Min, Avg, Max  time.Duration
	// Err is the transport error (if any) that ended the recv loop — e.g.
	// PermissionDenied, Unauthenticated, or a mesh-disabled rejection. Nil
	// means the recv loop never errored (a silent device, or the ping ended
	// normally). Callers should only fall back to the generic "may need a
	// WendyOS update" hint when this is nil; otherwise surface it (through
	// datagramOpenError, which still special-cases DeadlineExceeded/
	// Unavailable into the same hint).
	Err error
}

func newCloudPingCmd() *cobra.Command {
	var cloudGRPC, deviceName, brokerURL string
	var count int
	var interval time.Duration

	cmd := &cobra.Command{
		Use:   "ping [--device <name>]",
		Short: "Ping a cloud-enrolled device through the tunnel broker",
		Long:  "Measures agent round-trip time through Wendy Cloud. PKI sessions use the agent version RPC over an authorized tunnel; legacy sessions use datagram echoes. A reply proves the device's agent is up and measures true end-to-end round-trip time. No ICMP sockets or privileges are involved.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cloudPingCommand(cmd.Context(), cloudGRPC, deviceName, brokerURL, count, interval)
		},
	}
	cmd.Flags().StringVar(&cloudGRPC, "cloud-grpc", "", "Cloud gRPC endpoint (optional when a default session is set via 'wendy auth use')")
	cmd.Flags().StringVar(&deviceName, "device", "", "Device name (skips interactive picker)")
	cmd.Flags().StringVar(&brokerURL, "broker-url", os.Getenv("WENDY_BROKER_URL"), "Tunnel broker host:port")
	cmd.Flags().IntVarP(&count, "count", "c", 0, "Stop after this many echoes (0 = until Ctrl+C)")
	cmd.Flags().DurationVarP(&interval, "interval", "i", time.Second, "Time between echoes")
	return cmd
}

func cloudPingCommand(ctx context.Context, cloudGRPC, deviceName, brokerURL string, count int, interval time.Duration) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if count < 0 {
		return fmt.Errorf("count must be >= 0")
	}
	if interval <= 0 {
		return fmt.Errorf("interval must be > 0")
	}
	auth, err := pickAuthEntry(cloudGRPC)
	if err != nil {
		return err
	}
	asset, err := pickCloudDiscoveryDevice(ctx, auth, deviceName, brokerURL)
	if err != nil {
		return err
	}
	var session pingSession
	if asset.v2 != nil {
		conn, err := asset.connect(ctx, auth, brokerURL)
		if err != nil {
			return err
		}
		defer conn.Close()
		session = &agentRPCPingSession{ctx: ctx, conn: conn, replies: make(chan *cloudpb.TunnelData, 1)}
		cliLogln("PING %s (asset %s) via Wendy Cloud agent RPC", asset.GetName(), asset.key)
	} else {
		brokerConn, err := clouddefaults.DialBroker(auth, brokerURL)
		if err != nil {
			return err
		}
		defer brokerConn.Close()
		datagrams, err := openDatagramSession(ctx, brokerConn, auth, asset.legacy.GetId())
		if err != nil {
			return datagramOpenError(err, asset.GetName())
		}
		defer datagrams.close()
		session = datagrams
		cliLogln("PING %s (asset %s) via Wendy Cloud", asset.GetName(), asset.key)
	}

	stats := runPingLoop(ctx, session, asset.GetName(), count, interval, os.Stdout)

	loss := 0.0
	if stats.Sent > 0 {
		loss = 100 * float64(stats.Sent-stats.Received) / float64(stats.Sent)
	}
	cliLogln("--- %s ping statistics ---", asset.GetName())
	cliLogln("%d sent, %d received, %.0f%% loss", stats.Sent, stats.Received, loss)
	if stats.Received > 0 {
		cliLogln("rtt min/avg/max = %s/%s/%s", stats.Min.Round(time.Microsecond),
			stats.Avg.Round(time.Microsecond), stats.Max.Round(time.Microsecond))
	}
	if stats.Received == 0 && stats.Err != nil {
		return datagramOpenError(stats.Err, asset.GetName())
	}
	if stats.Received == 0 && stats.Sent > 0 {
		if stats.Err != nil {
			// A genuine transport error (PermissionDenied, Unauthenticated,
			// mesh-disabled, ...) ended the recv loop — surface it instead of
			// the generic hint. datagramOpenError still folds
			// DeadlineExceeded/Unavailable into that same hint.
			return datagramOpenError(stats.Err, asset.GetName())
		}
		return fmt.Errorf("no replies from %s: the device may be offline or need a WendyOS update for ping support", asset.GetName())
	}
	return nil
}

// runPingLoop sends one echo per interval (count of 0 = unbounded), prints
// each matched reply to out, and returns aggregate stats once every echo has
// either been answered or the ctx ends. Replies are matched to a pending
// request by sequence number; only replies carrying this process's identifier
// are considered (so two concurrent `wendy cloud ping` runs against the same
// device don't cross-count each other's replies).
func runPingLoop(ctx context.Context, session pingSession, target string, count int, interval time.Duration, out io.Writer) pingStats {
	identifier := uint32(os.Getpid() & 0xFFFF)
	type sentEcho struct{ originate time.Time }
	var (
		stats     pingStats
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
	// stats.Sent and pending are only updated after sendEcho actually
	// succeeds, so a transport failure doesn't get counted as a lost reply.
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
	// arrive; armed exactly once, when the send phase ends (all echoes
	// dispatched, or a transport error stopped it early).
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

	finish := func() pingStats {
		stats.Avg = pingAvg(total, stats.Received)
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

func pingAvg(total time.Duration, n int) time.Duration {
	if n == 0 {
		return 0
	}
	return total / time.Duration(n)
}

// agentRPCPingSession measures an actual request/response over the authorized
// wendy-agent tunnel. Cloud's standard catalog has no DATAGRAM/ping service.
type agentRPCPingSession struct {
	ctx     context.Context
	conn    *grpcclient.AgentConnection
	replies chan *cloudpb.TunnelData
}

func (s *agentRPCPingSession) sendEcho(req *cloudpb.IcmpEchoRequest) error {
	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()
	if _, err := s.conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{}); err != nil {
		return err
	}
	reply := &cloudpb.TunnelData{IcmpReply: &cloudpb.IcmpEchoReply{Identifier: req.Identifier, Sequence: req.Sequence, Payload: req.Payload, OriginateUnixNs: req.OriginateUnixNs}}
	select {
	case s.replies <- reply:
		return nil
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
}
func (s *agentRPCPingSession) recv() (*cloudpb.TunnelData, error) {
	select {
	case reply := <-s.replies:
		return reply, nil
	case <-s.ctx.Done():
		return nil, s.ctx.Err()
	}
}
