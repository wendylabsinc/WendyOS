package commands

import (
	"context"
	"strings"
	"sync"
	"testing"

	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
)

// fakeModelService records calls; Task 16 extends it with watching.
type fakeModelService struct {
	agentpbv2.UnimplementedWendyModelServiceServer
	catalog *agentpbv2.ListModelCatalogResponse
	list    *agentpbv2.ListModelsResponse
	events  []*agentpbv2.ModelWatchMessage
	hold    bool // keep WatchModel open until the client leaves

	mu      sync.Mutex
	stopped []string // "instance/watch"
	start   *agentpbv2.StartModelRequest
	watch   *agentpbv2.WatchModelRequest
}

func (f *fakeModelService) ListCatalog(context.Context, *agentpbv2.ListModelCatalogRequest) (*agentpbv2.ListModelCatalogResponse, error) {
	return f.catalog, nil
}

func (f *fakeModelService) ListModels(context.Context, *agentpbv2.ListModelsRequest) (*agentpbv2.ListModelsResponse, error) {
	return f.list, nil
}

func (f *fakeModelService) StopModel(_ context.Context, req *agentpbv2.StopModelRequest) (*agentpbv2.StopModelResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, req.GetInstanceId()+"/"+req.GetWatchId())
	return &agentpbv2.StopModelResponse{Instance: &agentpbv2.ModelInstance{
		InstanceId: req.GetInstanceId(), State: agentpbv2.ModelState_MODEL_STATE_STOPPED, StateDetail: "stopped"}}, nil
}

func (f *fakeModelService) stops() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.stopped...)
}

func serveModels(t *testing.T, fake *fakeModelService) {
	t.Helper()
	startUDSAgent(t, func(srv *grpc.Server) { agentpbv2.RegisterWendyModelServiceServer(srv, fake) })
	old := jsonOutput
	t.Cleanup(func() { jsonOutput = old })
}

func TestDeviceModelCatalogHumanAndJSON(t *testing.T) {
	serveModels(t, &fakeModelService{catalog: &agentpbv2.ListModelCatalogResponse{
		CatalogVersion: "test", MaxRunning: 2,
		Models: []*agentpbv2.CatalogModel{{Id: "coco-detector", Kind: "detector", Labels: []string{"person", "car"},
			Variant: &agentpbv2.CatalogVariant{Id: "d-cpu", Engine: "onnxruntime", DownloadBytes: 12 << 20}}},
		Cameras: []*agentpbv2.ModelCamera{{SourceId: "v4l2:/dev/video0", Name: "C920"}},
	}})
	cmd := newDeviceModelCatalogCmd()
	cmd.SetContext(context.Background())

	jsonOutput = false
	out, err := captureCommandStdout(t, func() error { return cmd.RunE(cmd, nil) })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"coco-detector", "onnxruntime", "2 classes", "downloads its runtime image", "12 MB", "v4l2:/dev/video0", "0 of 2 model slots"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}

	jsonOutput = true
	out, err = captureCommandStdout(t, func() error { return cmd.RunE(cmd, nil) })
	if err != nil || !strings.Contains(out, `"catalog_version":"test"`) {
		t.Fatalf("json = %s, %v", out, err)
	}
}

func TestDeviceModelListShowsState(t *testing.T) {
	serveModels(t, &fakeModelService{list: &agentpbv2.ListModelsResponse{Instances: []*agentpbv2.ModelInstance{{
		InstanceId: "m-1", ModelId: "coco-detector", CameraSourceId: "v4l2:/dev/video0",
		State: agentpbv2.ModelState_MODEL_STATE_READY, Watchers: 1, FileSha256: strings.Repeat("ab", 32),
		Stats: &agentpbv2.ModelStats{ProcessedFps: 9.8}}}}})
	jsonOutput = false
	cmd := newDeviceModelListCmd()
	cmd.SetContext(context.Background())
	out, err := captureCommandStdout(t, func() error { return cmd.RunE(cmd, nil) })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"m-1", "coco-detector", "ready (9.8 fps)", "abababababab…"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestDeviceModelStopStopsOutright(t *testing.T) {
	fake := &fakeModelService{}
	serveModels(t, fake)
	jsonOutput = false
	cmd := newDeviceModelStopCmd()
	cmd.SetContext(context.Background())
	out, err := captureCommandStdout(t, func() error { return cmd.RunE(cmd, []string{"m-1"}) })
	if err != nil {
		t.Fatal(err)
	}
	if got := fake.stops(); len(got) != 1 || got[0] != "m-1/" {
		t.Fatalf("stops = %v, want an outright stop of m-1", got)
	}
	if !strings.Contains(out, "Stopped m-1") {
		t.Fatalf("output = %q", out)
	}
}

func TestDeviceModelExplainsOldAgents(t *testing.T) {
	startUDSAgent(t) // serves no model service
	cmd := newDeviceModelListCmd()
	cmd.SetContext(context.Background())
	_, err := captureCommandStdout(t, func() error { return cmd.RunE(cmd, nil) })
	if err == nil || !strings.Contains(err.Error(), "does not run models yet") {
		t.Fatalf("err = %v", err)
	}
}
