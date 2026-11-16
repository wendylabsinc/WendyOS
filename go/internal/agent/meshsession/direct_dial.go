package meshsession

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/directstream"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// DialPromotion opens the agreed upper transport to an existing retained peer.
// address is the destination's application endpoint for that transport, supplied
// by the authenticated route/endpoint owner. No fallback transport is inferred
// from handshake failure. The call lasts for the promoted session, not setup.
// ctx must survive individual carrier outages while the upper connection lives.
func (m *DirectManager) DialPromotion(ctx context.Context, peer meshid.Device, address netip.AddrPort) error {
	return m.dialPromotionTarget(ctx, peer, address, nil)
}

func (m *DirectManager) dialPromotionTarget(ctx context.Context, peer meshid.Device, address netip.AddrPort, expected *managedDirectSession) error {
	if !address.IsValid() || address.Port() == 0 {
		return directstream.ErrBounds
	}
	m.mu.Lock()
	target := m.peers[peer]
	if m.closed || m.ctx.Err() != nil {
		m.mu.Unlock()
		return net.ErrClosed
	}
	if target == nil || expected != nil && expected != target {
		m.mu.Unlock()
		return ErrNoDirectSession
	}
	if target.promotionDial {
		m.mu.Unlock()
		return directstream.ErrPromoted
	}
	target.promotionDial = true
	m.workers.Add(1)
	m.mu.Unlock()
	defer m.workers.Done()
	defer func() { m.mu.Lock(); target.promotionDial = false; m.mu.Unlock() }()
	run, cancel := context.WithCancel(ctx)
	stopManager := context.AfterFunc(m.ctx, cancel)
	stopEpoch := context.AfterFunc(target.session.ctx, cancel)
	defer func() { stopManager(); stopEpoch(); cancel() }()
	if err := m.checkPromotion(run, target, peer, address); err != nil {
		return err
	}
	switch directstream.SelectedTransport(target.selected) {
	case directstream.CapabilityVersion | directstream.CapabilityQUIC:
		return m.dialPromotionQUIC(run, target, peer, address)
	case directstream.CapabilityVersion | directstream.CapabilityTLS:
		return m.dialPromotionTLS(run, target, peer, address)
	default:
		return directstream.ErrBounds
	}
}

func (m *DirectManager) checkPromotion(ctx context.Context, target *managedDirectSession, peer meshid.Device, address netip.AddrPort) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	current := !m.closed && m.peers[peer] == target
	m.mu.Unlock()
	if !current || target.session.ctx.Err() != nil {
		return ErrNoDirectSession
	}
	status := target.session.Status()
	// A coordinator dial may supersede a participant dial before COMMIT.
	// The actor owns the final phase/ownership decision.
	if status.Promoted || status.Promoting && target.session.cfg.Role != 0 {
		return directstream.ErrPromoted
	}
	if m.cfg.PromotionSnapshot != nil {
		current, err := ResolvePromotion(m.cfg.PromotionSnapshot(), peer, status.DirectAvailable, status.BLECost, target.selected, time.Now())
		if err != nil {
			return err
		}
		if current != address {
			return ErrNoRoute
		}
	}
	if m.cfg.PromotionCheck != nil {
		return m.cfg.PromotionCheck(peer, address, target.selected, status)
	}
	return nil
}

func (m *DirectManager) runDialedPromotion(ctx context.Context, target *managedDirectSession, peer meshid.Device, address netip.AddrPort, state tls.ConnectionState, control directstream.PromotedStream, open DirectPromotionOpener) error {
	records, err := NewDirectUpperRecords(ctx, control)
	if err != nil {
		return err
	}
	defer records.Close()
	digest, role, err := m.cfg.Credentials.DirectSessionIdentity(peer, state)
	if err != nil {
		return err
	}
	admission := localmesh.DirectCarrierAdmission{PromotionInitiator: true, Self: m.cfg.Credentials.Device, Peer: peer, IdentityDigest: digest, Role: role, Selected: target.selected, TLS: state, Records: records}
	session, err := m.acquireAdmission(admission, false, target.session)
	if err != nil {
		return err
	}
	if session != target.session {
		return ErrNoDirectSession
	}
	if err := m.checkPromotion(ctx, target, peer, address); err != nil {
		return err
	}
	// Only setup is bounded here. Cancelling its timer after the preface must not
	// close the already-authenticated control connection or promoted raw streams.
	setup, cancel := context.WithTimeout(ctx, TLSAppSessionSetupTimeout)
	deadline, _ := setup.Deadline()
	stop := context.AfterFunc(setup, func() { control.Close() })
	err = control.SetWriteDeadline(deadline)
	if err == nil {
		var preface [20]byte
		copy(preface[:4], "WDC1")
		instance := m.cfg.Credentials.Device.UUID()
		copy(preface[4:], instance[:])
		var n int
		n, err = control.Write(preface[:])
		if err == nil && n != len(preface) {
			err = io.ErrShortWrite
		}
	}
	stopped := stop()
	if !stopped && setup.Err() != nil {
		err = setup.Err()
	}
	cancel()
	if err != nil {
		return err
	}
	if err := control.SetWriteDeadline(time.Time{}); err != nil {
		return err
	}
	return session.Promote(ctx, admission, open)
}

