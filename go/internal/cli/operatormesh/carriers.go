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

	"github.com/wendylabsinc/wendy/go/internal/agent/bleprovider"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/cli/netmesh/carriers"
	"github.com/wendylabsinc/wendy/go/internal/shared/devicepin"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshoperatorlink"
)

type managedCarrier struct {
	peer meshid.Device
	cost uint16
}

func (s *Session) seedCandidate() carriers.Candidate {
	transport := s.options.Transport
	if transport == "" {
		transport = carriers.TCP
	}
	psm := s.options.BLEPSM
	cost := uint16(128)
	if transport == carriers.BLE {
		cost = bleprovider.LinkCost
		if psm == 0 {
			psm = bleprovider.DefaultPSM
		}
	}
	return carriers.Candidate{Device: s.routerConfig.Router, Address: s.options.RouterAddress, Transport: transport, PSM: psm, Cost: cost}
}

func (s *Session) runCarriers(ctx context.Context) error {
	sources := s.options.CarrierSources
	if sources != nil && s.options.Discover != nil {
		// Host fixtures can substitute this existing discovery callback without
		// starting any radio. Production nil Sources uses the native providers.
		sources = append(append([]carriers.Source{}, sources...), func(ctx context.Context, emit func([]carriers.Candidate)) error {
			for ctx.Err() == nil {
				options, err := s.options.Discover(ctx)
				if err != nil {
					return err
				}
				items := make([]carriers.Candidate, 0, len(options))
				for _, o := range options {
					peer, err := meshid.ParseDevice(s.Node.Credentials.Device.Tenant(), o.RouterUUID)
					if err != nil {
						continue
					}
					transport := o.Transport
					if transport == "" {
						transport = carriers.TCP
					}
					cost := uint16(128)
					if transport == carriers.BLE {
						cost = bleprovider.LinkCost
					}
					items = append(items, carriers.Candidate{Device: peer, Address: o.RouterAddress, Transport: transport, PSM: o.BLEPSM, Cost: cost})
				}
				emit(items)
				timer := time.NewTimer(time.Second)
				select {
				case <-ctx.Done():
					timer.Stop()
					return ctx.Err()
				case <-timer.C:
				}
			}
			return ctx.Err()
		})
	}
	var seeds []carriers.Candidate
	if s.options.RouterAddress != "" {
		seeds = []carriers.Candidate{s.seedCandidate()}
	}
	return carriers.Run(ctx, carriers.Config{Credentials: s.Node.Credentials, Snapshot: s.Snapshot, Connect: s.connectCandidate, Seeds: seeds, Sources: sources, Limits: carriers.Limits{AuthenticatedTTL: 15 * time.Minute}})
}

func (s *Session) connectCandidate(ctx context.Context, candidate carriers.Candidate) (result error) {
	if os.Getenv("WENDY_MESH_DEBUG") == "1" {
		defer func() {
			fmt.Fprintf(os.Stderr, "wendy: mesh %s candidate %s hint=%x capabilities=%02x finished: %v\n", candidate.Transport, candidate.Address, candidate.Hint, candidate.Capabilities, result)
		}()
	}
	var link *meshoperatorlink.RouterLink
	seed := s.seedCandidate()
	if candidate.Address == seed.Address && candidate.Transport == seed.Transport && candidate.PSM == seed.PSM {
		s.initialOnce.Do(func() { link = s.initialRouter })
	}
	if link == nil {
		var peer meshid.Device
		config := &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, NextProtos: []string{meshoperatorlink.RouterALPN}, Certificates: []tls.Certificate{s.Node.Credentials.Certificate}, InsecureSkipVerify: true, SessionTicketsDisabled: true,
			VerifyConnection: func(state tls.ConnectionState) error {
				if len(state.PeerCertificates) == 0 {
					return errors.New("mesh candidate has no peer certificate")
				}
				actual, err := meshid.DeviceFromCert(state.PeerCertificates[0])
				if err != nil {
					return err
				}
				if actual.Tenant() != s.Node.Credentials.Device.Tenant() {
					return errors.New("mesh candidate belongs to another tenant")
				}
				if candidate.Device.IsValid() && actual != candidate.Device {
					return errors.New("mesh candidate identity differs from discovery")
				}
				id := actual.UUID()
				if candidate.Transport == carriers.BLE && candidate.Hint != ([4]byte{}) && candidate.Hint != ([4]byte(id[:4])) {
					return errors.New("BLE candidate identity differs from the live hint")
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
		options := s.options
		options.Transport = candidate.Transport
		options.RouterAddress = candidate.Address
		options.BLEPSM = candidate.PSM
		secure, err := dialOperatorTLS(ctx, options, config)
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
		link, err = meshoperatorlink.RouterClient(ctx, secure, cfg)
		if err != nil {
			return err
		}
	}
	defer func() { _ = link.Close(); link.Wait() }()
	peer := link.Peer()
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
	defer func() { s.linkMu.Lock(); delete(s.links, link); s.choosePreferredLocked(); s.linkMu.Unlock() }()
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
