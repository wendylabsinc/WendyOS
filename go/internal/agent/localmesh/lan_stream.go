//go:build linux || darwin

package localmesh

import (
	"context"
	"crypto/tls"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/mdns"
	"github.com/miekg/dns"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

const TLSLANPort = 43024
const tlsLANService = "_wendy-net._tcp"
const tlsLANProtocol = "tls-v1"

type lanMDNSZone interface{ Records(dns.Question) []dns.RR }
type lanZones []lanMDNSZone

func (zones lanZones) Records(q dns.Question) []dns.RR {
	var out []dns.RR
	for _, zone := range zones {
		out = append(out, zone.Records(q)...)
	}
	return out
}
func tlsLANMDNSService(c *Credentials, selected lanInterface, role string, timePort ...uint16) (*mdns.MDNSService, error) {
	info := []string{"platform=WendyOS", "version=1", "protocol=" + tlsLANProtocol, "principal=" + c.Device.Principal(), "mesh=" + lanMeshHash(c.Device.Tenant()), "role=" + role}
	if len(timePort) > 0 && timePort[0] != 0 {
		info = append(info, "time=wnt1:"+strconv.Itoa(int(timePort[0])))
	}
	return mdns.NewMDNSService("wendy-net-tls-"+c.Device.UUID().String(), tlsLANService, "local.",
		"wendy-net-tls-"+c.Device.UUID().String()+"-"+strconv.Itoa(selected.iface.Index)+".local.", TLSLANPort, []net.IP{selected.ip},
		info)
}
func parseTLSLANEntry(entry *mdns.ServiceEntry, self meshid.Device) (meshid.Device, net.IP, string, bool) {
	if entry == nil || entry.Port != TLSLANPort {
		return meshid.Device{}, nil, "", false
	}
	// Reuse the exact tenant/principal, address and duplicate-TXT validation.
	copy := *entry
	copy.Port = LANPort
	copy.InfoFields = nil
	role, version, protocol := "", "", ""
	for _, field := range entry.InfoFields {
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			return meshid.Device{}, nil, "", false
		}
		switch strings.ToLower(key) {
		case "protocol":
			protocol = value
			copy.InfoFields = append(copy.InfoFields, key+"="+lanProtocol)
		case "role":
			if role != "" {
				return meshid.Device{}, nil, "", false
			}
			role = value
			copy.InfoFields = append(copy.InfoFields, field)
		case "version":
			version = value
			copy.InfoFields = append(copy.InfoFields, field)
		default:
			copy.InfoFields = append(copy.InfoFields, field)
		}
	}
	if version != "1" || protocol != tlsLANProtocol || (role != "router" && role != "embedded-router") {
		return meshid.Device{}, nil, "", false
	}
	device, ip, ok := parseLANEntry(&copy, self)
	return device, ip, role, ok
}

// Ordinary WendyOS peers retain QUIC. Native TLS discovery gives embedded
// peers both dial directions without a static device or address allow-list.
func scanTLSLAN(ctx context.Context, cfg LANConfig, selected lanInterface, claims *lanClaims) {
	scanTLSLANWithQuery(ctx, cfg, selected, claims, queryLANService)
}

func scanTLSLANWithQuery(ctx context.Context, cfg LANConfig, selected lanInterface, claims *lanClaims, query func(context.Context, lanInterface, string) ([]*mdns.ServiceEntry, error)) {
	var workers sync.WaitGroup
	defer workers.Wait()
	for ctx.Err() == nil {
		entries, _ := query(ctx, selected, tlsLANService)
		byKey := map[string]*mdns.ServiceEntry{}
		var keys []string
		for _, entry := range entries {
			peer, ip, role, ok := parseTLSLANEntry(entry, cfg.Credentials.Device)
			if !ok || !shouldDialTLSLAN(entry, role) || selected.net == nil || !selected.net.Contains(ip) {
				continue
			}
			key := "tls/" + peer.Principal()
			if _, exists := byKey[key]; !exists {
				keys = append(keys, key)
			}
			byKey[key] = entry
		}
		for _, key := range claims.candidates.Order(keys, time.Now()) {
			peer, ip, role, ok := parseTLSLANEntry(byKey[key], cfg.Credentials.Device)
			if !ok || !shouldDialTLSLAN(byKey[key], role) || !cfg.allow(peer, selected.cost) || cfg.Node.HasCheaperLink(peer, selected.cost) {
				continue
			}
			lease := claims.policy.ReserveGroup(peer.Principal()+"/"+strconv.Itoa(int(selected.cost)), peer, true, strconv.Itoa(int(selected.cost)))
			if lease == nil {
				continue
			}
			lease.ObserveReady(func() { claims.candidates.Succeeded(key) })
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer lease.Release()
				defer func() {
					if ctx.Err() == nil && !lease.Retired() {
						claims.candidates.Failed(key, time.Now())
					}
				}()
				config, err := cfg.Credentials.PeerTLS(peer)
				if err != nil {
					return
				}
				config.ClientSessionCache = nil
				config.SessionTicketsDisabled = true
				cfg.Credentials.ConfigureTLSClient(config, StreamALPN)
				attempt, cancel := context.WithTimeout(ctx, 8*time.Second)
				raw, err := (&net.Dialer{LocalAddr: &net.TCPAddr{IP: selected.ip}, Control: bindToInterface(selected.iface.Name)}).DialContext(attempt, "tcp4", net.JoinHostPort(ip.String(), strconv.Itoa(TLSLANPort)))
				if err != nil {
					cancel()
					return
				}
				defer raw.Close()
				stop := context.AfterFunc(ctx, func() { _ = raw.Close() })
				defer stop()
				secure := tls.Client(raw, config)
				err = secure.HandshakeContext(attempt)
				cancel()
				if err != nil {
					return
				}
				linkCtx := WithCarrier(WithConnectionLease(ctx, lease), selected.transport)
				_ = cfg.Node.AttachStream(linkCtx, peer, secure, selected.cost)
			}()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}
}
