package livefusion

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

func TestSessionKeepsOriginalsAcrossDuplicateMissingAndFreshCaptures(t *testing.T) {
	fixture, api := setup(t, "lidar")
	fixture.cfg.RequestTimeoutMS = 3000
	s, err := New(fixture.cfg, api)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	first, err := s.Step(context.Background())
	if err != nil || !first.LocallyVerified || first.Tick != 1 || fixture.published != 1 {
		t.Fatalf("first result: %s %v", first.Status, err)
	}
	duplicate, err := s.Step(context.Background())
	if err != nil || duplicate.LocallyVerified || fixture.published != 1 || duplicate.Status != "insufficient_evidence" {
		t.Fatalf("duplicate renewed result: %s %v", duplicate.Status, err)
	}
	fixture.mu.Lock()
	fixture.omitted = map[string]bool{"gas-1": true}
	fixture.sampleAdvance = 4 * time.Second
	fixture.mu.Unlock()
	missing, err := s.Step(context.Background())
	if err != nil || missing.LocallyVerified || fixture.published != 1 {
		t.Fatalf("missing input used cached hazard: %s %v", missing.Status, err)
	}
	fresh := map[string][]byte{}
	for _, source := range fixture.cfg.Sources {
		fresh[source.SensorID] = captureAt(t, fixture, source.SensorID, 4400*time.Millisecond)
	}
	fixture.mu.Lock()
	fixture.proofs, fixture.omitted = fresh, nil
	fixture.mu.Unlock()
	restored, err := s.Step(context.Background())
	if err != nil || !restored.LocallyVerified || restored.Tick != 4 || fixture.published != 2 {
		t.Fatalf("fresh result: %s %v", restored.Status, err)
	}
	if bytes.Equal(first.SignedRecordJSON, restored.SignedRecordJSON) {
		t.Fatal("restoration reused original result")
	}
	for _, p := range restored.Inputs {
		if !bytes.Equal(p.EnvelopeJSON, fresh[p.Reference.Key.Source.Stream]) {
			t.Fatal("restoration changed or reused original input")
		}
	}
}

func TestSessionEndsAfterUnknownPublication(t *testing.T) {
	fixture, api := setup(t, "uncertain")
	s, err := New(fixture.cfg, api)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	out, err := s.Step(context.Background())
	if !errors.Is(err, ErrUncertain) || out.Status != "unknown_outcome" || fixture.published != 1 {
		t.Fatalf("unknown result: %s %v", out.Status, err)
	}
	if _, err = s.Step(context.Background()); !errors.Is(err, ErrClosed) || fixture.published != 1 {
		t.Fatal("session retried unknown publication", err)
	}
}
