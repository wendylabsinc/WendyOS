package resultrecord

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/internal/testidentity"
	"github.com/wendylabsinc/wendy/go/world/production/proofref"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
)

type fixedClock struct{ window trustedtime.Interval }

func (c fixedClock) Now() (trustedtime.Interval, error) { return c.window, nil }

type fixture struct {
	result  Result
	request serviceorigin.PublishRequest
	inputs  []ValidatedInput
	proofs  [][]byte
}

func hash(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func setup(t *testing.T, origin world.Origin) fixture {
	t.Helper()
	physical := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	identity := testidentity.NewWith(t, physical, "spiffe://wendy.sh/tenant/"+testidentity.Tenant+"/device/22222222-2222-4222-8222-222222222222")
	certificates, err := wendycert.NewDevice(wendycert.Config{Tenant: identity.Tenant, Roots: [][]byte{identity.RootDER}})
	if err != nil {
		t.Fatal(err)
	}
	clock := fixedClock{trustedtime.Interval{Earliest: physical, Latest: physical.Add(time.Millisecond), BootID: "result-test-boot", Monotonic: time.Second, Domain: "physical", Authenticated: true, Source: "explicit-test-authenticated-anchor"}}
	domain, sourceDomain, sampled := "physical", "utc", physical
	if origin == world.Simulated {
		domain, sourceDomain, sampled = "simulation", "simulation:run-a", time.Unix(1700000000, 0).UTC()
	}
	frame := world.FrameRef{ID: "office-floor", Revision: "frame-v1"}
	f := fixture{result: Result{Version: Version, ResultSchema: "example.hazard.v1", ResultJSON: []byte("\n {\"location\":[1,2,3], \"uncertainty\":0.25,\"model\":{\"name\":\"development-model\",\"score\":1.00},\"big\":9007199254740993,\"optional\":null}\n"), RetainUntilUnixMS: physical.Add(7 * 24 * time.Hour).UnixMilli()}}
	f.request = serviceorigin.PublishRequest{Entity: "possible-hazard", ComponentType: ComponentType, Stream: "hazards", Frame: frame, Timing: world.Timing{SampleTime: sampled, ClockDomain: sourceDomain, ErrorKnown: true}, TTL: 1500 * time.Millisecond, Provenance: world.Provenance{Origin: world.Inferred, AppVersion: "domain-app-v1", ModelID: "development-model", ModelVersion: "1"}}
	for _, sensor := range []string{"gas-a", "gas-b"} {
		scope := serviceorigin.Scope{Tenant: identity.Tenant, Site: "office", Device: identity.Principal, App: "com.example.sensors", Service: sensor, DeploymentGeneration: 9, ExecutionDomain: domain, Publish: []serviceorigin.PublishScope{{ComponentType: "wendy.sensor.scalar.v1", Stream: sensor}}}
		publisher, err := serviceorigin.NewPublisher(serviceorigin.PublisherConfig{Scope: scope, Epoch: 7, Lifetime: 10 * time.Minute, Clock: clock, Certificates: certificates, ChainDER: identity.ChainDER, DeviceKey: identity.Key})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = publisher.Close() })
		provenance := world.Provenance{Origin: origin, AppVersion: "sensor-v1", ModelID: "source-model", ModelVersion: "v1"}
		if origin == world.Simulated {
			provenance.Simulation = &world.SimulationContext{RunID: "run-a", ScenarioVersion: "office-v1", Seed: 42, ClockDomain: sourceDomain}
		}
		request := serviceorigin.PublishRequest{Entity: "sensor/" + sensor, ComponentType: "wendy.sensor.scalar.v1", Stream: sensor, Frame: frame, Timing: world.Timing{SampleTime: sampled, ClockDomain: sourceDomain, ErrorKnown: true, ErrorBound: 10 * time.Millisecond}, TTL: 2 * time.Second, Provenance: provenance, Payload: []byte(`{"value":70,"unit":"ppm","sensor_type":"gas","calibration_revision":"cal-v1","uncertainty":2}`)}
		signed, err := publisher.Publish(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		verifier, err := serviceorigin.NewVerifier(serviceorigin.VerifierConfig{Tenant: identity.Tenant, Site: "office", ExecutionDomain: domain, Certificates: certificates, Clock: clock, Authorize: func(_ context.Context, claims serviceorigin.DelegationClaims, device wendycert.DeviceIdentity) error {
			if !reflect.DeepEqual(claims.Scope, scope) || claims.Epoch != 7 || device.Principal != identity.Principal {
				return errors.New("uncommissioned test source")
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
		component := verified.Component()
		envelope, _ := json.Marshal(signed)
		ref := proofref.Reference{Key: component.Key, Version: component.Version, EnvelopeSHA256: hash(envelope), RecordSHA256: hash(signed.Record)}
		physicalUntil := physical.Add(4 * time.Second)
		if sensor == "gas-b" {
			physicalUntil = physicalUntil.Add(-time.Second)
		}
		f.inputs = append(f.inputs, ValidatedInput{Reference: ref, Component: component, SampleValidUntil: sampled.Add(1800 * time.Millisecond), PhysicalValidUntil: physicalUntil})
		f.result.Inputs = append(f.result.Inputs, ref)
		f.request.Provenance.Inputs = append(f.request.Provenance.Inputs, world.Input{ID: ref.ID(), Origin: origin, Simulation: provenance.Simulation})
		f.proofs = append(f.proofs, envelope)
	}
	f.encode(t)
	return f
}

func (f *fixture) encode(t *testing.T) {
	t.Helper()
	var err error
	f.request.Payload, err = Encode(f.result)
	if err != nil {
		t.Fatal(err)
	}
}

func TestExactBodyProofReferencesAndIndependentDeadlines(t *testing.T) {
	for _, origin := range []world.Origin{world.Simulated, world.Measured} {
		t.Run(string(origin), func(t *testing.T) {
			f := setup(t, origin)
			// The trusted host list is a set; payload/provenance order is fixed.
			inputs := []ValidatedInput{f.inputs[1], f.inputs[0]}
			validated, err := ValidatePublication(f.request, inputs)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(validated.Result.ResultJSON, f.result.ResultJSON) || !reflect.DeepEqual(validated.Result.Inputs, f.result.Inputs) || !validated.SampleValidUntil.Equal(f.request.Timing.SampleTime.Add(f.request.TTL)) || !validated.PhysicalValidUntil.Equal(f.inputs[1].PhysicalValidUntil) || validated.Result.RetainUntilUnixMS != f.result.RetainUntilUnixMS {
				t.Fatal("validator changed original JSON, references or deadline domains")
			}
			if !time.UnixMilli(validated.Result.RetainUntilUnixMS).After(validated.PhysicalValidUntil) {
				t.Fatal("historical retention was confused with live validity")
			}
			for i, ref := range validated.Result.Inputs {
				var signed serviceorigin.SignedRecord
				_ = json.Unmarshal(f.proofs[i], &signed)
				if ref.EnvelopeSHA256 != hash(f.proofs[i]) || ref.RecordSHA256 != hash(signed.Record) || ref.Key != f.inputs[i].Component.Key || ref.Version != f.inputs[i].Component.Version || f.request.Provenance.Inputs[i].ID != ref.ID() {
					t.Fatal("input does not identify exact signed bytes and agent identity")
				}
			}
			validated.Result.ResultJSON[0] = ' '
			validated.Result.Inputs[0].Version.Sequence++
			again, err := ValidatePublication(f.request, f.inputs)
			if err != nil || !bytes.Equal(again.Result.ResultJSON, f.result.ResultJSON) || again.Result.Inputs[0].Version != f.result.Inputs[0].Version {
				t.Fatal("returned result aliases caller bytes", err)
			}
		})
	}
}

func TestRejectsForgedReferencesAndProvenanceRewrite(t *testing.T) {
	cases := map[string]func(*fixture){
		"envelope hash":       func(f *fixture) { f.result.Inputs[0].EnvelopeSHA256 = strings.Repeat("a", 64) },
		"inner record hash":   func(f *fixture) { f.result.Inputs[0].RecordSHA256 = strings.Repeat("b", 64) },
		"agent source":        func(f *fixture) { f.result.Inputs[0].Key.Source.Service = "impostor" },
		"agent sequence":      func(f *fixture) { f.result.Inputs[0].Version.Sequence++ },
		"entity":              func(f *fixture) { f.result.Inputs[0].Key.Entity = "other" },
		"component":           func(f *fixture) { f.result.Inputs[0].Key.Type = "example.other.v1" },
		"provenance proof id": func(f *fixture) { f.request.Provenance.Inputs[0].ID = "sha256:" + strings.Repeat("a", 64) },
		"provenance omitted":  func(f *fixture) { f.request.Provenance.Inputs = f.request.Provenance.Inputs[:1] },
		"provenance reordered": func(f *fixture) {
			f.request.Provenance.Inputs[0], f.request.Provenance.Inputs[1] = f.request.Provenance.Inputs[1], f.request.Provenance.Inputs[0]
		},
		"simulation rewritten": func(f *fixture) {
			sim := *f.request.Provenance.Inputs[0].Simulation
			sim.Seed++
			f.request.Provenance.Inputs[0].Simulation = &sim
		},
		"simulated to measured": func(f *fixture) {
			f.request.Provenance.Inputs[0].Origin, f.request.Provenance.Inputs[0].Simulation = world.Measured, nil
		},
		"inferred output downgraded": func(f *fixture) {
			f.request.Provenance = world.Provenance{Origin: world.Measured, AppVersion: "app-v1"}
		},
		"missing host input":     func(f *fixture) { f.inputs = f.inputs[:1] },
		"duplicate host input":   func(f *fixture) { f.inputs[1] = f.inputs[0] },
		"extra host input":       func(f *fixture) { f.inputs = append(f.inputs, f.inputs[0]) },
		"host component version": func(f *fixture) { f.inputs[0].Component.Version.Sequence++ },
		"host component source":  func(f *fixture) { f.inputs[0].Component.Key.Source.Service = "wrong-host-projection" },
		"frame":                  func(f *fixture) { f.request.Frame.Revision = "different-frame" },
		"clock domain":           func(f *fixture) { f.request.Timing.ClockDomain = "simulation:another-run" },
		"tombstone":              func(f *fixture) { f.request.Tombstone = true },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			f := setup(t, world.Simulated)
			change(&f)
			f.encode(t)
			if _, err := ValidatePublication(f.request, f.inputs); err == nil {
				t.Fatal("accepted forged or rewritten derived binding")
			}
		})
	}
}

func TestRejectsDerivedReplayMixedAndUnboundedHostInputs(t *testing.T) {
	for name, change := range map[string]func(*fixture){
		"derived leaf": func(f *fixture) {
			f.inputs[0].Component.Provenance = world.Provenance{Origin: world.Inferred, AppVersion: "app", ModelID: "model", ModelVersion: "v1", Inputs: []world.Input{{ID: "hidden-leaf", Origin: world.Measured}}}
		},
		"replay leaf": func(f *fixture) {
			original := f.inputs[0].Component.Timing.SampleTime
			p := &f.inputs[0].Component.Provenance
			p.Origin, p.OriginalCaptureTime, p.OriginalClockDomain = world.Replayed, &original, "original"
		},
		"mixed raw origins": func(f *fixture) {
			f.inputs[0].Component.Provenance = world.Provenance{Origin: world.Measured, AppVersion: "sensor"}
			f.request.Provenance.Inputs[0].Origin, f.request.Provenance.Inputs[0].Simulation = world.Measured, nil
		},
		"mixed simulation runs": func(f *fixture) {
			c := &f.inputs[0].Component
			sim := *c.Provenance.Simulation
			sim.RunID = "other-run"
			c.Provenance.Simulation = &sim
			f.request.Provenance.Inputs[0].Simulation = &sim
		},
		"mixed sites": func(f *fixture) {
			f.inputs[0].Reference.Key.Site = "other-site"
			f.inputs[0].Component.Key.Site = "other-site"
			f.result.Inputs[0] = f.inputs[0].Reference
		},
		"input tombstone":        func(f *fixture) { f.inputs[0].Component.Tombstone = true },
		"unknown input error":    func(f *fixture) { f.inputs[0].Component.Timing.ErrorKnown = false },
		"zero source deadline":   func(f *fixture) { f.inputs[0].SampleValidUntil = time.Time{} },
		"zero physical deadline": func(f *fixture) { f.inputs[0].PhysicalValidUntil = time.Time{} },
		"source deadline beyond raw TTL": func(f *fixture) {
			f.inputs[0].SampleValidUntil = f.inputs[0].Component.Timing.SampleTime.Add(3 * time.Second)
		},
		"source deadline before sample": func(f *fixture) { f.inputs[0].SampleValidUntil = f.inputs[0].Component.Timing.SampleTime },
		"input age already expired":     func(f *fixture) { f.inputs[0].Component.Timing.AgeHint = f.inputs[0].Component.TTL },
	} {
		t.Run(name, func(t *testing.T) {
			f := setup(t, world.Simulated)
			change(&f)
			f.encode(t)
			if _, err := ValidatePublication(f.request, f.inputs); err == nil {
				t.Fatal("accepted unsupported ancestry or missing effective host bounds")
			}
		})
	}
}

func TestTTLAndUncertaintyCannotExceedOldestInputDeadline(t *testing.T) {
	f := setup(t, world.Simulated)
	f.request.TTL = 1700 * time.Millisecond
	f.request.Timing.ErrorBound = 100 * time.Millisecond
	validated, err := ValidatePublication(f.request, f.inputs)
	if err != nil || !validated.SampleValidUntil.Equal(f.request.Timing.SampleTime.Add(1600*time.Millisecond)) {
		t.Fatal("conservative boundary refused or changed", err)
	}
	f.request.TTL++
	if _, err := ValidatePublication(f.request, f.inputs); err == nil {
		t.Fatal("output latest possible end exceeded required input")
	}
	f.request.TTL = 2 * time.Second
	f.request.Timing.ErrorBound = 500 * time.Millisecond
	f.request.Timing.AgeHint = time.Second
	if _, err := ValidatePublication(f.request, f.inputs); err == nil {
		t.Fatal("large error/age was used to justify extending nominal output TTL")
	}
	f.request.TTL = time.Second
	f.request.Timing.ErrorBound = 100 * time.Millisecond
	f.request.Timing.AgeHint = 900 * time.Millisecond
	if _, err := ValidatePublication(f.request, f.inputs); err == nil {
		t.Fatal("already exhausted error/age budget accepted")
	}
}

func TestResultJSONPreservesArbitraryDomainFieldsButRejectsAmbiguity(t *testing.T) {
	f := setup(t, world.Simulated)
	for name, body := range map[string][]byte{
		"duplicate nested": []byte(`{"domain":{"value":1,"value":2}}`),
		"invalid Unicode":  []byte(`{"domain":"\ud800"}`),
		"invalid UTF8":     append([]byte(`{"domain":"`), 0xff, '"', '}'),
		"trailing object":  []byte(`{"one":1}{"two":2}`),
		"array root":       []byte(`[1,2]`),
		"null root":        []byte(`null`),
		"too deep":         []byte(`{"domain":` + strings.Repeat("[", 40) + "0" + strings.Repeat("]", 40) + `}`),
		"too large":        append(bytes.Repeat([]byte(" "), MaxResultBytes), []byte(`{}`)...),
	} {
		t.Run(name, func(t *testing.T) {
			r := f.result
			r.ResultJSON = body
			if _, err := Encode(r); err == nil {
				t.Fatal("accepted ambiguous or oversized result body")
			}
		})
	}
	r := f.result
	r.ResultJSON = append(bytes.Repeat([]byte(" "), MaxResultBytes-2), []byte(`{}`)...)
	if err := r.Validate(); err != nil {
		t.Fatal("structural body should fit its own bound", err)
	}
	if _, err := Encode(r); err == nil {
		t.Fatal("base64 expansion and references exceeded full payload budget")
	}
}

func TestStrictEnvelopeFieldsAndDuplicateReferences(t *testing.T) {
	f := setup(t, world.Simulated)
	raw := f.request.Payload
	for _, name := range []string{"version", "result_schema", "result_json", "inputs", "retain_until_unix_ms"} {
		for _, missing := range []bool{true, false} {
			var fields map[string]json.RawMessage
			_ = json.Unmarshal(raw, &fields)
			if missing {
				delete(fields, name)
			} else {
				fields[name] = []byte("null")
			}
			bad, _ := json.Marshal(fields)
			if _, err := Decode(bad); err == nil {
				t.Fatalf("accepted missing/null required field %s", name)
			}
		}
	}
	for name, bad := range map[string][]byte{
		"duplicate":      bytes.Replace(raw, []byte(`"version":1`), []byte(`"version":1,"version":1`), 1),
		"case alias":     bytes.Replace(raw, []byte(`"result_schema"`), []byte(`"Result_schema"`), 1),
		"unknown":        append(append([]byte{}, raw[:len(raw)-1]...), []byte(`,"source":"impostor"}`)...),
		"trailing":       append(append([]byte{}, raw...), []byte(` {}`)...),
		"large envelope": bytes.Repeat([]byte(" "), MaxPayloadBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode(bad); err == nil {
				t.Fatal("accepted malformed envelope")
			}
		})
	}
	for name, change := range map[string]func(*Result){
		"same proof":                          func(r *Result) { r.Inputs[1] = r.Inputs[0] },
		"same source version different proof": func(r *Result) { r.Inputs[1].Key, r.Inputs[1].Version = r.Inputs[0].Key, r.Inputs[0].Version },
		"same record different envelope":      func(r *Result) { r.Inputs[1].RecordSHA256 = r.Inputs[0].RecordSHA256 },
		"invalid inner digest":                func(r *Result) { r.Inputs[0].RecordSHA256 = "no-hash" },
		"zero retention":                      func(r *Result) { r.RetainUntilUnixMS = 0 },
		"invalid version":                     func(r *Result) { r.Version++ },
		"missing refs":                        func(r *Result) { r.Inputs = nil },
		"too many refs":                       func(r *Result) { r.Inputs = make([]proofref.Reference, MaxInputs+1) },
	} {
		t.Run(name, func(t *testing.T) {
			r, err := Decode(raw)
			if err != nil {
				t.Fatal(err)
			}
			change(&r)
			if _, err := Encode(r); err == nil {
				t.Fatal("accepted invalid or conflicting input references")
			}
		})
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	fields["result_json"] = []byte(`[123,125]`)
	arrayBytes, _ := json.Marshal(fields)
	if _, err := Decode(arrayBytes); err == nil {
		t.Fatal("accepted byte array instead of base64 original result JSON")
	}
}
