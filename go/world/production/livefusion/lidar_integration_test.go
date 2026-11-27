package livefusion

import (
	"bytes"
	"context"
	"testing"
	"time"
)

func TestOrdinarySevenSourceLidarFusionUsesCurrentProofsAndPublishesOnce(t *testing.T) {
	s, api := setup(t, "lidar")
	out, err := Run(context.Background(), s.cfg, api)
	if err != nil || out.Status != "complete" || !out.LocallyVerified || s.published != 1 || len(out.Inputs) != 7 || len(out.Media) != 3 || out.Evaluation == nil || out.Evaluation.Hazard == nil || out.Evaluation.Hazard.Spatial == nil || !out.Evaluation.Hazard.Spatial.Associated {
		t.Fatalf("status=%s inputs=%d media=%d published=%d error=%v evaluation=%+v", out.Status, len(out.Inputs), len(out.Media), s.published, err, out.Evaluation)
	}
	for _, input := range out.Inputs {
		if !bytes.Equal(input.EnvelopeJSON, s.original[input.Reference.Key.Source.Stream]) {
			t.Fatal("original proof replaced")
		}
	}
	if s.queries != 5 {
		t.Fatalf("complete source-authority query rounds=%d want5", s.queries)
	}
	for _, id := range []string{"lidar-a", "lidar-b", "lidar-c"} {
		if s.owners[id] != 0 {
			t.Fatal("inline LiDAR attempted media fetch")
		}
	}
}

func TestOrdinaryFusionUsesAvailableLidarButNeverMissingCachedSupport(t *testing.T) {
	for _, test := range []struct {
		name        string
		omit, after map[string]bool
		want        bool
	}{
		{"one_current", map[string]bool{"lidar-b": true, "lidar-c": true}, nil, true},
		{"none_current", map[string]bool{"lidar-a": true, "lidar-b": true, "lidar-c": true}, nil, false},
		{"selected_revoked", map[string]bool{"lidar-b": true, "lidar-c": true}, map[string]bool{"lidar-a": true}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, api := setup(t, "lidar")
			s.omitted, s.omitAfterMedia = test.omit, test.after
			out, err := Run(context.Background(), s.cfg, api)
			if test.want {
				if err != nil || out.Status != "complete" || !out.LocallyVerified || len(out.Inputs) != 5 || s.published != 1 {
					t.Fatalf("%s %v", out.Status, err)
				}
			} else if out.LocallyVerified || s.published != 0 {
				t.Fatal("missing selected LiDAR still published")
			}
		})
	}
}

func TestCandidateWindowCannotFillAbsentLidarFromHistory(t *testing.T) {
	f, _ := setup(t, "lidar")
	s := &session{cfg: f.cfg, sources: map[string]Source{}}
	for _, source := range f.cfg.Sources {
		s.sources[source.SensorID] = source
	}
	w, err := newCandidateWindow(s, 350*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	all := []inputProof{}
	subset := []inputProof{}
	required := []inputProof{}
	for _, source := range f.cfg.Sources {
		p, err := s.parse(f.proofs[source.SensorID], s.sources)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, p)
		if source.SensorID != "lidar-b" && source.SensorID != "lidar-c" {
			subset = append(subset, p)
			if source.SensorID != "lidar-a" {
				required = append(required, p)
			}
		}
	}
	if got, err := w.updateAndSelect(all); err != nil || len(got) != 7 {
		t.Fatal("full catalog selection", err)
	}
	got, err := w.updateAndSelect(subset)
	if err != nil || len(got) != 5 {
		t.Fatal("absent optional scans were reused", err)
	}
	if err := w.retire(got); err != nil {
		t.Fatal(err)
	}
	if got, err := w.updateAndSelect(subset); err != nil || len(got) != 0 {
		t.Fatal("retired partial tuple reselected", err)
	}
	if got, err := w.updateAndSelect(required); err != nil || len(got) != 0 {
		t.Fatal("no LiDAR became legacy tuple", err)
	}
}
