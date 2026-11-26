package spatial

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

const simClock = "sim:spatial-test"

var (
	testScope = Scope{Tenant: "acme", Site: "plant-1", Source: world.Source{Device: "gateway-1", App: "wendy-world", Service: "spatial", Stream: "engine"}}
	siteFrame = world.FrameRef{ID: "site", Revision: "rev-1"}
	simulated = world.SimulationContext{RunID: "run-1", ScenarioVersion: "standard-1", Seed: 42, ClockDomain: simClock}
)

func testTiming() world.Timing {
	return world.Timing{SampleTime: t0, ClockDomain: simClock, ErrorKnown: true, ErrorBound: time.Millisecond}
}

func testProvenance(t *testing.T, feature string) world.Provenance {
	t.Helper()
	p, err := InferredProvenance(feature, "1.0.0", "wendy-test", []world.Input{
		{ID: "robot-1/lidar/lidar2d/2", Origin: world.Simulated, Simulation: &simulated},
		{ID: "robot-1/lidar/lidar2d/1", Origin: world.Simulated, Simulation: &simulated},
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func testReplica(t *testing.T, allowSynthetic bool) *world.Replica {
	t.Helper()
	r, err := world.NewReplica(world.Config{Tenant: testScope.Tenant, Site: testScope.Site, ClockDomain: simClock,
		Now: func() time.Time { return t0.Add(time.Second) }, MaxEntries: 64, AllowSynthetic: allowSynthetic, Frames: []world.FrameRef{siteFrame}})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// Every payload, wrapped by ToComponent with InferredProvenance, must be
// admitted by a replica that allows synthetic input in a simulation clock
// domain, come back out of its snapshot, and decode to the same payload.
func TestComponentsApplyToReplica(t *testing.T) {
	replica := testReplica(t, true)
	prov := testProvenance(t, "engine")
	applied, types := 0, map[string]bool{}
	for i, p := range samplePayloads() {
		if i == 1 {
			continue // the second calibration sample is applied on its own below
		}
		c, err := ToComponent(testScope, world.Version{Epoch: 1, Sequence: 1}, siteFrame, testTiming(), time.Minute, prov, p)
		if err != nil {
			t.Fatalf("%T: %v", p, err)
		}
		if c.Key != KeyFor(testScope, p) || c.Key.Type != p.Type() || c.Key.Entity != p.EntityKey() || len(c.Payload) > MaxPayloadBytes {
			t.Fatalf("%T: key %+v", p, c.Key)
		}
		result, err := replica.Apply(testScope.Source, c)
		if err != nil || result != world.Applied {
			t.Fatalf("%T: apply = %q, %v", p, result, err)
		}
		if result, err := replica.Apply(testScope.Source, c); err != nil || result != world.Duplicate {
			t.Fatalf("%T: reapply = %q, %v", p, result, err)
		}
		stored, ok, err := replica.Get(c.Key)
		if err != nil || !ok {
			t.Fatalf("%T: not visible after apply: %v", p, err)
		}
		back, err := DecodePayload(stored)
		if err != nil {
			t.Fatalf("%T: %v", p, err)
		}
		if reflect.TypeOf(back) != reflect.TypeOf(p) {
			t.Fatalf("decoded %T, want %T", back, p)
		}
		want, _ := json.Marshal(p)
		got, _ := json.Marshal(back)
		if !bytes.Equal(want, got) {
			t.Fatalf("%T changed in the replica:\n%s\n%s", p, want, got)
		}
		applied++
		types[p.Type()] = true
	}
	snapshot, err := replica.Snapshot()
	if err != nil || len(snapshot) != applied || len(types) != len(Types()) {
		t.Fatalf("snapshot has %d components, applied %d, %d of %d types: %v", len(snapshot), applied, len(types), len(Types()), err)
	}
	// The mounted calibration is another sensor, so another key.
	mounted := samplePayloads()[1]
	c, err := ToComponent(testScope, world.Version{Epoch: 1, Sequence: 2}, siteFrame, testTiming(), time.Minute, testProvenance(t, "registration"), mounted)
	if err != nil {
		t.Fatal(err)
	}
	if result, err := replica.Apply(testScope.Source, c); err != nil || result != world.Applied {
		t.Fatalf("mounted calibration: %q, %v", result, err)
	}
	if snapshot, _ := replica.Snapshot(); len(snapshot) != applied+1 {
		t.Fatalf("mounted calibration has its own key: %d components", len(snapshot))
	}
	// A replica that does not admit synthetic data refuses records inferred
	// from simulated inputs: simulation results stay labelled as such.
	if _, err := testReplica(t, false).Apply(testScope.Source, c); err != world.ErrSynthetic {
		t.Fatalf("strict replica: %v", err)
	}
}

func TestFromComponent(t *testing.T) {
	prov := testProvenance(t, "localize")
	pose := samplePayloads()[2].(DevicePose)
	c, err := ToComponent(testScope, world.Version{Epoch: 1, Sequence: 1}, siteFrame, testTiming(), time.Minute, prov, pose)
	if err != nil {
		t.Fatal(err)
	}
	got, err := FromComponent[DevicePose](c)
	if err != nil || !reflect.DeepEqual(got, pose) {
		t.Fatalf("typed decode: %+v, %v", got, err)
	}
	if _, err := FromComponent[Track](c); err == nil {
		t.Fatal("decoded a device pose as a track")
	}
	for name, mutate := range map[string]func(*world.Component){
		"tombstone":       func(c *world.Component) { c.Tombstone = true },
		"unknown field":   func(c *world.Component) { c.Payload = bytes.Replace(c.Payload, []byte(`{`), []byte(`{"extra":1,`), 1) },
		"duplicate field": func(c *world.Component) { c.Payload = bytes.Replace(c.Payload, []byte(`{`), []byte(`{"yaw":1,`), 1) },
		"invalid payload": func(c *world.Component) {
			c.Payload = bytes.Replace(c.Payload, []byte(`"score":0.93`), []byte(`"score":9`), 1)
		},
		"foreign entity": func(c *world.Component) { c.Key.Entity = "device/robot-9" },
		"oversized":      func(c *world.Component) { c.Payload = append(bytes.Repeat([]byte(" "), MaxPayloadBytes), c.Payload...) },
		"not json":       func(c *world.Component) { c.Payload = []byte(`nope`) },
	} {
		bad := c
		bad.Payload = append([]byte(nil), c.Payload...)
		mutate(&bad)
		if bytes.Equal(bad.Payload, c.Payload) && bad.Key == c.Key && !bad.Tombstone {
			t.Fatalf("%s: mutation did nothing", name)
		}
		if _, err := FromComponent[DevicePose](bad); err == nil {
			t.Fatalf("%s: accepted", name)
		}
		if _, err := DecodePayload(bad); err == nil {
			t.Fatalf("%s: accepted by DecodePayload", name)
		}
	}
	other := c
	other.Key.Type = "wendy.sensor.v1"
	if _, err := DecodePayload(other); err == nil {
		t.Fatal("non-spatial type decoded")
	}
}

type hugePayload struct{ Blob string }

func (hugePayload) Type() string      { return "wendy.spatial.test.v1" }
func (hugePayload) EntityKey() string { return "test" }
func (hugePayload) Validate() error   { return nil }

func TestToComponentRefusals(t *testing.T) {
	prov := testProvenance(t, "localize")
	pose := samplePayloads()[2]
	version := world.Version{Epoch: 1, Sequence: 1}
	build := func(scope Scope, v world.Version, f world.FrameRef, tm world.Timing, ttl time.Duration, pr world.Provenance, p Payload) error {
		_, err := ToComponent(scope, v, f, tm, ttl, pr, p)
		return err
	}
	if err := build(testScope, version, siteFrame, testTiming(), time.Minute, prov, pose); err != nil {
		t.Fatal(err)
	}
	measured := world.Provenance{Origin: world.Measured, AppVersion: "wendy-test"}
	foreignModel := prov
	foreignModel.ModelID = "acme.tracker"
	bareModel := prov
	bareModel.ModelID = ModelPrefix
	noInputs := prov
	noInputs.Inputs = nil
	for name, err := range map[string]error{
		"nil payload":       build(testScope, version, siteFrame, testTiming(), time.Minute, prov, nil),
		"invalid payload":   build(testScope, version, siteFrame, testTiming(), time.Minute, prov, DevicePose{}),
		"no tenant":         build(Scope{Site: "s", Source: testScope.Source}, version, siteFrame, testTiming(), time.Minute, prov, pose),
		"incomplete source": build(Scope{Tenant: "t", Site: "s", Source: world.Source{Device: "d"}}, version, siteFrame, testTiming(), time.Minute, prov, pose),
		"zero version":      build(testScope, world.Version{}, siteFrame, testTiming(), time.Minute, prov, pose),
		"zero ttl":          build(testScope, version, siteFrame, testTiming(), 0, prov, pose),
		"frame":             build(testScope, version, world.FrameRef{ID: "site"}, testTiming(), time.Minute, prov, pose),
		"timing":            build(testScope, version, siteFrame, world.Timing{}, time.Minute, prov, pose),
		"measured origin":   build(testScope, version, siteFrame, testTiming(), time.Minute, measured, pose),
		"foreign model":     build(testScope, version, siteFrame, testTiming(), time.Minute, foreignModel, pose),
		"bare model prefix": build(testScope, version, siteFrame, testTiming(), time.Minute, bareModel, pose),
		"no input ancestry": build(testScope, version, siteFrame, testTiming(), time.Minute, noInputs, pose),
		"oversized payload": build(testScope, version, siteFrame, testTiming(), time.Minute, prov, hugePayload{Blob: strings.Repeat("x", MaxPayloadBytes)}),
	} {
		if err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}

func TestInferredProvenance(t *testing.T) {
	p := testProvenance(t, "calib")
	if p.Origin != world.Inferred || p.ModelID != "wendy.spatial.calib" || p.ModelVersion != "1.0.0" || p.AppVersion != "wendy-test" {
		t.Fatalf("provenance: %+v", p)
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if p.Inputs[0].ID != "robot-1/lidar/lidar2d/1" || p.Inputs[1].ID != "robot-1/lidar/lidar2d/2" {
		t.Fatalf("inputs must be sorted by id: %+v", p.Inputs)
	}
	if !p.Synthetic() {
		t.Fatal("a record inferred from simulated inputs is synthetic")
	}
	capture := t0
	real, err := InferredProvenance("localize", "1.0.0", "wendy-test", []world.Input{
		{ID: "b", Origin: world.Measured},
		{ID: "a", Origin: world.Replayed, OriginalCaptureTime: &capture, OriginalClockDomain: "utc"},
	})
	if err != nil || real.Inputs[0].ID != "a" {
		t.Fatalf("measured and replayed inputs: %+v, %v", real, err)
	}
	dup := world.Input{ID: "x", Origin: world.Measured}
	for name, err := range map[string]error{
		"no inputs":        second(InferredProvenance("calib", "1", "app", nil)),
		"duplicate inputs": second(InferredProvenance("calib", "1", "app", []world.Input{dup, dup})),
		"no model version": second(InferredProvenance("calib", "", "app", []world.Input{dup})),
		"no app version":   second(InferredProvenance("calib", "1", "", []world.Input{dup})),
		"empty feature":    second(InferredProvenance("", "1", "app", []world.Input{dup})),
		"feature with gap": second(InferredProvenance("my feature", "1", "app", []world.Input{dup})),
		"inferred input":   second(InferredProvenance("calib", "1", "app", []world.Input{{ID: "y", Origin: world.Inferred}})),
	} {
		if err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}

func second[T any](_ T, err error) error { return err }

func TestComponentEntities(t *testing.T) {
	prov := testProvenance(t, "engine")
	wrap := func(p Payload) world.Component {
		c, err := ToComponent(testScope, world.Version{Epoch: 1, Sequence: 1}, siteFrame, testTiming(), time.Minute, prov, p)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	s := samplePayloads()
	// A pose has no time of its own: it takes the component's sample time and TTL.
	es, err := ComponentEntities(wrap(s[2]))
	if err != nil || len(es) != 1 {
		t.Fatalf("device pose entities: %v, %v", es, err)
	}
	if e := es[0]; !e.ObservedAt.Equal(t0) || !e.ValidUntil.Equal(t0.Add(time.Minute)) || e.Source != "robot-1" || e.Validate() != nil {
		t.Fatalf("device entity stamping: %+v", e)
	}
	// A track keeps its own last-seen time; one seen after the sample time
	// plus TTL is still not valid before it was observed.
	es, err = ComponentEntities(wrap(s[5]))
	if err != nil || len(es) != 1 || !es[0].ObservedAt.Equal(t0.Add(4*time.Second)) || es[0].Source != "gateway-1" {
		t.Fatalf("track entity stamping: %+v, %v", es, err)
	}
	late := s[14].(Object) // last seen an hour after the sample time
	es, err = ComponentEntities(wrap(late))
	if err != nil || len(es) != 1 || es[0].ValidUntil.Before(es[0].ObservedAt) || es[0].Validate() != nil {
		t.Fatalf("late object entity: %+v, %v", es, err)
	}
	for _, i := range []int{1, 3, 6, 9, 11, 12, 15, 16, 17} { // mounted calibration, registration, tile, floor, geo reference, keyframe, alignment, layers
		es, err := ComponentEntities(wrap(s[i]))
		if err != nil || len(es) != 0 {
			t.Fatalf("%T must project no entities: %v, %v", s[i], es, err)
		}
	}
	bad := wrap(s[2])
	bad.Payload = []byte(`{}`)
	if _, err := ComponentEntities(bad); err == nil {
		t.Fatal("invalid component projected")
	}
}
