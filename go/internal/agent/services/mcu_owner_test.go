package services

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// flashAs flashes a valid firmware set as appID and returns the service state.
func flashAs(t *testing.T, s *McuService, appID string, sketch []byte) {
	t.Helper()
	in := validFlash()
	in.spec.AppId = appID
	if sketch != nil {
		in.sketch = sketch
		in.spec.Sketch.Size, in.spec.Sketch.Sha256 = uint64(len(sketch)), sha(sketch)
	}
	stream, err := runFlash(context.Background(), s, flashRequests(in.spec, append(in.loader, in.sketch...), 64))
	if err != nil || lastResponse(t, stream).GetCompleted() == nil {
		t.Fatalf("flash as %s: %v, %v", appID, err, lastResponse(t, stream))
	}
}

func ownerOf(t *testing.T, s *McuService) string {
	t.Helper()
	st, _ := s.readState()
	return st.AppID
}

func recordStops(s *McuService) *[]string {
	var stopped []string
	s.SetAppStopper(func(_ context.Context, appID string) error {
		stopped = append(stopped, appID)
		return nil
	})
	return &stopped
}

func TestFlashKeepsTheFirmwareAndStopsTheReplacedApp(t *testing.T) {
	h := &fakeHelper{output: okStatus}
	s := newTestMcuService(t, h)
	stopped := recordStops(s)
	flashAs(t, s, "app.a", nil)
	if len(*stopped) != 0 {
		t.Fatalf("stopped %v on the first flash, want nothing", *stopped)
	}
	flashAs(t, s, "app.a", []byte("a2"))
	flashAs(t, s, "app.b", []byte("b"))
	if got := strings.Join(*stopped, ","); got != "app.a" {
		t.Fatalf("stopped = %q, want app.a once: only another app's flash replaces it", got)
	}
	for app, want := range map[string]string{"app.a": "a2", "app.b": "b"} {
		got, err := os.ReadFile(filepath.Join(s.firmwareDir(app), "sketch.bin"))
		if err != nil || string(got) != want {
			t.Errorf("kept sketch of %s = %q, %v; want %q", app, got, err, want)
		}
	}
	if ownerOf(t, s) != "app.b" {
		t.Fatalf("owner = %q, want app.b", ownerOf(t, s))
	}
}

func TestFlashRefusesAnAppIDThatIsNotOne(t *testing.T) {
	h := &fakeHelper{output: okStatus}
	s := newTestMcuService(t, h)
	in := validFlash()
	in.spec.AppId = "../../etc"
	stream, _ := runFlash(context.Background(), s, flashRequests(in.spec, nil, 64))
	if f := lastResponse(t, stream).GetFailed(); f == nil || !strings.Contains(f.GetErrorMessage(), "app_id") {
		t.Fatalf("response = %v, want an app_id failure", lastResponse(t, stream))
	}
	if h.callCount() != 0 {
		t.Fatal("helper ran for a bad app id")
	}
}

func TestRestoreSketchPutsTheAppsSketchBack(t *testing.T) {
	h := &fakeHelper{output: okStatus}
	s := newTestMcuService(t, h)
	stopped := recordStops(s)
	flashAs(t, s, "app.a", []byte("a"))
	flashAs(t, s, "app.b", []byte("b"))
	*stopped = nil
	flashes := h.callCount()

	// The owner, and an app that never had a sketch, start without a flash.
	for _, app := range []string{"app.b", "app.none"} {
		if err := s.RestoreSketch(context.Background(), app); err != nil || h.callCount() != flashes {
			t.Fatalf("RestoreSketch(%s) = %v, helper calls %d; want no flash", app, err, h.callCount()-flashes)
		}
	}
	if err := s.RestoreSketch(context.Background(), "app.a"); err != nil {
		t.Fatalf("RestoreSketch(app.a): %v", err)
	}
	last := h.calls[len(h.calls)-1].args
	if h.callCount() != flashes+1 || last[2] != filepath.Join(s.firmwareDir("app.a"), "sketch.bin") {
		t.Fatalf("helper args = %v, want one flash of app.a's kept sketch", last)
	}
	if ownerOf(t, s) != "app.a" || strings.Join(*stopped, ",") != "app.b" {
		t.Fatalf("owner %q, stopped %v; want app.a owning and app.b stopped", ownerOf(t, s), *stopped)
	}
}

func TestRestoreSketchFailureKeepsTheOwnerAndFailsTheStart(t *testing.T) {
	h := &fakeHelper{output: okStatus}
	s := newTestMcuService(t, h)
	stopped := recordStops(s)
	flashAs(t, s, "app.a", []byte("a"))
	flashAs(t, s, "app.b", []byte("b"))
	*stopped = nil
	h.output, h.waitErr = `wendy-mcu-status {"ok":false,"error":"OpenOCD failed (exit 1)"}`, errors.New("exit status 4")
	err := s.RestoreSketch(context.Background(), "app.a")
	if err == nil || !strings.Contains(err.Error(), "OpenOCD failed") {
		t.Fatalf("RestoreSketch = %v, want the flash failure", err)
	}
	if ownerOf(t, s) != "app.b" || len(*stopped) != 0 {
		t.Fatalf("owner %q, stopped %v; want app.b still owning and running", ownerOf(t, s), *stopped)
	}
}

func TestForgetAppErasesTheOwnersSketchOnly(t *testing.T) {
	h := &fakeHelper{output: okStatus}
	s := newTestMcuService(t, h)
	recordStops(s)
	flashAs(t, s, "app.a", []byte("a"))
	flashAs(t, s, "app.b", []byte("b"))
	calls := len(h.calls)

	s.ForgetApp(context.Background(), "app.a")
	if _, err := os.Stat(s.firmwareDir("app.a")); !os.IsNotExist(err) {
		t.Fatalf("app.a's firmware still kept: %v", err)
	}
	if len(h.calls) != calls || ownerOf(t, s) != "app.b" {
		t.Fatalf("helper calls %d, owner %q; removing a non-owner must not touch the chip", len(h.calls)-calls, ownerOf(t, s))
	}

	s.ForgetApp(context.Background(), "app.b")
	last := h.calls[len(h.calls)-1].args
	if len(h.calls) != calls+1 || last[0] != "--erase" || last[1] != "zephyr-1" {
		t.Fatalf("helper args = %v, want --erase zephyr-1", last)
	}
	if ownerOf(t, s) != "" {
		t.Fatalf("owner = %q after its app was removed, want none", ownerOf(t, s))
	}
}
