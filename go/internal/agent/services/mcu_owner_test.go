package services

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestFlashNotesTheSketchAppAndStopsTheReplacedApp(t *testing.T) {
	h := &fakeHelper{output: okStatus}
	s := newTestMcuService(t, h)
	stopped := recordStops(s)
	if s.AnySketchApp() {
		t.Fatal("a device with no flashes reports sketch apps")
	}
	flashAs(t, s, "app.a", nil)
	if len(*stopped) != 0 {
		t.Fatalf("stopped %v on the first flash, want nothing", *stopped)
	}
	flashAs(t, s, "app.a", []byte("a2"))
	flashAs(t, s, "app.b", []byte("b"))
	if got := strings.Join(*stopped, ","); got != "app.a" {
		t.Fatalf("stopped = %q, want app.a once: only another app's flash replaces it", got)
	}
	if !s.AnySketchApp() || !s.hasSketch("app.a") || !s.hasSketch("app.b") || s.hasSketch("app.none") {
		t.Fatal("the sketch apps are not app.a and app.b")
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

func TestSketchStartRefusal(t *testing.T) {
	h := &fakeHelper{output: okStatus}
	s := newTestMcuService(t, h)
	recordStops(s)
	flashAs(t, s, "app.a", []byte("a"))
	flashAs(t, s, "app.b", []byte("b"))
	check := func(app, want string) {
		t.Helper()
		got := s.SketchStartRefusal(app)
		if (want == "") != (got == "") || !strings.Contains(got, want) {
			t.Errorf("SketchStartRefusal(%s) = %q, want it to contain %q", app, got, want)
		}
	}
	check("app.b", "")    // its sketch is on the chip
	check("app.none", "") // no sketch of its own
	check("app.a", "holds the sketch of app.b")
	check("app.a", "deploy app.a again with wendy run")
	s.markContentUnknown()
	check("app.b", "interrupted flash")
	s.ForgetApp(context.Background(), "app.b")
	check("app.a", "holds no sketch")
	// A start never flashes: only deploys and the erase ran the helper.
	if h.callCount() != 2 {
		t.Fatalf("flashes = %d, want the 2 deploys only", h.callCount())
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
	if s.hasSketch("app.a") || len(h.calls) != calls || ownerOf(t, s) != "app.b" {
		t.Fatalf("sketch app %v, helper calls %d, owner %q; removing a non-owner must only forget it", s.hasSketch("app.a"), len(h.calls)-calls, ownerOf(t, s))
	}

	s.ForgetApp(context.Background(), "app.b")
	last := h.calls[len(h.calls)-1].args
	if len(h.calls) != calls+1 || last[0] != "--erase" || last[1] != "zephyr-1" {
		t.Fatalf("helper args = %v, want --erase zephyr-1", last)
	}
	if ownerOf(t, s) != "" || s.AnySketchApp() {
		t.Fatalf("owner = %q, sketch apps left %v; want none", ownerOf(t, s), s.AnySketchApp())
	}
}

func TestFlashThatWritesButDoesNotRunStillChangesTheOwner(t *testing.T) {
	h := &fakeHelper{output: okStatus}
	s := newTestMcuService(t, h)
	stopped := recordStops(s)
	flashAs(t, s, "app.a", []byte("a"))

	h.output, h.waitErr = `wendy-mcu-status {"ok":false,"sketch_written":true,"content_ok":true,"error":"the microcontroller does not run after the flash"}`, errors.New("exit status 4")
	in := validFlash()
	in.spec.AppId = "app.b"
	stream, _ := runFlash(context.Background(), s, flashRequests(in.spec, append(in.loader, in.sketch...), 64))
	if f := lastResponse(t, stream).GetFailed(); f == nil || !strings.Contains(f.GetErrorMessage(), "does not run") {
		t.Fatalf("response = %v, want the not-running failure", lastResponse(t, stream))
	}
	// app.a's sketch is gone from the chip, whatever state app.b's is in.
	if ownerOf(t, s) != "app.b" || strings.Join(*stopped, ",") != "app.a" {
		t.Fatalf("owner %q, stopped %v; want app.b owning and app.a stopped", ownerOf(t, s), *stopped)
	}
	if !s.hasSketch("app.b") {
		t.Fatal("app.b is not noted as a sketch app")
	}
}

func TestStopReplacedSkipsWhenTheChipChangedHandsAgain(t *testing.T) {
	h := &fakeHelper{output: okStatus}
	s := newTestMcuService(t, h)
	stopped := recordStops(s)
	flashAs(t, s, "app.a", []byte("a"))
	// app.b's flash finished, but app.a took the chip back before the stop ran.
	s.stopReplaced(context.Background(), "app.a", "app.b", false)
	if len(*stopped) != 0 {
		t.Fatalf("stopped %v, want nothing: app.a owns the chip", *stopped)
	}
}

func TestInterruptedFlashKeepsTheOwnerOnRecordButUnknown(t *testing.T) {
	killed := func(ctx context.Context, args ...string) (io.ReadCloser, func() error, error) {
		if args[0] != "--resume" {
			<-ctx.Done()
		}
		return io.NopCloser(strings.NewReader("")), func() error { return errors.New("signal: killed") }, nil
	}
	for name, interrupt := range map[string]func(s *McuService, h *fakeHelper){
		"helper killed on timeout": func(s *McuService, h *fakeHelper) {
			s.helperTimeout, s.runHelper = 20*time.Millisecond, killed
		},
		"write did not verify": func(s *McuService, h *fakeHelper) {
			h.output, h.waitErr = `wendy-mcu-status {"ok":false,"sketch_written":true,"error":"the sketch did not verify after writing"}`, errors.New("exit status 4")
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := &fakeHelper{output: okStatus}
			s := newTestMcuService(t, h)
			stopped := recordStops(s)
			flashAs(t, s, "app.a", []byte("a"))

			interrupt(s, h)
			in := validFlash()
			in.spec.AppId = "app.b"
			stream, _ := runFlash(context.Background(), s, flashRequests(in.spec, append(in.loader, in.sketch...), 64))
			if lastResponse(t, stream).GetFailed() == nil {
				t.Fatal("the flash did not fail")
			}
			// app.a stays on record, so app.b's retry still stops it; but its sketch may be gone.
			if ownerOf(t, s) != "app.a" || s.ownerIs("app.a") || len(*stopped) != 0 {
				t.Fatalf("owner %q, known %v, stopped %v; want app.a on record with unknown content", ownerOf(t, s), s.ownerIs("app.a"), *stopped)
			}

			s.helperTimeout, s.runHelper = 3*time.Minute, h.run
			h.output, h.waitErr = okStatus, nil
			flashAs(t, s, "app.b", nil)
			if !s.ownerIs("app.b") || strings.Join(*stopped, ",") != "app.a" {
				t.Fatalf("after the retry: owner known %v, stopped %v; want app.b owning and app.a stopped", s.ownerIs("app.b"), *stopped)
			}
		})
	}
}

func TestHelperDyingWithoutAStatusLeavesTheContentUnknown(t *testing.T) {
	h := &fakeHelper{output: okStatus}
	s := newTestMcuService(t, h)
	recordStops(s)
	flashAs(t, s, "app.a", []byte("a"))
	// Killed (not by the timeout): it may have been writing.
	h.output, h.waitErr = "Info : flashing\n", errors.New("signal: killed")
	in := validFlash()
	in.spec.AppId = "app.b"
	stream, _ := runFlash(context.Background(), s, flashRequests(in.spec, append(in.loader, in.sketch...), 64))
	if lastResponse(t, stream).GetFailed() == nil {
		t.Fatal("the flash did not fail")
	}
	if ownerOf(t, s) != "app.a" || s.ownerIs("app.a") {
		t.Fatalf("owner %q, known %v; want app.a on record with unknown content", ownerOf(t, s), s.ownerIs("app.a"))
	}
}

func TestFailureBeforeAnyWriteLeavesTheOwnerKnown(t *testing.T) {
	h := &fakeHelper{output: okStatus}
	s := newTestMcuService(t, h)
	recordStops(s)
	flashAs(t, s, "app.a", []byte("a"))
	h.output, h.waitErr = `wendy-mcu-status {"ok":false,"error":"the microcontroller's SWD lines are in use"}`, errors.New("exit status 3")
	in := validFlash()
	in.spec.AppId = "app.b"
	stream, _ := runFlash(context.Background(), s, flashRequests(in.spec, append(in.loader, in.sketch...), 64))
	if lastResponse(t, stream).GetFailed() == nil {
		t.Fatal("the flash did not fail")
	}
	if !s.ownerIs("app.a") {
		t.Fatal("a flash that touched nothing made the owner's sketch look gone")
	}
}

func TestStopReplacedStopsWhenAThirdAppTookTheChip(t *testing.T) {
	h := &fakeHelper{output: okStatus}
	s := newTestMcuService(t, h)
	stopped := recordStops(s)
	flashAs(t, s, "app.c", []byte("c"))
	s.stopReplaced(context.Background(), "app.a", "app.b", false)
	if got := strings.Join(*stopped, ","); got != "app.a" {
		t.Fatalf("stopped = %q, want app.a: its sketch is gone whoever owns the chip now", got)
	}
}

func TestFlashNotesAnOwnerLeftByAnOlderAgent(t *testing.T) {
	h := &fakeHelper{output: okStatus}
	s := newTestMcuService(t, h)
	recordStops(s)
	// An older agent recorded the owner without noting the app.
	if err := writeMCUState(s.statePath, mcuState{AppID: "app.old", Layout: "zephyr-1"}); err != nil {
		t.Fatal(err)
	}
	flashAs(t, s, "app.b", []byte("b"))
	if why := s.SketchStartRefusal("app.old"); !strings.Contains(why, "holds the sketch of app.b") {
		t.Fatalf("SketchStartRefusal(app.old) = %q, want a refusal: app.b replaced its sketch", why)
	}
}

func TestFlashFailsWhenTheOwnerCannotBeRecorded(t *testing.T) {
	h := &fakeHelper{output: okStatus}
	s := newTestMcuService(t, h)
	stopped := recordStops(s)
	flashAs(t, s, "app.a", []byte("a"))
	// A directory in the way makes every write of the record fail.
	if err := os.MkdirAll(filepath.Join(s.statePath+".tmp", "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	in := validFlash()
	in.spec.AppId = "app.b"
	stream, err := runFlash(context.Background(), s, flashRequests(in.spec, append(in.loader, in.sketch...), 64))
	failed := lastResponse(t, stream).GetFailed()
	if err != nil || failed == nil || !strings.Contains(failed.GetErrorMessage(), "could not record its owner") {
		t.Fatalf("flash = %v, %v; want a failure that names the record", err, lastResponse(t, stream))
	}
	if got := strings.Join(*stopped, ","); got != "app.a" {
		t.Fatalf("stopped = %q, want app.a: its sketch is gone", got)
	}
}
