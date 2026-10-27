package meshsharing

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshcatalog"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"go.uber.org/zap"
)

const gatewayOfferRefreshInterval = meshcatalog.GatewayOfferLease / 2

// Node supplies only authenticated and signature-validated directory state.
// SetGatewayOffer signs this host's capability offer; an empty interface
// withdraws it. Babel routes carry the IPv6 tunnel's outer packets.
type Node interface {
	Snapshot() localmesh.NodeSnapshot
	GatewayOffers() []meshcatalog.Record
	SetGatewayOffer(context.Context, string) error
}

// Policy owns its own NAT, forwarding, DNS proxy and resolver changes. The
// Linux implementation owns an authenticated IPv6 tunnel to the selected donor;
// tests use an in-memory policy. SetGateway returns errGatewayPending while it
// is still negotiating with a donor; a later call for that donor collects it.
type Policy interface {
	SetGateway(context.Context, meshid.Device) error
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

// errGatewayPending is returned by Policy.SetGateway while a lease negotiation
// with that gateway is still running. The policy keeps it alive across steps;
// withdrawing the gateway would cancel it.
var errGatewayPending = errors.New("negotiating a lease with the selected gateway")

type dnsState struct {
	gateway        meshid.Device
	iface, address string
}

type Controller struct {
	mu                    sync.Mutex
	self                  meshid.Device
	node                  Node
	policy                Policy
	probe                 Probe
	uplinkHealth          map[string]*HealthTracker
	gatewayHealth         map[meshid.Device]*HealthTracker
	gatewayDevice         meshid.Device
	gatewayProbeAfter     meshid.Device
	shareKey              string
	gatewayOfferRefreshed time.Time
	shareCleanupPending   bool
	logger                *zap.Logger
	dns                   dnsState
	dnsUncertain          bool
	closed                bool
	closeComplete         bool
	offerWithdrawn        bool
}

func NewWithDeps(self meshid.Device, node Node, policy Policy, probe Probe) (*Controller, error) {
	if !self.IsValid() {
		return nil, errors.New("Wendy Net Internet sharing needs a device identity")
	}
	if node == nil || policy == nil || probe == nil {
		return nil, errors.New("Wendy Net Internet sharing needs node, policy and probe")
	}
	return &Controller{self: self, node: node, policy: policy, probe: probe, logger: zap.NewNop(),
		uplinkHealth: map[string]*HealthTracker{}, gatewayHealth: map[meshid.Device]*HealthTracker{}}, nil
}

// cleanupController retains a partially initialized policy without permitting
// reconciliation or attempting node/DNS mutations that were never initialized.
func cleanupController(policy Policy) *Controller {
	return &Controller{policy: policy, closed: true, offerWithdrawn: true}
}

// SetLogger enables structured DNS transition audit events. Repeated successful
// reconciliations do not emit events; failures and subsequent recovery do.
func (c *Controller) SetLogger(logger *zap.Logger) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if logger == nil {
		logger = zap.NewNop()
	}
	c.logger = logger
}

// setDNS runs with c.mu held, just like all other policy transitions.
func (c *Controller) setDNS(ctx context.Context, next dnsState) error {
	err := c.policy.SetDNS(ctx, next.iface, next.address)
	return c.auditDNS(next, err)
}

func (c *Controller) auditDNS(next dnsState, err error) error {
	if err == nil && next == c.dns && !c.dnsUncertain {
		return nil
	}
	action, subject := "apply", next
	if next.iface == "" {
		action, subject = "remove", c.dns
	}
	fields := []zap.Field{
		zap.String("action", action), zap.String("organization", c.self.Tenant().String()),
		zap.String("gateway_principal", subject.gateway.Principal()), zap.String("interface", subject.iface),
		zap.String("resolver", subject.address), zap.String("dns_scope", "~."),
		zap.String("previous_gateway_principal", c.dns.gateway.Principal()),
		zap.String("previous_interface", c.dns.iface),
		zap.String("previous_resolver", c.dns.address),
	}
	if err != nil {
		// SetDNS may have reverted the old resolver before a new apply failed.
		// Preserve the last successful target for audit, but flag state unknown.
		c.dnsUncertain = true
		c.logger.Warn("Wendy Net DNS transition", append(fields, zap.String("outcome", "failed"), zap.Error(err))...)
		return err
	}
	c.logger.Info("Wendy Net DNS transition", append(fields, zap.String("outcome", "applied"))...)
	c.dns, c.dnsUncertain = next, false
	return nil
}

