package robotinspect

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func textSource(probe string) Source { return Source{Probe: probe, Origin: "agent:device_info"} }

func TestNewTextObservationRequiresExactlyOneValue(t *testing.T) {
	if _, err := NewTextObservation("", Declared, textSource("compute")); err == nil {
		t.Error("an empty textual value was accepted")
	}
	// Both forms at once means a caller built the struct by hand and got it wrong.
	both := Observation{Text: "jetson-orin-nano", Quantity: MustQuantity(6, Count), Kind: Declared, Source: textSource("compute")}
	if err := both.validate(); err == nil {
		t.Error("an observation carrying both a quantity and text was accepted")
	}
	if _, err := NewTextObservation("jetson-orin-nano", Declared, textSource("compute")); err != nil {
		t.Errorf("a well-formed textual observation was rejected: %v", err)
	}
}

// A textual reading is a reading, not a sample, so it is exempt from the sampling rule
// that a measured quantity must obey.
func TestMeasuredTextNeedsNoSamplingWindow(t *testing.T) {
	if _, err := NewTextObservation("rmw_fastrtps_cpp", Measured, textSource("dds-graph")); err != nil {
		t.Errorf("measured text rejected for want of a sampling window: %v", err)
	}
	if _, err := NewObservation(MustQuantity(5, Hertz), Measured, textSource("stream-sample")); err == nil {
		t.Error("the sampling rule stopped applying to quantities")
	}
}

// Two sources reporting the same firmware agree; two reporting different ones disagree.
// There is no tolerance for a version string.
func TestAssessTextHasNoTolerance(t *testing.T) {
	fromSDK, err := NewTextObservation("1.4.2", Declared, Source{Probe: "vendor-sdk", Origin: "sdk:device_info"})
	if err != nil {
		t.Fatal(err)
	}
	fromAgent, err := NewTextObservation("1.4.2", Declared, textSource("compute"))
	if err != nil {
		t.Fatal(err)
	}
	if got := (Property{ID: "identity.firmware.body", Observations: []Observation{fromSDK, fromAgent}}).Assess().Verdict; got != VerdictAgree {
		t.Errorf("verdict = %q, want %q", got, VerdictAgree)
	}

	stale, err := NewTextObservation("1.3.9", Declared, textSource("compute"))
	if err != nil {
		t.Fatal(err)
	}
	assessment := Property{ID: "identity.firmware.body", Observations: []Observation{fromSDK, stale}}.Assess()
	if assessment.Verdict != VerdictDisagree {
		t.Fatalf("verdict = %q, want %q", assessment.Verdict, VerdictDisagree)
	}
	for _, want := range []string{"1.4.2", "1.3.9"} {
		if !strings.Contains(assessment.Detail, want) {
			t.Errorf("detail %q should carry both versions", assessment.Detail)
		}
	}
}

// Text against a number is not a value disagreement — it means two sources disagree
// about what kind of fact this is, which is a schema problem worth its own verdict.
func TestAssessRefusesToCompareTextWithAQuantity(t *testing.T) {
	text, _ := NewTextObservation("6", Declared, textSource("compute"))
	number, _ := NewObservation(MustQuantity(6, Count), Declared, textSource("compute"))

	assessment := Property{ID: "compute.cpu_count", Observations: []Observation{text, number}}.Assess()
	if assessment.Verdict != VerdictIncomparable {
		t.Errorf("verdict = %q, want %q", assessment.Verdict, VerdictIncomparable)
	}
	if !strings.Contains(assessment.Detail, "kind of fact") {
		t.Errorf("detail %q should name the mismatch", assessment.Detail)
	}
}

func TestJSONCarriesTextInsteadOfAValue(t *testing.T) {
	board, err := NewTextObservation("jetson-orin-nano", Declared, textSource("compute"),
		WithObservedAt(time.Now().UTC()))
	if err != nil {
		t.Fatal(err)
	}
	cpus, err := NewObservation(MustQuantity(6, Count), Declared, textSource("compute"))
	if err != nil {
		t.Fatal(err)
	}
	doc := Document{Schema: Schema, Properties: []Property{
		{ID: "compute.board", Observations: []Observation{board}},
		{ID: "compute.cpu_count", Observations: []Observation{cpus}},
	}}

	raw, err := doc.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Properties []struct {
			ID           string `json:"id"`
			Observations []struct {
				Value *float64 `json:"value"`
				Text  string   `json:"text"`
			} `json:"observations"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, p := range decoded.Properties {
		o := p.Observations[0]
		switch p.ID {
		case "compute.board":
			if o.Text != "jetson-orin-nano" || o.Value != nil {
				t.Errorf("board serialised as value=%v text=%q; want text only", o.Value, o.Text)
			}
		case "compute.cpu_count":
			if o.Value == nil || *o.Value != 6 || o.Text != "" {
				t.Errorf("cpu_count serialised as value=%v text=%q; want a value only", o.Value, o.Text)
			}
		}
	}
}

func TestRenderPrintsTextualValues(t *testing.T) {
	board, err := NewTextObservation("jetson-orin-nano", Declared, textSource("compute"))
	if err != nil {
		t.Fatal(err)
	}
	out := Render(Document{Schema: Schema, Properties: []Property{
		{ID: "compute.board", Observations: []Observation{board}},
	}})
	if !strings.Contains(out, "jetson-orin-nano") {
		t.Errorf("report does not print the board:\n%s", out)
	}
}
