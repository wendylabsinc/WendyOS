package meshsession

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/directstream"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

type pendingPromotionFlow struct {
	state    tls.ConnectionState
	selected uint32
	result   chan DirectUpperFlow
	claimed  bool
}

func (m *DirectManager) promotionNotifyLocked() {
	if m.promotionChanged != nil {
		close(m.promotionChanged)
	}
	m.promotionChanged = make(chan struct{})
}

// awaitPromotedFlow registers only actor-requested flows from a committed
// transcript. An inbound preface must match all three fields before handoff;
// the actor then verifies the exporter/capability binding on that exact stream.
func (m *DirectManager) awaitPromotedFlow(ctx context.Context, request DirectPromotionRequest, state tls.ConnectionState, selected uint32) (DirectUpperFlow, error) {
	if request.SessionID == ([32]byte{}) || request.Generation == 0 || request.FlowID == 0 || !validPromotionTLS(state, selected) {
		return DirectUpperFlow{}, directstream.ErrBounds
	}
	p := &pendingPromotionFlow{state: state, selected: selected, result: make(chan DirectUpperFlow, 1)}
	m.promotionMu.Lock()
	if m.promotionPending == nil {
		m.promotionPending = make(map[DirectPromotionRequest]*pendingPromotionFlow)
	}
	if m.promotionPending[request] != nil {
		m.promotionMu.Unlock()
		return DirectUpperFlow{}, directstream.ErrBounds
	}
	m.promotionPending[request] = p
	m.promotionNotifyLocked()
	m.promotionMu.Unlock()
	defer func() {
		m.promotionMu.Lock()
		delete(m.promotionPending, request)
		m.promotionNotifyLocked()
		// Delivery and cancellation can win together. Release any delivered stream
		// not taken by the worker, while excluding a concurrent late delivery.
		var abandoned DirectUpperFlow
		select {
		case abandoned = <-p.result:
		default:
		}
		m.promotionMu.Unlock()
		if abandoned.Stream != nil {
			abandoned.Stream.Close()
		}
	}()
	select {
	case flow := <-p.result:
		return flow, nil
	case <-ctx.Done():
		return DirectUpperFlow{}, ctx.Err()
	case <-m.ctx.Done():
		return DirectUpperFlow{}, context.Cause(m.ctx)
	}
}

// deliverPromotedFlow may arrive before the peer's actor has registered its
// matching opener. The listener's bounded setup context limits this wait, and
// its connection/stream admission limit bounds such early candidates. No payload
// is consumed and no backend is opened while waiting.
func (m *DirectManager) deliverPromotedFlow(ctx context.Context, request DirectPromotionRequest, flow DirectUpperFlow) error {
	if request.SessionID == ([32]byte{}) || request.Generation == 0 || request.FlowID == 0 || flow.Stream == nil {
		return directstream.ErrBounds
	}
	for {
		m.promotionMu.Lock()
		if m.promotionChanged == nil {
			m.promotionChanged = make(chan struct{})
		}
		changed := m.promotionChanged
		if p := m.promotionPending[request]; p != nil {
			if p.claimed || !validPromotionTLS(flow.TLS, p.selected) || !bytes.Equal(flow.TLS.PeerCertificates[0].Raw, p.state.PeerCertificates[0].Raw) {
				m.promotionMu.Unlock()
				return errors.New("promoted candidate identity or ownership mismatch")
			}
			if ctx.Err() != nil || m.ctx.Err() != nil {
				m.promotionMu.Unlock()
				return net.ErrClosed
			}
			p.claimed = true
			p.result <- flow
			m.promotionMu.Unlock()
			return nil
		}
		m.promotionMu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		case <-m.ctx.Done():
			return context.Cause(m.ctx)
		}
	}
}

