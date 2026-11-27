package command

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/geography"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

const validFrameRequest = `{"from":{"id":"floor","revision":"1"},"to":{"id":"site","revision":"1"},"position_m":[0,0,0],"at":"2026-11-09T12:00:00Z","uncertainty_metres":0}`
const validGeographicRequest = `{"geographic":{"latitude_degrees":0,"longitude_degrees":0,"altitude_metres":0,"altitude_datum":"WGS84-ellipsoid"},"to":{"id":"site","revision":"1"},"at":"2026-11-09T12:00:00Z","uncertainty_metres":0}`

func TestFrameRequestRequiresExplicitCoordinatesAndFields(t *testing.T) {
	for _, locate := range []bool{false, true} {
		valid := validFrameRequest
		if locate {
			valid = validGeographicRequest
		}
		if _, _, err := decodeFrameRequest([]byte(valid), locate); err != nil {
			t.Fatalf("explicit zero coordinates rejected: %v", err)
		}
		bad := []string{
			"null", "[]", valid + "{}",
			strings.Replace(valid, `"at":`, `"At":`, 1),
			strings.Replace(valid, `"uncertainty_metres":0`, `"uncertainty_metres":null`, 1),
			strings.Replace(valid, `,"uncertainty_metres":0`, "", 1),
			strings.Replace(valid, `"uncertainty_metres":0`, `"uncertainty_metres":0,"uncertainty_metres":1`, 1),
			strings.Replace(valid, `"revision":"1"`, `"revision":null`, 1),
			strings.Replace(valid, `"revision":"1"`, `"revision":"1","unknown":0`, 1),
			strings.Replace(valid, `"at":"2026-11-09T12:00:00Z"`, `"at":"not-a-date"`, 1),
		}
		if locate {
			bad = append(bad,
				strings.Replace(valid, `"altitude_metres":0`, `"altitude_metres":null`, 1),
				strings.Replace(valid, `"longitude_degrees":0,`, "", 1),
				strings.Replace(valid, `"latitude_degrees":0`, `"Latitude_degrees":0`, 1))
		} else {
			for _, point := range []string{"[]", "[0,0]", "[0,0,0,0]", "[0,null,0]", `"0,0,0"`, "[0,true,0]"} {
				bad = append(bad, strings.Replace(valid, "[0,0,0]", point, 1))
			}
		}
		for _, raw := range bad {
			if _, _, err := decodeFrameRequest([]byte(raw), locate); err == nil {
				t.Errorf("accepted malformed request: %s", raw)
			}
		}
	}
}

func TestGeographicFilesAreBoundedAndRegular(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "request.json")
	if err := os.WriteFile(path, []byte(validFrameRequest), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readGeoFile(path, 20); err == nil {
		t.Fatal("accepted oversized input")
	}
	if _, err := readGeoFile(dir, geoSiteLimit); err == nil {
		t.Fatal("accepted directory input")
	}
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readGeoFile(fifo, geoRequestLimit); err == nil {
		t.Fatal("accepted FIFO input")
	}
}

func TestFrameCLIRejectsInvalidArgumentsWithoutOutput(t *testing.T) {
	for _, args := range [][]string{
		nil, {"bogus"}, {"convert"},
		{"convert", "--site", "unused", "--request", "unused", "--json=false"},
		{"convert", "--site", "unused", "--request", "unused", "extra"},
		{"convert", "--site", "unused", "--request", "unused", "--at", "2026-11-09T12:00:00Z"},
		{"project", "--site", "unused", "--sample", "unused"},
		{"project", "--site", "unused", "--sample", "unused", "--at", "2026-11-09T12:00:00Z", "--request", "unused"},
	} {
		var output bytes.Buffer
		if err := runFrame(args, &output); err == nil || output.Len() != 0 {
			t.Errorf("arguments %v: error=%v output=%q", args, err, output.String())
		}
	}
}

