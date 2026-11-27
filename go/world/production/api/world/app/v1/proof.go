package appv1

import (
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/proofref"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
)

// DecodeGetRecordProof resolves the exact requested source within the socket's
// commissioned tenant/site. Device resolution does not grant read authority.
func DecodeGetRecordProof(p *GetRecordProofRequest, scope serviceorigin.Scope) (proofref.Reference, error) {
	if p == nil || !knownFields(p.ProtoReflect()) || scope.Validate() != nil {
		return proofref.Reference{}, errPublication
	}
	s, err := source(p.Source)
	if err != nil {
		return proofref.Reference{}, err
	}
	s, err = s.Resolve(scope.Device)
	if err != nil {
		return proofref.Reference{}, err
	}
	r := proofref.Reference{Key: world.Key{Tenant: scope.Tenant, Site: scope.Site, Entity: p.Entity, Type: s.ComponentType, Source: world.Source{Device: s.Device, App: s.App, Service: s.Service, Stream: s.Stream}}, Version: world.Version{Epoch: p.Epoch, Sequence: p.Sequence}, EnvelopeSHA256: p.EnvelopeSha256, RecordSHA256: p.RecordSha256}
	if r.Validate() != nil {
		return proofref.Reference{}, errPublication
	}
	return r, nil
}
