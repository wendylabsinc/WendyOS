package meshsession

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"net"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/udpmessage"
	"github.com/wendylabsinc/wendy/go/internal/shared/directstream"
)

// DirectUpperFlow is a fresh authenticated upper stream. Its opener elects one
// dial direction on both peers independently of their logical recovery roles.
// TLS must be the actual connection's state, never the old carrier's state.
// Opener context bounds setup only; cancellation after return must not close a
// successfully handed-off stream. The session then owns its entire lifetime.
type DirectUpperFlow struct {
	// Shared node ledger for promoted TLS datagram receive storage.
	UDPManager *udpmessage.Manager
	Stream     directstream.PromotedStream
	TLS        tls.ConnectionState
	Initiator  bool
}

// DirectPromotionRequest identifies the locally committed epoch and flow. The
// dispatcher uses it to match an incoming candidate before the binding proof.
// It is a routing key, not authority to reopen a backend.
type DirectPromotionRequest struct {
	SessionID  [32]byte
	Generation uint64
	FlowID     uint32
}
type DirectPromotionOpener func(context.Context, DirectPromotionRequest) (DirectUpperFlow, error)
type directPromotedResult struct {
	packets net.Conn
	token   uint64
	id      uint32
	stream  directstream.PromotedStream
	err     error
}

func (r directPromotedResult) close() {
	if r.packets != nil {
		r.packets.Close()
	} else if r.stream != nil {
		r.stream.Close()
	}
}

// Promote replaces a retained BLE attachment with an independently authenticated
// application control connection. Admission is supplied by trusted TLS/QUIC
// dispatch after peer/instance validation, never directly from wire fields.
// It lasts until upper control transport/session death; carrier underlay loss
// alone must not cancel ctx. Outstanding OPENs/results finish on upper control
// using their retained immutable identity and original backend operation.
func (s *DirectSession) Promote(ctx context.Context, a localmesh.DirectCarrierAdmission, open DirectPromotionOpener) error {
	if open == nil || a.Peer != s.cfg.Peer || a.IdentityDigest != s.cfg.IdentityDigest || a.Role != s.cfg.Role || a.Records == nil || !validPromotionTLS(a.TLS, a.Selected) {
		return errors.New("invalid authenticated promotion admission")
	}
	if directstream.SelectedTransport(a.Selected) == 0 {
		return errors.New("missing promoted transport agreement")
	}
	return s.attachCandidate(ctx, a, directstream.Promoted, open)
}

// The authenticated upper protocol must match the agreement retained from BLE.
// A TLS connection and a QUIC connection have different application ALPNs even
// though both expose a TLS 1.3 connection state and exporter.
func validPromotionTLS(state tls.ConnectionState, selected uint32) bool {
	if !state.HandshakeComplete || state.Version != tls.VersionTLS13 || len(state.PeerCertificates) == 0 || state.PeerCertificates[0] == nil {
		return false
	}
	protocol := localmesh.ApplicationProtocol(state.NegotiatedProtocol)
	switch directstream.SelectedTransport(selected) {
	case directstream.CapabilityVersion | directstream.CapabilityTLS:
		return protocol == TLSALPN
	case directstream.CapabilityVersion | directstream.CapabilityQUIC:
		return protocol == ALPN || protocol == multiplexALPN
	default:
		return false
	}
}

// A candidate that has not possibly committed owns no application bytes.
// Retry its setup under the original context deadline, retaining the flow claim
// and backend. Once COMMIT may have reached the peer, failure is terminal.
func (s *DirectSession) openPromotedFlow(ctx context.Context, c *directCandidate, b directstream.SessionBinding, a directstream.Attachment, id uint32, r directstream.OpenRequest) (DirectUpperFlow, error) {
	for {
		if err := ctx.Err(); err != nil {
			return DirectUpperFlow{}, err
		}
		upper, err := c.request.openFlow(ctx, DirectPromotionRequest{SessionID: a.SessionID, Generation: a.Generation, FlowID: id})
		if err == nil {
			state := upper.TLS
			if upper.Stream == nil || !validPromotionTLS(state, c.request.admission.Selected) || !bytes.Equal(state.PeerCertificates[0].Raw, c.request.admission.TLS.PeerCertificates[0].Raw) {
				err = errors.New("promoted flow TLS identity mismatch")
			}
		}
		var commitPossible bool
		if err == nil {
			var nonce [16]byte
			if _, err = rand.Read(nonce[:]); err == nil {
				commitPossible, err = directstream.BindPromotedFlow(ctx, upper.Stream, b, a, id, r, nonce, upper.Initiator, upper.TLS.ExportKeyingMaterial)
			}
		}
		if err == nil || commitPossible {
			return upper, err
		}
		if upper.Stream != nil {
			upper.Stream.Close()
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return DirectUpperFlow{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *DirectSession) startPromotedFlow(c *directCandidate, b directstream.SessionBinding, a directstream.Attachment, id uint32, r directstream.OpenRequest) context.CancelFunc {
	ctx, cancel := context.WithDeadline(c.ctx, c.deadline)
	s.workers.Add(1)
	go func() {
		defer s.workers.Done()
		defer cancel()
		upper, err := s.openPromotedFlow(ctx, c, b, a, id, r)
		var packets net.Conn
		if err == nil && r.Service == directstream.ServiceUDP {
			if directstream.SelectedTransport(c.request.admission.Selected) == directstream.CapabilityVersion|directstream.CapabilityTLS {
				packets, err = newAccountedDirectTLSDatagramConn(upper.Stream, r.DatagramSize, upper.UDPManager, s.cfg.Peer)
			} else if factory, ok := upper.Stream.(directDatagramFactory); ok {
				packets, err = factory.openDatagrams(upper.Stream, s.cfg.Peer, r.DatagramSize)
			} else {
				err = errors.New("missing bound QUIC datagram multiplexer")
			}
		}
		result := directPromotedResult{token: c.token, id: id, stream: upper.Stream, packets: packets, err: err}
		select {
		case s.promoted <- result:
		case <-s.ctx.Done():
			result.close()
		}
	}()
	return cancel
}
