package replication

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/fusioncapture"
	"github.com/wendylabsinc/wendy/go/world/production/internal/testidentity"
	"github.com/wendylabsinc/wendy/go/world/production/proofref"
	"github.com/wendylabsinc/wendy/go/world/production/resultrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
)

// These unsigned requests were produced by fusioncapture's generated-identity
// capture/evaluation fixture. They contain public derived metadata and proof
// references, not camera credentials or media. Local input admission is tested
// by fusioncapture; the real result signature is generated below.
func hazardRequest(t *testing.T, name string) (serviceorigin.Scope, serviceorigin.PublishRequest) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "hazard-"+name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var request serviceorigin.PublishRequest
	if err = json.Unmarshal(raw, &request); err != nil {
		t.Fatal(err)
	}
	result, err := resultrecord.Decode(request.Payload)
	if err != nil {
		t.Fatal(err)
	}
	key := result.Inputs[0].Key
	scope := serviceorigin.Scope{Tenant: key.Tenant, Site: key.Site, Device: key.Source.Device, App: "com.example.fusion", Service: "fusion", DeploymentGeneration: 1, ExecutionDomain: "simulation", Publish: []serviceorigin.PublishScope{{ComponentType: resultrecord.ComponentType, Stream: request.Stream}}}
	return scope, request
}

func TestTypedHazardDispatchPreservesOriginalBoundsAndBytes(t *testing.T) {
	for name, count := range map[string]int{"four": 4, "lidar": 7} {
		t.Run(name, func(t *testing.T) {
			scope, request := hazardRequest(t, name)
			before, _ := json.Marshal(request)
			decoded, err := fusioncapture.ValidateReplicaPublication(request)
			if err != nil || len(decoded.Result.Inputs) != count {
				t.Fatal("invalid evaluated fixture", err)
			}
			until, err := RecordInterpretationUntil(serviceorigin.Record{Scope: scope, Publication: request})
			if err != nil || until.IsZero() || !until.Equal(decoded.PhysicalValidUntil) {
				t.Fatal("physical interpretation deadline changed", until, err)
			}
			// The helper has no receipt-time clock: even an expired original
			// retains its cap for repair; live readers enforce that cap later.
			after, _ := json.Marshal(request)
			if !bytes.Equal(before, after) || !decoded.SampleValidUntil.Equal(request.Timing.SampleTime.Add(request.TTL)) {
				t.Fatal("original result or sample deadline changed")
			}
		})
	}
}

func TestTypedHazardRejectsUnknownSchemaAndChangedLineage(t *testing.T) {
	for _, mode := range []string{"tenant", "site", "unbound-device-change", "foreign-tenant-input", "unknown-schema", "replay-schema", "missing-physical-cap", "unknown-field", "changed-input-version", "extended-ttl", "reordered-provenance", "tombstone-payload"} {
		t.Run(mode, func(t *testing.T) {
			scope, request := hazardRequest(t, "lidar")
			result, _ := resultrecord.Decode(request.Payload)
			var body map[string]json.RawMessage
			if err := json.Unmarshal(result.ResultJSON, &body); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "tenant":
				scope.Tenant = "55555555-5555-4555-8555-555555555555"
			case "site":
				scope.Site = "another-site"
			case "unbound-device-change":
				result.Inputs[2].Key.Source.Device = deviceC
			case "foreign-tenant-input":
				var inputs []map[string]json.RawMessage
				_ = json.Unmarshal(body["inputs"], &inputs)
				result.Inputs[2].Key.Tenant = "55555555-5555-4555-8555-555555555555"
				result.Inputs[2].Key.Source.Device = "spiffe://wendy.sh/tenant/55555555-5555-4555-8555-555555555555/device/44444444-4444-4444-8444-444444444444"
				inputs[2]["source"], _ = json.Marshal(result.Inputs[2].Key.Source)
				body["inputs"], _ = json.Marshal(inputs)
				request.Provenance.Inputs[2].ID = result.Inputs[2].ID()
			case "unknown-schema":
				result.ResultSchema = "wendy.unknown.v1"
			case "replay-schema":
				result.ResultSchema = "wendy.hazard.replay.v1"
			case "missing-physical-cap":
				delete(body, "physical_valid_until")
			case "unknown-field":
				body["unrecognized"] = json.RawMessage(`true`)
			case "changed-input-version":
				result.Inputs[0].Version.Sequence++
			case "extended-ttl":
				request.TTL += time.Second
			case "reordered-provenance":
				request.Provenance.Inputs[0], request.Provenance.Inputs[1] = request.Provenance.Inputs[1], request.Provenance.Inputs[0]
			case "tombstone-payload":
				request.Tombstone = true
			}
			result.ResultJSON, _ = json.Marshal(body)
			var err error
			request.Payload, err = resultrecord.Encode(result)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = RecordInterpretationUntil(serviceorigin.Record{Scope: scope, Publication: request}); !errors.Is(err, ErrProtocol) {
				t.Fatal("changed typed hazard accepted", err)
			}
		})
	}
	scope, request := hazardRequest(t, "four")
	request.Tombstone, request.Payload = true, nil
	if until, err := RecordInterpretationUntil(serviceorigin.Record{Scope: scope, Publication: request}); err != nil || !until.IsZero() {
		t.Fatal("empty result tombstone refused", until, err)
	}
}

