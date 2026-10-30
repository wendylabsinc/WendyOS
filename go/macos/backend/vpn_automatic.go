package backend

import (
	"context"
	"reflect"
	"sort"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/commands"
	"github.com/wendylabsinc/wendy/go/internal/cli/sessionbroker"
	"github.com/wendylabsinc/wendy/go/macos/vpn"
)

// The control listener lives for the entire VPN session. Data relays can be
// replaced without losing the extension's ability to recover after donor loss.
type automaticVPN struct {
	mu       sync.Mutex
	control  *vpn.Relay
	relay    *vpn.Relay
	probes   *vpnProbes
	internet *vpn.Internet
	cfg      vpn.Config
	routes   []vpn.Route
	gateway  string
	retry    map[string]time.Time
	discover func(context.Context) (sessionbroker.OperatorDirectory, error)
	open     func(context.Context, string) (*vpn.Internet, error)
}

func newAutomaticVPN() (*automaticVPN, vpn.Config, error) {
	a := &automaticVPN{discover: sessionbroker.ReadOperatorDirectoryAutomatic, open: openVPNInternet, retry: map[string]time.Time{}}
	control, cfg, err := vpn.NewRelay(nil, nil, nil)
	if err != nil {
		return nil, vpn.Config{}, err
	}
	a.control = control
	control.SetRefresh(a.refresh)
	cfg.Gateway = "auto"
	return a, cfg, nil
}

func (a *automaticVPN) refresh(ctx context.Context, online bool) (vpn.Config, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	scan, cancel := context.WithTimeout(ctx, 4*time.Second)
	directory, err := a.discover(scan)
	cancel()
	routes := a.routes
	var candidates []sessionbroker.OperatorTargetInfo
	if err == nil && directory.State == "ready" {
		routes = []vpn.Route{}
		for _, device := range directory.Devices {
			routes = append(routes, vpn.Route{Selector: device.Selector, Name: device.Name})
			if device.InternetSharing {
				candidates = append(candidates, device)
			}
		}
	}
	sort.Slice(routes, func(i, j int) bool { return routes[i].Selector < routes[j].Selector })
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Metric != candidates[j].Metric {
			return candidates[i].Metric < candidates[j].Metric
		}
		return candidates[i].Selector < candidates[j].Selector
	})
	healthy := a.internet != nil && !a.internet.Closed()
	// Retain a working uplink until normal internet returns. Metric jitter must
	// not interrupt existing connections by moving their public source address.
	change := a.relay == nil || !reflect.DeepEqual(routes, a.routes) || (online && a.internet != nil) || (!online && a.internet != nil && !healthy)
	if !online && a.internet == nil {
		for _, candidate := range candidates {
			if !time.Now().Before(a.retry[candidate.Selector]) {
				change = true
				break
			}
		}
	}
	if !change {
		return a.cfg, nil
	}
	previous := a.gateway
	if a.internet != nil && !healthy {
		a.retry[previous] = time.Now().Add(30 * time.Second)
	}
	a.closeData()
	a.gateway = ""
	if !online {
		// Prefer the previous healthy donor when only the device directory changed.
		if healthy {
			sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].Selector == previous && candidates[j].Selector != previous })
		}
		attempts := 0
		for _, candidate := range candidates {
			if attempts >= 2 || ctx.Err() != nil {
				break
			}
			if time.Now().Before(a.retry[candidate.Selector]) {
				continue
			}
			attempts++
			setup, cancel := context.WithTimeout(ctx, 15*time.Second)
			uplink, err := a.open(setup, candidate.Selector)
			cancel()
			if err != nil {
				a.retry[candidate.Selector] = time.Now().Add(30 * time.Second)
				continue
			}
			a.internet, a.gateway = uplink, candidate.Selector
			break
		}
	}
	probes := newVPNProbes(routes, commands.DesktopConnect)
	relay, cfg, err := vpn.NewRelayWithInternet(routes, desktopDial, probes.Probe, a.internet)
	if err != nil {
		probes.Close()
		if a.internet != nil {
			a.internet.Close()
			a.internet = nil
		}
		return vpn.Config{}, err
	}
	a.relay, a.probes, a.routes, a.cfg = relay, probes, append([]vpn.Route{}, routes...), cfg
	return cfg, nil
}

func (a *automaticVPN) closeData() {
	if a.probes != nil {
		a.probes.Close()
		a.probes = nil
	}
	if a.relay != nil {
		a.relay.Close()
		a.relay = nil
	}
	a.internet = nil
}
func (a *automaticVPN) Close() {
	a.control.Close() // Cancels in-progress discovery and waits for refresh.
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closeData()
}
