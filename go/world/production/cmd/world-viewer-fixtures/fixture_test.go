package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/production/fusioncapture"
	"github.com/wendylabsinc/wendy/go/world/production/resultrecord"
	"github.com/wendylabsinc/wendy/go/world/production/trackcapture"
)

func TestCanonicalFixtureMatchesGenerator(t *testing.T) {
	f, err := generate()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, '\n')
	stored, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "native-inference-viewer-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, stored) {
		t.Fatal("canonical fixture drift; regenerate with world-viewer-fixtures")
	}
}

func TestDirectSnapshotCarriesOriginalBase64Result(t *testing.T) {
	f, err := generate()
	if err != nil {
		t.Fatal(err)
	}
	// This is the actual Go HTTP client DTO, not a native QueryReply or an
	// app reader receipt. world.Component.Payload is an object in this wire.
	var roundtrip client.Snapshot
	if err = json.Unmarshal(jsonBytes(f.Snapshot), &roundtrip); err != nil {
		t.Fatal(err)
	}
	for i, c := range roundtrip.Components {
		if err = validate(c); err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		var wrapper map[string]json.RawMessage
		if err = json.Unmarshal(c.Payload, &wrapper); err != nil {
			t.Fatal(err)
		}
		var encoded string
		if err = json.Unmarshal(wrapper["result_json"], &encoded); err != nil {
			t.Fatal("result_json is not a base64 string", err)
		}
		original, err := base64.StdEncoding.Strict().DecodeString(encoded)
		if err != nil || hash(original) != f.Cases[i].ResultJSONSHA256 {
			t.Fatal("typed original changed", err)
		}
		decoded, err := resultrecord.Decode(c.Payload)
		if err != nil || !bytes.Equal(original, decoded.ResultJSON) {
			t.Fatal("wrapper does not preserve exact original", err)
		}
		if !reflect.DeepEqual(decoded.Inputs, f.Cases[i].Expected["input_refs"]) {
			t.Fatal("reference set changed")
		}
	}
}

func TestTypedMotionAndSpatialHazardKeepIndependentBounds(t *testing.T) {
	f, err := generate()
	if err != nil {
		t.Fatal(err)
	}
	known, err := trackcapture.ValidatePublication(request(f.Snapshot.Components[0]))
	if err != nil {
		t.Fatal(err)
	}
	unknown, err := trackcapture.ValidatePublication(request(f.Snapshot.Components[1]))
	if err != nil {
		t.Fatal(err)
	}
	if !known.MotionKnown || known.VelocityXZ == nil || len(known.Current) != 2 || len(known.Previous) != 2 || known.Ambiguous {
		t.Fatal("known two-observation motion lost")
	}
	if unknown.MotionKnown || unknown.VelocityXZ != nil || len(unknown.Previous) != 0 || !unknown.Ambiguous {
		t.Fatal("unknown motion invented or ambiguity lost")
	}
	if !known.ValidUntil.Before(unknown.ValidUntil) || !known.PhysicalValidUntil().Before(unknown.PhysicalValidUntil()) {
		t.Fatal("previous input did not retain its earlier bounds")
	}
	h, err := fusioncapture.ValidateReplicaPublication(request(f.Snapshot.Components[2]))
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Hazard.Inputs) != 5 || len(h.Hazard.Evidence) != 3 || h.Hazard.Spatial == nil || h.Hazard.Spatial.LidarSupportCount != 2 {
		t.Fatal("hazard source or evidence data lost")
	}
	lidar := h.Hazard.Inputs[4]
	if lidar.Sample != nil || lidar.LidarSummary == nil || lidar.LidarSummary.PointCount != 2 {
		t.Fatal("producer LiDAR summary representation changed")
	}
	if !h.SampleValidUntil.Before(h.PhysicalValidUntil) || !h.PhysicalValidUntil.Before(time.UnixMilli(h.Result.RetainUntilUnixMS)) {
		t.Fatal("separate clock/retention bounds lost")
	}
	if h.Hazard.Inputs[0].Source.Device == h.Hazard.Inputs[1].Source.Device {
		t.Fatal("cross-gateway source identity lost")
	}
}

func TestInvalidViewerCasesFailTypedProducerChecks(t *testing.T) {
	f, err := generate()
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Rejections) != 7 {
		t.Fatal("missing contract rejection cases")
	}
	for _, test := range f.Rejections {
		t.Run(test.Name, func(t *testing.T) {
			if err := validate(test.Component); err == nil {
				t.Fatal("invalid typed component accepted")
			}
		})
	}
}
