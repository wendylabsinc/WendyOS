package appv1

import (
	"encoding/hex"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
)

// DecodePhysicalClockReply validates shape, scope and physical time only. It
// does not authenticate an endpoint, compare the request nonce or compensate
// for RPC transit/suspension. The commissioned client must do those checks.
// No source-clock sample or observation freshness is established by this reply.
func DecodePhysicalClockReply(p *PhysicalClockReply) (trustedtime.Interval, error) {
	if p == nil || !knownFields(p.ProtoReflect()) || len(p.Nonce) != ClockNonceSize || p.Physical == nil {
		return trustedtime.Interval{}, errPublication
	}
	scope := serviceorigin.Scope{Tenant: p.Tenant, Site: p.Site, Device: p.Device, App: p.App, Service: p.Service, ExecutionDomain: p.ExecutionDomain, DeploymentGeneration: p.DeploymentGeneration}
	if scope.Validate() != nil {
		return trustedtime.Interval{}, errPublication
	}
	incarnation, err := hex.DecodeString(p.ServerBootId)
	if err != nil || len(incarnation) != 16 || hex.EncodeToString(incarnation) != p.ServerBootId {
		return trustedtime.Interval{}, errPublication
	}
	x := p.Physical
	if x.EarliestUnixNs <= 0 || x.LatestUnixNs < x.EarliestUnixNs {
		return trustedtime.Interval{}, errPublication
	}
	w := trustedtime.Interval{Earliest: time.Unix(0, x.EarliestUnixNs), Latest: time.Unix(0, x.LatestUnixNs), BootID: x.BootId, Monotonic: time.Duration(x.MonotonicNs), Domain: "physical", Authenticated: true, Source: x.Source}
	if w.Validate() != nil {
		return trustedtime.Interval{}, errPublication
	}
	return w, nil
}
