package livefusion

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"reflect"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/jws"
	"github.com/wendylabsinc/wendy/go/world/production/proofref"
	"github.com/wendylabsinc/wendy/go/world/production/resultrecord"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceaccess"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var errQueryUnavailable = errors.New("source query transport unavailable")

func digest(data []byte) string { h := sha256.Sum256(data); return hex.EncodeToString(h[:]) }
func selector(s serviceaccess.Source) *appv1.SourceSelector {
	return &appv1.SourceSelector{Device: s.Device, App: s.App, Service: s.Service, ComponentType: s.ComponentType, Stream: s.Stream}
}

type inputProof struct {
	raw       []byte
	signed    serviceorigin.SignedRecord
	record    serviceorigin.Record
	capture   sensorrecord.Capture
	reference proofref.Reference
}
type sourceBinding struct {
	scope      serviceorigin.Scope
	epoch      uint64
	delegation string
	sequence   uint64
	envelope   string
}
type session struct {
	cfg         Config
	api         appv1.WorldAppServiceClient
	clock       *diagnosticClock
	verifier    *serviceorigin.BatchVerifier
	sources     map[string]Source
	bindings    map[string]sourceBinding
	failureCode string
}

func decodeEnvelope(raw []byte) (serviceorigin.SignedRecord, serviceorigin.Record, error) {
	var signed serviceorigin.SignedRecord
	var record serviceorigin.Record
	if len(raw) == 0 || len(raw) > 256<<10 || authority.DecodeStrictLimit(raw, &signed, 256<<10) != nil || jws.DecodeStrict(signed.Record, &record) != nil {
		return signed, record, ErrSource
	}
	return signed, record, nil
}

func (s *session) parse(raw []byte, wanted map[string]Source) (inputProof, error) {
	signed, r, err := decodeEnvelope(raw)
	if err != nil {
		return inputProof{}, err
	}
	source, ok := wanted[r.Publication.Stream]
	if !ok || r.Version != serviceorigin.Version || r.AuthorityDomain != serviceorigin.AuthorityDomain || r.DelegationSHA256 != digest(signed.Delegation.Claims) || r.Scope.Validate() != nil || r.Scope.Tenant != s.cfg.Tenant || r.Scope.Site != s.cfg.Site || r.Scope.Device != source.Device || r.Scope.ExecutionDomain != "simulation" || r.Scope.App != source.App || r.Scope.Service != source.Service || len(r.Scope.Publish) != 1 || r.Scope.Publish[0] != (serviceorigin.PublishScope{ComponentType: sensorrecord.ComponentType, Stream: source.SensorID}) || r.Epoch == 0 || r.Sequence == 0 || r.Publication.Frame != s.cfg.Frame {
		return inputProof{}, ErrSource
	}
	capture, err := sensorrecord.ValidatePublication(r.Publication)
	if err != nil {
		return inputProof{}, ErrSource
	}
	sample, err := capture.Sample()
	if err != nil || capture.SceneBootID != s.cfg.SceneBootID || capture.SimulationAnchorUnixNS != s.cfg.SimulationAnchorUnixNS || sample.RunID != s.cfg.RunID || sample.SiteID != s.cfg.Site || sample.Frame != s.cfg.Frame || sample.PackageSHA256 != s.cfg.PackageSHA256 || sample.ScenarioVersion != s.cfg.ScenarioVersion || sample.Seed != s.cfg.Seed {
		return inputProof{}, ErrSource
	}
	ref := proofref.Reference{Key: world.Key{Tenant: r.Scope.Tenant, Site: r.Scope.Site, Entity: r.Publication.Entity, Type: r.Publication.ComponentType, Source: world.Source{Device: r.Scope.Device, App: r.Scope.App, Service: r.Scope.Service, Stream: r.Publication.Stream}}, Version: world.Version{Epoch: r.Epoch, Sequence: r.Sequence}, EnvelopeSHA256: digest(raw), RecordSHA256: digest(signed.Record)}
	if ref.Validate() != nil {
		return inputProof{}, ErrSource
	}
	return inputProof{bytes.Clone(raw), signed, r, capture, ref}, nil
}

