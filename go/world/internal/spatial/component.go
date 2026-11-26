package spatial

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/strictjson"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

// ModelPrefix starts the provenance ModelID of every spatial record:
// "wendy.spatial.<feature>".
const ModelPrefix = "wendy.spatial."

// MaxPayloadBytes bounds an encoded payload. It leaves headroom under the
// 64 KiB that World readers accept for a component payload.
const MaxPayloadBytes = 48 << 10

// MaxInputs bounds the input ancestry of one spatial record, as existing
// World publishers do (16 to 32). An estimate that integrates more
// measurements lists a reduced set: see ReduceInputs.
const MaxInputs = 32

// MaxComponentBytes bounds a whole JSON-encoded spatial component: payload,
// key, timing and ancestry. It stays under the 64 KiB of a signed World
// envelope (authority.MaxEnvelopeBytes) with room for the signature.
const MaxComponentBytes = 60 << 10

// Scope is the part of a component key that does not depend on the payload.
type Scope struct {
	Tenant string
	Site   string
	Source world.Source
}

// KeyFor is the component key under which p is published in scope.
func KeyFor(scope Scope, p Payload) world.Key {
	return world.Key{Tenant: scope.Tenant, Site: scope.Site, Entity: p.EntityKey(), Type: p.Type(), Source: scope.Source}
}

// ToComponent validates p and wraps it as a World component. The key is
// KeyFor(scope, p). Spatial records are derived and advisory: the provenance
// must be Inferred with a ModelID under ModelPrefix (see InferredProvenance).
// Everything world.Replica.Apply would reject on the record itself is
// rejected here, and so is what World transport would: more than MaxInputs
// inputs, a payload above MaxPayloadBytes, a component above
// MaxComponentBytes. A payload that names the frame it is expressed in
// (DevicePose.FrameID, FrameAlignment.ParentFrameID) must name frame.ID.
// Freshness and scope are the replica's to judge.
func ToComponent(scope Scope, version world.Version, frame world.FrameRef, timing world.Timing, ttl time.Duration, provenance world.Provenance, p Payload) (world.Component, error) {
	if p == nil {
		return world.Component{}, errors.New("spatial: nil payload")
	}
	if err := p.Validate(); err != nil {
		return world.Component{}, err
	}
	if scope.Tenant == "" || scope.Site == "" || version.Epoch == 0 || version.Sequence == 0 || ttl <= 0 {
		return world.Component{}, invalid("component scope, version or TTL")
	}
	if err := scope.Source.Validate(); err != nil {
		return world.Component{}, err
	}
	if err := frame.Validate(); err != nil {
		return world.Component{}, err
	}
	if err := timing.Validate(); err != nil {
		return world.Component{}, err
	}
	if err := provenance.Validate(); err != nil {
		return world.Component{}, err
	}
	if provenance.Origin != world.Inferred || !strings.HasPrefix(provenance.ModelID, ModelPrefix) || len(provenance.ModelID) == len(ModelPrefix) {
		return world.Component{}, invalid("provenance: spatial records are inferred by a wendy.spatial.<feature> model")
	}
	if len(provenance.Inputs) > MaxInputs {
		return world.Component{}, fmt.Errorf("spatial: %d provenance inputs, limit %d (see ReduceInputs)", len(provenance.Inputs), MaxInputs)
	}
	if f, ok := p.(interface{ componentFrame() string }); ok && f.componentFrame() != frame.ID {
		return world.Component{}, fmt.Errorf("spatial: %s is expressed in frame %q but published in frame %q", p.Type(), f.componentFrame(), frame.ID)
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return world.Component{}, err
	}
	if len(raw) > MaxPayloadBytes {
		return world.Component{}, fmt.Errorf("spatial: %s payload is %d bytes, limit %d", p.Type(), len(raw), MaxPayloadBytes)
	}
	c := world.Component{Key: KeyFor(scope, p), Version: version, Frame: frame, Timing: timing, TTL: ttl, Provenance: provenance, Payload: raw}
	whole, err := json.Marshal(c)
	if err != nil {
		return world.Component{}, err
	}
	if len(whole) > MaxComponentBytes {
		return world.Component{}, fmt.Errorf("spatial: %s component is %d bytes, limit %d", p.Type(), len(whole), MaxComponentBytes)
	}
	return c, nil
}

