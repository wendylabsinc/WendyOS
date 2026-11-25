package replication

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/internal/testidentity"
	"github.com/wendylabsinc/wendy/go/world/production/proofref"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
)

type envelopeClock struct{ window trustedtime.Interval }

func (c envelopeClock) Now() (trustedtime.Interval, error) { return c.window, nil }

func TestLargeRealSignedScalarEnvelopeSurvivesRepair(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1794400000, 0).UTC()
	identity := testidentity.NewWith(t, now, deviceA)
	certificates, err := wendycert.NewDevice(wendycert.Config{Tenant: tenant, Roots: [][]byte{identity.RootDER}})
	if err != nil {
		t.Fatal(err)
	}
	clock := envelopeClock{trustedtime.Interval{Earliest: now, Latest: now.Add(time.Millisecond), BootID: "test-only", Monotonic: time.Second, Domain: "physical", Authenticated: true, Source: "test-only-anchor"}}
	scope := serviceorigin.Scope{Tenant: tenant, Site: "office", Device: deviceA, App: source(deviceA).App, Service: source(deviceA).Service, DeploymentGeneration: 3, ExecutionDomain: "simulation", Publish: []serviceorigin.PublishScope{{ComponentType: ScalarComponent, Stream: "gas"}}}
	publisher, err := serviceorigin.NewPublisher(serviceorigin.PublisherConfig{Scope: scope, Epoch: 2, Lifetime: time.Minute, Clock: clock, Certificates: certificates, ChainDER: identity.ChainDER, DeviceKey: identity.Key})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = publisher.Close() })
	payload, err := json.Marshal(world.ScalarSample{Value: 1, Unit: "ppm", SensorType: "gas", CalibrationRevision: strings.Repeat("c", 30000), Uncertainty: 0.1})
	if err != nil {
		t.Fatal(err)
	}
	request := serviceorigin.PublishRequest{Entity: "gas", ComponentType: ScalarComponent, Stream: "gas", Frame: world.FrameRef{ID: "floor", Revision: "one"}, Timing: world.Timing{SampleTime: now, ClockDomain: endpoint(deviceA).SampleClockDomain, ErrorKnown: true}, TTL: 3 * time.Second, Payload: payload, Provenance: world.Provenance{Origin: world.Simulated, AppVersion: "test", Simulation: &world.SimulationContext{RunID: "replication-test", ScenarioVersion: "v1", Seed: 1, ClockDomain: endpoint(deviceA).SampleClockDomain}}}
	signed, err := publisher.Publish(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(signed)
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) > serviceorigin.MaxPayloadBytes || len(signed.Record) > serviceorigin.MaxRecordBytes || len(raw) <= serviceorigin.MaxRecordBytes || len(raw) > MaxEnvelopeBytes {
		t.Fatalf("invalid bound regression fixture: payload=%d record=%d envelope=%d", len(payload), len(signed.Record), len(raw))
	}
	verifier, err := serviceorigin.NewVerifier(serviceorigin.VerifierConfig{Tenant: tenant, Site: "office", ExecutionDomain: "simulation", Certificates: certificates, Clock: clock, Authorize: func(_ context.Context, c serviceorigin.DelegationClaims, d wendycert.DeviceIdentity) error {
		if !reflect.DeepEqual(c.Scope, scope) || c.Epoch != 2 || d.Principal != deviceA {
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
	c := verified.Component()
	item := Item{Entry: Entry{Kind: Record, Reference: proofref.Reference{Key: c.Key, Version: c.Version, EnvelopeSHA256: hash(raw), RecordSHA256: hash(signed.Record)}, ExpiresAtUnixNS: now.Add(request.TTL).UnixNano()}, Envelope: raw}
	f := setup(t, item)
	// Exercise the real original-proof verifier at both injected admission
	// boundaries. This generated test issuer is not a runtime trust claim.
	f.b.cfg.RemoteAuthority = func(ctx context.Context, i Item) error {
		var proof serviceorigin.SignedRecord
		if err := authority.DecodeStrictLimit(i.Envelope, &proof, MaxEnvelopeBytes); err != nil {
			return err
		}
		_, err := verifier.Verify(ctx, proof)
		return err
	}
	result, err := f.b.SyncPeer(ctx, deviceA)
	if err != nil || !result.Complete || result.Admitted != 1 || !bytes.Equal(f.local.admitted[0].Envelope, raw) {
		t.Fatal(result, err)
	}
	t.Logf("preserved original payload=%d, inner record=%d, signed envelope=%d bytes", len(payload), len(signed.Record), len(raw))

	for _, mode := range []string{"unknown-outer", "duplicate-outer", "oversized-outer", "oversized-inner"} {
		t.Run(mode, func(t *testing.T) {
			bad := cloneItem(item)
			switch mode {
			case "unknown-outer":
				bad.Envelope = append([]byte(`{"unknown":1,`), raw[1:]...)
			case "duplicate-outer":
				bad.Envelope = append([]byte(`{"signature":"AA==",`), raw[1:]...)
			case "oversized-outer":
				bad.Envelope = bytes.Repeat([]byte(" "), MaxEnvelopeBytes+1)
			case "oversized-inner":
				copy := signed
				copy.Record = append(bytes.Clone(signed.Record), bytes.Repeat([]byte(" "), serviceorigin.MaxRecordBytes+1-len(signed.Record))...)
				bad.Envelope, _ = json.Marshal(copy)
				bad.Entry.Reference.RecordSHA256 = hash(copy.Record)
			}
			bad.Entry.Reference.EnvelopeSHA256 = hash(bad.Envelope)
			if ValidateItem(bad, endpoint(deviceA), deviceA) == nil {
				t.Fatal("invalid envelope accepted")
			}
		})
	}
}
