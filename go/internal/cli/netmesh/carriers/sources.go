package carriers

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshdiscovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

const tcpService = "_wendy-net._tcp"
const quicService = "_wendy-net._udp"

var browseMDNS = discovery.BrowseMDNSServicesContinuous

func defaultSources(cfg Config) []Source {
	sources := []Source{lanSource(cfg.Credentials.Device, TCP), lanSource(cfg.Credentials.Device, QUIC)}
	if source := nativeBLESource(cfg.Credentials.Device); source != nil {
		sources = append(sources, source)
	}
	return sources
}

// Adapted from Joannis Orlandos's bounded operator LAN hints (PR #2466,
// 957fb0bf3 and 511f31f327a4). Use both current native service types and their
// declared ports; ordinary agent advertisements are not mesh endpoints.
func lanCandidates(services []discovery.MDNSService, self meshid.Device, transport string) []Candidate {
	out := make([]Candidate, 0, len(services))
	seen := make(map[string]bool)
	protocol := "tls-v1"
	if transport == QUIC {
		protocol = "quic-v1"
	}
	for _, s := range services {
		if s.Port < 1 || s.Port > 65535 || s.TXTRecords["version"] != "1" || s.TXTRecords["protocol"] != protocol || s.TXTRecords["mesh"] != meshdiscovery.DefaultHint(self.Tenant()).String() {
			continue
		}
		// Lite device carriers do not implement the primary operator-router
		// role. They remain reachable through an authenticated WendyOS hop.
		if role := s.TXTRecords["role"]; role != "router" || s.TXTRecords["platform"] != "WendyOS" {
			continue
		}
		peer, err := meshid.PeerFromPrincipal(s.TXTRecords["principal"])
		if err != nil || peer.Tenant() != self.Tenant() {
			continue
		}
		if peer.IsOperator() {
			if instance, err := uuid.Parse(s.TXTRecords["instance"]); err == nil && instance != uuid.Nil {
				peer, err = peer.WithInstance(instance)
				if err != nil {
					continue
				}
			}
		}
		if peer == self {
			continue
		}
		ip := net.ParseIP(s.IPAddress).To4()
		if ip == nil || !ip.IsGlobalUnicast() || ip.IsLinkLocalUnicast() {
			continue
		}
		c := Candidate{Device: peer, Address: net.JoinHostPort(ip.String(), strconv.Itoa(s.Port)), Transport: transport, Cost: 128, InterfaceName: s.InterfaceName}
		if !seen[c.key()] {
			seen[c.key()] = true
			out = append(out, c)
		}
		if len(out) >= 256 {
			break
		}
	}
	return out
}
func lanSource(self meshid.Device, transport string) Source {
	service := tcpService
	if transport == QUIC {
		service = quicService
	}
	return func(ctx context.Context, emit func([]Candidate)) error {
		for ctx.Err() == nil {
			query, cancel := context.WithTimeout(ctx, 3*time.Second)
			reports, err := browseMDNS(query, service)
			if err != nil {
				cancel()
				return fmt.Errorf("%s discovery: %w", transport, err)
			}
			candidates := make([]Candidate, 0, 256)
			seen := make(map[string]bool)
			collecting := true
			for collecting {
				select {
				case <-query.Done():
					collecting = false
				case report, ok := <-reports:
					if !ok {
						collecting = false
						break
					}
					if len(candidates) >= 256 {
						continue
					}
					for _, candidate := range lanCandidates([]discovery.MDNSService{report}, self, transport) {
						if !seen[candidate.key()] {
							seen[candidate.key()] = true
							candidates = append(candidates, candidate)
						}
					}
				}
			}
			cancel()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			emit(candidates)
			timer := time.NewTimer(10 * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
		return ctx.Err()
	}
}