// FromComponent decodes and validates the payload of a component of type T.
// A tombstone, another component type, unknown or duplicate fields, an
// oversized payload and an entity key that does not match the payload are
// errors.
func FromComponent[T Payload](c world.Component) (T, error) {
	var p T
	if c.Tombstone {
		return p, errors.New("spatial: component is a tombstone")
	}
	if c.Key.Type != p.Type() {
		return p, fmt.Errorf("spatial: component type %q is not %q", c.Key.Type, p.Type())
	}
	if err := strictjson.DecodeLimit(c.Payload, &p, MaxPayloadBytes); err != nil {
		return p, err
	}
	if err := p.Validate(); err != nil {
		return p, err
	}
	if c.Key.Entity != p.EntityKey() {
		return p, fmt.Errorf("spatial: component entity %q does not match payload %q", c.Key.Entity, p.EntityKey())
	}
	return p, nil
}

func decodeAny[T Payload](c world.Component) (Payload, error) {
	p, err := FromComponent[T](c)
	if err != nil {
		return nil, err
	}
	return p, nil
}

// DecodePayload decodes a component of any spatial type, returning the
// payload struct by value.
func DecodePayload(c world.Component) (Payload, error) {
	switch c.Key.Type {
	case TypeCalibration:
		return decodeAny[Calibration](c)
	case TypeDevicePose:
		return decodeAny[DevicePose](c)
	case TypeSensorRegistration:
		return decodeAny[SensorRegistration](c)
	case TypeAnchor:
		return decodeAny[Anchor](c)
	case TypeTrack:
		return decodeAny[Track](c)
	case TypeMapTile:
		return decodeAny[MapTile](c)
	case TypeRegion:
		return decodeAny[Region](c)
	case TypeChangeEvent:
		return decodeAny[ChangeEvent](c)
	case TypeFloorInfo:
		return decodeAny[FloorInfo](c)
	case TypePortal:
		return decodeAny[Portal](c)
	case TypeGeoReference:
		return decodeAny[GeoReference](c)
	case TypeKeyframe:
		return decodeAny[Keyframe](c)
	case TypeLandmark:
		return decodeAny[Landmark](c)
	case TypeObject:
		return decodeAny[Object](c)
	case TypeFrameAlignment:
		return decodeAny[FrameAlignment](c)
	case TypeLayerTile:
		return decodeAny[LayerTile](c)
	}
	return nil, fmt.Errorf("spatial: %q is not a spatial component type", c.Key.Type)
}

// ComponentEntities projects a spatial component onto entities. Entities that
// do not carry their own times are stamped with the component's sample time,
// every entity is valid until sample time plus TTL, and an entity without a
// source takes the publishing device. Types without an entity projection
// (tiles, keyframes, the geo reference, frame alignments, floors, sensor
// registrations) yield none.
//
// Positions are in the component's frame. For every type but DevicePose that
// is the site frame; a DevicePose published in a device-local frame projects
// in that frame (attribute "frame_id"), so a consumer that mixes frames must
// filter on c.Frame or apply the FrameAlignment first.
//
// Each entity key has one owning type, so projecting every component of a
// source yields no duplicates: a portal comes from its Portal component, not
// from the FloorInfo records that repeat it, and a sensor from its
// Calibration, not from its SensorRegistration.
func ComponentEntities(c world.Component) ([]Entity, error) {
	p, err := DecodePayload(c)
	if err != nil {
		return nil, err
	}
	projector, ok := p.(interface{ Entities() []Entity })
	if !ok {
		return nil, nil
	}
	out := projector.Entities()
	for i := range out {
		if out[i].ObservedAt.IsZero() {
			out[i].ObservedAt = c.Timing.SampleTime
		}
		out[i].ValidUntil = c.Timing.SampleTime.Add(c.TTL)
		if out[i].ValidUntil.Before(out[i].ObservedAt) {
			out[i].ValidUntil = out[i].ObservedAt
		}
		if out[i].Source == "" {
			out[i].Source = DeviceID(c.Key.Source.Device)
		}
	}
	SortEntities(out)
	return out, nil
}

