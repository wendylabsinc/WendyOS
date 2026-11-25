package livepeople

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"reflect"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/fusioncapture"
	"github.com/wendylabsinc/wendy/go/world/production/jws"
	"github.com/wendylabsinc/wendy/go/world/production/proofref"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceaccess"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"google.golang.org/grpc"
)

func digest(raw []byte) string { v := sha256.Sum256(raw); return hex.EncodeToString(v[:]) }
func selector(s serviceaccess.Source) *appv1.SourceSelector {
	return &appv1.SourceSelector{App: s.App, Service: s.Service, Device: s.Device, ComponentType: s.ComponentType, Stream: s.Stream}
}

type proof struct {
	raw       []byte
	signed    serviceorigin.SignedRecord
	record    serviceorigin.Record
	capture   sensorrecord.Capture
	reference proofref.Reference
}
type binding struct {
	scope                serviceorigin.Scope
	epoch, sequence      uint64
	delegation, envelope string
}

// Only the initial read-only preparation uses this marker. BatchVerifier keeps
// authorization failures opaque; this preserves the specific successful-query
// absence locally, without turning it into a successful verification.
type preparationKey struct{}
type preparationState struct{ missing map[string]bool }

var errExpiredCamera = errors.New("original camera capture expired")

func decodeEnvelope(raw []byte) (serviceorigin.SignedRecord, serviceorigin.Record, error) {
	var signed serviceorigin.SignedRecord
	var record serviceorigin.Record
	if len(raw) == 0 || len(raw) > fusioncapture.MaxProofBytes || authority.DecodeStrictLimit(raw, &signed, fusioncapture.MaxProofBytes) != nil || jws.DecodeStrict(signed.Record, &record) != nil {
		return signed, record, ErrSource
	}
	return signed, record, nil
}
func (s *Session) parse(raw []byte) (proof, error) {
	signed, r, err := decodeEnvelope(raw)
	if err != nil {
		return proof{}, err
	}
	source, ok := s.sources[r.Publication.Stream]
	if !ok || r.Version != serviceorigin.Version || r.AuthorityDomain != serviceorigin.AuthorityDomain || r.Scope.Validate() != nil || r.Scope.Tenant != s.cfg.Tenant || r.Scope.Site != s.cfg.Site || r.Scope.Device != source.Device || r.Scope.App != source.App || r.Scope.Service != source.Service || r.Scope.ExecutionDomain != "simulation" || len(r.Scope.Publish) != 1 || r.Scope.Publish[0] != (serviceorigin.PublishScope{ComponentType: sensorrecord.ComponentType, Stream: source.SensorID}) || r.DelegationSHA256 != digest(signed.Delegation.Claims) || r.Epoch == 0 || r.Sequence == 0 || r.Publication.Frame != s.cfg.Frame {
		return proof{}, ErrSource
	}
	c, err := sensorrecord.ValidatePublication(r.Publication)
	if err != nil {
		return proof{}, ErrSource
	}
	sample, err := c.NormalizedSample()
	if err != nil || sample.Kind != "camera" || sample.SensorID != source.SensorID || c.SceneBootID != s.cfg.SceneBootID || c.SimulationAnchorUnixNS != s.cfg.SimulationAnchorUnixNS || sample.RunID != s.cfg.RunID || sample.SiteID != s.cfg.Site || sample.Frame != s.cfg.Frame || sample.PackageSHA256 != s.cfg.PackageSHA256 || sample.ScenarioVersion != s.cfg.ScenarioVersion || sample.Seed != s.cfg.Seed {
		return proof{}, ErrSource
	}
	ref := proofref.Reference{Key: world.Key{Tenant: r.Scope.Tenant, Site: r.Scope.Site, Entity: r.Publication.Entity, Type: r.Publication.ComponentType, Source: world.Source{Device: r.Scope.Device, App: r.Scope.App, Service: r.Scope.Service, Stream: r.Publication.Stream}}, Version: world.Version{Epoch: r.Epoch, Sequence: r.Sequence}, EnvelopeSHA256: digest(raw), RecordSHA256: digest(signed.Record)}
	if ref.Validate() != nil {
		return proof{}, ErrSource
	}
	return proof{bytes.Clone(raw), signed, r, c, ref}, nil
}

// Query is an authenticated current-source witness. It never substitutes for
// verifying the original captured envelope or renews its sample time.
func (s *Session) query(ctx context.Context) ([]proof, error) {
	q := &appv1.QueryRequest{Limit: uint32(len(s.cfg.Sources))}
	for _, source := range s.cfg.Sources {
		q.Sources = append(q.Sources, selector(source.selector()))
	}
	rpc, cancel := context.WithTimeout(ctx, time.Duration(s.cfg.RequestTimeoutMS)*time.Millisecond)
	defer cancel()
	reply, err := s.api.Query(rpc, q, grpc.MaxCallRecvMsgSize(serviceaccess.MaxQueryBytes+1024), grpc.MaxRetryRPCBufferSize(0))
	if err != nil || reply == nil || len(reply.ProtoReflect().GetUnknown()) != 0 || reply.Next != "" || len(reply.SignedRecordJson) > len(s.sources) {
		return nil, ErrSource
	}
	var out []proof
	total := 0
	seen := map[string]bool{}
	for _, raw := range reply.SignedRecordJson {
		total += len(raw)
		if total > serviceaccess.MaxQueryBytes {
			return nil, ErrSource
		}
		p, err := s.parse(raw)
		if err != nil {
			return nil, err
		}
		id := p.record.Publication.Stream
		if seen[id] {
			return nil, ErrSource
		}
		seen[id] = true
		if old, exists := s.bindings[id]; exists && (!reflect.DeepEqual(old.scope, p.record.Scope) || old.epoch != p.record.Epoch || old.delegation != p.record.DelegationSHA256 || p.record.Sequence < old.sequence || p.record.Sequence == old.sequence && old.envelope != p.reference.EnvelopeSHA256) {
			return nil, ErrSource
		}
		s.bindings[id] = binding{p.record.Scope, p.record.Epoch, p.record.Sequence, p.record.DelegationSHA256, p.reference.EnvelopeSHA256}
		out = append(out, p)
	}
	return out, nil
}