func (m *DirectManager) dialPromotionQUIC(ctx context.Context, target *managedDirectSession, peer meshid.Device, address netip.AddrPort) error {
	config, err := m.cfg.Credentials.PeerTLSWithTickets(peer, multiplexALPN, "app-quic")
	if err != nil {
		return err
	}
	setup, cancel := context.WithTimeout(ctx, TLSAppSessionSetupTimeout)
	defer cancel()
	stopWatch := watchPromotionCommit(setup, target.session, cancel)
	defer stopWatch()
	// Recheck after credential setup, immediately before starting the upper dial.
	if err := m.checkPromotion(setup, target, peer, address); err != nil {
		return err
	}
	options := quicConfig()
	if m.cfg.PromotionSnapshot != nil {
		view := m.cfg.PromotionSnapshot()
		options = quicConfigForRoute(view, peer, time.Now())
	}
	conn, cleanup, err := m.dialPromotionQUICSocket(setup, address, config, options)
	if err != nil {
		return err
	}
	defer cleanup()
	defer conn.CloseWithError(0, "retained promotion ended")
	if _, err := limitSessionLifetime(m.cfg.Credentials, conn); err != nil {
		return err
	}
	stream, err := conn.OpenStreamSync(setup)
	if err != nil {
		return err
	}
	mux := newUDPMultiplexer(conn)
	go mux.receive()
	control := &promotedQUICStream{Stream: stream, conn: conn}
	defer control.Close()
	stopWatch()
	cancel()
	return m.runDialedPromotion(ctx, target, peer, address, conn.ConnectionState().TLS, control, func(setup context.Context, request DirectPromotionRequest) (DirectUpperFlow, error) {
		stream, err := conn.OpenStreamSync(setup)
		if err != nil {
			return DirectUpperFlow{}, err
		}
		return DirectUpperFlow{Stream: &promotedQUICStream{Stream: stream, conn: conn, mux: mux, udpManager: m.udpManager}, TLS: conn.ConnectionState().TLS, Initiator: true}, nil
	})
}

func (m *DirectManager) dialPromotionTLS(ctx context.Context, target *managedDirectSession, peer meshid.Device, address netip.AddrPort) error {
	setup, cancel := context.WithCancel(ctx)
	stopWatch := watchPromotionCommit(setup, target.session, cancel)
	upper, err := m.dialPromotionTLSStream(setup, peer, address)
	stopWatch()
	cancel()
	if err != nil {
		return err
	}
	defer upper.Stream.Close()
	// Lite and OS TLS listeners have bounded handshake capacity. Serialize this
	// peer's candidates instead of launching one expensive handshake per flow.
	handshakes := make(chan struct{}, 1)
	return m.runDialedPromotion(ctx, target, peer, address, upper.TLS, upper.Stream, func(setup context.Context, request DirectPromotionRequest) (DirectUpperFlow, error) {
		select {
		case handshakes <- struct{}{}:
		case <-setup.Done():
			return DirectUpperFlow{}, setup.Err()
		}
		defer func() { <-handshakes }()
		return m.dialPromotionTLSStream(setup, peer, address)
	})
}

// Before this dial has authenticated, a committed upper connection necessarily
// belongs to another candidate. Stop spending handshake CPU and memory on the
// loser. Promoting alone is insufficient: crossed candidates may still need
// arbitration, and either candidate can fail before COMMIT. Join this watcher
// before handing the authenticated candidate to the actor, so it cannot cancel
// its own successful promotion or the subsequent per-flow handshakes.
func watchPromotionCommit(ctx context.Context, session *DirectSession, cancel context.CancelFunc) func() {
	done := make(chan struct{})
	stop := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(directPromotionPoll)
		defer ticker.Stop()
		for {
			if status := session.Status(); status.Promoted || status.Closed {
				cancel()
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			case <-ticker.C:
			}
		}
	}()
	return sync.OnceFunc(func() { close(stop); <-done })
}

func (m *DirectManager) dialPromotionTLSStream(ctx context.Context, peer meshid.Device, address netip.AddrPort) (DirectUpperFlow, error) {
	var out DirectUpperFlow
	config, err := m.cfg.Credentials.PeerTLS(peer)
	if err != nil {
		return out, err
	}
	m.cfg.Credentials.ConfigureTLSClient(config, TLSALPN)
	config.ClientSessionCache = nil
	config.SessionTicketsDisabled = true
	setup, cancel := context.WithTimeout(ctx, TLSAppSessionSetupTimeout)
	defer cancel()
	dialer := net.Dialer{}
	if meshid.MeshPrefix.Contains(address.Addr()) {
		dialer.LocalAddr = &net.TCPAddr{IP: net.IP(m.cfg.Credentials.Device.MeshAddress().AsSlice())}
	}
	dial := m.cfg.PromotionDialContext
	if dial == nil {
		dial = dialer.DialContext
	}
	raw, err := dial(setup, "tcp", address.String())
	if err != nil {
		return out, err
	}
	secure := tls.Client(raw, config)
	stop := context.AfterFunc(setup, func() { raw.Close() })
	ok := false
	defer func() {
		if !stop() && setup.Err() != nil {
			raw.Close()
		}
		if !ok {
			raw.Close()
		}
	}()
	if err := secure.HandshakeContext(setup); err != nil {
		return out, fmt.Errorf("promoted TLS handshake: %w", err)
	}
	state := secure.ConnectionState()
	if !validPromotionTLS(state, directstream.CapabilityVersion|directstream.CapabilityTLS) || state.DidResume {
		return out, errors.New("invalid promoted TLS state")
	}
	lifetime, err := newSessionLifetime(m.cfg.Credentials, state)
	if err != nil {
		return out, err
	}
	if err := setup.Err(); err != nil {
		return out, err
	}
	conn := &tlsAppConn{tlsRecordConn: &tlsRecordConn{Conn: secure}, peer: peer, lifetime: lifetime}
	conn.mu.Lock()
	conn.expiry = time.AfterFunc(max(lifetime.delay(lifetime.deadline), 0), conn.checkExpiry)
	conn.mu.Unlock()
	ok = true
	return DirectUpperFlow{Stream: conn, TLS: state, Initiator: true, UDPManager: m.udpManager}, nil
}
