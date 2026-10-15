package meshsharing

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshcatalog"
)

// Node supplies only authenticated and signature-validated directory entries.
// SetUplink signs the local offer and originates/withdraws its Babel default.
type Node interface {
	Snapshot() localmesh.NodeSnapshot
	GatewayOffers() []meshcatalog.Record
	SetUplink(context.Context, string) error
	SetRoaming(context.Context, bool) error
}

// Policy owns its own NAT, forwarding, DNS proxy and resolver changes. The
// Linux implementation is localmesh.HostPolicy; tests use an in-memory policy.
type Policy interface {
	SetSharing(context.Context, string, string) error
	SetDNS(context.Context, string, string) error
	Close() error
}

// Probe never uses the mesh route to establish independent uplink health.
// Candidates must have a physical interface, a physical default and DNS from
// that interface. Uplink binds both DNS and HTTPS probes to the interface.
type Probe interface {
	Candidates(context.Context, Config) (map[string]string, error)
	Uplink(context.Context, string, string) bool
	Gateway(context.Context, netip.Addr, netip.Addr) bool
}

type Controller struct {
	mu                  sync.Mutex
	org, asset          int32
	node                Node
	policy              Policy
	probe               Probe
	uplinkHealth        map[string]*HealthTracker
	gatewayHealth       map[int32]*HealthTracker
	shareKey            string
	shareCleanupPending bool
	current             int32
	closed              bool
}

func NewWithDeps(org, asset int32, node Node, policy Policy, probe Probe) (*Controller, error) {
	if _, _, err := localmesh.Addresses(org, asset); err != nil {
		return nil, err
	}
	if node == nil || policy == nil || probe == nil {
		return nil, errors.New("mesh sharing needs node, policy and probe")
	}
	return &Controller{org: org, asset: asset, node: node, policy: policy, probe: probe,
		uplinkHealth: map[string]*HealthTracker{}, gatewayHealth: map[int32]*HealthTracker{}}, nil
}

// Step reconciles the complete sharing policy. It may block for bounded
// probes. The same Controller serializes all transitions and can be stepped
// after config changes as well as periodically. A disappeared uplink or signed
// offer bypasses health hysteresis and is withdrawn on this step.
func (c *Controller) Step(ctx context.Context, config Config) (Decision, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return Decision{}, errors.New("mesh sharing controller closed")
	}
	var uplinks []Uplink
	var gateway Gateway
	var discoveryErr error
	if config.Participate && config.Validate() == nil {
		candidates, err := c.probe.Candidates(ctx, config)
		if err != nil {
			discoveryErr = err
			candidates = nil
		}
		type result struct {
			iface, dns string
			ok         bool
		}
		results := make(chan result, len(candidates))
		for iface, dns := range candidates {
			go func() { results <- result{iface, dns, c.probe.Uplink(ctx, iface, dns)} }()
		}
		for range candidates {
			res := <-results
			health := c.uplinkHealth[res.iface]
			if health == nil {
				health = &HealthTracker{}
				c.uplinkHealth[res.iface] = health
			}
			uplinks = append(uplinks, Uplink{Interface: res.iface, DNS: res.dns, Healthy: health.Observe(res.ok), Independent: true})
		}
		for iface := range c.uplinkHealth {
			if _, ok := candidates[iface]; !ok {
				delete(c.uplinkHealth, iface)
			}
		}
		if config.Roam {
			gateway = SignedGateway(c.org, c.asset, c.node.Snapshot(), c.node.GatewayOffers(), time.Now())
			if gateway.Verified {
				health := c.gatewayHealth[gateway.Asset]
				if health == nil {
					health = &HealthTracker{}
					c.gatewayHealth[gateway.Asset] = health
				}
				local, _, _ := localmesh.Addresses(c.org, c.asset)
				gateway.Reachable = health.Observe(c.probe.Gateway(ctx, local, gateway.Address))
			}
		}
		for asset := range c.gatewayHealth {
			if asset != gateway.Asset {
				delete(c.gatewayHealth, asset)
			}
		}
	} else {
		clear(c.uplinkHealth)
		clear(c.gatewayHealth)
	}
	decision := Decide(config, c.asset, uplinks, gateway, time.Now())
	if err := c.reconcile(ctx, decision); err != nil {
		return decision, err
	}
	return decision, errors.Join(config.Validate(), discoveryErr)
}

func (c *Controller) reconcile(ctx context.Context, d Decision) error {
	cleanup, cancelCleanup := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancelCleanup()
	shareKey := ""
	if d.ShareInterface != "" {
		shareKey = d.ShareInterface + "|" + d.ShareDNS
	}
	if shareKey != c.shareKey {
		// Signed offer and default must be withdrawn before old NAT/DNS is
		// removed, even if later route or resolver cleanup fails.
		if err := c.node.SetUplink(ctx, ""); err != nil {
			return err
		}
	}
	// A locally healthy physical uplink takes precedence. Remove any borrowed
	// default before enabling advertisement, so borrowed Internet is never
	// re-exported even for one reconciliation interval.
	if d.Mode != "roaming" {
		if err := c.node.SetRoaming(ctx, false); err != nil {
			return err
		}
		if err := c.policy.SetDNS(ctx, "", ""); err != nil {
			return err
		}
	}
	if shareKey != c.shareKey {
		if err := c.policy.SetSharing(ctx, d.ShareInterface, d.ShareDNS); err != nil {
			c.shareKey = ""
			return errors.Join(err, c.withdrawSharing(cleanup))
		}
		if d.ShareInterface != "" {
			if err := c.node.SetUplink(ctx, d.ShareInterface); err != nil {
				c.shareKey = ""
				_ = c.node.SetUplink(cleanup, "")
				return errors.Join(err, c.withdrawSharing(cleanup))
			}
		}
		c.shareKey = shareKey
		c.shareCleanupPending = false
	} else if shareKey != "" {
		// SetSharing also detects a DNS proxy that exited after the previous
		// step. An unchanged interface must not keep a stale signed offer.
		if err := c.policy.SetSharing(ctx, d.ShareInterface, d.ShareDNS); err != nil {
			c.shareKey = ""
			return errors.Join(err, c.node.SetUplink(cleanup, ""),
				c.withdrawSharing(cleanup))
		}
	} else if c.shareCleanupPending {
		// A previous withdrawal exhausted its cleanup timeout. The policy
		// retained ownership of failed rules; retry on the next Step even
		// though the desired share key is already empty.
		if err := c.withdrawSharing(ctx); err != nil {
			return err
		}
	}
	if d.Mode != "roaming" {
		c.current = 0
		return nil
	}
	if c.shareKey != "" {
		return errors.New("refusing to borrow Internet while sharing an uplink")
	}
	if err := c.node.SetRoaming(ctx, true); err != nil {
		return err
	}
	link := fmt.Sprintf("wlmp%x", uint64(d.DNSLink))
	if err := c.policy.SetDNS(ctx, link, d.GatewayAddress.String()); err != nil {
		return errors.Join(err, c.node.SetRoaming(cleanup, false))
	}
	c.current = d.GatewayAsset
	return nil
}

func (c *Controller) withdrawSharing(ctx context.Context) error {
	err := c.policy.SetSharing(ctx, "", "")
	c.shareCleanupPending = err != nil
	return err
}

func (c *Controller) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// Withdrawing routes and signed capability precedes NAT/DNS removal.
	roamErr := c.node.SetRoaming(ctx, false)
	uplinkErr := c.node.SetUplink(ctx, "")
	return errors.Join(roamErr, uplinkErr, c.policy.Close())
}
