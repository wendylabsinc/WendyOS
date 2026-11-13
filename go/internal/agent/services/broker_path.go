package services

import (
	"context"
	"errors"
	"net"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/peer"
)

const (
	// brokerPathCheckInterval paces the check that the broker connection still
	// leaves through the interface a new connection would use.
	brokerPathCheckInterval = 10 * time.Second
	// brokerPathProbeTimeout bounds the check that the preferred path reaches
	// the broker before the working connection is dropped.
	brokerPathProbeTimeout = 5 * time.Second
	// brokerPathMaxWait caps, in checks, the back-off after the preferred path
	// failed to reach the broker.
	brokerPathMaxWait = 30
	// brokerHeartbeatSettle is how long after a heartbeat the stream is kept:
	// the broker records each heartbeat before it can drop a closed stream,
	// and a drop that lands after the new registration erases it.
	brokerHeartbeatSettle = 5 * time.Second
)

// errBrokerPathMoved ends a broker connection so it re-dials over the path the
// routing table now prefers.
var errBrokerPathMoved = errors.New("preferred network path changed")

// preferredSourceFn and brokerPathReachableFn are seams for tests.
var (
	preferredSourceFn     = preferredSource
	brokerPathReachableFn = brokerPathReachable
)

// preferredSource returns the local address the kernel would pick to reach
// remote now. Connecting a UDP socket selects a route without sending anything.
func preferredSource(remote *net.TCPAddr) (net.IP, error) {
	c, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: remote.IP, Port: remote.Port, Zone: remote.Zone})
	if err != nil {
		return nil, err
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).IP, nil
}

// brokerPathMoved reports whether a new connection to the stream's peer would
// use another source address, as when Ethernet returns under a stream opened
// over Wi-Fi: a connection keeps its source address even when its packets
// take the new route.
func brokerPathMoved(p *peer.Peer) (preferred net.IP, moved bool) {
	if p == nil {
		return nil, false
	}
	local, ok := p.LocalAddr.(*net.TCPAddr)
	if !ok {
		return nil, false
	}
	remote, ok := p.Addr.(*net.TCPAddr)
	if !ok {
		return nil, false
	}
	src, err := preferredSourceFn(remote)
	if err != nil {
		return nil, false
	}
	return src, !src.Equal(local.IP)
}

// brokerPathReachable reports whether a fresh connection, which follows the
// preferred route, gets through to the broker. A wired port may have a gateway
// but no internet, or a portal that answers TCP but not TLS.
func brokerPathReachable(ctx context.Context, target string, opts []grpc.DialOption) bool {
	conn, err := grpc.NewClient(target, opts...)
	if err != nil {
		return false
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(ctx, brokerPathProbeTimeout)
	defer cancel()
	conn.Connect()
	for state := conn.GetState(); state != connectivity.Ready; state = conn.GetState() {
		if state == connectivity.TransientFailure || !conn.WaitForStateChange(ctx, state) {
			return false
		}
	}
	return true
}

// brokerPathWatch decides when a moved path is worth a re-dial: the move must
// outlast one check, so a flapping link does not churn the connection, and
// re-dialling waits until no tunnel session would be cut.
type brokerPathWatch struct {
	stale   int
	wait    int
	penalty int
}

func (w *brokerPathWatch) shouldRedial(moved, busy bool) bool {
	if !moved {
		*w = brokerPathWatch{}
		return false
	}
	if w.wait > 0 {
		w.wait--
		return false
	}
	w.stale++
	return w.stale >= 2 && !busy
}

// brokerBusy reports whether a re-dial now would cut a tunnel session, or one
// about to start, or race the broker's record of the last heartbeat.
func brokerBusy(sessions int32, queued int, sinceHeartbeat time.Duration) bool {
	return sessions > 0 || queued > 0 || sinceHeartbeat < brokerHeartbeatSettle
}

// unreachable records that the preferred path failed to reach the broker and
// backs off before the next probe.
func (w *brokerPathWatch) unreachable() {
	w.stale = 0
	w.penalty = min(max(2*w.penalty, 1), brokerPathMaxWait)
	w.wait = w.penalty
}
