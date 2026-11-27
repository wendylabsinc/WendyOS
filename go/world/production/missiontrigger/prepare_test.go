package missiontrigger

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/fusion"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/fusioncapture"
	"github.com/wendylabsinc/wendy/go/world/production/internal/testidentity"
	"github.com/wendylabsinc/wendy/go/world/production/internal/testtrack"
	"github.com/wendylabsinc/wendy/go/world/production/resultrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trackcapture"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
)

type fixtureClock struct{ interval trustedtime.Interval }

func (c fixtureClock) Now() (trustedtime.Interval, error) { return c.interval, nil }

func request(t *testing.T, kind string) (serviceorigin.Scope, serviceorigin.PublishRequest, time.Time) {
	t.Helper()
	if kind == "hazard" {
		// Shared checked-in public metadata fixture; no images, keys or signatures.
		raw, err := os.ReadFile("../replication/testdata/hazard-lidar.json")
		if err != nil {
			t.Fatal(err)
		}
		var p serviceorigin.PublishRequest
		if json.Unmarshal(raw, &p) != nil {
			t.Fatal("hazard fixture")
		}
		h, err := fusioncapture.ValidateReplicaPublication(p)
		if err != nil {
			t.Fatal(err)
		}
		key := h.Result.Inputs[0].Key
		scope := serviceorigin.Scope{Tenant: key.Tenant, Site: key.Site, Device: key.Source.Device, App: "com.example.fusion", Service: "fusion", DeploymentGeneration: 3, ExecutionDomain: "simulation", Publish: []serviceorigin.PublishScope{{ComponentType: resultrecord.ComponentType, Stream: p.Stream}}}
		return scope, p, h.PhysicalValidUntil.Add(-time.Second)
	}
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	scope := serviceorigin.Scope{Tenant: "11111111-1111-4111-8111-111111111111", Site: "office", Device: "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/device/22222222-2222-4222-8222-222222222222", App: "com.example.people", Service: "people", DeploymentGeneration: 4, ExecutionDomain: "simulation", Publish: []serviceorigin.PublishScope{{ComponentType: resultrecord.ComponentType, Stream: "tracks"}}}
	return scope, testtrack.Request(t, scope, world.FrameRef{ID: "floor", Revision: "one"}, "test-run", now, now), now
}

func sign(t *testing.T, scope serviceorigin.Scope, p serviceorigin.PublishRequest, now time.Time) Input {
	t.Helper()
	return signLifetime(t, scope, p, now, time.Minute)
}

