package geography

import (
	"encoding/json"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"math"
	"os"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T) ([]byte, *Site) {
	t.Helper()
	b, e := os.ReadFile("../../examples/geography/office.json")
	if e != nil {
		t.Fatal(e)
	}
	s, e := DecodeSite(b)
	if e != nil {
		t.Fatal(e)
	}
	return b, s
}
func TestSiteRoundTripAndValidity(t *testing.T) {
	_, s := fixture(t)
	at := time.Date(2026, 11, 9, 22, 0, 0, 0, time.UTC)
	req := Request{From: world.FrameRef{ID: "mezzanine", Revision: "mezzanine-v1"}, To: world.FrameRef{ID: "site-origin", Revision: "geo-v1"}, Position: [3]float64{2, 1, 0}, At: at, UncertaintyMetres: .02}
	r, e := s.Convert(req)
	if e != nil {
		t.Fatal(e)
	}
	for j, want := range [3]float64{10, 4, 3} {
		if math.Abs(r.Position[j]-want) > 1e-12 {
			t.Fatal(r.Position)
		}
	}
	if math.Abs(r.UncertaintyMetres-100.12) > 1e-9 || r.Placement != "illustrative" || r.UncertaintyModel != "translation-bound-only" || r.GeoreferenceRevision != "illustrative-geo-v1" {
		t.Fatal(r)
	}
	back, e := s.Geographic(GeoRequest{Coordinate: r.Geographic, To: req.From, At: at})
	if e != nil {
		t.Fatal(e)
	}
	for j, v := range req.Position {
		if math.Abs(back.Position[j]-v) > 1e-6 {
			t.Fatal(back)
		}
	}
	req.At = r.ValidUntil
	if _, e = s.Convert(req); e == nil {
		t.Fatal("expired site accepted")
	}
	req.At = at
	req.From.Revision = "unknown"
	if _, e = s.Convert(req); e == nil {
		t.Fatal("unknown revision accepted")
	}
}
func TestSiteStrictDecoding(t *testing.T) {
	raw, _ := fixture(t)
	variants := []string{
		strings.Replace(string(raw), `"latitude_degrees": 52,`, "", 1),
		strings.Replace(string(raw), `"latitude_degrees": 52`, `"latitude_degrees": null`, 1),
		strings.Replace(string(raw), `"latitude_degrees": 52`, `"latitude_degrees": 52, "Latitude_Degrees": 0`, 1),
		strings.Replace(string(raw), `"WGS84-ellipsoid"`, `"MSL"`, 1),
		strings.Replace(string(raw), `"illustrative"`, `"unknown"`, 1),
	}
	for _, v := range variants {
		if _, e := DecodeSite([]byte(v)); e == nil {
			t.Fatal("invalid document accepted", v)
		}
	}
	var doc map[string]any
	json.Unmarshal(raw, &doc)
	frames := doc["frames"].([]any)
	frames[0].(map[string]any)["translation"] = []any{0, 0}
	bad, _ := json.Marshal(doc)
	if _, e := DecodeSite(bad); e == nil {
		t.Fatal("short vector accepted")
	}
}
func TestActualCapturedSampleProjectionAndIsolation(t *testing.T) {
	_, s := fixture(t)
	b, e := os.ReadFile("../../docs/verification/2026-11-09-live-inspection.json")
	if e != nil {
		t.Fatal(e)
	}
	var report struct {
		Inspection struct {
			Captured int64            `json:"captured_unix_ms"`
			Samples  []sensing.Sample `json:"samples"`
		} `json:"inspection_result"`
	}
	if e = json.Unmarshal(b, &report); e != nil {
		t.Fatal(e)
	}
	if len(report.Inspection.Samples) != 4 {
		t.Fatal("missing captured samples")
	}
	at := time.UnixMilli(report.Inspection.Captured)
	for _, sample := range report.Inspection.Samples {
		projected, e := s.ProjectSample(sample, at)
		if e != nil {
			t.Fatal(e)
		}
		original, _ := json.Marshal(sample)
		copy, _ := json.Marshal(projected.Sample)
		if string(original) != string(copy) {
			t.Fatal("projection changed lineage")
		}
		back, e := s.Geographic(GeoRequest{Coordinate: projected.Location.Geographic, To: sample.Frame, At: at})
		if e != nil {
			t.Fatal(e)
		}
		for j, v := range sample.PositionM {
			if math.Abs(back.Position[j]-v) > 1e-6 {
				t.Fatal("capture position changed")
			}
		}
		projected.Sample.PositionM[0] += 10
		if sample.PositionM[0] == projected.Sample.PositionM[0] {
			t.Fatal("sample aliases output")
		}
		sample.SiteID = "another-site"
		if _, e = s.ProjectSample(sample, at); e == nil {
			t.Fatal("wrong site accepted")
		}
	}
	bad := report.Inspection.Samples[0]
	bad.PackageSHA256 = strings.Repeat("a", 64)
	if _, e = s.ProjectSample(bad, at); e == nil {
		t.Fatal("wrong package accepted")
	}
}