func TestRealSignedHazardEnvelopeSurvivesRepair(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		t.Run(map[bool]string{false: "own-inputs", true: "mixed-device-inputs"}[mixed], func(t *testing.T) {
			scope, request := hazardRequest(t, "lidar")
			if mixed {
				result, _ := resultrecord.Decode(request.Payload)
				var body map[string]json.RawMessage
				_ = json.Unmarshal(result.ResultJSON, &body)
				var inputs []map[string]json.RawMessage
				_ = json.Unmarshal(body["inputs"], &inputs)
				changed := false
				for i := range result.Inputs {
					if result.Inputs[i].Key.Source.Stream != "gas-1" {
						continue
					}
					result.Inputs[i].Key.Source.Device = deviceB
					inputs[i]["source"], _ = json.Marshal(result.Inputs[i].Key.Source)
					changed = true
				}
				if !changed {
					t.Fatal("fixture has no gas input")
				}
				body["inputs"], _ = json.Marshal(inputs)
				result.ResultJSON, _ = json.Marshal(body)
				request.Payload, _ = resultrecord.Encode(result)
			}
			view, err := fusioncapture.ValidateReplicaPublication(request)
			if err != nil {
				t.Fatal(err)
			}
			now := view.PhysicalValidUntil.Add(-time.Second)
			identity := testidentity.NewWith(t, now, scope.Device)
			certificates, err := wendycert.NewDevice(wendycert.Config{Tenant: scope.Tenant, Roots: [][]byte{identity.RootDER}})
			if err != nil {
				t.Fatal(err)
			}
			clock := envelopeClock{trustedtime.Interval{Earliest: now, Latest: now.Add(time.Millisecond), BootID: "hazard-test", Monotonic: time.Second, Domain: "physical", Authenticated: true, Source: "test-only-anchor"}}
			publisher, err := serviceorigin.NewPublisher(serviceorigin.PublisherConfig{Scope: scope, Epoch: 2, Lifetime: time.Minute, Clock: clock, Certificates: certificates, ChainDER: identity.ChainDER, DeviceKey: identity.Key})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = publisher.Close() })
			ctx := context.Background()
			signed, err := publisher.Publish(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			verifier, err := serviceorigin.NewVerifier(serviceorigin.VerifierConfig{Tenant: scope.Tenant, Site: scope.Site, ExecutionDomain: scope.ExecutionDomain, Certificates: certificates, Clock: clock, Authorize: func(_ context.Context, claims serviceorigin.DelegationClaims, identity wendycert.DeviceIdentity) error {
				if !reflect.DeepEqual(claims.Scope, scope) || claims.Epoch != 2 || identity.Principal != scope.Device {
					return serviceorigin.ErrDenied
				}
				return nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			verified, err := verifier.Verify(ctx, signed)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(signed)
			component := verified.Component()
			item := Item{Entry: Entry{Kind: Record, Reference: proofref.Reference{Key: component.Key, Version: component.Version, EnvelopeSHA256: hash(raw), RecordSHA256: hash(signed.Record)}, ExpiresAtUnixNS: request.Timing.SampleTime.Add(request.TTL).UnixNano()}, Envelope: raw}
			f := setup(t, item)
			for _, service := range []*Service{f.a, f.b} {
				service.cfg.Endpoint.Site = scope.Site
				service.cfg.Endpoint.SampleClockDomain = request.Timing.ClockDomain
				service.cfg.Authorize = func(context.Context, Identity, Direction) (Access, error) {
					return Access{Generation: 1, Sources: []proofref.Source{item.Entry.Reference.Source()}}, nil
				}
			}
			f.b.cfg.RemoteAuthority = func(ctx context.Context, item Item) error {
				var received serviceorigin.SignedRecord
				if err := json.Unmarshal(item.Envelope, &received); err != nil {
					return err
				}
				_, err := verifier.Verify(ctx, received)
				return err
			}
			result, err := f.b.SyncPeer(ctx, scope.Device)
			if err != nil || !result.Complete || result.Admitted != 1 || len(f.local.admitted) != 1 || !bytes.Equal(f.local.admitted[0].Envelope, raw) {
				t.Fatal("signed hazard changed during repair", result, err)
			}
			if mixed {
				var received serviceorigin.SignedRecord
				var record serviceorigin.Record
				_ = json.Unmarshal(f.local.admitted[0].Envelope, &received)
				_ = json.Unmarshal(received.Record, &record)
				got, err := fusioncapture.ValidateReplicaPublication(record.Publication)
				if err != nil || !reflect.DeepEqual(got.Result.Inputs, view.Result.Inputs) || !got.PhysicalValidUntil.Equal(view.PhysicalValidUntil) {
					t.Fatal("mixed-origin lineage or deadline changed", err)
				}
			}
		})
	}
}
