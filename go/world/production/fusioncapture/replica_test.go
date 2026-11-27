package fusioncapture

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/fusion"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/production/proofref"
	"github.com/wendylabsinc/wendy/go/world/production/resultrecord"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
)

func replicaFixture(t *testing.T, spatial bool) (serviceorigin.PublishRequest, []resultrecord.ValidatedInput) {
	t.Helper()
	f := setup(t)
	if spatial {
		f = setupLidar(t, true)
	}
	r, err := EvaluateBatch(context.Background(), f.cfg, Options{Physical: f.clock}, f.proofs, f.batchVerify, f.load)
	if err != nil || r.State != fusion.HazardDetected {
		t.Fatalf("hazard fixture: %s %v", r.State, err)
	}
	var inputs []resultrecord.ValidatedInput
	for _, raw := range f.proofs {
		var signed serviceorigin.SignedRecord
		if json.Unmarshal(raw, &signed) != nil {
			t.Fatal("original fixture envelope")
		}
		v, err := f.verify(context.Background(), signed)
		if err != nil {
			t.Fatal(err)
		}
		c := v.Component()
		sampleUntil, physicalUntil, err := sensorrecord.LiveDeadlines(c)
		if err != nil {
			t.Fatal(err)
		}
		inputs = append(inputs, resultrecord.ValidatedInput{Reference: proofref.Reference{Key: c.Key, Version: c.Version, EnvelopeSHA256: hash(raw), RecordSHA256: hash(signed.Record)}, Component: c, SampleValidUntil: sampleUntil, PhysicalValidUntil: physicalUntil})
	}
	// A stricter original host bound must survive the origin-attested result.
	inputs[0].PhysicalValidUntil = inputs[0].PhysicalValidUntil.Add(-time.Second)
	p, err := BuildPublication(r, inputs, "hazards", f.clock.interval.Latest.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return p, inputs
}

func TestReplicaHazardPreservesExactAncestryAndOriginalDeadlines(t *testing.T) {
	for _, spatial := range []bool{false, true} {
		name := "four-source"
		if spatial {
			name = "seven-source-lidar"
		}
		t.Run(name, func(t *testing.T) {
			p, inputs := replicaFixture(t, spatial)
			before, _ := json.Marshal(p)
			local, err := resultrecord.ValidatePublication(p, inputs)
			if err != nil {
				t.Fatal(err)
			}
			remote, err := ValidateReplicaPublication(p)
			if err != nil {
				t.Fatal(err)
			}
			if !remote.SampleValidUntil.Equal(local.SampleValidUntil) || !remote.PhysicalValidUntil.Equal(local.PhysicalValidUntil) || !reflect.DeepEqual(remote.Result.Inputs, local.Result.Inputs) || !bytes.Equal(remote.Result.ResultJSON, local.Result.ResultJSON) || len(p.Payload) > resultrecord.MaxPayloadBytes {
				t.Fatal("replica changed original proof ancestry, JSON or interpretation bounds")
			}
			if spatial && (remote.Hazard.Spatial == nil || remote.Hazard.Spatial.LidarSupportCount != 432 || len(remote.Hazard.Spatial.LidarSupportPoints) != 32 || len(remote.Hazard.Inputs) != 7) {
				t.Fatal("compact scan support lost")
			}
			after, _ := json.Marshal(p)
			if !bytes.Equal(before, after) {
				t.Fatal("validation mutated original request")
			}
			// Optional public fixture export for the separate native adapter
			// tests. It contains no keys, signatures, image or audio bytes.
			if dir := os.Getenv("WENDY_FUSION_REPLICA_TEST_OUTPUT"); dir != "" {
				if !filepath.IsAbs(dir) {
					t.Fatal("absolute fixture output directory required")
				}
				f, err := os.OpenFile(filepath.Join(dir, name+".json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				_, err = f.Write(before)
				closeErr := f.Close()
				if err != nil || closeErr != nil {
					t.Fatal("fixture output failed", err, closeErr)
				}
			}
		})
	}
}

func TestReplicaHazardRefusesMissingBoundAndChangedAncestry(t *testing.T) {
	p, _ := replicaFixture(t, true)
	original, err := resultrecord.Decode(p.Payload)
	if err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*serviceorigin.PublishRequest, *resultrecord.Result, map[string]json.RawMessage){
		"legacy missing physical bound": func(_ *serviceorigin.PublishRequest, _ *resultrecord.Result, h map[string]json.RawMessage) {
			delete(h, "physical_valid_until")
		},
		"null physical bound": func(_ *serviceorigin.PublishRequest, _ *resultrecord.Result, h map[string]json.RawMessage) {
			h["physical_valid_until"] = []byte("null")
		},
		"unknown field": func(_ *serviceorigin.PublishRequest, _ *resultrecord.Result, h map[string]json.RawMessage) {
			h["unverified_authority"] = []byte("true")
		},
		"short position": func(_ *serviceorigin.PublishRequest, _ *resultrecord.Result, h map[string]json.RawMessage) {
			h["position_m"] = []byte("[0,0]")
		},
		"unbound longer sample deadline": func(q *serviceorigin.PublishRequest, _ *resultrecord.Result, _ map[string]json.RawMessage) {
			q.TTL += time.Second
		},
		"changed model": func(q *serviceorigin.PublishRequest, _ *resultrecord.Result, _ map[string]json.RawMessage) {
			q.Provenance.ModelVersion = "other"
		},
		"changed proof hash": func(_ *serviceorigin.PublishRequest, r *resultrecord.Result, _ map[string]json.RawMessage) {
			r.Inputs[0].EnvelopeSHA256 = strings.Repeat("e", 64)
		},
		"reordered proofs": func(_ *serviceorigin.PublishRequest, r *resultrecord.Result, _ map[string]json.RawMessage) {
			r.Inputs[0], r.Inputs[1] = r.Inputs[1], r.Inputs[0]
		},
		"omitted lineage": func(q *serviceorigin.PublishRequest, _ *resultrecord.Result, _ map[string]json.RawMessage) {
			q.Provenance.Inputs = q.Provenance.Inputs[1:]
		},
		"mixed site": func(_ *serviceorigin.PublishRequest, r *resultrecord.Result, _ map[string]json.RawMessage) {
			r.Inputs[0].Key.Site = "other-site"
		},
	} {
		t.Run(name, func(t *testing.T) {
			q := p
			q.Provenance.Inputs = append(q.Provenance.Inputs[:0:0], q.Provenance.Inputs...)
			r := original
			r.Inputs = append(r.Inputs[:0:0], r.Inputs...)
			var h map[string]json.RawMessage
			json.Unmarshal(r.ResultJSON, &h)
			change(&q, &r, h)
			r.ResultJSON, _ = json.Marshal(h)
			q.Payload, err = resultrecord.Encode(r)
			if err != nil {
				t.Fatal("invalid generic fixture", err)
			}
			if _, err = ValidateReplicaPublication(q); err == nil {
				t.Fatal("accepted changed hazard")
			}
		})
	}
}

func TestReplicaHazardStrictCompactAndJSONBounds(t *testing.T) {
	p, _ := replicaFixture(t, true)
	r, _ := resultrecord.Decode(p.Payload)
	for name, change := range map[string]func([]byte) []byte{
		"duplicate key": func(b []byte) []byte {
			return bytes.Replace(b, []byte(`"label":`), []byte(`"label":"possible-gas-hazard","label":`), 1)
		},
		"case alias": func(b []byte) []byte {
			return bytes.Replace(b, []byte(`"physical_valid_until":`), []byte(`"Physical_Valid_Until":`), 1)
		},
		"oversized result": func(b []byte) []byte { return append(b, bytes.Repeat([]byte(" "), resultrecord.MaxResultBytes)...) },
		"short support point": func(b []byte) []byte {
			var h map[string]json.RawMessage
			json.Unmarshal(b, &h)
			var spatial map[string]json.RawMessage
			json.Unmarshal(h["spatial"], &spatial)
			spatial["lidar_support_points"] = []byte(`[[0,0]]`)
			h["spatial"], _ = json.Marshal(spatial)
			out, _ := json.Marshal(h)
			return out
		},
	} {
		t.Run(name, func(t *testing.T) {
			copy := r
			copy.ResultJSON = change(bytes.Clone(r.ResultJSON))
			q := p
			q.Payload, _ = json.Marshal(copy) // Deliberately bypass generic validation.
			if _, err := ValidateReplicaPublication(q); err == nil {
				t.Fatal("accepted ambiguous or unbounded result")
			}
		})
	}
}

func TestReplicaCameraBoundsAreHalfOpenAtImageEdges(t *testing.T) {
	// These are the pixel-centre/half-open bounds emitted by observeRed for
	// a 2x2 region at the bottom-right corner of an 8x6 image.
	cameras := []fusion.CameraFeature{}
	samples := map[string]*sensing.Sample{}
	for _, id := range []string{"camera-a", "camera-b"} {
		cameras = append(cameras, fusion.CameraFeature{SensorID: id, RedFraction: 4.0 / 48, Observation: &fusion.CameraObservation{Pixels: 4, CentroidPX: [2]float64{7, 5}, BoundsPX: [4]int{6, 4, 8, 6}, RayDirection: [3]float64{0, 0, 1}}})
		samples[id] = &sensing.Sample{Camera: &sensing.Camera{Width: 8, Height: 6}}
	}
	raw, _ := json.Marshal(cameras)
	if err := replicaCameras(raw, cameras, samples, true); err != nil {
		t.Fatal("valid edge-touching half-open bounds refused", err)
	}
	cameras[0].Observation.BoundsPX[2] = 9
	raw, _ = json.Marshal(cameras)
	if replicaCameras(raw, cameras, samples, true) == nil {
		t.Fatal("out-of-image bound accepted")
	}
	cameras[0].Observation.BoundsPX = [4]int{8, 4, 8, 6}
	raw, _ = json.Marshal(cameras)
	if replicaCameras(raw, cameras, samples, true) == nil {
		t.Fatal("empty half-open region accepted")
	}
}

func TestLegacyHazardStillValidLocallyButHasNoRemotePhysicalCap(t *testing.T) {
	p, inputs := replicaFixture(t, false)
	r, _ := resultrecord.Decode(p.Payload)
	var body map[string]json.RawMessage
	json.Unmarshal(r.ResultJSON, &body)
	delete(body, "physical_valid_until")
	r.ResultJSON, _ = json.Marshal(body)
	p.Payload, _ = resultrecord.Encode(r)
	if _, err := resultrecord.ValidatePublication(p, inputs); err != nil {
		t.Fatal("local exact-input path lost legacy compatibility", err)
	}
	if _, err := ValidateReplicaPublication(p); err == nil {
		t.Fatal("remote path invented physical cap from sample or signing time")
	}
}
