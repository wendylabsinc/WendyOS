package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/cameratwin"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

// TestTwinNativeWorkflow runs the real solver and native twin controller against
// two synthetic camera stores. The fixture model and JPEG remain outside Git.
func TestTwinNativeWorkflow(t *testing.T) {
	binary, site, imagePath, helper := os.Getenv("WORLD_TWIN_NATIVE_CHECK"), os.Getenv("WORLD_TWIN_SITE_DIR"), os.Getenv("WORLD_TWIN_FRAME_JPEG"), os.Getenv("WORLD_POSE_HELPER")
	if binary == "" || site == "" || imagePath == "" || helper == "" {
		t.Skip("set WORLD_TWIN_NATIVE_CHECK, WORLD_TWIN_SITE_DIR, WORLD_TWIN_FRAME_JPEG and WORLD_POSE_HELPER")
	}
	out := os.Getenv("WORLD_TWIN_REVIEW_DIR")
	if out == "" {
		out = t.TempDir()
	}
	if !filepath.IsAbs(out) {
		t.Fatal("private absolute output directory required")
	}
	if err := os.MkdirAll(out, 0700); err != nil {
		t.Fatal(err)
	}
	image, err := os.ReadFile(imagePath)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	secret := make([]byte, 32)
	if _, err = rand.Read(secret); err != nil {
		t.Fatal(err)
	}
	token := hex.EncodeToString(secret)
	configs := []cameraConfig{{ID: "camera-1", Name: "Window camera", Device: "wendy-lite:synthetic-one", CameraID: 0}, {ID: "camera-2", Name: "Door camera", Device: "wendy-lite:synthetic-two", CameraID: 0}}
	calibrationProfile := os.Getenv("WORLD_TWIN_CALIBRATION_PROFILE")
	if calibrationProfile != "" {
		binding, err := readCalibrationBinding(os.Getenv("WORLD_TWIN_CALIBRATION_BINDING"))
		if err != nil {
			t.Fatal(err)
		}
		for i := range configs {
			bound := *binding
			bound.HardwareID = configs[i].Device
			configs[i].CalibrationProfile = calibrationProfile
			configs[i].CalibrationBinding = &bound
		}
	}
	service, err := newTwin(ctx, configs, filepath.Join(out, "state"), "/usr/bin/false", site, helper, token, false)
	if err != nil {
		t.Fatal(err)
	}
	defer service.close()
	for i, c := range service.cameras {
		if err = c.store.AcceptJPEG(image, "synthetic-twin-session-"+string(rune('a'+i)), 1, world.Simulated); err != nil {
			t.Fatal(err)
		}
	}
	// Drive only simulated receipts. No Wendy hardware stream is opened here.
	done := make(chan struct{})
	defer func() { cancel(); <-done }()
	go func() {
		defer close(done)
		ticker := time.NewTicker(400 * time.Millisecond)
		defer ticker.Stop()
		seq := uint64(1)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				seq++
				for i, c := range service.cameras {
					if c.store.State().Connection != "stopped" {
						_ = c.store.AcceptJPEG(image, "synthetic-twin-session-"+string(rune('a'+i)), seq, world.Simulated)
					}
				}
			}
		}
	}()
	service.done = make(chan struct{})
	go func() { defer close(service.done); service.automaticLoop() }()
	var httpMu sync.Mutex
	var httpChecks []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/twin/observations" {
			service.ServeHTTP(w, r)
			return
		}
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 128<<10))
		r.Body = io.NopCloser(bytes.NewReader(raw))
		record := httptest.NewRecorder()
		service.ServeHTTP(record, r)
		httpMu.Lock()
		if len(httpChecks) < 512 {
			httpChecks = append(httpChecks, map[string]any{"method": r.Method, "status": record.Code, "request": string(raw), "response": record.Body.String()})
		}
		httpMu.Unlock()
		for k, v := range record.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(record.Code)
		_, _ = w.Write(record.Body.Bytes())
	}))
	defer server.Close()
	profile, _ := json.Marshal(map[string]string{"url": server.URL, "token": token})
	profilePath := filepath.Join(out, "viewer-profile.json")
	if err = os.WriteFile(profilePath, profile, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, binary, "--record", profilePath, filepath.Join(out, "recording"))
	output, runErr := cmd.CombinedOutput()
	if err = os.WriteFile(filepath.Join(out, "native.log"), output, 0600); err != nil {
		t.Fatal(err)
	}
	httpMu.Lock()
	trace, _ := json.MarshalIndent(httpChecks, "", "  ")
	httpMu.Unlock()
	if err = os.WriteFile(filepath.Join(out, "observation-http.json"), trace, 0600); err != nil {
		t.Fatal(err)
	}
	if runErr != nil {
		t.Fatalf("native twin workflow: %v; inspect private native.log and observation-http.json", runErr)
	}
	t.Log("Native twin controller completed; detailed output retained in private native.log")
	state := service.snapshot()
	for _, c := range state.Cameras {
		if !c.Pose.Fresh || c.Pose.Pose == nil || c.Pose.InputOrigin != world.Simulated {
			t.Fatalf("camera %s has no accepted synthetic pose: %s", c.ID, c.Pose.Detail)
		}
		if calibrationProfile != "" && (c.Pose.Pose.Lens.Quality != "model_calibration" || c.Pose.Pose.Lens.Profile == nil || c.Pose.Calibration == nil || c.Pose.Pose.Lens.Profile.Origin != world.Simulated) {
			t.Fatalf("camera %s lost model calibration provenance", c.ID)
		}
	}
	// Publish one explicitly synthetic projected person through the authenticated
	// HTTP contract, then read it through an independent client. This is a
	// projection fixture, not a claim that Vision detected the generated texture.
	camera := state.Cameras[0]
	receipt := camera.State.Latest
	pose := camera.Pose.Pose
	batch := cameratwin.Batch{Schema: cameratwin.Schema, CoordinateFrame: camera.Pose.CoordinateFrame, ModelSHA256: state.Site.ModelSHA256, FloorY: 0, Observations: []cameratwin.Observation{{ID: "synthetic-projection-check", Position: [3]float64{1, 0, 3}, ObservedAt: receipt.ReceivedAt, ValidUntil: receipt.ReceivedAt.Add(cameratwin.Lifetime), Provenance: "inferred", InputOrigin: world.Simulated, Inputs: []cameratwin.Input{{CameraID: camera.ID, ReceiptID: receipt.ID, SHA256: receipt.SHA256, SourceSession: receipt.SourceSession, Sequence: receipt.Sequence, ReceivedAt: receipt.ReceivedAt, Origin: receipt.Origin, PoseObservedAt: pose.ObservedAt}}}}}
	payload, _ := json.Marshal(batch)
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/twin/observations", bytes.NewReader(payload))
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("shared observation publication: %s", body)
	}
	request, _ = http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/v1/twin/observations", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response, err = (&http.Client{Timeout: 3 * time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var observed cameratwin.Batch
	err = json.NewDecoder(response.Body).Decode(&observed)
	response.Body.Close()
	if err != nil || len(observed.Observations) != 1 {
		t.Fatal("independent reader did not see shared synthetic person", err)
	}
	state = service.snapshot()
	report, _ := json.MarshalIndent(state, "", "  ")
	if err = os.WriteFile(filepath.Join(out, "twin-result.json"), report, 0600); err != nil {
		t.Fatal(err)
	}
}
