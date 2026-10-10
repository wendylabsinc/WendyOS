package meshcatalog

import (
	"fmt"
	"slices"
	"time"

	"github.com/miekg/dns"
	"github.com/wendylabsinc/wendy/go/internal/agent/hostnetwork"
)

// LANProjector ties every unsigned LAN RRset to a live, per-app, per-port
// egress grant. Call Sync/Close from one app bridge event loop.
type LANProjector struct {
	netns, appIP, gateway, appBridge string
	tracked                          map[string]hostnetwork.LANServiceAccess
	visible                          map[string]bool
	blocked                          map[string]bool
	ensure                           func(hostnetwork.LANServiceAccess) error
	withdraw                         func(hostnetwork.LANServiceAccess) error
	withdrawRoute                    func(hostnetwork.LANServiceAccess) error
	withdrawDrop                     func(hostnetwork.LANServiceAccess) error
	onError                          func(error)
	reported                         map[string]string
}

func NewLANProjector(netns, appIP, gateway, appBridge string) *LANProjector {
	return &LANProjector{netns: netns, appIP: appIP, gateway: gateway, appBridge: appBridge,
		tracked: make(map[string]hostnetwork.LANServiceAccess), visible: make(map[string]bool), blocked: make(map[string]bool),
		reported: make(map[string]string),
		ensure:   hostnetwork.EnsureLANServiceAccess, withdraw: hostnetwork.WithdrawLANServiceAccess,
		withdrawRoute: hostnetwork.WithdrawLANRoute, withdrawDrop: hostnetwork.WithdrawLANDrop}
}

// SetErrorHandler reports grant failures once per distinct error until the
// grant succeeds. The bridge retries on every projection sweep.
func (p *LANProjector) SetErrorHandler(handler func(error)) { p.onError = handler }

func (p *LANProjector) report(key string, err error) {
	if err == nil || p.onError == nil || p.reported[key] == err.Error() {
		return
	}
	p.reported[key] = err.Error()
	p.onError(err)
}

func lanAccessKey(a hostnetwork.LANServiceAccess) string {
	return fmt.Sprintf("%s/%s/%s/%d", a.Destination, a.Interface, a.Protocol, a.Port)
}

func lanDestinationKey(a hostnetwork.LANServiceAccess) string { return a.Destination }

func lanInterfaceKey(a hostnetwork.LANServiceAccess) string {
	return a.Destination + "/" + a.Interface
}

func (p *LANProjector) access(service LANService) hostnetwork.LANServiceAccess {
	return hostnetwork.LANServiceAccess{NetNS: p.netns, AppIP: p.appIP, Gateway: p.gateway, AppBridge: p.appBridge,
		Destination: service.Address.String(), Interface: service.Interface.Name,
		Protocol: service.Protocol, Port: service.Port}
}

func (p *LANProjector) Sync(services []LANService) []dns.RR {
	if p == nil {
		return nil
	}
	desired := make(map[string]hostnetwork.LANServiceAccess, len(services))
	previousDestinations := make(map[string]hostnetwork.LANServiceAccess)
	previousInterfaces := make(map[string]hostnetwork.LANServiceAccess)
	for _, old := range p.tracked {
		previousDestinations[lanDestinationKey(old)] = old
		previousInterfaces[lanInterfaceKey(old)] = old
	}
	for _, service := range services {
		a := p.access(service)
		desired[lanAccessKey(a)] = a
		previousDestinations[lanDestinationKey(a)] = a
		previousInterfaces[lanInterfaceKey(a)] = a
	}
	for key, old := range p.tracked {
		if _, keep := desired[key]; keep {
			continue
		}
		p.visible[key] = false
		if err := p.withdraw(old); err != nil {
			p.report(key, fmt.Errorf("revoke physical-LAN service %s: %w", key, err))
			p.blocked[lanDestinationKey(old)] = true
			_ = p.withdrawRoute(old)
			continue
		}
		delete(p.tracked, key)
		delete(p.visible, key)
		delete(p.reported, key)
	}
	// A failed withdrawal can leave an old allow rule. Keep the entire
	// destination unrouted until its stale rules are removed on a later sweep.
	for destination := range p.blocked {
		stillPending := false
		for key, old := range p.tracked {
			if _, keep := desired[key]; !keep && lanDestinationKey(old) == destination {
				stillPending = true
				break
			}
		}
		if !stillPending {
			if a, known := previousDestinations[destination]; known && p.withdrawRoute(a) == nil {
				delete(p.blocked, destination)
			}
		}
	}
	for key, a := range desired {
		if p.blocked[lanDestinationKey(a)] {
			p.visible[key] = false
			continue
		}
		if p.visible[key] {
			continue
		}
		p.tracked[key] = a // retain partial setup for later cleanup/retry
		if err := p.ensure(a); err == nil {
			p.visible[key] = true
			delete(p.reported, key)
		} else {
			p.report(key, fmt.Errorf("grant physical-LAN service %s: %w", key, err))
			p.blocked[lanDestinationKey(a)] = true
			_ = p.withdrawRoute(a)
		}
	}
	for _, a := range p.tracked {
		if p.blocked[lanDestinationKey(a)] {
			p.visible[lanAccessKey(a)] = false
		}
	}
	activeDestinations := make(map[string]bool)
	activeInterfaces := make(map[string]bool)
	for key, a := range p.tracked {
		if _, keep := desired[key]; keep && p.visible[key] && !p.blocked[lanDestinationKey(a)] {
			activeDestinations[lanDestinationKey(a)] = true
			activeInterfaces[lanInterfaceKey(a)] = true
		}
	}
	for destination, a := range previousDestinations {
		if !activeDestinations[destination] {
			if err := p.withdrawRoute(a); err != nil {
				p.blocked[destination] = true
			}
		}
	}
	for iface, a := range previousInterfaces {
		if !activeInterfaces[iface] && !p.blocked[lanDestinationKey(a)] {
			_ = p.withdrawDrop(a)
		}
	}
	// Distinct service names can share one A RR. Emit each wire record once.
	records := make(map[string]dns.RR)
	for _, service := range services {
		a := p.access(service)
		if !p.visible[lanAccessKey(a)] || p.blocked[lanDestinationKey(a)] {
			continue
		}
		for _, rr := range service.Records {
			key := projectionKey(rr)
			if old := records[key]; old == nil || old.Header().Ttl < rr.Header().Ttl {
				records[key] = dns.Copy(rr)
			}
		}
	}
	keys := make([]string, 0, len(records))
	for key := range records {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	out := make([]dns.RR, 0, len(keys))
	for _, key := range keys {
		out = append(out, records[key])
	}
	return out
}

func (p *LANProjector) Close() {
	if p == nil {
		return
	}
	for range 3 {
		if len(p.tracked) == 0 {
			break
		}
		p.Sync(nil)
		time.Sleep(100 * time.Millisecond)
	}
}
