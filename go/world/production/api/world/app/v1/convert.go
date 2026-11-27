package appv1

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

var errPublication = errors.New("invalid World publication request")

// DecodePublish preserves the app's original sample and ancestry. This only
// validates the wire representation; the agent and sink still enforce scope,
// schema, frame, source freshness and authorization.
func DecodePublish(p *PublishRequest) (serviceorigin.PublishRequest, error) {
	if p == nil || proto.Size(p) > serviceorigin.MaxRecordBytes || !knownFields(p.ProtoReflect()) || p.Frame == nil || p.Timing == nil || p.Provenance == nil || p.TtlNs == 0 || p.TtlNs > math.MaxInt64 || p.Timing.ErrorBoundNs > math.MaxInt64 || p.Timing.AgeHintNs > math.MaxInt64 || !p.Timing.ErrorKnown && p.Timing.ErrorBoundNs != 0 {
		return serviceorigin.PublishRequest{}, errPublication
	}
	provenance, err := decodeProvenance(p.Provenance)
	if err != nil {
		return serviceorigin.PublishRequest{}, err
	}
	r := serviceorigin.PublishRequest{
		Entity: p.Entity, ComponentType: p.ComponentType, Stream: p.Stream,
		Frame:  world.FrameRef{ID: p.Frame.Id, Revision: p.Frame.Revision},
		Timing: world.Timing{SampleTime: time.Unix(0, p.Timing.SampleUnixNs).UTC(), ClockDomain: p.Timing.ClockDomain, ErrorKnown: p.Timing.ErrorKnown, ErrorBound: time.Duration(p.Timing.ErrorBoundNs), AgeHint: time.Duration(p.Timing.AgeHintNs)},
		TTL:    time.Duration(p.TtlNs), Provenance: provenance, Payload: bytes.Clone(p.PayloadJson), Tombstone: p.Tombstone,
	}
	if r.Frame.Validate() != nil || r.Timing.Validate() != nil || r.Provenance.Validate() != nil {
		return serviceorigin.PublishRequest{}, errPublication
	}
	return r, nil
}

// Unknown fields are refused at every level rather than silently accepting an
// extension that an app might mistake for caller identity or authority.
func knownFields(m protoreflect.Message) bool {
	if !m.IsValid() || len(m.GetUnknown()) != 0 {
		return false
	}
	valid := true
	m.Range(func(f protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if f.Message() != nil {
			if f.IsList() {
				for i := 0; i < v.List().Len(); i++ {
					if !knownFields(v.List().Get(i).Message()) {
						valid = false
						break
					}
				}
			} else {
				valid = knownFields(v.Message())
			}
		}
		return valid
	})
	return valid
}

func decodeOrigin(o Origin) (world.Origin, error) {
	switch o {
	case Origin_ORIGIN_MEASURED:
		return world.Measured, nil
	case Origin_ORIGIN_SIMULATED:
		return world.Simulated, nil
	case Origin_ORIGIN_REPLAYED:
		return world.Replayed, nil
	case Origin_ORIGIN_INFERRED:
		return world.Inferred, nil
	case Origin_ORIGIN_AUTHORED:
		return world.Authored, nil
	default:
		return "", errPublication
	}
}
func decodeSimulation(s *SimulationContext) *world.SimulationContext {
	if s == nil {
		return nil
	}
	return &world.SimulationContext{RunID: s.RunId, ScenarioVersion: s.ScenarioVersion, Seed: s.Seed, ClockDomain: s.ClockDomain}
}
func captureTime(ns *int64) *time.Time {
	if ns == nil {
		return nil
	}
	t := time.Unix(0, *ns).UTC()
	return &t
}
func decodeProvenance(p *Provenance) (world.Provenance, error) {
	o, err := decodeOrigin(p.Origin)
	if err != nil || len(p.Inputs) > 256 {
		return world.Provenance{}, errPublication
	}
	r := world.Provenance{Origin: o, AppVersion: p.AppVersion, ModelID: p.ModelId, ModelVersion: p.ModelVersion, Simulation: decodeSimulation(p.Simulation), OriginalCaptureTime: captureTime(p.OriginalCaptureUnixNs), OriginalClockDomain: p.OriginalClockDomain}
	for _, in := range p.Inputs {
		if in == nil {
			return world.Provenance{}, errPublication
		}
		o, err := decodeOrigin(in.Origin)
		if err != nil {
			return world.Provenance{}, err
		}
		r.Inputs = append(r.Inputs, world.Input{ID: in.Id, Origin: o, Simulation: decodeSimulation(in.Simulation), OriginalCaptureTime: captureTime(in.OriginalCaptureUnixNs), OriginalClockDomain: in.OriginalClockDomain})
	}
	return r, nil
}

// EncodePublishReply must only be returned after the broker's durable sink
// commit. Marshaling the envelope never re-encodes the signed inner bytes.
func EncodePublishReply(record serviceorigin.SignedRecord) (*PublishReply, error) {
	if len(record.Record) == 0 || len(record.Record) > serviceorigin.MaxRecordBytes {
		return nil, errPublication
	}
	raw, err := json.Marshal(record)
	if err != nil || len(raw) > 256<<10 {
		return nil, errPublication
	}
	return &PublishReply{SignedRecordJson: raw, Accepted: true}, nil
}