func (s *Session) authorizeBatch(ctx context.Context, batch serviceorigin.BatchAuthorization) error {
	preparation, _ := ctx.Value(preparationKey{}).(*preparationState)
	if preparation != nil {
		preparation.missing = nil
	}
	if len(batch.Claims) < 1 || len(batch.Claims) > fusioncapture.MaxBatchRecords || len(batch.Devices) != len(batch.Claims) {
		return ErrSource
	}
	claims := map[string]serviceorigin.DelegationClaims{}
	for i, c := range batch.Claims {
		if len(c.Scope.Publish) != 1 {
			return ErrSource
		}
		pair := c.Scope.Publish[0]
		source, ok := s.sources[pair.Stream]
		if !ok || pair.ComponentType != sensorrecord.ComponentType || c.Scope.Tenant != s.cfg.Tenant || c.Scope.Site != s.cfg.Site || c.Scope.Device != source.Device || batch.Devices[i].Principal != source.Device || c.Scope.App != source.App || c.Scope.Service != source.Service || c.Scope.ExecutionDomain != "simulation" {
			return ErrSource
		}
		if old, duplicate := claims[pair.Stream]; duplicate && !reflect.DeepEqual(old, c) {
			return ErrSource
		}
		claims[pair.Stream] = c
	}
	current, err := s.authorizationOriginals(ctx)
	if err != nil {
		return err
	}
	matched := map[string]bool{}
	for _, p := range current {
		c, ok := claims[p.record.Publication.Stream]
		if !ok {
			continue
		}
		matched[p.record.Publication.Stream] = true
		var active serviceorigin.DelegationClaims
		if p.record.Epoch != c.Epoch || !reflect.DeepEqual(p.record.Scope, c.Scope) || jws.DecodeStrict(p.signed.Delegation.Claims, &active) != nil || !reflect.DeepEqual(active, c) {
			return ErrSource
		}
	}
	if len(matched) != len(claims) {
		if preparation != nil {
			preparation.missing = map[string]bool{}
			for id := range claims {
				if !matched[id] {
					preparation.missing[id] = true
				}
			}
		}
		return ErrSource
	}
	return nil
}

func (s *Session) prepareCurrent(ctx context.Context, proofs []proof) ([]serviceorigin.VerifiedRecord, error) {
	preparation := &preparationState{}
	ctx = context.WithValue(ctx, preparationKey{}, preparation)
	raw := make([][]byte, len(proofs))
	for i, p := range proofs {
		raw[i] = p.raw
	}
	var verified []serviceorigin.VerifiedRecord
	_, _, err := fusioncapture.PrepareBatch(ctx, fusioncapture.Options{Physical: s.clock}, raw, func(ctx context.Context, records []serviceorigin.SignedRecord) ([]serviceorigin.VerifiedRecord, error) {
		v, err := s.verifier.VerifyBatch(ctx, records)
		if err != nil {
			return nil, err
		}
		verified = v
		return v, nil
	})
	if err != nil {
		var stale *fusioncapture.StaleCaptureError
		if errors.As(err, &stale) {
			if e := ctx.Err(); e != nil {
				return nil, e
			}
			return nil, errExpiredCamera
		}
		if errors.Is(err, serviceorigin.ErrDenied) && len(preparation.missing) > 0 {
			// All immutable signatures passed before the authorizer ran. A
			// successful Query can lose an original as its capture ages. Only
			// proven expiry permits another acquisition tick, never publication.
			observation, e := s.clock.PhysicalObserve(ctx)
			if e != nil {
				return nil, e
			}
			for _, p := range proofs {
				captured := time.Unix(0, p.capture.CapturedUnixNS)
				if p.capture.CapturedUnixNS <= 0 || captured.After(observation.Physical.Latest) {
					return nil, ErrSource
				}
				if preparation.missing[p.record.Publication.Stream] && observation.Physical.Latest.Sub(captured) <= sensorrecord.MaxPhysicalAge {
					return nil, ErrSource
				}
			}
			if e := ctx.Err(); e != nil {
				return nil, e
			}
			return nil, errExpiredCamera
		}
		return nil, err
	}
	if len(verified) != len(proofs) {
		return nil, ErrSource
	}
	for i, v := range verified {
		if !bytes.Equal(v.Bytes, proofs[i].signed.Record) || !reflect.DeepEqual(v.Record, proofs[i].record) {
			return nil, ErrSource
		}
	}
	return verified, nil
}

// Previous and current measurements may share a source with different original
// versions. The generic verifier admits that union; PrepareBatch deliberately
// requires distinct sensor sources. trackcapture validates every capture again.
func (s *Session) verify(ctx context.Context, proofs []proof) ([]serviceorigin.VerifiedRecord, error) {
	records := make([]serviceorigin.SignedRecord, len(proofs))
	for i := range proofs {
		records[i] = proofs[i].signed
	}
	verified, err := s.verifier.VerifyBatch(ctx, records)
	if err != nil {
		return nil, err
	}
	if len(verified) != len(proofs) {
		return nil, ErrSource
	}
	for i := range verified {
		if !bytes.Equal(verified[i].Bytes, proofs[i].signed.Record) || !reflect.DeepEqual(verified[i].Record, proofs[i].record) {
			return nil, ErrSource
		}
	}
	return verified, nil
}
