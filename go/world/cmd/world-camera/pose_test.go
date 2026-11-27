package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/camerapose"
	"github.com/wendylabsinc/wendy/go/world/internal/stationarycamera"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func TestPoseEndpointAccess(t *testing.T) {
	s, err := stationarycamera.Open(stationarycamera.Config{Dir: t.TempDir(), Tenant: "test", Site: "test", Device: "fixture", Camera: "0"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p, err := camerapose.New(camerapose.Config{}, s)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	h := handler(s, "private-token", func(string) error { return nil }, poseEndpoint{p, context.Background()})
	for _, path := range []string{"/v1/pose", "/v1/pose/estimate"} {
		r := httptest.NewRequest(http.MethodPost, path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatal("pose API allowed unauthenticated access")
		}
	}
	r := httptest.NewRequest(http.MethodGet, "/v1/pose", nil)
	r.Header.Set("Authorization", "Bearer private-token")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var state camerapose.State
	if json.Unmarshal(w.Body.Bytes(), &state) != nil || state.Status != "disabled" || state.Pose != nil {
		t.Fatal(w.Body.String())
	}
}

// This opt-in check runs WendyOS's actual native localization solver on generated
// texture/depth references. Every World receipt is explicitly simulated.
func TestNativePoseWorkflow(t *testing.T) {
	helper, fixture := os.Getenv("WORLD_POSE_HELPER"), os.Getenv("WORLD_POSE_FIXTURE")
	if helper == "" || fixture == "" {
		t.Skip("set WORLD_POSE_HELPER and WORLD_POSE_FIXTURE for real native solver integration")
	}
	out := os.Getenv("WORLD_POSE_REVIEW_DIR")
	if out == "" {
		out = t.TempDir()
	}
	if !filepath.IsAbs(out) {
		t.Fatal("absolute private output required")
	}
	if err := os.MkdirAll(out, 0700); err != nil {
		t.Fatal(err)
	}
	write := func(name string, data []byte) {
		t.Helper()
		if err := os.WriteFile(name, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	read := func(name string) []byte {
		t.Helper()
		b, e := os.ReadFile(name)
		if e != nil {
			t.Fatal(e)
		}
		return b
	}
	site := filepath.Join(out, "site")
	if err := os.MkdirAll(site, 0700); err != nil {
		t.Fatal(err)
	}
	// These model bytes bind this synthetic reference fixture only; no model is rendered.
	model := []byte("World native localization synthetic reference fixture v1\n")
	digest := sha256.Sum256(model)
	write(filepath.Join(site, "model.usdz"), model)
	meta, _ := json.Marshal(map[string]any{"id": "synthetic-room", "name": "Synthetic reference scene", "modelRevision": "synthetic-v1"})
	write(filepath.Join(site, "site.json"), meta)
	meta, _ = json.Marshal(map[string]any{"version": 1, "modelRevision": "synthetic-v1", "modelSHA256": hex.EncodeToString(digest[:]), "frameCount": 1, "capturedAt": 0, "landmarks": read(filepath.Join(fixture, "map.f32"))})
	write(filepath.Join(site, "visual-map.json"), meta)
	pgmJPEG := func(name string) []byte {
		t.Helper()
		parts := bytes.SplitN(read(filepath.Join(fixture, name)), []byte("\n"), 4)
		if len(parts) != 4 || string(parts[0]) != "P5" || string(parts[2]) != "255" {
			t.Fatal("invalid synthetic PGM")
		}
		var w, h int
		fmt.Sscanf(string(parts[1]), "%d %d", &w, &h)
		if w <= 0 || h <= 0 || len(parts[3]) != w*h {
			t.Fatal("invalid synthetic pixels")
		}
		im := image.NewGray(image.Rect(0, 0, w, h))
		copy(im.Pix, parts[3])
		var b bytes.Buffer
		if err := jpeg.Encode(&b, im, &jpeg.Options{Quality: 100}); err != nil {
			t.Fatal(err)
		}
		return b.Bytes()
	}
	images := [][]byte{pgmJPEG("first.pgm"), pgmJPEG("second.pgm")}
	s, err := stationarycamera.Open(stationarycamera.Config{Dir: filepath.Join(out, "state"), Tenant: "native-pose-fixture", Site: "synthetic-room", Device: "simulated-camera", Camera: "Synthetic camera"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.AcceptJPEG(images[0], "synthetic-pose-session", 1, world.Simulated); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	defer func() { cancel(); <-done }()
	go func() {
		defer close(done)
		tick := time.NewTicker(500 * time.Millisecond)
		defer tick.Stop()
		seq := uint64(1)
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				seq++
				_ = s.AcceptJPEG(images[seq%2], "synthetic-pose-session", seq, world.Simulated)
			}
		}
	}()
	p, err := camerapose.New(camerapose.Config{Helper: helper, SiteDir: site, WorkDir: filepath.Join(out, "work")}, s)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	secret := sha256.Sum256([]byte(out + time.Now().String()))
	token := hex.EncodeToString(secret[:])
	server := httptest.NewServer(handler(s, token, func(string) error { return nil }, poseEndpoint{p, ctx}))
	defer server.Close()
	profile, _ := json.Marshal(map[string]string{"url": server.URL, "token": token})
	profilePath := filepath.Join(out, "viewer-profile.json")
	write(profilePath, profile)
	if binary := os.Getenv("WORLD_POSE_NATIVE_CHECK"); binary != "" {
		runCtx, stop := context.WithTimeout(ctx, 2*time.Minute)
		defer stop()
		cmd := exec.CommandContext(runCtx, binary, "--record", profilePath, filepath.Join(out, "recording"))
		result, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("native pose recording: %v\n%s", err, result)
		}
		t.Log(string(result))
	} else {
		r, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/pose/estimate", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		resp, err := server.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			t.Fatal(resp.Status)
		}
	}
	deadline := time.Now().Add(90 * time.Second)
	state := p.Snapshot()
	for state.Status == "estimating" && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		state = p.Snapshot()
	}
	if state.Status != "located" || state.Pose == nil || !state.Fresh || state.InputOrigin != world.Simulated {
		t.Fatalf("actual native solve: %+v", state)
	}
	for i, want := range []float64{1, 1.5, 2} {
		if math.Abs(state.Pose.Position[i]-want) > .02 {
			t.Fatalf("unexpected native pose: %v", state.Pose.Position)
		}
	}
	if len(state.Evidence) != 2 || state.Pose.Inliers < 16 {
		t.Fatal("missing two-frame geometric evidence")
	}
	result, _ := json.MarshalIndent(state, "", "  ")
	write(filepath.Join(out, "pose-result.json"), result)
	t.Logf("actual native solver: position %v, inliers %d, RMS %.4f pixels", state.Pose.Position, state.Pose.Inliers, state.Pose.RMSPixels)
}
