package peoplereader

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trackcapture"
)

func TestVerifiedReaderPreparesExactFixedMissionLocation(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "mission"}[enabled], func(t *testing.T) {
			f, api := readerFixture(t)
			floor := .24
			if enabled {
				f.cfg.MissionFloorY = &floor
			}
			s, err := New(f.cfg, api)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			r, err := s.Step(context.Background())
			if err != nil || len(r.Tracks) != 1 {
				t.Fatalf("read: %v, tracks=%d", err, len(r.Tracks))
			}
			track := r.Tracks[0]
			if !enabled {
				if track.MissionInput != nil {
					t.Fatal("ordinary read acquired mission handoff")
				}
				return
			}
			handoff := track.MissionInput
			if handoff == nil || handoff.Version != 1 || handoff.ResultSchema != trackcapture.ResultSchema || handoff.EnvelopeSHA256 != hash(f.raw) || !bytes.Equal(track.EnvelopeJSON, f.raw) || handoff.Target != [3]float64{track.Payload.PositionXZ[0], floor, track.Payload.PositionXZ[1]} {
				t.Fatal("handoff changed observed location or original envelope")
			}
			var trigger tasking.TriggerRef
			if json.Unmarshal(handoff.TriggerJSON, &trigger) != nil || trigger.SHA256 != handoff.TriggerSHA256 || trigger.Component.Key.Type != "wendy.inference.result.v1" || trigger.Component.Timing.AgeHint != 0 {
				t.Fatal("invalid native canonical trigger")
			}
			body, err := json.Marshal(trigger.Component)
			if err != nil || hash(body) != trigger.SHA256 || handoff.SampleValidUntil.After(track.Payload.ValidUntil) || handoff.PhysicalValidUntil.After(track.Payload.PhysicalValidUntil()) {
				t.Fatal("handoff changed canonical component or extended original deadline")
			}
		})
	}
}

func TestUnverifiedOriginalNeverProducesMissionInput(t *testing.T) {
	f, api := readerFixture(t)
	floor := .24
	f.cfg.MissionFloorY = &floor
	var signed serviceorigin.SignedRecord
	if err := json.Unmarshal(f.raw, &signed); err != nil {
		t.Fatal(err)
	}
	signed.Signature[0] ^= 1
	f.raw, _ = json.Marshal(signed)
	s, err := New(f.cfg, api)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r, err := s.Step(context.Background())
	if err == nil || r.Status != "failed" || len(r.Tracks) != 0 {
		t.Fatal("unverified original reached mission handoff")
	}
}
