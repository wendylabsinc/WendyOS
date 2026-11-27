package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func twinFixture(t *testing.T) *twinService {
	t.Helper()
	site := t.TempDir()
	_ = os.WriteFile(filepath.Join(site, "site.json"), []byte(`{"id":"test-site","name":"Synthetic room","modelRevision":"r1","metresPerModelUnit":0.5}`), 0600)
	_ = os.WriteFile(filepath.Join(site, "model.usdz"), []byte("test model identity"), 0600)
	service, err := newTwin(context.Background(), []cameraConfig{{ID: "one", Name: "First", Device: "wendy-lite:test-one", CameraID: 0}, {ID: "two", Name: "Second", Device: "wendy-lite:test-two", CameraID: 1}}, t.TempDir(), "/usr/bin/false", site, "", "test-token", false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.close)
	return service
}
func requestTwin(t *testing.T, s http.Handler, method, path, body string, authorized bool) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	if authorized {
		r.Header.Set("Authorization", "Bearer test-token")
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}
func TestTwinMultiCameraAndModel(t *testing.T) {
	s := twinFixture(t)
	for _, path := range []string{"/v1/twin", "/v1/site/model", "/v1/state?camera=two", "/v1/twin/observations"} {
		if w := requestTwin(t, s, "GET", path, "", false); w.Code != 401 {
			t.Fatal(path, w.Code)
		}
	}
	w := requestTwin(t, s, "GET", "/v1/twin", "", true)
	var state twinState
	if json.Unmarshal(w.Body.Bytes(), &state) != nil || len(state.Cameras) != 2 || !state.ModelAvailable || state.Site.MetresPerModelUnit != 0.5 || state.Cameras[1].ID != "two" || state.Cameras[1].IdentityStatus != "configured" {
		t.Fatal(w.Body.String())
	}
	w = requestTwin(t, s, "GET", "/v1/site/model", "", true)
	if w.Code != 200 || w.Header().Get("X-Content-SHA256") != state.Site.ModelSHA256 || w.Body.String() != "test model identity" {
		t.Fatal("wrong model")
	}
	w = requestTwin(t, s, "GET", "/v1/state?camera=two", "", true)
	if !bytes.Contains(w.Body.Bytes(), []byte("wendy-lite:test-two")) {
		t.Fatal("wrong camera")
	}
	if w = requestTwin(t, s, "GET", "/v1/state?camera=missing", "", true); w.Code != 404 {
		t.Fatal("unknown camera accepted")
	}
	if w = requestTwin(t, s, "POST", "/v1/pose/automatic?camera=two", `{"enabled":false}`, true); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w = requestTwin(t, s, "POST", "/v1/pose/automatic?camera=two", `{"enabled":false,"extra":1}`, true); w.Code != 400 {
		t.Fatal("unknown field accepted")
	}
}
func TestCameraConfigValidation(t *testing.T) {
	valid := []cameraConfig{{ID: "one", Name: "First", Device: "wendy-lite:test-one", CameraID: 0}}
	if err := validateCameras(valid); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]cameraConfig{nil, append(valid, valid[0]), {{ID: "../one", Name: "Name", Device: "wendy-lite:host"}}, {{ID: "one", Name: "Name", Device: "http://host"}}} {
		if validateCameras(bad) == nil {
			t.Fatal("invalid config accepted")
		}
	}
}
func TestRetryBackoffBounded(t *testing.T) {
	for i, want := range []time.Duration{30, 60, 120, 240, 300, 300} {
		if got := retryDelay(i + 1); got != want*time.Second {
			t.Fatal(i, got)
		}
	}
}
func TestGeometryOnlySiteAndModelBounds(t *testing.T) {
	s := twinFixture(t)
	if state := s.snapshot(); state.Cameras[0].Pose.Status != "disabled" || !state.ModelAvailable {
		t.Fatal("geometry-only site unavailable")
	}
	r := requestTwin(t, s, "GET", "/v1/site/model", "", true)
	b, _ := io.ReadAll(r.Result().Body)
	if len(b) == 0 {
		t.Fatal("missing model")
	}
}