// SetDirectManager installs promotion dispatch before either application
// listener starts. The manager remains owned by the enclosing runtime/client.
func (s *Server) SetDirectManager(m *DirectManager) error {
	if m == nil || m.cfg.Credentials != s.credentials {
		return errors.New("promotion manager credentials differ from server")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.direct != nil || s.listener != nil || s.quicStarting || s.tlsListener != nil {
		return errors.New("promotion manager must be installed before serving")
	}
	s.direct = m
	return nil
}

func (m *DirectManager) upperAdmission(instance uuid.UUID, state tls.ConnectionState, records localmesh.DirectRecordCarrier) (localmesh.DirectCarrierAdmission, error) {
	var a localmesh.DirectCarrierAdmission
	if !state.HandshakeComplete || len(state.PeerCertificates) == 0 || state.PeerCertificates[0] == nil {
		return a, directstream.ErrBounds
	}
	peer, err := meshid.PeerFromCert(state.PeerCertificates[0])
	if err != nil {
		return a, err
	}
	if peer.IsOperator() {
		peer, err = peer.WithInstance(instance)
	} else if peer.UUID() != instance {
		err = errors.New("promotion instance does not match TLS principal")
	}
	if err != nil {
		return a, err
	}
	m.mu.Lock()
	e := m.peers[peer]
	if m.closed || m.ctx.Err() != nil {
		m.mu.Unlock()
		return a, net.ErrClosed
	}
	if e == nil {
		m.mu.Unlock()
		return a, ErrNoDirectSession
	}
	selected := e.selected
	m.mu.Unlock()
	digest, role, err := m.cfg.Credentials.DirectSessionIdentity(peer, state)
	if err != nil {
		return a, err
	}
	return localmesh.DirectCarrierAdmission{Self: m.cfg.Credentials.Device, Peer: peer, Role: role, IdentityDigest: digest, Selected: selected, TLS: state, Records: records}, nil
}

type inboundPromotedStream struct {
	directstream.PromotedStream
	prefix *bytes.Reader
	once   sync.Once
	done   chan struct{}
}

func (s *inboundPromotedStream) Read(p []byte) (int, error) {
	if s.prefix.Len() != 0 {
		return s.prefix.Read(p)
	}
	return s.PromotedStream.Read(p)
}
func (s *inboundPromotedStream) Close() error {
	err := s.PromotedStream.Close()
	s.once.Do(func() { close(s.done) })
	return err
}

// servePromotion consumes only recognized upper promotion prefaces. WDC1
// carries the sender installation UUID (16 bytes); it nominates an EXISTING
// authenticated epoch, whose full identity and capability are still checked by
// the recovery exchange. WDP1 is the existing 64-byte per-flow binding hello.
func (s *Server) servePromotion(ctx context.Context, conn directstream.PromotedStream, state tls.ConnectionState, first [6]byte) bool {
	magic := string(first[:4])
	if magic != "WDC1" && magic != "WDP1" {
		return false
	}
	if s.direct == nil {
		return true
	}
	if magic == "WDC1" {
		var instance uuid.UUID
		copy(instance[:2], first[4:])
		if _, err := io.ReadFull(conn, instance[2:]); err != nil {
			return true
		}
		if err := conn.SetDeadline(time.Time{}); err != nil {
			return true
		}
		records, err := NewDirectUpperRecords(ctx, conn)
		if err != nil {
			return true
		}
		defer records.Close()
		admission, err := s.direct.upperAdmission(instance, state, records)
		if err != nil {
			return true
		}
		_ = s.direct.HandlePromotion(ctx, admission, func(setup context.Context, request DirectPromotionRequest) (DirectUpperFlow, error) {
			return s.direct.awaitPromotedFlow(setup, request, state, admission.Selected)
		})
		return true
	}
	var hello [64]byte
	copy(hello[:6], first[:])
	if _, err := io.ReadFull(conn, hello[6:]); err != nil {
		return true
	}
	var request DirectPromotionRequest
	copy(request.SessionID[:], hello[4:36])
	request.Generation = binary.BigEndian.Uint64(hello[36:44])
	request.FlowID = binary.BigEndian.Uint32(hello[44:48])
	stream := &inboundPromotedStream{PromotedStream: conn, prefix: bytes.NewReader(hello[:]), done: make(chan struct{})}
	defer stream.Close()
	setup, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return true
	}
	if err := s.direct.deliverPromotedFlow(setup, request, DirectUpperFlow{Stream: stream, TLS: state, UDPManager: s.direct.udpManager}); err != nil {
		return true
	}
	// The actor now owns raw stream I/O. Keep the listener's connection slot and
	// security-lifetime watcher until the owner actually closes it.
	select {
	case <-stream.done:
	case <-ctx.Done():
	}
	return true
}
