package appv1

import (
	"bytes"
	"encoding/json"

	"github.com/wendylabsinc/wendy/go/world/production/serviceaccess"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"google.golang.org/protobuf/proto"
)

const MaxAuthoritySources = 16
const MaxAuthorityReplyBytes = 192 << 10

func DecodeSourceAuthorityRequest(p *GetSourceAuthorityRequest) ([]byte, []serviceaccess.Source, error) {
	if p == nil || !knownFields(p.ProtoReflect()) || len(p.Nonce) != ClockNonceSize || len(p.Sources) == 0 || len(p.Sources) > MaxAuthoritySources {
		return nil, nil, errPublication
	}
	sources := make([]serviceaccess.Source, len(p.Sources))
	for i, wire := range p.Sources {
		var err error
		sources[i], err = source(wire)
		if err != nil {
			return nil, nil, errPublication
		}
	}
	if serviceaccess.ValidateSources(sources, MaxAuthoritySources) != nil {
		return nil, nil, errPublication
	}
	return bytes.Clone(p.Nonce), sources, nil
}

// DecodeSourceAuthorityReply checks bounded shape only. It authenticates neither
// the socket nor a delegation, and proves no observation membership/freshness.
// A consumer must bind nonce, reader and server incarnation, compare every exact
// requested source and delegation, and independently verify original proofs.
func DecodeSourceAuthorityReply(p *SourceAuthorityReply) (trustedtime.Interval, error) {
	if p == nil || !knownFields(p.ProtoReflect()) || len(p.Authorities) == 0 || len(p.Authorities) > MaxAuthoritySources || proto.Size(p) > MaxAuthorityReplyBytes {
		return trustedtime.Interval{}, errPublication
	}
	w, err := DecodePhysicalClockReply(&PhysicalClockReply{Nonce: p.Nonce, Physical: p.Physical, Tenant: p.Tenant, Site: p.Site, Device: p.Device, App: p.App, Service: p.Service, ExecutionDomain: p.ExecutionDomain, DeploymentGeneration: p.DeploymentGeneration, ServerBootId: p.ServerBootId})
	if err != nil {
		return w, err
	}
	seen := map[serviceaccess.Source]bool{}
	for _, item := range p.Authorities {
		if item == nil || len(item.SignedDelegationJson) == 0 || len(item.SignedDelegationJson) > MaxAuthorityReplyBytes || !json.Valid(item.SignedDelegationJson) {
			return trustedtime.Interval{}, errPublication
		}
		s, err := source(item.Source)
		if err != nil {
			return trustedtime.Interval{}, errPublication
		}
		s, err = s.Resolve(p.Device)
		if err != nil || s.Device != p.Device || seen[s] {
			return trustedtime.Interval{}, errPublication
		}
		seen[s] = true
	}
	return w, nil
}
