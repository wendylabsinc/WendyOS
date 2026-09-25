package models_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/models"
	"github.com/wendylabsinc/wendy/go/internal/agent/models/modelstest"
)

func TestCrashedHostRestartsAfterBackoff(t *testing.T) {
	h := newHarness(t, models.EngineONNXRuntime)
	info := h.startReady(t, frontDoor)
	if _, _, _, err := h.sup.Watch(models.WatchRequest{InstanceID: info.ID}); err != nil {
		t.Fatal(err)
	}
	h.runtime.Exit(info.ID, 1)
	modelstest.Eventually(t, "restarting", func() bool { return h.info(info.ID).State == models.StateRestarting })
	if detail := h.info(info.ID).StateDetail; !strings.Contains(detail, "status 1") {
		t.Fatalf("detail = %q", detail)
	}
	h.clock.Advance(time.Second)
	time.Sleep(10 * time.Millisecond)
	if h.runtime.Starts(info.ID) != 1 {
		t.Fatal("restarted before the 2 s backoff")
	}
	modelstest.AdvanceUntil(t, h.clock, 500*time.Millisecond, "the restart", func() bool { return h.runtime.Starts(info.ID) == 2 })
	h.sup.PublishApplicationRecord(models.AppIDPrefix+info.ID, modelstest.Status(models.HostReady))
	modelstest.Eventually(t, "ready again", func() bool { return h.info(info.ID).State == models.StateReady })
}

func TestFourthFailureIsFinal(t *testing.T) {
	h := newHarness(t, models.EngineONNXRuntime)
	info := h.startReady(t, frontDoor)
	w, _, _, err := h.sup.Watch(models.WatchRequest{InstanceID: info.ID})
	if err != nil {
		t.Fatal(err)
	}
	for start := 1; start <= 3; start++ {
		h.runtime.Exit(info.ID, 1)
		modelstest.AdvanceUntil(t, h.clock, time.Second, "a restart", func() bool { return h.runtime.Starts(info.ID) == start+1 })
	}
	h.runtime.Exit(info.ID, 1)
	final := drainToEnd(t, w)
	if final == nil || final.State != models.StateFailed || !strings.Contains(final.StateDetail, "host failed 4 times") {
		t.Fatalf("final = %+v", final)
	}
}

func TestReportedFailureIsFinal(t *testing.T) {
	h := newHarness(t, models.EngineONNXRuntime)
	info := h.startReady(t, frontDoor)
	w, _, _, err := h.sup.Watch(models.WatchRequest{InstanceID: info.ID})
	if err != nil {
		t.Fatal(err)
	}
	h.sup.PublishApplicationRecord(models.AppIDPrefix+info.ID, modelstest.Failed("no frames from camera"))
	final := drainToEnd(t, w)
	if final == nil || final.State != models.StateFailed || final.StateDetail != "no frames from camera" {
		t.Fatalf("final = %+v", final)
	}
	if n := h.runtime.Starts(info.ID); n != 1 {
		t.Fatalf("a reported failure was retried: %d starts", n)
	}
}

func TestSilentHostIsRestarted(t *testing.T) {
	h := newHarness(t, models.EngineONNXRuntime)
	info, _, err := h.sup.Start(context.Background(), "coco-detector", frontDoor)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := h.sup.Watch(models.WatchRequest{InstanceID: info.ID}); err != nil {
		t.Fatal(err)
	}
	modelstest.Eventually(t, "the host to start", func() bool { return h.runtime.Running(info.ID) })
	modelstest.AdvanceUntil(t, h.clock, time.Second, "the silent host to be replaced", func() bool { return h.runtime.Starts(info.ID) == 2 })
	if !slices.Contains(h.runtime.Removed(), info.ID) {
		t.Fatal("the silent host was not removed before its restart")
	}
}

func TestStaleStatusDuringRestartIsIgnored(t *testing.T) {
	h := newHarness(t, models.EngineONNXRuntime)
	info := h.startReady(t, frontDoor)
	if _, _, _, err := h.sup.Watch(models.WatchRequest{InstanceID: info.ID}); err != nil {
		t.Fatal(err)
	}
	h.runtime.Exit(info.ID, 1)
	modelstest.Eventually(t, "restarting", func() bool { return h.info(info.ID).State == models.StateRestarting })
	h.sup.PublishApplicationRecord(models.AppIDPrefix+info.ID, modelstest.Status(models.HostReady))
	time.Sleep(20 * time.Millisecond)
	if st := h.info(info.ID).State; st != models.StateRestarting {
		t.Fatalf("a status from the dead host moved the instance to %v", st)
	}
}

func TestEngineBuildsRunOneAtATime(t *testing.T) {
	h := newHarness(t, models.EngineTensorRT)
	ctx := context.Background()
	first, _, err := h.sup.Start(ctx, "coco-detector", frontDoor)
	if err != nil {
		t.Fatal(err)
	}
	modelstest.Eventually(t, "the first host", func() bool { return h.runtime.Running(first.ID) })
	if h.runtime.LastSpec().EngineCache == "" {
		t.Fatal("a TensorRT host got no engine cache")
	}
	second, _, err := h.sup.Start(ctx, "coco-detector", garage)
	if err != nil {
		t.Fatal(err)
	}
	modelstest.Eventually(t, "the second to wait", func() bool {
		return h.info(second.ID).StateDetail == "waiting for another engine build"
	})
	h.sup.PublishApplicationRecord(models.AppIDPrefix+first.ID, modelstest.Status(models.HostBuildingEngine))
	modelstest.Eventually(t, "building", func() bool { return h.info(first.ID).StateDetail == "building TensorRT engine" })
	if h.runtime.Running(second.ID) {
		t.Fatal("two engine builds ran at once")
	}
	h.sup.PublishApplicationRecord(models.AppIDPrefix+first.ID, modelstest.Status(models.HostReady))
	modelstest.Eventually(t, "the second host to start", func() bool { return h.runtime.Running(second.ID) })
}

func TestEngineBuildTimesOut(t *testing.T) {
	h := newHarness(t, models.EngineTensorRT)
	info, _, err := h.sup.Start(context.Background(), "coco-detector", frontDoor)
	if err != nil {
		t.Fatal(err)
	}
	w, _, _, err := h.sup.Watch(models.WatchRequest{InstanceID: info.ID})
	if err != nil {
		t.Fatal(err)
	}
	modelstest.Eventually(t, "the host", func() bool { return h.runtime.Running(info.ID) })
	app := models.AppIDPrefix + info.ID
	for i := 0; i < 200 && h.info(info.ID).State != 0; i++ {
		h.sup.PublishApplicationRecord(app, modelstest.Status(models.HostBuildingEngine))
		h.clock.Advance(5 * time.Second)
		time.Sleep(time.Millisecond)
	}
	final := drainToEnd(t, w)
	if final == nil || final.State != models.StateFailed || !strings.Contains(final.StateDetail, "engine build took longer than 15 min") {
		t.Fatalf("final = %+v", final)
	}
}

func TestCleanupOrphansRemovesLeftovers(t *testing.T) {
	h := newHarness(t, models.EngineONNXRuntime)
	h.runtime.AddLeftover("m-0ld00001")
	stale := filepath.Join(h.root, "run", "m-0ld00001")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := h.sup.CleanupOrphans(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(h.runtime.Removed(), "m-0ld00001") {
		t.Fatal("the leftover host was not removed")
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("the leftover run directory remains: %v", err)
	}
}
