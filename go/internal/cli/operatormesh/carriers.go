package operatormesh

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/netmesh/carriers"
	"github.com/wendylabsinc/wendy/go/internal/shared/devicepin"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshoperatorlink"
)

type managedCarrier struct {
	peer meshid.Device
	cost uint16
}

func (s *Session) carrierConfig() carriers.Config {
	return carriers.Config{Credentials: s.Node.Credentials, Snapshot: s.Snapshot, Connect: s.connectCandidate, Sources: s.options.CarrierSources, Limits: carriers.Limits{AuthenticatedTTL: 15 * time.Minute}}
}

func (s *Session) runCarriers(ctx context.Context) error {
	return carriers.Run(ctx, s.carrierConfig())
}

func (s *Session) connectCandidate(ctx context.Context, candidate carriers.Candidate) (result error) {
	if os.Getenv("WENDY_MESH_DEBUG") == "1" {
		defer func() {
			fmt.Fprintf(os.Stderr, "wendy: mesh %s candidate %s hint=%x capabilities=%02x finished: %v\n", candidate.Transport, candidate.Address, candidate.Hint, candidate.Capabilities, result)
		}()
	}
	defer func() {
		if result != nil && ctx.Err() == nil && !errors.Is(result, carriers.ErrUnsupportedPeer) && s.options.ReportCarrierFailure != nil {
			s.options.ReportCarrierFailure(candidate.Transport, result)
		}
	}()
	var peer meshid.Device
	config := &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, NextProtos: []string{meshoperatorlink.RouterALPN}, Certificates: []tls.Certificate{s.Node.Credentials.Certificate}, InsecureSkipVerify: true, SessionTicketsDisabled: true,
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 {
				return errors.New("the discovered router did not provide a TLS certificate")
			}
			actual, err := meshid.DeviceFromCert(state.PeerCertificates[0])
			if err != nil {
				return err
			}
			if actual.Tenant() != s.Node.Credentials.Device.Tenant() {
				return errors.New("the discovered router belongs to a different organization")
			}
			if candidate.Device.IsValid() && actual != candidate.Device {
				return errors.New("the router certificate does not match its advertised device identity")
			}
			id := actual.UUID()
			if candidate.Transport == carriers.BLE && candidate.Hint != ([4]byte{}) && candidate.Hint != ([4]byte(id[:4])) {
				return errors.New("the Bluetooth router certificate does not match its advertised device identity")
			}
			cfg := s.routerConfig
			cfg.Router = actual
			pinned, err := meshoperatorlink.ClientTLSConfig(cfg, s.Node.Credentials.Certificate)
			if err != nil {
				return err
			}
			if err = pinned.VerifyConnection(state); err != nil {
				return err
			}
			pins, err := devicepin.Open(s.options.StateDirectory)
			if err != nil {
				return err
			}
			if err = pins.CheckAndUpdate(state.PeerCertificates[0], actual.Principal()); err != nil {
				return err
			}
			peer = actual
			return nil
		}}
	// Router binding independently verifies the TLS wire chain at both ends.
	// Known-issuer ALPN can omit cached CAs from that chain even though the
	// handshake's cache-aware verifier passed. Retain the full-chain role
	// until binding also consumes that exact issuer snapshot. Ordinary mesh
	// TLS continues to use known-issuer negotiation.
	config.NextProtos = []string{meshoperatorlink.RouterALPN}
	secure, err := dialOperatorTLS(ctx, candidate, config)
	if err != nil {
		// BLE's existing advertisement has no platform/role bit. A first
		// ordinary ALPN mismatch is remembered for this exact radio hint;
		// do not repeatedly spend TLS flights on a Lite-only carrier.
		if strings.Contains(err.Error(), "no application protocol") {
			return fmt.Errorf("%w: %s", carriers.ErrUnsupportedPeer, candidate.Address)
		}
		return err
	}
	cfg := s.routerConfig
	cfg.Router = peer
	cfg.IssuerCandidates = s.Node.Credentials.TLSIssuerCandidates
	link, err := meshoperatorlink.RouterClient(ctx, secure, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = link.Close(); link.Wait() }()
	peer = link.Peer()
	if os.Getenv("WENDY_MESH_DEBUG") == "1" {
		fmt.Fprintf(os.Stderr, "wendy: mesh %s authenticated %s\n", candidate.Transport, peer.Principal())
	}
	candidate.Authenticated(peer)
	if s.Snapshot().HasCheaperLink(peer, candidate.Cost) {
		return nil
	}
	s.linkMu.Lock()
	s.links[link] = managedCarrier{peer: peer, cost: candidate.Cost}
	s.choosePreferredLocked()
	s.linkMu.Unlock()
	defer func() {
		s.linkMu.Lock()
		delete(s.links, link)
		s.choosePreferredLocked()
		// A management stream belongs to this peer's physical carriers. An
		// indefinite router can encounter many peers over its lifetime; retain
		// no gRPC reconnect loop after the last usable carrier disappears.
		var retired *grpcclient.AgentConnection
		if !s.hasDirectPeerLocked(peer) {
			s.agentMu.Lock()
			retired = s.agentClients[peer]
			delete(s.agentClients, peer)
			s.agentMu.Unlock()
		}
		s.linkMu.Unlock()
		if retired != nil {
			_ = retired.Close()
		}
	}()
	return s.Node.AttachOperatorRouterLink(localmesh.WithCarrier(ctx, candidate.Transport), link, candidate.Cost)
}

func (s *Session) choosePreferredLocked() {
	defer func() {
		if s.linkChanged != nil {
			select {
			case s.linkChanged <- struct{}{}:
			default:
			}
		}
	}()
	var best *meshoperatorlink.RouterLink
	var metadata managedCarrier
	for link, value := range s.links {
		if !link.Live() {
			continue
		}
		if best == nil || value.cost < metadata.cost || (value.cost == metadata.cost && meshid.Compare(value.peer, metadata.peer) < 0) {
			best, metadata = link, value
		}
	}
	if best == nil {
		s.link = nil
		return
	}
	s.link = best.Link
	s.adjacentPeer = metadata.peer
}

func (s *Session) hasDirectPeer(peer meshid.Device) bool {
	s.linkMu.RLock()
	defer s.linkMu.RUnlock()
	return s.hasDirectPeerLocked(peer)
}

func (s *Session) hasDirectPeerLocked(peer meshid.Device) bool {
	for link, value := range s.links {
		if value.peer == peer && link.Live() {
			return true
		}
	}
	return false
}

func (s *Session) openAgentForPeer(ctx context.Context, peer meshid.Device) (net.Conn, error) {
	s.linkMu.RLock()
	var best *meshoperatorlink.RouterLink
	cost := ^uint16(0)
	for link, value := range s.links {
		if value.peer == peer && link.Live() && value.cost < cost {
			best, cost = link, value.cost
		}
	}
	s.linkMu.RUnlock()
	if best == nil {
		return nil, io.ErrClosedPipe
	}
	return best.OpenAgent(ctx)
}
