package cloudrelay

import (
	"context"
	"errors"
	"net"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/peer"
)

var errPresenceSourceRemoved = errors.New("Cloud presence source address was removed")
var errPresencePathMoved = errors.New("Cloud presence preferred network path changed")

// Removing a borrowed mesh interface does not close TCP sockets bound to its
// lease address. An idle presence stream can otherwise stay registered until
// lease renewal, while every incoming Cloud command times out. Reconnect once
// that source is no longer assigned.
func presenceSourceRemoved(p *peer.Peer, addresses func() ([]net.Addr, error)) bool {
	if p == nil {
		return false
	}
	local, ok := p.LocalAddr.(*net.TCPAddr)
	if !ok || local.IP == nil || local.IP.IsUnspecified() {
		return false
	}
	if addresses == nil {
		addresses = net.InterfaceAddrs
	}
	assigned, err := addresses()
	if err != nil {
		return false
	}
	for _, addr := range assigned {
		switch a := addr.(type) {
		case *net.IPNet:
			if a.IP.Equal(local.IP) {
				return false
			}
		case *net.IPAddr:
			if a.IP.Equal(local.IP) {
				return false
			}
		}
	}
	return true
}

func presencePreferredSource(remote *net.TCPAddr) (net.IP, error) {
	c, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: remote.IP, Port: remote.Port, Zone: remote.Zone})
	if err != nil {
		return nil, err
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).IP, nil
}

func (a *Agent) presencePathReachable(ctx context.Context, endpoint string) bool {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// A fallback connection only proves the current alternative still works.
	// Probe the system route alone before moving presence back to it.
	dial := a.Verifier.dialRelay
	if a.Verifier.RelayDial == nil && a.Verifier.relayDial == nil {
		dial = func(endpoint string) (*grpc.ClientConn, error) { return dialBroker(endpoint, false) }
	}
	c, err := dial(endpoint)
	if err != nil {
		return false
	}
	defer c.Close()
	c.Connect()
	for state := c.GetState(); state != connectivity.Ready; state = c.GetState() {
		if state == connectivity.TransientFailure || !c.WaitForStateChange(ctx, state) {
			return false
		}
	}
	return true
}

// Run probes outside the presence receive loop so DNS/TLS delays do not hold
// up offers or lease expiry. A stable new source needs a successful authenticated
// broker transport before replacing a still-assigned source. Failed probes back
// off to five minutes; a removed source bypasses that delay.
func (a *Agent) watchPresencePath(ctx context.Context, p *peer.Peer, endpoint string) error {
	interval := a.sourceCheckInterval
	if interval <= 0 {
		interval = 10 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	preferred := a.preferredSource
	if preferred == nil {
		preferred = presencePreferredSource
	}
	reachable := a.pathReachable
	if reachable == nil {
		reachable = a.presencePathReachable
	}
	var candidate net.IP
	stable, wait, penalty := 0, 0, 0
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
		if presenceSourceRemoved(p, a.sourceAddresses) {
			return errPresenceSourceRemoved
		}
		if p == nil {
			continue
		}
		local, lok := p.LocalAddr.(*net.TCPAddr)
		remote, rok := p.Addr.(*net.TCPAddr)
		if !lok || !rok || local.IP == nil || local.IP.IsUnspecified() {
			continue
		}
		source, err := preferred(remote)
		if err != nil || source == nil || source.IsUnspecified() || source.Equal(local.IP) {
			candidate, stable, wait, penalty = nil, 0, 0, 0
			continue
		}
		if !source.Equal(candidate) {
			candidate, stable, wait, penalty = append(net.IP(nil), source...), 0, 0, 0
		}
		stable++
		if wait > 0 {
			wait--
			continue
		}
		if stable < 2 {
			continue
		}
		if reachable(ctx, endpoint) {
			// DNS/TLS can take seconds. The probe may even succeed through
			// the original route if it was restored in the meantime. Require
			// the same candidate after the probe before cancelling presence;
			// any replacement must stabilize anew. Active sessions run on the
			// agent's context and outlive the presence reconnect.
			if presenceSourceRemoved(p, a.sourceAddresses) {
				return errPresenceSourceRemoved
			}
			source, err := preferred(remote)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err == nil && source.Equal(candidate) {
				return errPresencePathMoved
			}
			candidate, stable, wait, penalty = nil, 0, 0, 0
			continue
		}
		penalty = min(max(2*penalty, 1), 30)
		wait = penalty
	}
}
