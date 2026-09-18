package robotinspect

import (
	"strings"
	"testing"
	"time"
)

func TestRenderShowsTheG1CampaignAsAnOperatorWouldReadIt(t *testing.T) {
	doc := Document{
		Schema: Schema, Device: "unitree-g1-nx-2", VendorKind: "unitree-g1", PassiveOnly: true,
		Vendor: &VendorState{
			Raw:              map[string]string{"fsm_id": "801", "fsm_mode": "3"},
			Label:            "main operation, ready",
			ControlAuthority: "unitree-sdk",
			CommandLegality:  LegalityYes,
		},
		Properties: []Property{
			g1FieldOfView(t), g1FrameRate(t), g1Extrinsics(), g1WaistGate(t),
		},
		Skipped: map[string]Unknown{
			"self-test": NewUnknown(ReasonNotPassive, `"self-test" may actuate the robot and is never run by inspection`),
		},
	}
	out := Render(doc)
	t.Log("\n" + out)

	for _, want := range []string{
		"unitree-g1-nx-2 (unitree-g1)",
		"fsm_id=801",
		"commands owned by: unitree-sdk",
		"75 deg horizontal",
		"43.4 deg vertical",
		"incomparable",
		"UNKNOWN",
		"Nothing was commanded.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report does not contain %q", want)
		}
	}
}

func TestRenderNeverPrintsABareNumber(t *testing.T) {
	// Every angle in the report must carry its axis. This is the regression guard for
	// the substitution that started all of this.
	out := Render(Document{Schema: Schema, Properties: []Property{g1FieldOfView(t)}})
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "deg") {
			continue
		}
		if !strings.Contains(line, AxisHorizontal) && !strings.Contains(line, AxisVertical) {
			t.Errorf("angle printed without its axis: %q", line)
		}
	}
}

func TestRenderOmitsTheCommandedClaimWhenThePlanWasNotPassive(t *testing.T) {
	doc := Document{Schema: Schema, PassiveOnly: false, Properties: []Property{g1FrameRate(t)}}
	if strings.Contains(Render(doc), "Nothing was commanded") {
		t.Error("report claimed nothing was commanded for a plan that was not passive-only")
	}
}

func TestRenderGroupsPropertiesIntoSections(t *testing.T) {
	observation, err := NewObservation(MustQuantity(31, Count), Declared, Source{Probe: "dds-graph", Origin: "dds:domain0"})
	if err != nil {
		t.Fatal(err)
	}
	doc := Document{Schema: Schema, Properties: []Property{
		{ID: "camera.color.rate", Observations: []Observation{observation}},
		{ID: "camera.color.fov", Observations: []Observation{observation}},
		{ID: "ros2.topics", Observations: []Observation{observation}},
	}}
	out := Render(doc)
	camera, ros2 := strings.Index(out, "camera\n"), strings.Index(out, "ros2\n")
	if camera < 0 || ros2 < 0 {
		t.Fatalf("expected camera and ros2 sections:\n%s", out)
	}
	if camera > ros2 {
		t.Error("sections are not in the document's sorted order")
	}
	if strings.Count(out, "camera\n") != 1 {
		t.Error("camera section header repeated instead of grouping its properties")
	}
}

func TestRenderSummaryCountsReadAsProse(t *testing.T) {
	single := Document{Schema: Schema, Properties: []Property{g1FieldOfView(t)}}
	if got := Render(single); !strings.Contains(got, "1 incomparable") {
		t.Errorf("summary missing the singular count:\n%s", got)
	}
	none := Document{Schema: Schema, Properties: []Property{g1AgreedRate(t)}}
	if got := Render(none); !strings.Contains(got, "0 disagreements") || !strings.Contains(got, "1 confirmed") {
		t.Errorf("summary should report nothing found and one confirmation:\n%s", got)
	}
}

func g1AgreedRate(t *testing.T) Property {
	t.Helper()
	declared, err := NewObservation(MustQuantity(500, Hertz), Declared, Source{Probe: "vendor-sdk", Origin: "datasheet"})
	if err != nil {
		t.Fatal(err)
	}
	measured, err := NewObservation(MustQuantity(487, Hertz), Measured,
		Source{Probe: "topic-hz", Origin: "dds:rt/lowstate"}, WithSampling(5*time.Second, 2435))
	if err != nil {
		t.Fatal(err)
	}
	return Property{ID: "control.rate", Observations: []Observation{declared, measured}}
}