// Step reconciles the complete sharing policy. It may block for bounded
// probes. The same Controller serializes all transitions and can be stepped
// after config changes as well as periodically. A disappeared uplink or signed
// offer bypasses health hysteresis and is withdrawn on this step.
func (c *Controller) Step(ctx context.Context, config Config) (Decision, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return Decision{}, errors.New("Wendy Net Internet sharing controller closed")
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
			gateway = c.reachableGateway(ctx)
		} else {
			clear(c.gatewayHealth)
			c.gatewayDevice = meshid.Device{}
		}
	} else {
		clear(c.uplinkHealth)
		clear(c.gatewayHealth)
		c.gatewayDevice = meshid.Device{}
	}
	decision := Decide(config, c.self, uplinks, gateway, time.Now())
	if err := c.reconcile(ctx, decision); errors.Is(err, errGatewayPending) {
		// Name the selected donor, but claim no Internet access before its
		// lease and tunnel exist.
		decision.Mode, decision.Reason = "offline", err.Error()
	} else if err != nil {
		return decision, err
	}
	return decision, errors.Join(config.Validate(), discoveryErr)
}

func (c *Controller) reachableGateway(ctx context.Context) Gateway {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	candidates := signedGateways(c.self, c.node.Snapshot(), c.node.GatewayOffers(), time.Now())
	present := make(map[meshid.Device]bool, len(candidates))
	for _, candidate := range candidates {
		present[candidate.Device] = true
	}
	for device := range c.gatewayHealth {
		if !present[device] {
			delete(c.gatewayHealth, device)
		}
	}
	type result struct {
		index int
		ok    bool
	}
	indices := c.gatewayProbeIndices(candidates)
	jobs, results := make(chan int, len(indices)), make(chan result, len(indices))
	for _, i := range indices {
		jobs <- i
	}
	close(jobs)
	// Probe alternates as well as the preferred donor, retaining each donor's
	// own hysteresis. A failed preferred probe must not starve another live
	// route. Limit concurrent subprocesses; every probe still binds the local
	// ULA and targets only a signed candidate's routed device ULA.
	for range min(4, len(indices)) {
		go func() {
			for i := range jobs {
				results <- result{i, c.probe.Gateway(ctx, c.self.MeshAddress(), candidates[i].Address)}
			}
		}()
	}
	for range indices {
		res := <-results
		candidate := &candidates[res.index]
		health := c.gatewayHealth[candidate.Device]
		if health == nil {
			health = &HealthTracker{}
			c.gatewayHealth[candidate.Device] = health
		}
		candidate.Reachable = health.Observe(res.ok)
	}
	// Completion order never influences preference. Recheck expiry after the
	// probes so a lease expiring during observation cannot be selected.
	now := time.Now()
	// Once a donor is carrying traffic, advertisement lease refreshes must not
	// move borrowers to another equally healthy donor. Keep the current donor
	// until it disappears, expires, or its debounced health turns bad. Candidate
	// expiry remains the deterministic preference when selecting initially or
	// after the current donor is no longer usable.
	for _, candidate := range candidates {
		if candidate.Device == c.gatewayDevice && candidate.Reachable && now.Before(candidate.Expires) {
			return candidate
		}
	}
	for _, candidate := range candidates {
		if candidate.Reachable && now.Before(candidate.Expires) {
			c.gatewayDevice = candidate.Device
			return candidate
		}
	}
	c.gatewayDevice = meshid.Device{}
	return Gateway{}
}

// Bound work even when many signed offers survive a partition. Always observe
// the current and preferred donors; rotate the remaining slots in stable UUID
// order so changing advertisement expiries cannot starve a distant alternate.
// Unprobed candidates retain hysteresis but are never selected on stale health.
func (c *Controller) gatewayProbeIndices(candidates []Gateway) []int {
	const limit = 8
	if len(candidates) <= limit {
		indices := make([]int, len(candidates))
		for i := range indices {
			indices[i] = i
		}
		return indices
	}
	indices := []int{0}
	for i, candidate := range candidates {
		if i != 0 && candidate.Device == c.gatewayDevice {
			indices = append(indices, i)
			break
		}
	}
	ordered := make([]int, len(candidates))
	for i := range ordered {
		ordered[i] = i
	}
	slices.SortFunc(ordered, func(a, b int) int { return meshid.Compare(candidates[a].Device, candidates[b].Device) })
	start := 0
	for i, index := range ordered {
		if meshid.Compare(candidates[index].Device, c.gatewayProbeAfter) > 0 {
			start = i
			break
		}
	}
	for offset := 0; offset < len(ordered) && len(indices) < limit; offset++ {
		index := ordered[(start+offset)%len(ordered)]
		c.gatewayProbeAfter = candidates[index].Device
		if !slices.Contains(indices, index) {
			indices = append(indices, index)
		}
	}
	return indices
}

