package appv1

import (
	"bytes"
	"encoding/hex"
	"github.com/wendylabsinc/wendy/go/world/production/serviceaccess"
	"github.com/wendylabsinc/wendy/go/world/production/serviceevidence"
)

func source(p *SourceSelector) (serviceaccess.Source, error) {
	if p == nil || !knownFields(p.ProtoReflect()) {
		return serviceaccess.Source{}, errPublication
	}
	s := serviceaccess.Source{App: p.App, Service: p.Service, ComponentType: p.ComponentType, Stream: p.Stream, Device: p.Device}
	return s, s.Validate()
}
func DecodeQuery(p *QueryRequest) (serviceaccess.Query, error) {
	if p == nil || !knownFields(p.ProtoReflect()) {
		return serviceaccess.Query{}, errPublication
	}
	q := serviceaccess.Query{Limit: p.Limit, After: p.After}
	for _, selector := range p.Sources {
		s, e := source(selector)
		if e != nil {
			return q, e
		}
		q.Sources = append(q.Sources, s)
	}
	return q, q.Validate()
}
func digest(s string) bool {
	b, e := hex.DecodeString(s)
	return e == nil && len(b) == 32 && hex.EncodeToString(b) == s
}

func DecodePutEvidence(p *PutEvidenceRequest) (serviceevidence.PutRequest, error) {
	if p == nil || !knownFields(p.ProtoReflect()) || !digest(p.Sha256) || p.Size < 1 || p.Size > 8<<20 || p.Offset < 0 || p.Offset > p.Size || len(p.Data) > serviceevidence.MaxChunk || int64(len(p.Data)) > p.Size-p.Offset || p.RetainUntilUnixMs < 0 {
		return serviceevidence.PutRequest{}, errPublication
	}
	// Reuse the exact-selector syntax without accepting caller identity.
	if (serviceaccess.Source{App: "validation", Service: "validation", ComponentType: p.ComponentType, Stream: p.Stream}).Validate() != nil {
		return serviceevidence.PutRequest{}, errPublication
	}
	switch p.ContentType {
	case "image/png", "image/jpeg", "audio/wav":
	default:
		return serviceevidence.PutRequest{}, errPublication
	}
	return serviceevidence.PutRequest{SHA256: p.Sha256, Size: p.Size, Offset: p.Offset, Data: bytes.Clone(p.Data), ContentType: p.ContentType, RetainUntilUnixMS: p.RetainUntilUnixMs}, nil
}
func DecodeGetEvidence(p *GetEvidenceRequest) (serviceaccess.Source, serviceevidence.GetRequest, error) {
	if p == nil || !knownFields(p.ProtoReflect()) || !digest(p.Sha256) || p.Offset < 0 || p.Length < 1 || p.Length > serviceevidence.MaxChunk {
		return serviceaccess.Source{}, serviceevidence.GetRequest{}, errPublication
	}
	s, e := source(p.Source)
	return s, serviceevidence.GetRequest{SHA256: p.Sha256, Offset: p.Offset, Length: int(p.Length)}, e
}
