package services

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"syscall"

	"github.com/wendylabsinc/wendy/go/internal/agent/meshsession"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type applicationLANAddress struct {
	address        netip.AddrPort
	applicationTLS bool
}

// Keep discovery alive during connection attempts: a sibling address can arrive
// after the first exact-principal sighting, or while that address times out.
// Discovery and authenticated setup have separate budgets. Each socket connect
// is bounded independently; TLS and port admission retain their existing budget.
func dialApplicationLAN[T any](ctx context.Context, d *MeshDialer, peer meshid.Device, datagrams bool, dial func(context.Context, applicationLANAddress) (T, error)) (T, bool, error) {
	var zero T
	setup := ctx
	var stopSetup context.CancelFunc
	defer func() {
		if stopSetup != nil {
			stopSetup()
		}
	}()
	browse, cancel := context.WithTimeout(ctx, lanDiscoveryBudget)
	events := d.lanEvents(browse, discovery.StreamOptions{OnBackendError: func(err error) { d.logger.Debug("LAN peer discovery backend unavailable", zap.Error(err)) }})
	defer func() {
		cancel()
		for range events {
		}
	}()
	attempted := map[applicationLANAddress]bool{}
	var legacy []applicationLANAddress
	found, hasApplicationTLS := false, false
	var last error
	try := func(candidate applicationLANAddress) (T, bool, error) {
		if attempted[candidate] {
			return zero, false, nil
		}
		attempted[candidate] = true
		found = true
		if stopSetup == nil {
			setup, stopSetup = context.WithTimeout(ctx, meshsession.TLSAppSessionSetupTimeout)
		}
		result, err := dial(setup, candidate)
		if err == nil {
			return result, true, nil
		}
		last = err
		if !retryableLANAddressError(err) {
			return zero, true, err
		}
		if setup.Err() != nil {
			return zero, true, setup.Err()
		}
		return zero, false, nil
	}
	for event := range events {
		if event.Kind != discovery.LANFound && event.Kind != discovery.LANUpdated {
			continue
		}
		device := event.Device
		if device.PrincipalURI != peer.Principal() || !device.IsMTLS {
			continue
		}
		applicationTLS := device.AppTLSPort != 0
		if datagrams && !applicationTLS {
			continue
		}
		port := int(device.AppTLSPort)
		if !applicationTLS {
			port = device.Port
		}
		if port <= 0 || port > 65535 {
			continue
		}
		hasApplicationTLS = hasApplicationTLS || applicationTLS
		addresses := append([]string{device.IPAddress}, device.Addresses...)
		addresses = append(addresses, device.WiredAddresses...)
		for _, address := range addresses {
			ip, err := netip.ParseAddr(address)
			if err != nil || ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() {
				continue
			}
			candidate := applicationLANAddress{netip.AddrPortFrom(ip, uint16(port)), applicationTLS}
			// Readiness can add app-tls in a later exact-principal update. Do not
			// send legacy MeshDial until discovery confirms no dedicated endpoint.
			if !applicationTLS {
				legacy = append(legacy, candidate)
				continue
			}
			if result, done, err := try(candidate); done {
				return result, true, err
			}
		}
	}
	if !hasApplicationTLS {
		for _, candidate := range legacy {
			if result, done, err := try(candidate); done {
				return result, true, err
			}
		}
	}
	if setup.Err() != nil {
		return zero, found, setup.Err()
	}
	return zero, found, last
}

// Retry only failures that describe an unavailable network address. A TLS
// identity/certificate failure or live app-port refusal remains terminal.
func retryableLANAddressError(err error) bool {
	if errors.Is(err, syscall.ENETUNREACH) || errors.Is(err, syscall.EHOSTUNREACH) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ETIMEDOUT) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if status.Code(err) == codes.DeadlineExceeded {
		return true
	}
	var network *net.OpError
	return errors.As(err, &network) && network.Timeout()
}
func (a applicationLANAddress) String() string { return a.address.String() }
