package robotinspect

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Two units off the same line, inspected a week apart, must produce documents that
// differ only where the robots differ. Wall-clock fields would make every diff noisy
// and hide the one row that matters, so the canonical form drops them.
func TestCanonicalJSONOmitsWallClockSoDocumentsDiffOnSubstance(t *testing.T) {
	first := g1Document(t, time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC))
	second := g1Document(t, time.Date(2026, 9, 8, 17, 30, 0, 0, time.UTC))

	firstJSON, err := first.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	secondJSON, err := second.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if string(firstJSON) != string(secondJSON) {
		t.Errorf("canonical documents differ only by run time:\n%s\n---\n%s", firstJSON, secondJSON)
	}
	if strings.Contains(string(firstJSON), "startedAt") || strings.Contains(string(firstJSON), "observedAt") {
		t.Error("canonical JSON still carries a timestamp")
	}

	// The full form keeps them, for the record of a single pass.
	full, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(full), "observedAt") {
		t.Error("full JSON dropped the observation time")
	}
}

func TestJSONCarriesQualifiersAndTheComputedVerdict(t *testing.T) {
	doc := g1Document(t, time.Now().UTC())
	raw, err := doc.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}

	var decoded struct {
		Schema  string `json:"schema"`
		Summary struct {
			Incomparable int `json:"incomparable"`
			Findings     int `json:"findings"`
		} `json:"summary"`
		Properties []struct {
			ID           string `json:"id"`
			Verdict      string `json:"verdict"`
			Observations []struct {
				Kind     string `json:"kind"`
				Value    float64
				Unit     string `json:"unit"`
				Axis     string `json:"axis"`
				Sampling *struct {
					WindowMS int64 `json:"windowMs"`
					Samples  int   `json:"samples"`
				} `json:"sampling"`
			} `json:"observations"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}

	if decoded.Schema != Schema {
		t.Errorf("schema = %q, want %q", decoded.Schema, Schema)
	}
	if decoded.Summary.Incomparable != 1 || decoded.Summary.Findings != 1 {
		t.Errorf("summary = %+v, want one incomparable finding", decoded.Summary)
	}
	if len(decoded.Properties) != 1 {
		t.Fatalf("got %d properties, want 1", len(decoded.Properties))
	}
	fov := decoded.Properties[0]
	if fov.Verdict != string(VerdictIncomparable) {
		t.Errorf("verdict = %q, want %q; a consumer must not have to recompute it", fov.Verdict, VerdictIncomparable)
	}
	axes := map[string]string{}
	for _, o := range fov.Observations {
		axes[o.Kind] = o.Axis
	}
	if axes["declared"] != AxisHorizontal || axes["measured"] != AxisVertical {
		t.Errorf("axes = %v, want declared horizontal and measured vertical inline on each observation", axes)
	}
	for _, o := range fov.Observations {
		if o.Kind == "measured" {
			if o.Sampling == nil {
				t.Fatal("measured observation serialised without its sampling window")
			}
			if o.Sampling.WindowMS != 60000 || o.Sampling.Samples != 27 {
				t.Errorf("sampling = %+v, want 60000ms over 27 samples", *o.Sampling)
			}
		}
	}
}

// g1Document is the field-of-view row from the campaign, timestamped so the canonical
// form can be shown to ignore it.
func g1Document(t *testing.T, at time.Time) Document {
	t.Helper()
	declared, err := NewObservation(MustQuantity(75, Degrees, WithAxis(AxisHorizontal)), Declared,
		Source{Probe: "camerainfo", Origin: "datasheet"}, WithObservedAt(at))
	if err != nil {
		t.Fatal(err)
	}
	measured, err := NewObservation(MustQuantity(43.4, Degrees, WithAxis(AxisVertical)), Measured,
		Source{Probe: "checkerboard", Origin: "topic:/camera/color/camera_info"},
		WithSampling(time.Minute, 27),
		WithConditions(map[string]string{"resolution": "640x480"}),
		WithObservedAt(at))
	if err != nil {
		t.Fatal(err)
	}
	return Document{
		Schema:      Schema,
		Device:      "unitree-g1-nx-2",
		VendorKind:  "unitree-g1",
		StartedAt:   at,
		FinishedAt:  at.Add(5 * time.Second),
		PassiveOnly: true,
		ProbesRun:   []string{"camerainfo", "checkerboard"},
		Properties:  []Property{{ID: "camera.color.fov", Observations: []Observation{declared, measured}}},
	}
}