func (c *Controller) reconcile(ctx context.Context, d Decision) error {
	cleanup, cancelCleanup := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancelCleanup()
	shareKey := ""
	if d.ShareInterface != "" {
		shareKey = d.ShareInterface + "|" + d.ShareDNS
	}
	if shareKey != c.shareKey {
		c.gatewayOfferRefreshed = time.Time{}
		// Signed offer and default must be withdrawn before old NAT/DNS is
		// removed, even if later route or resolver cleanup fails.
		if err := c.node.SetGatewayOffer(ctx, ""); err != nil {
			return err
		}
	}
	// A locally healthy physical uplink takes precedence. Remove any borrowed
	// default before enabling advertisement, so borrowed Internet is never
	// re-exported even for one reconciliation interval.
	if d.Mode != "roaming" {
		if err := errors.Join(c.policy.SetGateway(ctx, meshid.Device{}), c.setDNS(ctx, dnsState{})); err != nil {
			return err
		}
	}
	if shareKey != c.shareKey {
		if err := c.policy.SetSharing(ctx, d.ShareInterface, d.ShareDNS); err != nil {
			c.shareKey = ""
			return errors.Join(err, c.withdrawSharing(cleanup))
		}
		if d.ShareInterface != "" {
			if err := c.node.SetGatewayOffer(ctx, d.ShareInterface); err != nil {
				c.shareKey = ""
				c.gatewayOfferRefreshed = time.Time{}
				_ = c.node.SetGatewayOffer(cleanup, "")
				return errors.Join(err, c.withdrawSharing(cleanup))
			}
			c.gatewayOfferRefreshed = time.Now()
		}
		c.shareKey = shareKey
		c.shareCleanupPending = false
	} else if shareKey != "" {
		// SetSharing also detects a DNS proxy that exited after the previous
		// step. An unchanged interface must not keep a stale signed offer.
		if err := c.policy.SetSharing(ctx, d.ShareInterface, d.ShareDNS); err != nil {
			c.shareKey = ""
			c.gatewayOfferRefreshed = time.Time{}
			return errors.Join(err, c.node.SetGatewayOffer(cleanup, ""),
				c.withdrawSharing(cleanup))
		}
		if time.Since(c.gatewayOfferRefreshed) >= gatewayOfferRefreshInterval {
			// Refresh the signed capability well before its lease expires. If we
			// cannot renew it, stop serving borrowers and withdraw the stale offer
			// rather than leave an apparently shared uplink that will disappear
			// silently from peers' directories.
			if err := c.node.SetGatewayOffer(ctx, d.ShareInterface); err != nil {
				c.shareKey = ""
				c.gatewayOfferRefreshed = time.Time{}
				return errors.Join(err, c.node.SetGatewayOffer(cleanup, ""), c.withdrawSharing(cleanup))
			}
			c.gatewayOfferRefreshed = time.Now()
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
		return nil
	}
	if c.shareKey != "" {
		return errors.New("refusing to borrow Internet while sharing an uplink")
	}
	if err := c.policy.SetGateway(ctx, d.GatewayDevice); errors.Is(err, errGatewayPending) {
		// Withdrawing the gateway would cancel the negotiation. No borrowed
		// default exists yet, so only remove a previous donor's resolver.
		if err := c.setDNS(cleanup, dnsState{}); err != nil {
			return err
		}
		return errGatewayPending
	} else if err != nil {
		return errors.Join(err, c.withdrawGateway(cleanup))
	}
	// DNS reaches the donor's mesh ULA over Babel independently of the borrowed
	// Internet default. resolved binds queries to the current first-hop TUN.
	link := fmt.Sprintf("wlmp%x", uint64(d.DNSLink))
	if err := c.setDNS(ctx, dnsState{d.GatewayDevice, link, d.GatewayDevice.MeshAddress().String()}); err != nil {
		return errors.Join(err, c.withdrawGateway(cleanup))
	}
	return nil
}

func (c *Controller) withdrawGateway(ctx context.Context) error {
	// SetDNS may have partially changed resolved before failing. Attempt both
	// withdrawals even if one fails, using the caller's independent cleanup context.
	return errors.Join(c.setDNS(ctx, dnsState{}), c.policy.SetGateway(ctx, meshid.Device{}))
}

func (c *Controller) withdrawSharing(ctx context.Context) error {
	err := c.policy.SetSharing(ctx, "", "")
	c.shareCleanupPending = err != nil
	return err
}

func (c *Controller) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closeComplete {
		return nil
	}
	firstAttempt := !c.closed
	c.closed = true
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// Withdrawing routes and signed capability precedes NAT/DNS removal.
	var uplinkErr, roamErr error
	if !c.offerWithdrawn {
		uplinkErr = c.node.SetGatewayOffer(ctx, "")
		c.offerWithdrawn = uplinkErr == nil
	}
	if firstAttempt {
		roamErr = c.withdrawGateway(ctx)
	}
	// Policy.Close retains ownership of failed cleanup and can be retried.
	// Do not call its normal mutation methods after the first Close: the
	// Linux policy is already closed to new gateway/sharing transitions.
	closeErr := c.policy.Close()
	if c.dnsUncertain && closeErr == nil {
		// Close retries policy cleanup. A successful retry must also be visible
		// after a failed explicit resolver-removal event.
		_ = c.auditDNS(dnsState{}, nil)
	}
	err := errors.Join(uplinkErr, roamErr, closeErr)
	c.closeComplete = err == nil
	return err
}
