package fusioncapture

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/fusion"
	"github.com/wendylabsinc/wendy/go/world/production/proofref"
	"github.com/wendylabsinc/wendy/go/world/production/resultrecord"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
)

func evaluatedResult(t *testing.T) (*fixture, fusion.Result, []resultrecord.ValidatedInput) {
	t.Helper()
	f := setup(t)
	result, err := Evaluate(context.Background(), f.cfg, Options{Physical: f.clock}, f.proofs, f.verify, f.load)
	if err != nil || result.State != fusion.HazardDetected {
		t.Fatal("fixture did not produce a hazard", err)
	}
	var inputs []resultrecord.ValidatedInput
	for _, raw := range f.proofs {
		var signed serviceorigin.SignedRecord
		if err := json.Unmarshal(raw, &signed); err != nil {
			t.Fatal(err)
		}
		verified, err := f.verify(context.Background(), signed)
		if err != nil {
			t.Fatal(err)
		}
		component := verified.Component()
		input := resultrecord.ValidatedInput{Reference: proofref.Reference{Key: component.Key, Version: component.Version, EnvelopeSHA256: hash(raw), RecordSHA256: hash(signed.Record)}, Component: component, PhysicalValidUntil: f.clock.interval.Earliest.Add(5 * time.Second)}
		for _, used := range result.Hazard.Inputs {
			if used.Capture.ProofSHA256 == input.Reference.EnvelopeSHA256 {
				input.SampleValidUntil = used.ValidUntil
			}
		}
		inputs = append(inputs, input)
	}
	return f, result, inputs
}

