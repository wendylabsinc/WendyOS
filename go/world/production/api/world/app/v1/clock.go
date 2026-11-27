package appv1

import (
	"bytes"
	"encoding/hex"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
)

const ClockNonceSize = 32
const MaxClockResponseAge = time.Second

func DecodeClock(p *ClockRequest) ([]byte, error) {
	if p == nil || !knownFields(p.ProtoReflect()) || len(p.Nonce) != ClockNonceSize {
		return nil, errPublication
	}
	return bytes.Clone(p.Nonce), nil
}

// DecodeClockReply validates the sampled interval; it does not establish trust
// in the endpoint, check the caller's nonce or compensate for RPC transit time.
// Consumers must do those checks before using it as a physical time provider.
func DecodeClockReply(p *ClockReply) (trustedtime.Interval, error) {
	if p == nil || !knownFields(p.ProtoReflect()) || len(p.Nonce) != ClockNonceSize || p.Physical == nil || p.SampleUnixNs <= 0 || len(p.SampleClockDomain) > 256 {
		return trustedtime.Interval{}, errPublication
	}
	scope := serviceorigin.Scope{Tenant: p.Tenant, Site: p.Site, Device: p.Device, App: p.App, Service: p.Service, ExecutionDomain: p.ExecutionDomain, DeploymentGeneration: p.DeploymentGeneration}
	if scope.Validate() != nil || p.ExecutionDomain == "physical" && p.SampleClockDomain != "utc" || p.ExecutionDomain == "simulation" && (!strings.HasPrefix(p.SampleClockDomain, "simulation:") || len(p.SampleClockDomain) == len("simulation:")) {
		return trustedtime.Interval{}, errPublication
	}
	incarnation, e := hex.DecodeString(p.ServerBootId)
	if e != nil || len(incarnation) != 16 || hex.EncodeToString(incarnation) != p.ServerBootId {
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