// InferredProvenance builds the provenance of a record derived by the spatial
// feature named feature ("calib", "localize", ...): Origin Inferred, ModelID
// ModelPrefix+feature. Inputs are the measured, simulated or replayed leaves
// the estimate was computed from; they are sorted by ID so that the same
// evidence yields the same component bytes. The result passes
// world.Provenance.Validate or an error is returned. More than MaxInputs
// inputs is an error: pass a longer list through ReduceInputs first.
func InferredProvenance(feature, modelVersion, appVersion string, inputs []world.Input) (world.Provenance, error) {
	if !validID(feature) {
		return world.Provenance{}, invalid("provenance feature name")
	}
	if len(inputs) > MaxInputs {
		return world.Provenance{}, fmt.Errorf("spatial: %d provenance inputs, limit %d (see ReduceInputs)", len(inputs), MaxInputs)
	}
	sorted := append([]world.Input(nil), inputs...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	p := world.Provenance{Origin: world.Inferred, AppVersion: appVersion, ModelID: ModelPrefix + feature, ModelVersion: modelVersion, Inputs: sorted}
	if err := p.Validate(); err != nil {
		return world.Provenance{}, err
	}
	return p, nil
}

// inputStream splits an InputID into its stream, "<device>/<sensor>/<kind>",
// and its instant. An ID in another form is a stream of its own.
func inputStream(id string) (stream string, nanos int64) {
	parts := strings.SplitN(id, "/", 5)
	if len(parts) < 4 {
		return id, 0
	}
	nanos, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil {
		return id, 0
	}
	return strings.Join(parts[:3], "/"), nanos
}

// ReduceInputs bounds an ancestry list to MaxInputs by a fixed rule, so the
// same evidence always yields the same list whatever order it was gathered in:
//
//  1. inputs with the same ID collapse to the first of them, and the rest
//     are sorted by ID; a list that now fits is returned whole;
//  2. otherwise each stream (the "<device>/<sensor>/<kind>" prefix of an
//     InputID) keeps its earliest and its latest input, the span of evidence
//     the estimate rests on;
//  3. if that is still too many, the list is thinned evenly in ID order,
//     keeping its first and last entries.
func ReduceInputs(inputs []world.Input) []world.Input {
	out := append([]world.Input(nil), inputs...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	out = slices.CompactFunc(out, func(a, b world.Input) bool { return a.ID == b.ID })
	if len(out) <= MaxInputs {
		return out
	}
	type span struct {
		first, last   int // indices into out
		tFirst, tLast int64
	}
	spans := map[string]*span{}
	for i, in := range out {
		stream, t := inputStream(in.ID)
		switch s := spans[stream]; {
		case s == nil:
			spans[stream] = &span{i, i, t, t}
		case t < s.tFirst:
			s.first, s.tFirst = i, t
		case t >= s.tLast:
			s.last, s.tLast = i, t
		}
	}
	keep := make([]bool, len(out))
	for _, s := range spans {
		keep[s.first], keep[s.last] = true, true
	}
	kept := out[:0]
	for i, in := range out {
		if keep[i] {
			kept = append(kept, in)
		}
	}
	if len(kept) <= MaxInputs {
		return kept
	}
	thinned := make([]world.Input, MaxInputs)
	for i := range thinned {
		thinned[i] = kept[i*(len(kept)-1)/(MaxInputs-1)]
	}
	return thinned
}

// InputID is the ancestry identifier of one measurement:
// "<device>/<sensor>/<kind>/<unix nanoseconds>", followed by whatever else
// tells apart measurements that share a sensor and an instant: "/<peer>" and,
// when set, "/<peer sensor>" for a Range, "/<source>" for a Bearing that names
// one, "/<quantity>" for a Scalar. A SensorManifest uses the kind segment
// "manifest", since it is stamped like its sensor's first sample. Distinct
// measurements therefore have distinct IDs, except two unlabelled Bearings of
// one array at one instant. The format is ancestry identity for recorded
// history; it must not change.
func InputID(m Measurement) string {
	device, sensor := m.From()
	kind, rest := string(m.Kind()), ""
	switch v := m.(type) {
	case Range:
		rest = "/" + string(v.Peer)
		if v.PeerSensor != "" {
			rest += "/" + string(v.PeerSensor)
		}
	case Bearing:
		if v.Source != "" {
			rest = "/" + v.Source
		}
	case Scalar:
		rest = "/" + v.Quantity
	case SensorManifest:
		kind = "manifest"
	}
	return string(device) + "/" + string(sensor) + "/" + kind + "/" + strconv.FormatInt(m.When().UnixNano(), 10) + rest
}
