package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/stationarycamera"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

// The optional native check exercises the actual Swift client against this
// command's HTTP handler and durable stores. Every input is marked simulated.
func TestNativeCameraIntegration(t *testing.T) {
	binary := os.Getenv("WORLD_CAMERA_NATIVE_CHECK")
	if binary == "" {
		t.Skip("set WORLD_CAMERA_NATIVE_CHECK to the compiled StationaryCameraChecks executable")
	}
	out := os.Getenv("WORLD_CAMERA_REVIEW_DIR")
	if out == "" {
		out = t.TempDir()
	} else if !filepath.IsAbs(out) {
		t.Fatal("absolute review output required")
	}
	if err := os.MkdirAll(out, 0700); err != nil {
		t.Fatal(err)
	}
	s, err := stationarycamera.Open(stationarycamera.Config{Dir: filepath.Join(out, "state"), Tenant: "native-fixture", Site: "synthetic-camera-check", Device: "simulated-source", Camera: "Test camera"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	makeJPEG := func(second bool) []byte {
		img := image.NewRGBA(image.Rect(0, 0, 960, 540))
		for y := 0; y < 540; y++ {
			for x := 0; x < 960; x++ {
				c := color.RGBA{R: uint8(28 + x/12), G: uint8(70 + y/8), B: 90, A: 255}
				if x > 160 && x < 800 && y > 100 && y < 440 {
					c = color.RGBA{R: 140, G: 190, B: 175, A: 255}
				}
				if x > 350 && x < 610 && y > 180 && y < 360 {
					if second {
						c = color.RGBA{R: 220, G: 150, B: 80, A: 255}
					} else {
						c = color.RGBA{R: 70, G: 110, B: 130, A: 255}
					}
				}
				img.SetRGBA(x, y, c)
			}
		}
		var b bytes.Buffer
		if err := jpeg.Encode(&b, img, &jpeg.Options{Quality: 85}); err != nil {
			t.Fatal(err)
		}
		return b.Bytes()
	}
	first, second := makeJPEG(false), makeJPEG(true)
	var mu sync.Mutex
	active, changed := true, false
	sequence := uint64(0)
	accept := func() error {
		sequence++
		data, session := first, "native-fixture-a"
		if changed {
			data, session = second, "native-fixture-b"
		}
		return s.AcceptJPEG(data, session, sequence, world.Simulated)
	}
	if err = accept(); err != nil {
		t.Fatal(err)
	}
	var secret [32]byte
	if _, err = rand.Read(secret[:]); err != nil {
		t.Fatal(err)
	}
	token := hex.EncodeToString(secret[:])
	server := httptest.NewServer(handler(s, token, func(action string) error {
		mu.Lock()
		defer mu.Unlock()
		if action == "pause" {
			active = false
			s.SetConnection("stopped", "Fixture feed paused")
			return nil
		}
		changed, active = true, true
		s.SetConnection("connecting", "Fixture reconnect")
		return accept()
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	defer func() { cancel(); <-done }()
	go func() {
		defer close(done)
		timer := time.NewTicker(500 * time.Millisecond)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				mu.Lock()
				if active {
					_ = accept()
				}
				mu.Unlock()
			}
		}
	}()
	profile, _ := json.Marshal(map[string]string{"url": server.URL, "token": token})
	path := filepath.Join(out, "viewer-profile.json")
	if err = os.WriteFile(path, profile, 0600); err != nil {
		t.Fatal(err)
	}
	runCtx, runCancel := context.WithTimeout(ctx, 90*time.Second)
	defer runCancel()
	mode := "--integration"
	if os.Getenv("WORLD_CAMERA_RECORD") == "1" {
		mode = "--record"
	}
	cmd := exec.CommandContext(runCtx, binary, mode, path, out)
	result, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("native integration failed: %v\n%s", err, result)
	}
	t.Log(string(result))
}

func TestCameraHTTPWorkflow(t *testing.T) {
	s, err := stationarycamera.Open(stationarycamera.Config{Dir: t.TempDir(), Tenant: "test", Site: "test", Device: "fixture", Camera: "4"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	img := image.NewRGBA(image.Rect(0, 0, 32, 24))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var jpegBytes bytes.Buffer
	if err = jpeg.Encode(&jpegBytes, img, nil); err != nil {
		t.Fatal(err)
	}
	if err = s.AcceptJPEG(jpegBytes.Bytes(), "fixture-session", 1, world.Simulated); err != nil {
		t.Fatal(err)
	}
	var secret [32]byte
	if _, err = rand.Read(secret[:]); err != nil {
		t.Fatal(err)
	}
	token := hex.EncodeToString(secret[:])
	server := httptest.NewServer(handler(s, token, func(action string) error {
		if action == "pause" {
			s.SetConnection("stopped", "paused")
		} else {
			s.SetConnection("connecting", "reconnecting")
		}
		return nil
	}))
	defer server.Close()
	request := func(method, path, body, auth string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if auth != "" {
			req.Header.Set("Authorization", "Bearer "+auth)
		}
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	for _, path := range []string{"/v1/state", "/v1/evidence/" + s.State().Latest.SHA256, "/v1/control"} {
		resp := request("GET", path, "", "")
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Fatalf("unauthorized %s: %d", path, resp.StatusCode)
		}
	}
	resp := request("GET", "/v1/state", "", token)
	var before stationarycamera.State
	err = json.NewDecoder(resp.Body).Decode(&before)
	resp.Body.Close()
	if err != nil || !before.Fresh || before.Latest.Origin != world.Simulated || before.ExposureTimeKnown {
		t.Fatalf("wrong state: %+v %v", before, err)
	}
	resp = request("POST", "/v1/control", `{"action":"pause"}`, token)
	resp.Body.Close()
	if resp.StatusCode != 200 || s.State().Fresh {
		t.Fatal("pause did not invalidate feed")
	}
	resp = request("GET", "/v1/evidence/"+before.Latest.SHA256, "", token)
	got, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != 200 || !bytes.Equal(got, jpegBytes.Bytes()) {
		t.Fatal("paused evidence did not preserve original JPEG")
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("private evidence must not be cached by shared HTTP caches")
	}
	resp = request("POST", "/v1/control", `{"action":"resume"}`, token)
	resp.Body.Close()
	if resp.StatusCode != 200 || s.State().Fresh {
		t.Fatal("resume must wait for a fresh frame")
	}
	if err = s.AcceptJPEG(jpegBytes.Bytes(), "fixture-session-2", 1, world.Simulated); err != nil {
		t.Fatal(err)
	}
	if !s.State().Fresh {
		t.Fatal("new stream did not restore feed")
	}
}

func TestCameraControlRejectsAmbiguousRequests(t *testing.T) {
	s, err := stationarycamera.Open(stationarycamera.Config{Dir: t.TempDir(), Tenant: "test", Site: "test", Device: "fixture", Camera: "4"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	calls := 0
	h := handler(s, "token", func(string) error { calls++; return nil })
	for _, body := range []string{`{"action":"move"}`, `{"action":"resume","device":"another"}`, `{"action":"pause"} {"action":"resume"}`, strings.Repeat("x", 2048)} {
		req := httptest.NewRequest("POST", "/v1/control", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer token")
		response := httptest.NewRecorder()
		h.ServeHTTP(response, req)
		if response.Code != 400 {
			t.Errorf("request accepted: %d", response.Code)
		}
	}
	if calls != 0 {
		t.Fatal("invalid request changed capture")
	}
	for _, path := range []string{"/v1/evidence/not-a-digest", "/v1/evidence/" + strings.Repeat("a", 64)} {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Authorization", "Bearer token")
		response := httptest.NewRecorder()
		h.ServeHTTP(response, req)
		if response.Code != 404 {
			t.Fatal("unretained evidence served")
		}
	}
}
