package services

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/agent/models"
	"github.com/wendylabsinc/wendy/go/internal/agent/models/modelstest"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func newTestModelService(t *testing.T) (*ModelService, *modelstest.Runtime) {
	t.Helper()
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
	return NewModelService(zap.NewNop(), sup), rt
}

func startTestModel(t *testing.T, svc *ModelService, rt *modelstest.Runtime) string {
	t.Helper()
	started, err := svc.StartModel(context.Background(), &agentpbv2.StartModelRequest{ModelId: "coco-detector", CameraSourceId: "v4l2:/dev/video0"})
	if err != nil {
		t.Fatal(err)
	}
	id := started.GetInstance().GetInstanceId()
	modelstest.Eventually(t, "the host to start", func() bool { return rt.Running(id) })
	return id
}

// watchInBackground runs WatchModel and hands every sent message to the test.
func watchInBackground(ctx context.Context, svc *ModelService, req *agentpbv2.WatchModelRequest) (<-chan *agentpbv2.ModelWatchMessage, <-chan error) {
	sent := make(chan *agentpbv2.ModelWatchMessage, 64)
	stream := &fakeServerStream[agentpbv2.ModelWatchMessage]{ctx: ctx}
	stream.onSend = func(n int) error { sent <- stream.sent[n-1]; return nil }
	done := make(chan error, 1)
	go func() { done <- svc.WatchModel(req, stream) }()
	return sent, done
}

func receive(t *testing.T, sent <-chan *agentpbv2.ModelWatchMessage, match func(*agentpbv2.ModelWatchMessage) bool) *agentpbv2.ModelWatchMessage {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case msg := <-sent:
			if match(msg) {
				return msg
			}
		case <-deadline:
			t.Fatal("the expected message never arrived")
		}
	}
}

func TestModelServiceCatalogStartAndList(t *testing.T) {
	svc, rt := newTestModelService(t)
	ctx := context.Background()
	cat, err := svc.ListCatalog(ctx, &agentpbv2.ListModelCatalogRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.GetModels()) != 1 || cat.GetModels()[0].GetVariant().GetEngine() != models.EngineONNXRuntime ||
		len(cat.GetCameras()) != 1 || cat.GetMaxRunning() != 2 || cat.GetModels()[0].GetVariant().GetDownloadBytes() != 10 {
		t.Fatalf("catalog = %v", cat)
	}
	id := startTestModel(t, svc, rt)
	list, err := svc.ListModels(ctx, &agentpbv2.ListModelsRequest{})
	if err != nil || len(list.GetInstances()) != 1 || list.GetInstances()[0].GetInstanceId() != id {
		t.Fatalf("list = %v, %v", list, err)
	}
	if list.GetInstances()[0].GetStartedUnixNanos() == 0 {
		t.Fatal("the start time is not reported")
	}
}

func TestModelServiceMapsErrorsToCodes(t *testing.T) {
	svc, _ := newTestModelService(t)
	ctx := context.Background()
	cases := []struct {
		name string
		call func() error
		want codes.Code
	}{
		{"unknown model", func() error {
			_, err := svc.StartModel(ctx, &agentpbv2.StartModelRequest{ModelId: "nope", CameraSourceId: "v4l2:/dev/video0"})
			return err
		}, codes.NotFound},
		{"unknown camera", func() error {
			_, err := svc.StartModel(ctx, &agentpbv2.StartModelRequest{ModelId: "coco-detector", CameraSourceId: "v4l2:/dev/video9"})
			return err
		}, codes.NotFound},
		{"unknown instance", func() error {
			_, err := svc.StopModel(ctx, &agentpbv2.StopModelRequest{InstanceId: "m-missing"})
			return err
		}, codes.NotFound},
	}
	for _, tc := range cases {
		if got := status.Code(tc.call()); got != tc.want {
			t.Errorf("%s: code %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestWatchModelStreamsEventsAndDetachesOnDisconnect(t *testing.T) {
	svc, rt := newTestModelService(t)
	id := startTestModel(t, svc, rt)
	ctx, cancel := context.WithCancel(context.Background())
	sent, done := watchInBackground(ctx, svc, &agentpbv2.WatchModelRequest{InstanceId: id, Classes: []string{"person"}})

	started := receive(t, sent, func(m *agentpbv2.ModelWatchMessage) bool { return m.GetStarted() != nil })
	if started.GetStarted().GetWatchId() == "" || started.GetStarted().GetInstance().GetInstanceId() != id {
		t.Fatalf("started = %v", started)
	}
	svc.supervisor.PublishApplicationRecord(models.AppIDPrefix+id, modelstest.Entered("car", 0.9, 1))
	svc.supervisor.PublishApplicationRecord(models.AppIDPrefix+id, modelstest.Entered("person", 0.9, 2))
	event := receive(t, sent, func(m *agentpbv2.ModelWatchMessage) bool { return m.GetEvent() != nil }).GetEvent()
	if event.GetClassName() != "person" || event.GetSequence() != 2 || event.GetBox().GetHeight() == 0 {
		t.Fatalf("event = %v", event)
	}

	cancel()
	if err := <-done; status.Code(err) != codes.Canceled {
		t.Fatalf("WatchModel after disconnect = %v", err)
	}
	list, _ := svc.ListModels(context.Background(), &agentpbv2.ListModelsRequest{})
	if got := list.GetInstances()[0].GetWatchers(); got != 0 {
		t.Fatalf("watchers after disconnect = %d; the lost watch must be detached", got)
	}
}

func TestWatchModelEndsWithFinalStatus(t *testing.T) {
	svc, rt := newTestModelService(t)
	id := startTestModel(t, svc, rt)
	sent, done := watchInBackground(context.Background(), svc, &agentpbv2.WatchModelRequest{InstanceId: id})
	receive(t, sent, func(m *agentpbv2.ModelWatchMessage) bool { return m.GetStarted() != nil })
	if _, err := svc.StopModel(context.Background(), &agentpbv2.StopModelRequest{InstanceId: id}); err != nil {
		t.Fatal(err)
	}
	final := receive(t, sent, func(m *agentpbv2.ModelWatchMessage) bool {
		return m.GetStatus().GetState() == agentpbv2.ModelState_MODEL_STATE_STOPPED
	})
	if final.GetStatus().GetStateDetail() != "stopped" {
		t.Fatalf("final = %v", final)
	}
	if err := <-done; err != nil {
		t.Fatalf("WatchModel = %v, want a clean end", err)
	}
}

func TestModelCamerasListsHealthyLocalCameras(t *testing.T) {
	m, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m.SetSourceProvider(func(context.Context) []data.Source {
		return []data.Source{
			{ID: "v4l2:/dev/video0", Kind: "camera", Healthy: true, Detail: "C920 USB"},
			{ID: "ipcamera:200", Kind: "camera", Healthy: true, Detail: "porch"},
			{ID: "v4l2:/dev/video4", Kind: "camera", Healthy: false},
		}
	})
	got := ModelCameras{Data: m}.List(context.Background())
	if want := []models.Camera{{SourceID: "v4l2:/dev/video0", Name: "C920 USB"}}; !slices.Equal(got, want) {
		t.Fatalf("cameras = %+v, want %+v", got, want)
	}
}
