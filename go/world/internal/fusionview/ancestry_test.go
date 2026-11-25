package fusionview

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/wendylabsinc/wendy/go/world/internal/fusion"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func TestRejectNonSimulatedOrMixedContextLeaves(t *testing.T) {
	cases := map[string]func(*Report){
		"all measured": func(r *Report) {
			for i := range r.Claim.Provenance.Inputs {
				r.Claim.Provenance.Inputs[i].Origin = world.Measured
				r.Claim.Provenance.Inputs[i].Simulation = nil
			}
		},
		"one measured": func(r *Report) {
			r.Claim.Provenance.Inputs[1].Origin = world.Measured
			r.Claim.Provenance.Inputs[1].Simulation = nil
		},
		"missing simulation": func(r *Report) { r.Claim.Provenance.Inputs[0].Simulation = nil },
		"different run":      func(r *Report) { r.Claim.Provenance.Inputs[0].Simulation.RunID = "other" },
		"different clock":    func(r *Report) { r.Claim.Provenance.Inputs[0].Simulation.ClockDomain = "simulation:other" },
		"different scenario": func(r *Report) { r.Claim.Provenance.Inputs[0].Simulation.ScenarioVersion = "other" },
		"different seed":     func(r *Report) { r.Claim.Provenance.Inputs[0].Simulation.Seed++ },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			s := selector(t)
			bad := claim("b", 1, 0, .4, 0)
			mutate(&bad)
			if _, err := s.Update(anchor, []Report{claim("a", 1, 0, .5, 0), bad}); err == nil {
				t.Fatal("unsupported ancestry accepted")
			}
			if v := update(t, s, 0); v.State != "unavailable" {
				t.Fatal("invalid ancestry partially committed batch")
			}
		})
	}
}

func TestEachInputConsumesOneDistinctAncestor(t *testing.T) {
	r := claim("a", 1, 0, .4, 0)
	var h fusion.Hazard
	if err := json.Unmarshal(r.Claim.Payload, &h); err != nil {
		t.Fatal(err)
	}
	// Keep distinct sample IDs and equal input/ancestry counts, but bind every
	// reference to the first source/entity/version. The other leaves are unused.
	for i := 1; i < len(h.Inputs); i++ {
		h.Inputs[i].SensorID = h.Inputs[0].SensorID
		h.Inputs[i].Source = h.Inputs[0].Source
		h.Inputs[i].Version = h.Inputs[0].Version
	}
	r.Claim.Payload, _ = json.Marshal(h)
	if _, err := selector(t).Update(anchor, []Report{r}); err == nil {
		t.Fatal("multiple references consumed the same ancestor")
	}
}

func TestDistinctLeavesAllowSharedPublisherAndIndependentOrdering(t *testing.T) {
	r := claim("a", 1, 0, .4, 0)
	var h fusion.Hazard
	if err := json.Unmarshal(r.Claim.Payload, &h); err != nil {
		t.Fatal(err)
	}
	// One gateway service may publish several sensors. Ancestry identity binds
	// the sensor entity and version as well as publisher, not publisher alone.
	shared := h.Inputs[0].Source
	for i := range h.Inputs {
		h.Inputs[i].Source = shared
		sourceJSON, _ := json.Marshal(shared)
		r.Claim.Provenance.Inputs[i].ID = fmt.Sprintf("%x/%d/test/%s/%d", sha256.Sum256(sourceJSON), h.Inputs[i].Version.Epoch, h.Inputs[i].SensorID, h.Inputs[i].Version.Sequence)
	}
	for i, j := 0, len(r.Claim.Provenance.Inputs)-1; i < j; i, j = i+1, j-1 {
		r.Claim.Provenance.Inputs[i], r.Claim.Provenance.Inputs[j] = r.Claim.Provenance.Inputs[j], r.Claim.Provenance.Inputs[i]
	}
	r.Claim.Payload, _ = json.Marshal(h)
	v, err := selector(t).Update(anchor, []Report{r})
	if err != nil || v.Selected != "a" || !reflect.DeepEqual(v.Claim, r.Claim) {
		t.Fatalf("valid independently ordered ancestry changed or refused: %v", err)
	}
}