func signLifetime(t *testing.T, scope serviceorigin.Scope, p serviceorigin.PublishRequest, now time.Time, lifetime time.Duration) Input {
	t.Helper()
	id := testidentity.NewWith(t, now, scope.Device)
	certificates, err := wendycert.NewDevice(wendycert.Config{Tenant: scope.Tenant, Roots: [][]byte{id.RootDER}})
	if err != nil {
		t.Fatal(err)
	}
	clock := fixtureClock{trustedtime.Interval{Earliest: now, Latest: now.Add(time.Millisecond), BootID: "mission-trigger-test", Monotonic: time.Second, Domain: "physical", Authenticated: true, Source: "test-issued"}}
	publisher, err := serviceorigin.NewPublisher(serviceorigin.PublisherConfig{Scope: scope, Epoch: 7, Lifetime: lifetime, Clock: clock, Certificates: certificates, ChainDER: id.ChainDER, DeviceKey: id.Key})
	if err != nil {
		t.Fatal(err)
	}
	defer publisher.Close()
	signed, err := publisher.Publish(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := serviceorigin.NewVerifier(serviceorigin.VerifierConfig{Tenant: scope.Tenant, Site: scope.Site, ExecutionDomain: scope.ExecutionDomain, Certificates: certificates, Clock: clock, Authorize: func(_ context.Context, claims serviceorigin.DelegationClaims, device wendycert.DeviceIdentity) error {
		if !reflect.DeepEqual(claims.Scope, scope) || claims.Epoch != 7 || device.Principal != scope.Device {
			return serviceorigin.ErrDenied
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	verified, err := verifier.Verify(context.Background(), signed)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.MarshalIndent(signed, "", " ")
	return Input{Verified: verified, Envelope: raw}
}

func bound(in Input) Binding {
	c := in.Verified.Component()
	return Binding{Tenant: c.Key.Tenant, Site: c.Key.Site, Frame: c.Frame, FloorY: .25}
}

func TestPreparedNativeTriggerKeepsOriginalIdentityAndEvidence(t *testing.T) {
	for _, kind := range []string{"hazard", "person"} {
		t.Run(kind, func(t *testing.T) {
			scope, p, now := request(t, kind)
			p.Timing.AgeHint = 100 * time.Millisecond
			in := sign(t, scope, p, now)
			before, _ := json.Marshal(in.Verified)
			got, err := Prepare(in, bound(in))
			if err != nil {
				t.Fatal(err)
			}
			want := in.Verified.Component()
			want.Timing.AgeHint = 0
			canonical, _ := json.Marshal(want)
			if !reflect.DeepEqual(got.Trigger.Component, want) || got.Trigger.SHA256 != digest(canonical) || got.Trigger.Component.Key.Type != resultrecord.ComponentType || !bytes.Equal(got.Envelope, in.Envelope) || got.EnvelopeSHA256 != digest(in.Envelope) {
				t.Fatal("native original changed")
			}
			if !got.SampleValidUntil.Equal(p.Timing.SampleTime.Add(p.TTL - p.Timing.AgeHint)) {
				t.Fatal("original age or deadline changed")
			}
			if kind == "hazard" {
				h, _ := fusioncapture.ValidateReplicaPublication(p)
				if got.ResultSchema != fusion.HazardType || got.Target != h.Hazard.PositionM || !got.PhysicalValidUntil.Equal(h.PhysicalValidUntil) {
					t.Fatal("hazard location or cap changed")
				}
			} else {
				track, _ := trackcapture.ValidatePublication(p)
				if got.ResultSchema != trackcapture.ResultSchema || got.Target != [3]float64{1, .25, 2} || !got.PhysicalValidUntil.Equal(track.PhysicalValidUntil()) {
					t.Fatal("person target or cap changed")
				}
			}
			if got.Trigger.Validate(scope.Site, p.Frame) != nil {
				t.Fatal("core tasking refused prepared native trigger")
			}
			core, err := tasking.TriggerFromComponent(in.Verified.Component())
			if err != nil || core.SHA256 != got.Trigger.SHA256 || !reflect.DeepEqual(core.Component, got.Trigger.Component) {
				t.Fatal("shared core trigger changed native identity", err)
			}
			after, _ := json.Marshal(in.Verified)
			if !bytes.Equal(before, after) {
				t.Fatal("preparation mutated verified input")
			}
			in.Envelope[0] = 'X'
			in.Verified.Record.Publication.Payload[0] = 'X'
			in.Verified.Record.Publication.Provenance.Inputs[0].Simulation.RunID = "changed"
			if !json.Valid(got.Envelope) || !json.Valid(got.Trigger.Component.Payload) || !reflect.DeepEqual(got.Trigger.Component, want) {
				t.Fatal("prepared result aliases caller memory")
			}
		})
	}
}

func TestPersonTargetIsObservedPositionWithoutPursuit(t *testing.T) {
	scope, p, now := request(t, "person")
	r, _ := resultrecord.Decode(p.Payload)
	track, _ := trackcapture.Decode(r.ResultJSON)
	previous := track.Current[0]
	previous.Proof.Version.Sequence++
	previous.Proof.EnvelopeSHA256 = digest([]byte("previous-proof"))
	previous.Proof.RecordSHA256 = digest([]byte("previous-record"))
	previous.SampleID = "test-run/camera-a/2"
	previous.CaptureSequence++
	previous.SampleTime = previous.SampleTime.Add(-100 * time.Millisecond)
	track.Previous = []trackcapture.Member{previous}
	track.PreviousObservedAt = &previous.SampleTime
	track.VelocityXZ = &[2]float64{10, -5}
	track.VelocityCovariance = &[4]float64{.2, 0, 0, .2}
	track.MotionKnown = true
	r.Inputs = append(r.Inputs, previous.Proof)
	r.ResultJSON, _ = json.Marshal(track)
	p.Payload, _ = resultrecord.Encode(r)
	p.Timing.SampleTime = previous.SampleTime
	p.TTL += 100 * time.Millisecond
	leaf := p.Provenance.Inputs[0]
	leaf.ID = previous.Proof.ID()
	p.Provenance.Inputs = append(p.Provenance.Inputs, leaf)
	if _, err := trackcapture.ValidatePublication(p); err != nil {
		t.Fatal(err)
	}
	in := sign(t, scope, p, now)
	got, err := Prepare(in, bound(in))
	if err != nil || got.Target != [3]float64{1, .25, 2} || !bytes.Equal(got.Trigger.Component.Payload, p.Payload) {
		t.Fatal("target extrapolated or original motion metadata changed", got.Target, err)
	}
}

func TestPreparedPhysicalBoundKeepsShorterOriginalSigningDeadline(t *testing.T) {
	scope, p, now := request(t, "person")
	in := signLifetime(t, scope, p, now, 500*time.Millisecond)
	got, err := Prepare(in, bound(in))
	if err != nil || !got.PhysicalValidUntil.Equal(time.UnixMilli(in.Verified.Record.ExpiresAtUnixMS)) || !got.SampleValidUntil.Equal(p.Timing.SampleTime.Add(p.TTL)) {
		t.Fatal("original signing or independent sample deadline changed", err)
	}
}

func TestPreparationRefusesWrongBindingOrChangedVerifiedProjection(t *testing.T) {
	for _, mode := range []string{"tenant", "site", "frame", "floor", "empty", "large", "malformed", "inner-bytes", "projection", "delegation", "device"} {
		t.Run(mode, func(t *testing.T) {
			scope, p, now := request(t, "person")
			in := sign(t, scope, p, now)
			binding := bound(in)
			switch mode {
			case "tenant":
				binding.Tenant = "other"
			case "site":
				binding.Site = "other"
			case "frame":
				binding.Frame.Revision = "other"
			case "floor":
				binding.FloorY = math.NaN()
			case "empty":
				in.Envelope = nil
			case "large":
				in.Envelope = make([]byte, MaxEnvelopeBytes+1)
			case "malformed":
				in.Envelope = []byte(`{"record":null}`)
			case "inner-bytes":
				in.Verified.Bytes = append(in.Verified.Bytes, ' ')
			case "projection":
				in.Verified.Record.Sequence++
			case "delegation":
				in.Verified.Delegation.Epoch++
			case "device":
				in.Verified.Device.Principal = "other"
			}
			if _, err := Prepare(in, binding); err == nil {
				t.Fatal("changed original or binding accepted")
			}
		})
	}
}

func TestPreparationRefusesSignedUnsupportedOrMalformedTypedResults(t *testing.T) {
	for _, mode := range []string{"legacy-type", "unknown-schema", "unknown-field", "missing-hazard-cap", "tombstone", "foreign-input-site", "exhausted-age"} {
		t.Run(mode, func(t *testing.T) {
			scope, p, now := request(t, "hazard")
			r, _ := resultrecord.Decode(p.Payload)
			var body map[string]json.RawMessage
			_ = json.Unmarshal(r.ResultJSON, &body)
			switch mode {
			case "legacy-type":
				p.ComponentType = fusion.HazardType
				scope.Publish[0].ComponentType = p.ComponentType
			case "unknown-schema":
				r.ResultSchema = "wendy.unknown.v1"
			case "unknown-field":
				body["pursuit"] = []byte("true")
			case "missing-hazard-cap":
				delete(body, "physical_valid_until")
			case "tombstone":
				p.Tombstone = true
			case "foreign-input-site":
				r.Inputs[0].Key.Site = "other"
			case "exhausted-age":
				p.Timing.AgeHint = p.TTL
			}
			r.ResultJSON, _ = json.Marshal(body)
			p.Payload, _ = resultrecord.Encode(r)
			if p.Tombstone {
				p.Payload = nil
			}
			in := sign(t, scope, p, now)
			if _, err := Prepare(in, bound(in)); err == nil {
				t.Fatal("unsupported typed inference accepted")
			}
		})
	}
}
