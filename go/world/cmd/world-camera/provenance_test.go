package main

import (
	"context"
	"encoding/json"
	"image"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/stationarycamera"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

// This subprocess is a unit-test transport fixture only. Firmware execution is
// verified separately by the emulator integration exercise.
func TestProvenanceCaptureHelper(t *testing.T) {
	if os.Getenv("WORLD_PROVENANCE_HELPER") != "1" {
		return
	}
	for {
		if err := jpeg.Encode(os.Stdout, image.NewRGBA(image.Rect(0, 0, 8, 8)), nil); err != nil {
			os.Exit(1)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestCaptureOriginReachesAuthenticatedViewer(t *testing.T) {
	t.Setenv("WORLD_PROVENANCE_HELPER", "1")
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cli := filepath.Join(t.TempDir(), "test-camera-cli")
	if err := os.WriteFile(cli, []byte("#!/bin/sh\nexec "+strconv.Quote(binary)+" -test.run=^TestProvenanceCaptureHelper$ -- \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, runID := range []string{"", "esp32-firmware-run"} {
		t.Run("run="+runID, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cfg := cameraConfig{ID: "camera-1", Name: "XIAO", Device: "wendy-lite:test-source", CameraID: 0}
			if runID != "" {
				cfg.EmulatedSource = cli
			}
			twin, err := newTwin(ctx, []cameraConfig{cfg}, t.TempDir(), cli, "", "", "private-test-token", true, runID)
			if err != nil {
				t.Fatal(err)
			}
			defer twin.close()
			twin.start()
			camera := twin.cameras[0]
			for len(camera.store.State().Recent) < 2 && ctx.Err() == nil {
				time.Sleep(10 * time.Millisecond)
			}
			if ctx.Err() != nil {
				t.Fatalf("capture never received frames: %+v", camera.store.State())
			}
			request := httptest.NewRequest(http.MethodGet, "/v1/state", nil)
			request.Header.Set("Authorization", "Bearer private-test-token")
			response := httptest.NewRecorder()
			twin.ServeHTTP(response, request)
			var state stationarycamera.State
			if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &state) != nil || len(state.Recent) < 2 {
				t.Fatalf("viewer state: %d %s", response.Code, response.Body.String())
			}
			want := world.Measured
			if runID != "" {
				want = world.Simulated
			}
			for _, receipt := range state.Recent {
				if receipt.Origin != want || receipt.SimulationRun != runID {
					t.Fatalf("capture/viewer provenance: %+v", receipt)
				}
			}
			unauthorized := httptest.NewRecorder()
			twin.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/v1/state", nil))
			if unauthorized.Code != http.StatusUnauthorized {
				t.Fatal("simulation bypassed viewer authentication")
			}
		})
	}
}

func TestSimulationFlagRejectsEmptyRun(t *testing.T) {
	for _, value := range []string{"", "   ", "bad\nrun", strings.Repeat("x", 257)} {
		if err := run([]string{"--simulation-run", value}); err == nil || !strings.Contains(err.Error(), "--simulation-run") {
			t.Fatalf("invalid run %q: %v", value, err)
		}
	}
}

func TestEmulatedSourceRequiresExplicitSimulation(t *testing.T) {
	configs := []cameraConfig{{ID: "camera-1", Name: "XIAO", Device: "wendy-lite:test-source", EmulatedSource: "/private/observer"}}
	if service, err := newTwin(context.Background(), configs, t.TempDir(), "wendy", "", "", "token", true); err == nil {
		service.close()
		t.Fatal("emulated source accepted without a simulation run")
	}
	configs[0].EmulatedSource = "relative-observer"
	if err := validateCameras(configs); err == nil {
		t.Fatal("relative emulated executable accepted")
	}
	if err := run([]string{"--emulated-source", "/private/observer"}); err == nil || !strings.Contains(err.Error(), "--simulation-run") {
		t.Fatalf("CLI accepted emulated source without run: %v", err)
	}
}
