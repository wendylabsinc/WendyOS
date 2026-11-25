package livepeople

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/pose"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/production/poseprocess"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
)

// This optional local check runs the actual pinned image model on recorded
// MuJoCo pixels/calibration. Certificates, app RPC and capture clock are test
// fixtures. It does not claim a deployed Agent or native peer transfer.
func TestActualONNXThroughSignedAppRPC(t *testing.T) {
	root := os.Getenv("WENDY_LIVEPEOPLE_MODEL_SMOKE_ROOT")
	if root == "" {
		t.Skip("set WENDY_LIVEPEOPLE_MODEL_SMOKE_ROOT for the local pinned-model check")
	}
	f, api := setupPeople(t)
	file, err := os.Open(filepath.Join(root, ".local/people-walkthrough-20261112-b/perception-inputs.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 256<<10)
	if !scanner.Scan() {
		t.Fatal("missing captured sample row")
	}
	// Deliberately decode only samples. Recorded pose results and separate
	// simulator truth are not used to produce or assert detections here.
	var row struct {
		Images []struct {
			Sample sensing.Sample `json:"sample"`
		} `json:"images"`
	}
	if err = json.Unmarshal(scanner.Bytes(), &row); err != nil || len(row.Images) != 2 {
		t.Fatalf("captured sample row: %v", err)
	}
	f.cfg.ModelID = "google/movenet/multipose/lightning"
	f.cfg.ModelVersion = "1+onnx.8506174df310cf154b97352afa98d1ea4b1e4b47.adapter1.input384"
	f.cfg.ModelSHA256 = "60f724a56f0119d22d2afcf530019db307c77d4ea598eb5bd53bb65afe93930d"
	for _, image := range row.Images {
		sample := image.Sample
		if sample.Evidence == nil || !sensing.DigestValid(sample.Evidence.SHA256) {
			t.Fatal("captured image reference missing")
		}
		data, e := os.ReadFile(filepath.Join(root, ".local/people-walkthrough-20261112-b/timeline/assets", sample.Evidence.SHA256+".png"))
		if e != nil || digest(data) != sample.Evidence.SHA256 {
			t.Fatalf("original image unavailable: %v", e)
		}
		// Keep actual pixels, camera geometry and image reference. The ordinary
		// app test signs them under its disposable test scene/identity pins.
		sample.RunID, sample.SiteID, sample.Frame = f.cfg.RunID, f.cfg.Site, f.cfg.Frame
		sample.Sequence, sample.SampleNS = 18, int64(250*time.Millisecond)
		sample.ID = sample.RunID + "/" + sample.SensorID + "/18"
		sample.PackageSHA256, sample.ScenarioVersion, sample.Seed = f.cfg.PackageSHA256, f.cfg.ScenarioVersion, f.cfg.Seed
		raw, _ := json.Marshal(sample)
		w, _ := f.clock.Now()
		capture := sensorrecord.Capture{Version: 1, SceneBootID: f.cfg.SceneBootID, CapturedUnixNS: w.Earliest.UnixNano(), SimulationAnchorUnixNS: f.cfg.SimulationAnchorUnixNS, SampleJSON: raw}
		request, e := sensorrecord.Publication(capture, 3*time.Second)
		if e != nil {
			t.Fatal(e)
		}
		signed, e := f.publishers[sample.SensorID].Publish(context.Background(), request)
		if e != nil {
			t.Fatal(e)
		}
		f.proofs[sample.SensorID], _ = json.Marshal(signed)
		f.media[sample.SensorID+"/"+sample.Evidence.SHA256] = data
	}
	directory := t.TempDir()
	if err = os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	worker, err := poseprocess.New(context.Background(), poseprocess.Config{Command: []string{filepath.Join(root, ".local/perception-venv/bin/python"), filepath.Join(root, "perception/pose.py"), "--model", filepath.Join(root, ".local/perception-models/movenet-multipose-lightning.onnx"), "--serve"}, Directory: directory, Timeout: 4 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	session, err := New(f.cfg, api, worker.Infer)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	receipt, err := session.Step(context.Background())
	if err != nil || len(receipt.Tracks) != 2 || len(receipt.Publications) != 2 {
		t.Fatalf("actual model/app cycle tracks=%d publications=%d: %v", len(receipt.Tracks), len(receipt.Publications), err)
	}
	for _, p := range receipt.Publications {
		if !p.LocallyVerified {
			t.Fatal("actual image result acknowledgment unverified")
		}
	}
	t.Logf("actual ONNX image inference: %d camera proofs, %d anonymous tracks, %d verified acknowledgments", len(receipt.Inputs), len(receipt.Tracks), len(receipt.Publications))
	saveSmoke(t, "signed-app-fixture.json", receipt)
}

func TestActualCameraSourceDecoderAndONNX(t *testing.T) {
	root := os.Getenv("WENDY_LIVEPEOPLE_MODEL_SMOKE_ROOT")
	if root == "" {
		t.Skip("set WENDY_LIVEPEOPLE_MODEL_SMOKE_ROOT for the local pinned-model check")
	}
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	worker, err := poseprocess.New(context.Background(), poseprocess.Config{Command: []string{filepath.Join(root, ".local/perception-venv/bin/python"), filepath.Join(root, "perception/pose.py"), "--model", filepath.Join(root, ".local/perception-models/movenet-multipose-lightning.onnx"), "--serve"}, Directory: directory, Timeout: 4 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	for _, name := range []string{"camera-a", "camera-b"} {
		raw, e := os.ReadFile(filepath.Join(root, ".local/people-app-source-20261112-a", name+".json"))
		if e != nil {
			t.Fatal(e)
		}
		var envelope struct {
			Capture sensorrecord.Capture `json:"capture"`
		}
		if e = json.Unmarshal(raw, &envelope); e != nil {
			t.Fatal(e)
		}
		data, e := os.ReadFile(filepath.Join(root, ".local/people-app-source-20261112-a", name+".png"))
		if e != nil {
			t.Fatal(e)
		}
		if e = sensorrecord.ValidateMedia(envelope.Capture, data); e != nil {
			t.Fatal(e)
		}
		sample, e := envelope.Capture.NormalizedSample()
		if e != nil {
			t.Fatal(e)
		}
		result, e := worker.Infer(context.Background(), data)
		if e != nil {
			t.Fatal(e)
		}
		if result.ImageSHA256 != sample.Evidence.SHA256 {
			t.Fatal("actual source image digest changed")
		}
		projected, e := pose.Project(*sample.Camera, result, pose.Config{AnkleHeightM: .08, FloorUncertaintyM: .08, CameraUncertaintyM: sample.UncertaintyM})
		if e != nil || len(projected.Detections) != 2 {
			t.Fatalf("%s actual model/projector: %d usable detections, %v", name, len(projected.Detections), e)
		}
		t.Logf("%s: exact image %s, two projected people; original timestamp retained, no current authority assertion", name, result.ImageSHA256)
		saveSmoke(t, name+"-projection.json", struct {
			Poses            pose.ImageResult `json:"poses"`
			Projection       pose.Result      `json:"projection"`
			CapturedUnixNS   int64            `json:"original_captured_unix_ns"`
			CurrentAuthority bool             `json:"current_authority_asserted"`
		}{result, projected, envelope.Capture.CapturedUnixNS, false})
	}
}

func saveSmoke(t *testing.T, name string, value any) {
	t.Helper()
	directory := os.Getenv("WENDY_LIVEPEOPLE_SMOKE_OUTPUT")
	if directory == "" {
		return
	}
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(directory, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.Write(append(raw, '\n')); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
}
