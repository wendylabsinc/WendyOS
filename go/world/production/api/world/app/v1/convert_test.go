package appv1

import (
	"bytes"
	"encoding/json"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func requestFixture() *PublishRequest {
	return &PublishRequest{Entity: "sensor-1", ComponentType: "wendy.observation.v1", Stream: "samples", Frame: &FrameRef{Id: "floor", Revision: "r1"}, Timing: &Timing{SampleUnixNs: 10_000_000_123, ClockDomain: "simulation:run", ErrorKnown: true, ErrorBoundNs: 1234, AgeHintNs: 5678}, TtlNs: uint64(time.Second), Provenance: &Provenance{Origin: Origin_ORIGIN_SIMULATED, AppVersion: "v1", Simulation: &SimulationContext{RunId: "run", ScenarioVersion: "v1", Seed: 123, ClockDomain: "simulation:run"}}, PayloadJson: []byte("{ \"note\": \"原本\", \"value\": 1.00 }\n")}
}

func TestWirePreservesExactSampleAndPayload(t *testing.T) {
	p := requestFixture()
	wire, err := proto.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var received PublishRequest
	if err := proto.Unmarshal(wire, &received); err != nil {
		t.Fatal(err)
	}
	r, err := DecodePublish(&received)
	if err != nil {
		t.Fatal(err)
	}
	if r.Timing.SampleTime.UnixNano() != p.Timing.SampleUnixNs || r.Timing.ErrorBound != 1234 || r.Timing.AgeHint != 5678 || r.Provenance.Origin != world.Simulated || r.Provenance.Simulation.ClockDomain != p.Timing.ClockDomain || !bytes.Equal(r.Payload, p.PayloadJson) {
		t.Fatal("wire conversion changed sample, ancestry or payload")
	}
	received.PayloadJson[0] = 'x'
	received.Provenance.Simulation.RunId = "different"
	if !bytes.Equal(r.Payload, p.PayloadJson) || r.Provenance.Simulation.RunID != "run" {
		t.Fatal("decoded data aliases mutable input")
	}
}

func TestInferredReplayAncestryIsNotRelabeled(t *testing.T) {
	p := requestFixture()
	capture := int64(0) // Presence at the Unix epoch differs from an absent capture.
	p.Provenance = &Provenance{Origin: Origin_ORIGIN_INFERRED, AppVersion: "v2", ModelId: "fusion", ModelVersion: "v3", Inputs: []*Input{{Id: "recorded-sample", Origin: Origin_ORIGIN_REPLAYED, OriginalCaptureUnixNs: &capture, OriginalClockDomain: "device:old", Simulation: p.Provenance.Simulation}}}
	r, err := DecodePublish(p)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Provenance.Synthetic() || r.Provenance.Inputs[0].Origin != world.Replayed || r.Provenance.Inputs[0].OriginalCaptureTime == nil || r.Provenance.Inputs[0].OriginalCaptureTime.UnixNano() != 0 || r.Provenance.Inputs[0].OriginalClockDomain != "device:old" {
		t.Fatal("replay ancestry lost")
	}
	capture = 1
	if r.Provenance.Inputs[0].OriginalCaptureTime.UnixNano() != 0 {
		t.Fatal("aliased capture time")
	}
}

func TestRejectsAmbiguousWireExtensionsAndOverflow(t *testing.T) {
	unknown := protowire.AppendTag(nil, 100, protowire.BytesType)
	unknown = protowire.AppendString(unknown, "forged-identity")
	cases := map[string]func(*PublishRequest){
		"root identity":            func(p *PublishRequest) { p.ProtoReflect().SetUnknown(unknown) },
		"frame identity":           func(p *PublishRequest) { p.Frame.ProtoReflect().SetUnknown(unknown) },
		"ancestry identity":        func(p *PublishRequest) { p.Provenance.Simulation.ProtoReflect().SetUnknown(unknown) },
		"unknown origin":           func(p *PublishRequest) { p.Provenance.Origin = 999 },
		"absent provenance":        func(p *PublishRequest) { p.Provenance = nil },
		"absent timing":            func(p *PublishRequest) { p.Timing = nil },
		"ttl overflow":             func(p *PublishRequest) { p.TtlNs = math.MaxUint64 },
		"error overflow":           func(p *PublishRequest) { p.Timing.ErrorBoundNs = math.MaxUint64 },
		"age overflow":             func(p *PublishRequest) { p.Timing.AgeHintNs = math.MaxUint64 },
		"unknown error with bound": func(p *PublishRequest) { p.Timing.ErrorKnown = false },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			p := requestFixture()
			change(p)
			if _, err := DecodePublish(p); err == nil {
				t.Fatal("ambiguous request accepted")
			}
		})
	}
}

func TestReplyPreservesSignedBytes(t *testing.T) {
	r := serviceorigin.SignedRecord{Record: []byte("{ \"sample\": 1.00 }\n"), Signature: []byte{1, 2, 3}, Delegation: serviceorigin.SignedDelegation{Claims: []byte("{ \"opaque\": true }\n"), Signature: []byte{4, 5, 6}, ChainDER: [][]byte{{7, 8, 9}}}}
	p, err := EncodePublishReply(r)
	if err != nil {
		t.Fatal(err)
	}
	var decoded serviceorigin.SignedRecord
	if err := json.Unmarshal(p.SignedRecordJson, &decoded); err != nil {
		t.Fatal(err)
	}
	if !p.Accepted || !reflect.DeepEqual(decoded, r) {
		t.Fatal("reply changed signed bytes")
	}
}
