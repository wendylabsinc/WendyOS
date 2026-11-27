package livefusion

import (
	"bytes"
	"context"
	"testing"
	"time"

	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/fusioncapture"
	"google.golang.org/grpc"
)

func TestNativeSessionUsesOneAdmittedQueryAndExactRemoteMedia(t *testing.T) {
	for _, mode := range []string{"complete", "lidar"} {
		t.Run(mode, func(t *testing.T) {
			f, api := setup(t, mode)
			remoteFusionSources(t, f, "camera-b", "audio-1")
			s, err := NewNative(f.cfg, api)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			out, err := s.Step(context.Background())
			if err != nil || out.Status != "complete" || !out.LocallyVerified || out.InputAuthority != "native_agent_admission" || f.queries != 1 || f.published != 1 || s.state.verifier != nil {
				t.Fatalf("native session: %s %v; queries=%d published=%d", out.Status, err, f.queries, f.published)
			}
			if len(out.Media) != 3 {
				t.Fatal("native mode omitted original media")
			}
			for _, input := range out.Inputs {
				if !bytes.Equal(input.EnvelopeJSON, f.proofs[input.Reference.Key.Source.Stream]) {
					t.Fatal("native original changed")
				}
			}
			for _, media := range out.Media {
				if media.Source.Stream == "camera-b" || media.Source.Stream == "audio-1" {
					if media.Source.Device != remoteFusionDevice {
						t.Fatal("remote evidence owner changed")
					}
				}
				if digest(media.Data) != media.Reference.SHA256 {
					t.Fatal("evidence bytes changed")
				}
			}
			_, record, err := decodeEnvelope(out.SignedRecordJSON)
			if err != nil {
				t.Fatal(err)
			}
			typed, err := fusioncapture.ValidateReplicaPublication(record.Publication)
			if err != nil || len(typed.Result.Inputs) != len(out.Inputs) {
				t.Fatal("invalid exact result lineage", err)
			}
			for i, ref := range typed.Result.Inputs {
				if ref != out.Inputs[i].Reference {
					t.Fatal("published reference changed")
				}
			}
		})
	}
}

func TestNativeSessionRefusesOriginSubstitutionAndMediaHash(t *testing.T) {
	for _, mode := range []string{"origin", "media-forgery"} {
		t.Run(mode, func(t *testing.T) {
			f, api := setup(t, mode)
			if mode == "origin" {
				local := bytes.Clone(f.proofs["camera-b"])
				remoteFusionSources(t, f, "camera-b")
				f.proofs["camera-b"] = local
			}
			s, err := NewNative(f.cfg, api)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			out, err := s.Step(context.Background())
			if err == nil || out.LocallyVerified || f.published != 0 || f.queries != 1 {
				t.Fatal("invalid native input accepted", out.Status, err)
			}
		})
	}
}

type nativeSampleExpiryClient struct {
	appv1.WorldAppServiceClient
	fixture *fixtureServer
}

func (c nativeSampleExpiryClient) GetEvidence(ctx context.Context, p *appv1.GetEvidenceRequest, opts ...grpc.CallOption) (*appv1.GetEvidenceReply, error) {
	r, err := c.WorldAppServiceClient.GetEvidence(ctx, p, opts...)
	c.fixture.mu.Lock()
	c.fixture.sampleAdvance = 4 * time.Second
	c.fixture.mu.Unlock()
	return r, err
}

func TestNativeSessionKeepsOriginalDeadlinesAfterMedia(t *testing.T) {
	for _, mode := range []string{"physical", "sample"} {
		t.Run(mode, func(t *testing.T) {
			fixtureMode := "complete"
			if mode == "physical" {
				fixtureMode = "expiry"
			}
			f, api := setup(t, fixtureMode)
			if mode == "sample" {
				api = nativeSampleExpiryClient{api, f}
			}
			s, err := NewNative(f.cfg, api)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			out, err := s.Step(context.Background())
			if err != nil || out.Status != "insufficient_evidence" || out.LocallyVerified || f.published != 0 || f.queries != 1 {
				t.Fatal("expired native capture was extended or published", out.Status, err)
			}
			if out.Diagnostics == nil || out.Diagnostics.Verify.Count != 0 || out.Diagnostics.Verify.Batches != 0 {
				t.Fatal("native input was labeled independently verified")
			}
		})
	}
}

func TestNativeSessionNeverFillsMissingCurrentSourceFromHistory(t *testing.T) {
	f, api := setup(t, "complete")
	s, err := NewNative(f.cfg, api)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if first, err := s.Step(context.Background()); err != nil || !first.LocallyVerified {
		t.Fatal("first", err)
	}
	f.mu.Lock()
	f.omitted = map[string]bool{"gas-1": true}
	f.mu.Unlock()
	out, err := s.Step(context.Background())
	if err != nil || out.Status != "insufficient_evidence" || out.LocallyVerified || f.queries != 2 || f.published != 1 {
		t.Fatal("missing source filled from history", out.Status, err)
	}
}

func TestNativeSessionDoesNotSelectSupersededRemoteHistory(t *testing.T) {
	f, api := setup(t, "complete")
	remoteFusionSources(t, f, "gas-1")
	s, err := NewNative(f.cfg, api)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if first, err := s.Step(context.Background()); err != nil || !first.LocallyVerified {
		t.Fatal("first", err)
	}
	newer := map[string][]byte{}
	for _, source := range f.cfg.Sources {
		at := 500 * time.Millisecond
		if source.SensorID == "gas-1" {
			at = 1500 * time.Millisecond
		}
		newer[source.SensorID] = captureAt(t, f, source.SensorID, at)
	}
	f.mu.Lock()
	f.proofs = newer
	f.sampleAdvance = time.Second
	f.mu.Unlock()
	out, err := s.Step(context.Background())
	if err != nil || out.Status != "insufficient_evidence" || out.LocallyVerified || f.queries != 2 || f.published != 1 {
		t.Fatal("superseded remote original selected for native admission", out.Status, err)
	}
	if len(s.window.state["gas-1"].records) != 1 {
		t.Fatal("remote register history survived native selection")
	}
}