// Query is the current-authority witness supplied by the commissioned agent
// socket, not a signature verifier. Every original input is also verified
// locally against public roots. A newer sample may replace it while preserving
// the exact active source generation/epoch/delegation.
func (s *session) query(ctx context.Context, wanted map[string]Source) ([]inputProof, error) {
	s.failureCode = ""
	start := time.Now()
	defer s.recordQuery(start)
	q := &appv1.QueryRequest{Limit: uint32(len(wanted))}
	for _, configured := range s.cfg.Sources {
		if _, ok := wanted[configured.SensorID]; ok {
			q.Sources = append(q.Sources, selector(configured.selector()))
		}
	}
	rpc, cancel := context.WithTimeout(ctx, time.Duration(s.cfg.RequestTimeoutMS)*time.Millisecond)
	defer cancel()
	reply, err := s.api.Query(rpc, q, grpc.MaxCallRecvMsgSize(serviceaccess.MaxQueryBytes+1024), grpc.MaxRetryRPCBufferSize(0))
	if err != nil {
		s.failureCode = "source_query_rpc_" + status.Code(err).String()
		if ctx.Err() == nil && (status.Code(err) == codes.DeadlineExceeded || status.Code(err) == codes.Unavailable) {
			return nil, errors.Join(ErrSource, errQueryUnavailable)
		}
		return nil, ErrSource
	}
	if reply == nil || len(reply.ProtoReflect().GetUnknown()) != 0 || reply.Next != "" || len(reply.SignedRecordJson) > len(wanted) {
		s.failureCode = "source_query_shape_refused"
		return nil, ErrSource
	}
	var proofs []inputProof
	total := 0
	seen := map[string]bool{}
	for _, raw := range reply.SignedRecordJson {
		total += len(raw)
		if total > serviceaccess.MaxQueryBytes {
			return nil, ErrSource
		}
		p, err := s.parse(raw, wanted)
		if err != nil || seen[p.record.Publication.Stream] {
			return nil, ErrSource
		}
		sensor := p.record.Publication.Stream
		seen[sensor] = true
		if old, exists := s.bindings[sensor]; exists {
			if !reflect.DeepEqual(old.scope, p.record.Scope) || old.epoch != p.record.Epoch || old.delegation != p.record.DelegationSHA256 || p.record.Sequence < old.sequence || p.record.Sequence == old.sequence && p.reference.EnvelopeSHA256 != old.envelope {
				return nil, ErrSource
			}
		}
		s.bindings[sensor] = sourceBinding{p.record.Scope, p.record.Epoch, p.record.DelegationSHA256, p.record.Sequence, p.reference.EnvelopeSHA256}
		proofs = append(proofs, p)
	}
	return proofs, nil
}

