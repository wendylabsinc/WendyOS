package agentpairverify

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/internal/testidentity"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
	"google.golang.org/protobuf/encoding/protojson"
)

type testClock struct {
	now     time.Time
	unknown bool
}

func (c testClock) Now() (trustedtime.Interval, error) {
	return trustedtime.Interval{Earliest: c.now, Latest: c.now.Add(time.Millisecond), BootID: "test-boot", Monotonic: time.Second, Domain: "physical", Authenticated: !c.unknown, Source: "test-clock"}, nil
}

func fixture(t *testing.T, tombstone bool) (Expectation, []byte, []byte, testClock) {
	t.Helper()
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	id := testidentity.NewWith(t, now, "spiffe://wendy.sh/tenant/"+testidentity.Tenant+"/device/22222222-2222-4222-8222-222222222222")
	c := testClock{now: now}
	certs, err := wendycert.NewDevice(wendycert.Config{Tenant: testidentity.Tenant, Roots: [][]byte{id.RootDER}})
	if err != nil {
		t.Fatal(err)
	}
	device, err := certs.Verify(id.ChainDER, now, now.Add(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	scope := serviceorigin.Scope{Tenant: testidentity.Tenant, Site: "office", Device: device.Principal, App: "com.wendy.world.agentpair", Service: "probe", DeploymentGeneration: 7, ExecutionDomain: "simulation", Publish: []serviceorigin.PublishScope{{ComponentType: "wendy.sensor.scalar.v1", Stream: "gas"}}}
	p, err := serviceorigin.NewPublisher(serviceorigin.PublisherConfig{Scope: scope, Epoch: 11, Lifetime: time.Minute, Clock: c, Certificates: certs, ChainDER: id.ChainDER, DeviceKey: id.Key})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	q := serviceorigin.PublishRequest{Entity: "replica-probe", ComponentType: "wendy.sensor.scalar.v1", Stream: "gas", Frame: world.FrameRef{ID: "floor", Revision: "v1"}, Timing: world.Timing{SampleTime: now.Add(-24 * time.Hour), ClockDomain: "simulation:test", ErrorKnown: true}, TTL: 30 * time.Second, Tombstone: tombstone, Provenance: world.Provenance{Origin: world.Simulated, AppVersion: "1", Simulation: &world.SimulationContext{RunID: "test", ScenarioVersion: "1", Seed: 1, ClockDomain: "simulation:test"}}}
	if !tombstone {
		q.Payload = []byte(`{"value":7,"unit":"ppm","sensor_type":"gas","calibration_revision":"1","uncertainty":0.5}`)
	}
	signed, err := p.Publish(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	origin, _ := json.Marshal(signed)
	wire, err := appv1.EncodePublish(q)
	if err != nil {
		t.Fatal(err)
	}
	request, _ := protojson.Marshal(wire)
	e := Expectation{Kind: Kind, RootDER: id.RootDER, LeafSHA256: device.CertificateSHA256, KeySHA256: device.SigningKeySHA256, Scope: scope, Epoch: 11, Sequence: 1, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour)}
	return e, request, origin, c
}

func TestOriginalAndExactReceivedProofs(t *testing.T) {
	for _, tombstone := range []bool{false, true} {
		e, q, o, c := fixture(t, tombstone)
		r, err := Verify(context.Background(), e, q, o, bytes.Clone(o), c)
		if err != nil || !r.ExactReceivedBytes || r.EnvelopeSHA256 != digest(o) || r.Generation != 7 || r.Epoch != 11 || r.Sequence != 1 {
			t.Fatalf("receipt=%+v err=%v", r, err)
		}
		r, err = Verify(context.Background(), e, q, o, nil, c)
		if err != nil || r.ExactReceivedBytes {
			t.Fatalf("origin-only receipt=%+v err=%v", r, err)
		}
	}
}

func TestFalseProofsAndAuthorityAreRefused(t *testing.T) {
	for _, name := range []string{"signature", "delegation", "record", "request", "received", "root", "leaf", "key", "generation", "epoch", "sequence", "policy", "expired", "unknown_clock", "canceled"} {
		t.Run(name, func(t *testing.T) {
			e, q, o, c := fixture(t, false)
			received := bytes.Clone(o)
			ctx := context.Background()
			switch name {
			case "signature", "delegation", "record":
				var signed serviceorigin.SignedRecord
				_ = json.Unmarshal(o, &signed)
				if name == "signature" {
					signed.Signature[0] ^= 1
				}
				if name == "delegation" {
					signed.Delegation.Signature[0] ^= 1
				}
				if name == "record" {
					signed.Record = bytes.Replace(signed.Record, []byte(`"sequence":1`), []byte(`"sequence":2`), 1)
					e.Sequence = 2
				}
				o, _ = json.Marshal(signed)
				received = bytes.Clone(o)
			case "request":
				q = bytes.Replace(q, []byte("replica-probe"), []byte("another-entity"), 1)
			case "received":
				received = append(received, '\n')
			case "root":
				e.RootDER = []byte("wrong root")
			case "leaf":
				e.LeafSHA256 = digest([]byte("wrong leaf"))
			case "key":
				e.KeySHA256 = digest([]byte("wrong key"))
			case "generation":
				e.Scope.DeploymentGeneration++
			case "epoch":
				e.Epoch++
			case "sequence":
				e.Sequence++
			case "policy":
				e.NotAfter = c.now
			case "expired":
				c.now = c.now.Add(2 * time.Hour)
			case "unknown_clock":
				c.unknown = true
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if _, err := Verify(ctx, e, q, o, received, c); err == nil {
				t.Fatal("false evidence accepted")
			}
		})
	}
}

func TestExpectationRejectsUnknownFieldsAndOversize(t *testing.T) {
	for _, raw := range [][]byte{[]byte(`{"kind":"x","authority_override":true}`), bytes.Repeat([]byte(" "), MaxInput+1), nil} {
		if _, err := DecodeExpectation(raw); err == nil {
			t.Fatal("invalid expectation accepted")
		}
	}
}

type finalClock struct {
	base  testClock
	calls int
	last  func(*testClock)
}

func (c *finalClock) Now() (trustedtime.Interval, error) {
	c.calls++
	if c.calls == 5 {
		c.last(&c.base)
	}
	return c.base.Now()
}

func TestFinalObservationStillRequiresExpectationAndContext(t *testing.T) {
	for _, name := range []string{"policy_expired", "canceled"} {
		t.Run(name, func(t *testing.T) {
			e, q, o, c := fixture(t, false)
			e.NotAfter = c.now.Add(100 * time.Millisecond)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			clock := &finalClock{base: c, last: func(c *testClock) {
				if name == "canceled" {
					cancel()
				} else {
					c.now = e.NotAfter
				}
			}}
			if _, err := Verify(ctx, e, q, o, bytes.Clone(o), clock); err == nil {
				t.Fatal("final boundary accepted")
			}
			if clock.calls != 5 {
				t.Fatalf("final observation not exercised: %d", clock.calls)
			}
		})
	}
}

func TestCallbackCannotRewriteVerifiedInputsOrReceiptHashes(t *testing.T) {
	e, q, o, c := fixture(t, false)
	wantProof, wantRequest := digest(o), digest(q)
	received := bytes.Clone(o)
	clock := &finalClock{base: c, last: func(_ *testClock) {
		q[0], o[0], received[0] = 'x', 'x', 'x'
		e.RootDER[0] ^= 1
		e.Scope.Publish[0].Stream = "different"
	}}
	r, err := Verify(context.Background(), e, q, o, received, clock)
	if err != nil || r.EnvelopeSHA256 != wantProof || r.RequestSHA256 != wantRequest || !r.ExactReceivedBytes {
		t.Fatalf("callback changed verified result: %+v, %v", r, err)
	}
}