func writeGeoJSON(t *testing.T, value any) string {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "input.json")
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestFrameCLIRotatedPlacementAndGeographicRoundTrip(t *testing.T) {
	site := "../examples/geography/office.json"
	var output bytes.Buffer
	if err := runFrame([]string{"convert", "--site", site, "--request", "../examples/geography/point.json", "--json"}, &output); err != nil {
		t.Fatal(err)
	}
	var converted geography.Result
	if err := json.Unmarshal(output.Bytes(), &converted); err != nil {
		t.Fatal(err)
	}
	for i, expected := range [3]float64{10, 4, 3} {
		if math.Abs(converted.Position[i]-expected) > 1e-10 {
			t.Fatalf("rotated placement: %v", converted.Position)
		}
	}
	if converted.Site != "synthetic-office" || converted.Placement != "illustrative" || converted.UncertaintyMetres < 100 || converted.Geographic.AltitudeDatum != "WGS84-ellipsoid" {
		t.Fatalf("lost placement context: %+v", converted)
	}
	at := time.Date(2026, 11, 9, 22, 0, 0, 0, time.UTC)
	request := geography.GeoRequest{Coordinate: converted.Geographic, To: world.FrameRef{ID: "mezzanine", Revision: "mezzanine-v1"}, At: at, UncertaintyMetres: .02}
	output.Reset()
	if err := runFrame([]string{"locate", "--site", site, "--request", writeGeoJSON(t, request), "--json"}, &output); err != nil {
		t.Fatal(err)
	}
	var located geography.Result
	if err := json.Unmarshal(output.Bytes(), &located); err != nil {
		t.Fatal(err)
	}
	for i, expected := range [3]float64{2, 1, 0} {
		if math.Abs(located.Position[i]-expected) > 1e-6 {
			t.Fatalf("geographic location: %v", located.Position)
		}
	}
	for _, invalid := range []geography.GeoRequest{
		{Coordinate: converted.Geographic, To: world.FrameRef{ID: "mezzanine", Revision: "unknown"}, At: at},
		{Coordinate: converted.Geographic, To: request.To, At: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)},
	} {
		output.Reset()
		if err := runFrame([]string{"locate", "--site", site, "--request", writeGeoJSON(t, invalid)}, &output); err == nil || output.Len() != 0 {
			t.Fatalf("accepted invalid revision/time: %v", err)
		}
	}
}

func TestFrameCLIProjectsActualInspectionSamplesWithoutLosingLineage(t *testing.T) {
	data, err := os.ReadFile("../docs/verification/2026-11-09-live-inspection.json")
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		Inspection struct {
			CapturedUnixMS int64            `json:"captured_unix_ms"`
			Samples        []sensing.Sample `json:"samples"`
		} `json:"inspection_result"`
	}
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Inspection.Samples) != 4 {
		t.Fatal("fixture lost four-sensor capture")
	}
	at := time.UnixMilli(report.Inspection.CapturedUnixMS).UTC().Format(time.RFC3339Nano)
	for _, sample := range report.Inspection.Samples {
		var output bytes.Buffer
		if err := runFrame([]string{"project", "--site", "../examples/geography/office.json", "--sample", writeGeoJSON(t, sample), "--at", at, "--json"}, &output); err != nil {
			t.Fatal(err)
		}
		var result geography.ProjectedSample
		if err := json.Unmarshal(output.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(result.Sample, sample) || result.Location.Placement != "illustrative" || result.Location.Site != sample.SiteID {
			t.Fatalf("projection changed sample lineage or placement: %+v", result)
		}
	}
	sample := report.Inspection.Samples[0]
	sample.PackageSHA256 = strings.Repeat("a", 64)
	var output bytes.Buffer
	if err := runFrame([]string{"project", "--site", "../examples/geography/office.json", "--sample", writeGeoJSON(t, sample), "--at", at}, &output); err == nil || output.Len() != 0 {
		t.Fatal("project accepted another package")
	}
}
