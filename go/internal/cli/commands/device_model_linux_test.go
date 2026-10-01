package commands

// This file runs `wendy device model run` against the agent's own model
// service. It is Linux-only because the agent's services package builds only
// there, and CI cross-compiles this package's tests for Windows.

import (
	"context"
	"io"
	"slices"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/models"
	"github.com/wendylabsinc/wendy/go/internal/agent/models/modelstest"
	"github.com/wendylabsinc/wendy/go/internal/agent/services"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"go.uber.org/zap"
	"google.golang.org/grpc"
)

// TestModelRunCtrlCStopsTheModel serves the real ModelService and Supervisor,
// with fake hosts and cameras, on the test's agent socket. Ctrl+C in `run`
// must detach the watch before the stream closes, so the device stops the
// model at once (design §5, §8.1). A watch that is merely lost keeps the
// model for the 60 s grace, and the supervisor's fake clock never advances,
// so a model the run only abandoned would still be listed afterwards.
func TestModelRunCtrlCStopsTheModel(t *testing.T) {
	rt := modelstest.NewRuntime()
	sup := models.NewSupervisor(models.Config{
		Catalog: modelstest.Catalog(models.EngineONNXRuntime), Device: models.DeviceProfile{Arch: "arm64"},
		Runtime: rt, Cameras: modelstest.NewCameras(models.Camera{SourceID: "v4l2:/dev/video0", Name: "front door"}),
		Files: modelstest.NewFiles(), Root: t.TempDir(), Clock: modelstest.NewClock(),
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		sup.Shutdown(ctx)
	})
	startUDSAgent(t, func(srv *grpc.Server) {
		agentpbv2.RegisterWendyModelServiceServer(srv, services.NewModelService(zap.NewNop(), sup))
	})
	old := jsonOutput
	t.Cleanup(func() { jsonOutput = old })
	jsonOutput = true

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- withModelClient(ctx, func(c agentpbv2.WendyModelServiceClient) error {
			return runModelWatch(ctx, c, io.Discard, modelRunOptions{Model: "coco-detector", Camera: "v4l2:/dev/video0"})
		})
	}()
	// Interrupt as soon as the device has attached the watch, which may be
	// before the run has read the device's confirmation of it.
	modelstest.Eventually(t, "the run to attach its watch", func() bool {
		list := sup.List()
		return len(list) == 1 && list[0].Watchers == 1
	})
	id := sup.List()[0].ID
	cancel() // Ctrl+C
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("an interrupted run returned %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the run did not return after Ctrl+C")
	}
	if list := sup.List(); len(list) != 0 {
		t.Fatalf("after Ctrl+C the model still runs: %+v; the run abandoned its watch instead of detaching it", list)
	}
	if !slices.Contains(rt.Removed(), id) {
		t.Fatal("the model host was not removed after Ctrl+C")
	}
}