func TestLidarPublicationKeepsExactSevenProofLineageWithinPayloadBound(t *testing.T) {
	f := setupLidar(t, true)
	result, err := EvaluateBatch(context.Background(), f.cfg, Options{Physical: f.clock}, f.proofs, f.batchVerify, f.load)
	if err != nil || result.State != fusion.HazardDetected || result.Hazard.Spatial == nil {
		t.Fatalf("seven-source fusion: %v %+v", err, result)
	}
	before, err := json.Marshal(result.Hazard)
	if err != nil {
		t.Fatal(err)
	}
	var inputs []resultrecord.ValidatedInput
	for _, raw := range f.proofs {
		var signed serviceorigin.SignedRecord
		if err := json.Unmarshal(raw, &signed); err != nil {
			t.Fatal(err)
		}
		verified, err := f.verify(context.Background(), signed)
		if err != nil {
			t.Fatal(err)
		}
		component := verified.Component()
		sampleUntil, physicalUntil, err := sensorrecord.LiveDeadlines(component)
		if err != nil {
			t.Fatal(err)
		}
		inputs = append(inputs, resultrecord.ValidatedInput{Reference: proofref.Reference{Key: component.Key, Version: component.Version, EnvelopeSHA256: hash(raw), RecordSHA256: hash(signed.Record)}, Component: component, SampleValidUntil: sampleUntil, PhysicalValidUntil: physicalUntil})
	}
	publication, err := BuildPublication(result, inputs, "hazards", f.clock.interval.Latest.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	validated, err := resultrecord.ValidatePublication(publication, inputs)
	if err != nil {
		t.Fatal(err)
	}
	if len(publication.Payload) >= 32<<10 || len(validated.Result.Inputs) != 7 {
		t.Fatal("derived publication exceeds original payload budget or lost inputs")
	}
	var view struct {
		Inputs  []map[string]json.RawMessage `json:"inputs"`
		Spatial fusion.SpatialFeatures       `json:"spatial"`
	}
	if err := json.Unmarshal(validated.Result.ResultJSON, &view); err != nil {
		t.Fatal(err)
	}
	lidar := 0
	for i, used := range result.Hazard.Inputs {
		reference := validated.Result.Inputs[i]
		if reference.Key.Source != used.Source || reference.Version != used.Version || reference.EnvelopeSHA256 != used.Capture.ProofSHA256 || publication.Provenance.Inputs[i].ID != reference.ID() {
			t.Fatal("exact proof source/version/hash lineage changed")
		}
		original, _ := json.Marshal(used)
		var fields map[string]json.RawMessage
		json.Unmarshal(original, &fields)
		for key, value := range fields {
			if key != "sample" && !bytes.Equal(value, view.Inputs[i][key]) {
				t.Fatalf("input field %s changed", key)
			}
		}
		if used.Sample.Lidar == nil {
			if !bytes.Equal(fields["sample"], view.Inputs[i]["sample"]) {
				t.Fatal("non-LiDAR sample changed")
			}
			continue
		}
		lidar++
		if _, present := view.Inputs[i]["sample"]; present {
			t.Fatal("raw scan duplicated in derived claim")
		}
		var summary struct {
			PointCount             int     `json:"point_count"`
			MaxRangeM              float64 `json:"max_range_m"`
			NormalizedSampleSHA256 string  `json:"normalized_sample_sha256"`
		}
		if err := json.Unmarshal(view.Inputs[i]["lidar_summary"], &summary); err != nil {
			t.Fatal(err)
		}
		if summary.PointCount != 144 || summary.MaxRangeM != 6 || summary.NormalizedSampleSHA256 != hash(fields["sample"]) {
			t.Fatal("scan summary is not bound to the exact normalized original")
		}
	}
	after, _ := json.Marshal(result.Hazard)
	if !bytes.Equal(before, after) || lidar != 3 || view.Spatial.LidarSupportCount != 432 || len(view.Spatial.LidarSupportPoints) != 32 || len(result.Hazard.Spatial.LidarSupportPoints) != 432 {
		t.Fatal("summary changed evaluated inputs or hid total support count")
	}
	t.Logf("seven exact proofs, 3x144 points: full hazard=%d bytes, derived payload=%d bytes", len(before), len(publication.Payload))
}

func TestBuildPublicationPreservesHazardAndExactProofReferences(t *testing.T) {
	f, result, inputs := evaluatedResult(t)
	wantJSON, err := json.Marshal(result.Hazard)
	if err != nil {
		t.Fatal(err)
	}
	retainUntil := f.clock.interval.Latest.Add(time.Hour)
	publication, err := BuildPublication(result, inputs, "hazards", retainUntil)
	if err != nil {
		t.Fatal(err)
	}
	validated, err := resultrecord.ValidatePublication(publication, inputs)
	if err != nil {
		t.Fatal(err)
	}
	deadline, _ := json.Marshal(validated.PhysicalValidUntil.UTC())
	wantJSON = append(wantJSON[:len(wantJSON)-1], []byte(`,"physical_valid_until":`)...)
	wantJSON = append(wantJSON, deadline...)
	wantJSON = append(wantJSON, '}')
	if !bytes.Equal(validated.Result.ResultJSON, wantJSON) || validated.Result.ResultSchema != fusion.HazardType || validated.Result.RetainUntilUnixMS != retainUntil.UnixMilli() || publication.Entity != "run-a/possible-gas-hazard" || publication.Stream != "hazards" || !publication.Timing.SampleTime.Equal(result.Hazard.Inputs[0].CaptureStart) || !publication.Timing.SampleTime.Add(publication.TTL).Equal(result.Hazard.ValidUntil) {
		t.Fatal("builder changed hazard bytes, source timing or result identity")
	}
	for i, reference := range validated.Result.Inputs {
		used := result.Hazard.Inputs[i]
		if reference.Key.Source != used.Source || reference.Version != used.Version || reference.EnvelopeSHA256 != used.Capture.ProofSHA256 || publication.Provenance.Inputs[i].ID != reference.ID() {
			t.Fatal("builder invented source, sequence or ancestry")
		}
		found := false
		for _, input := range inputs {
			found = found || reference == input.Reference
		}
		if !found {
			t.Fatal("builder invented an inner record hash")
		}
	}
}

func TestBuildPublicationRejectsMissingForgedOrRewrittenInputs(t *testing.T) {
	for name, change := range map[string]func(*fusion.Result, *[]resultrecord.ValidatedInput){
		"missing host input":   func(_ *fusion.Result, inputs *[]resultrecord.ValidatedInput) { *inputs = (*inputs)[:3] },
		"duplicate host input": func(_ *fusion.Result, inputs *[]resultrecord.ValidatedInput) { (*inputs)[1] = (*inputs)[0] },
		"wrong envelope hash": func(_ *fusion.Result, inputs *[]resultrecord.ValidatedInput) {
			(*inputs)[0].Reference.EnvelopeSHA256 = strings.Repeat("a", 64)
		},
		"wrong actual source": func(r *fusion.Result, _ *[]resultrecord.ValidatedInput) {
			r.Hazard.Inputs[0].Source.Service = "impostor"
		},
		"native sequence as agent version": func(r *fusion.Result, _ *[]resultrecord.ValidatedInput) {
			r.Hazard.Inputs[0].Version.Sequence = r.Hazard.Inputs[0].Sample.Sequence
		},
		"rewritten sample": func(r *fusion.Result, _ *[]resultrecord.ValidatedInput) {
			r.Hazard.Inputs[0].Sample.SensorModel = "different-model"
		},
		"wrong capture time": func(r *fusion.Result, _ *[]resultrecord.ValidatedInput) {
			r.Hazard.Inputs[0].CaptureStart = r.Hazard.Inputs[0].CaptureStart.Add(time.Nanosecond)
		},
		"wrong boot": func(r *fusion.Result, _ *[]resultrecord.ValidatedInput) {
			r.Hazard.Inputs[0].Capture.SceneBootID = "other-boot"
		},
		"later hazard deadline": func(r *fusion.Result, _ *[]resultrecord.ValidatedInput) {
			r.Hazard.ValidUntil = r.Hazard.ValidUntil.Add(time.Second)
		},
		"missing host deadline": func(_ *fusion.Result, inputs *[]resultrecord.ValidatedInput) {
			(*inputs)[0].PhysicalValidUntil = time.Time{}
		},
		"host shortened deadline": func(_ *fusion.Result, inputs *[]resultrecord.ValidatedInput) {
			(*inputs)[0].SampleValidUntil = (*inputs)[0].SampleValidUntil.Add(-time.Nanosecond)
		},
		"not detected": func(r *fusion.Result, _ *[]resultrecord.ValidatedInput) { r.State = fusion.NotDetected },
	} {
		t.Run(name, func(t *testing.T) {
			f, result, inputs := evaluatedResult(t)
			change(&result, &inputs)
			if _, err := BuildPublication(result, inputs, "hazards", f.clock.interval.Latest.Add(time.Hour)); err == nil {
				t.Fatal("accepted forged or rewritten hazard bindings")
			}
		})
	}
}