// authorizeBatch observes all commissioned sources in one complete query. A
// newer sample is only a witness for the same original scope/epoch/delegation;
// it never replaces an input or renews that input's deadlines.
func (s *session) authorizeBatch(ctx context.Context, batch serviceorigin.BatchAuthorization) error {
	read, _ := ctx.Value(captureReadKey{}).(*captureReadState)
	if read != nil {
		read.missing, read.batch = nil, serviceorigin.BatchAuthorization{}
	}
	if len(s.sources) < 4 || len(s.sources) > serviceaccess.MaxQueryRecords || len(batch.Claims) < 4 || len(batch.Claims) > len(s.sources) || len(batch.Devices) != len(batch.Claims) {
		return ErrSource
	}
	claimsBySensor := make(map[string]serviceorigin.DelegationClaims, len(s.sources))
	for i, claims := range batch.Claims {
		device := batch.Devices[i]
		if claims.Scope.Tenant != s.cfg.Tenant || claims.Scope.Site != s.cfg.Site || claims.Scope.ExecutionDomain != "simulation" || len(claims.Scope.Publish) != 1 {
			return ErrSource
		}
		pair := claims.Scope.Publish[0]
		configured, ok := s.sources[pair.Stream]
		if !ok || device.Principal != configured.Device || claims.Scope.Device != configured.Device || pair.ComponentType != sensorrecord.ComponentType || claims.Scope.App != configured.App || claims.Scope.Service != configured.Service {
			return ErrSource
		}
		if _, duplicate := claimsBySensor[pair.Stream]; duplicate {
			return ErrSource
		}
		claimsBySensor[pair.Stream] = claims
	}
	if !s.completeSensors(claimsBySensor) {
		return ErrSource
	}
	current, err := s.query(ctx, s.sources)
	if err != nil {
		return err
	}
	matched := map[string]bool{}
	for _, p := range current {
		claims, ok := claimsBySensor[p.record.Publication.Stream]
		if !ok {
			continue
		}
		matched[p.record.Publication.Stream] = true
		if !reflect.DeepEqual(p.record.Scope, claims.Scope) || p.record.Epoch != claims.Epoch {
			s.failureCode = "current_source_replaced"
			return ErrSource
		}
		var active serviceorigin.DelegationClaims
		if jws.DecodeStrict(p.signed.Delegation.Claims, &active) != nil || !reflect.DeepEqual(active, claims) {
			s.failureCode = "current_source_delegation_changed"
			return ErrSource
		}
	}
	if len(matched) != len(claimsBySensor) {
		s.failureCode = "current_source_absent"
		if read != nil {
			read.missing = map[string]bool{}
			for id := range claimsBySensor {
				if !matched[id] {
					read.missing[id] = true
				}
			}
			read.batch = batch
		}
		return ErrSource
	}
	return nil
}

// buildValidated projects the final successful EvaluateBatch verification. It
// performs no I/O and grants no authority beyond that attempt; the caller still
// checks both clocks before building the publication and the host validates it.
func (s *session) buildValidated(ctx context.Context, proofs []inputProof, verified []serviceorigin.VerifiedRecord) ([]resultrecord.ValidatedInput, error) {
	if !s.completeProofs(proofs) || len(verified) != len(proofs) {
		return nil, ErrSource
	}
	var inputs []resultrecord.ValidatedInput
	for i, p := range proofs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		v := verified[i]
		if !bytes.Equal(v.Bytes, p.signed.Record) || !reflect.DeepEqual(v.Record, p.record) {
			return nil, ErrSource
		}
		component := v.Component()
		sampleUntil, physicalUntil, err := sensorrecord.LiveDeadlines(component)
		if err != nil {
			return nil, ErrSource
		}
		expires := time.UnixMilli(v.Record.ExpiresAtUnixMS)
		if expires.Before(physicalUntil) {
			physicalUntil = expires
		}
		inputs = append(inputs, resultrecord.ValidatedInput{Reference: p.reference, Component: component, SampleValidUntil: sampleUntil, PhysicalValidUntil: physicalUntil})
	}
	return inputs, nil
}

// The four corroborating sources are mandatory. A spatial configuration also
// needs at least one currently selected LiDAR; other catalog scans are optional.
func (s *session) completeSensors(selected map[string]serviceorigin.DelegationClaims) bool {
	for _, id := range []string{"camera-a", "camera-b", "gas-1", "audio-1"} {
		if _, ok := selected[id]; !ok {
			return false
		}
	}
	spatial, present := false, false
	for id := range s.sources {
		if sensing.LidarSensorID(id) {
			spatial = true
			if _, ok := selected[id]; ok {
				present = true
			}
		}
	}
	return !spatial || present
}
func (s *session) completeProofs(proofs []inputProof) bool {
	if len(proofs) < 4 || len(proofs) > len(s.sources) {
		return false
	}
	selected := map[string]serviceorigin.DelegationClaims{}
	for _, p := range proofs {
		id := p.record.Publication.Stream
		if _, ok := s.sources[id]; !ok {
			return false
		}
		if _, seen := selected[id]; seen {
			return false
		}
		selected[id] = serviceorigin.DelegationClaims{}
	}
	return s.completeSensors(selected)
}
