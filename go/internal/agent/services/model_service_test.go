package services

import (
	"context"
	"errors"
	"fmt"
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

// drainSent returns every message currently buffered on sent, without blocking.
func drainSent(sent <-chan *agentpbv2.ModelWatchMessage) []*agentpbv2.ModelWatchMessage {
	var out []*agentpbv2.ModelWatchMessage
	for {
		select {
		case msg := <-sent:
			out = append(out, msg)
		default:
			return out
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

// TestModelStatusErrorCodes checks modelStatusError directly against every
// sentinel the supervisor can return, wrapped the way the supervisor wraps
// it (design §5), plus the context errors, nil, and an unrelated error.
func TestModelStatusErrorCodes(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want codes.Code
	}{
		{"unknown model", fmt.Errorf("%w %q", models.ErrUnknownModel, "nope"), codes.NotFound},
		{"unknown camera", fmt.Errorf("%w %q", models.ErrUnknownCamera, "v4l2:/dev/video9"), codes.NotFound},
		{"unknown instance", fmt.Errorf("%w %q", models.ErrUnknownInstance, "m-missing"), codes.NotFound},
		{"unknown watch", fmt.Errorf("%w %q", models.ErrUnknownWatch, "w-1"), codes.NotFound},
		{"no variant", fmt.Errorf("%w: needs arch arm64", models.ErrNoVariant), codes.FailedPrecondition},
		{"camera not streamable", fmt.Errorf("%w: %v", models.ErrCameraNotStreamable, errors.New("no frame identity")), codes.FailedPrecondition},
		{"capacity", fmt.Errorf("%w (2): m-aaa, m-bbb", models.ErrCapacity), codes.ResourceExhausted},
		{"invalid filter", fmt.Errorf("%w: unknown event type %q", models.ErrInvalidFilter, "bogus"), codes.InvalidArgument},
		{"shutting down", models.ErrShuttingDown, codes.Unavailable},
		{"context canceled", context.Canceled, codes.Canceled},
		{"context deadline exceeded", context.DeadlineExceeded, codes.DeadlineExceeded},
		{"unrelated error", errors.New("boom"), codes.Internal},
	}
	for _, tc := range cases {
		if got := status.Code(modelStatusError(tc.err)); got != tc.want {
			t.Errorf("%s: code %v, want %v", tc.name, got, tc.want)
		}
	}
	if err := modelStatusError(nil); err != nil {
		t.Errorf("modelStatusError(nil) = %v, want nil", err)
	}
}

// TestWatchModelInvalidClassReturnsInvalidArgument covers the routing from
// WatchModel's own filter-validation error through to the gRPC code, not just
// modelStatusError in isolation.
func TestWatchModelInvalidClassReturnsInvalidArgument(t *testing.T) {
	svc, rt := newTestModelService(t)
	id := startTestModel(t, svc, rt)
	stream := &fakeServerStream[agentpbv2.ModelWatchMessage]{ctx: context.Background()}
	err := svc.WatchModel(&agentpbv2.WatchModelRequest{InstanceId: id, Classes: []string{"unicorn"}}, stream)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("WatchModel with an unreported class = %v, want InvalidArgument", err)
	}
	if len(stream.sent) != 0 {
		t.Fatalf("stream.sent = %v, want nothing sent", stream.sent)
	}
}

// TestWatchModelSendFailureDetaches checks that a stream.Send failure --
// whether on the initial WatchStarted message or on a later one -- ends
// WatchModel with that error and detaches the watch (lease grace, not a
// stop), rather than leaving it attached.
func TestWatchModelSendFailureDetaches(t *testing.T) {
	sendErr := errors.New("send boom")

	t.Run("first send", func(t *testing.T) {
		svc, rt := newTestModelService(t)
		id := startTestModel(t, svc, rt)
		stream := &fakeServerStream[agentpbv2.ModelWatchMessage]{ctx: context.Background()}
		stream.onSend = func(int) error { return sendErr }

		err := svc.WatchModel(&agentpbv2.WatchModelRequest{InstanceId: id}, stream)
		if !errors.Is(err, sendErr) {
			t.Fatalf("WatchModel = %v, want %v", err, sendErr)
		}
		list, _ := svc.ListModels(context.Background(), &agentpbv2.ListModelsRequest{})
		if len(list.GetInstances()) != 1 || list.GetInstances()[0].GetInstanceId() != id {
			t.Fatalf("list after send failure = %v, want the instance still listed", list)
		}
		if got := list.GetInstances()[0].GetWatchers(); got != 0 {
			t.Fatalf("watchers after send failure = %d, want 0", got)
		}
	})

	t.Run("later send", func(t *testing.T) {
		svc, rt := newTestModelService(t)
		id := startTestModel(t, svc, rt)
		sent := make(chan *agentpbv2.ModelWatchMessage, 64)
		stream := &fakeServerStream[agentpbv2.ModelWatchMessage]{ctx: context.Background()}
		stream.onSend = func(n int) error {
			sent <- stream.sent[n-1]
			if n > 1 {
				return sendErr
			}
			return nil
		}
		done := make(chan error, 1)
		go func() { done <- svc.WatchModel(&agentpbv2.WatchModelRequest{InstanceId: id}, stream) }()
		receive(t, sent, func(m *agentpbv2.ModelWatchMessage) bool { return m.GetStarted() != nil })

		svc.supervisor.PublishApplicationRecord(models.AppIDPrefix+id, modelstest.Entered("person", 0.9, 1))

		if err := <-done; !errors.Is(err, sendErr) {
			t.Fatalf("WatchModel = %v, want %v", err, sendErr)
		}
		list, _ := svc.ListModels(context.Background(), &agentpbv2.ListModelsRequest{})
		if len(list.GetInstances()) != 1 || list.GetInstances()[0].GetInstanceId() != id {
			t.Fatalf("list after send failure = %v, want the instance still listed", list)
		}
		if got := list.GetInstances()[0].GetWatchers(); got != 0 {
			t.Fatalf("watchers after send failure = %d, want 0", got)
		}
	})
}

// TestStopModelByWatchEndsOnlyThatWatch checks that StopModel with a watch_id
// ends only that watch: its WatchModel call returns cleanly with no final
// status message, while the instance and its other watch carry on.
func TestStopModelByWatchEndsOnlyThatWatch(t *testing.T) {
	svc, rt := newTestModelService(t)
	id := startTestModel(t, svc, rt)

	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	sent1, done1 := watchInBackground(ctx1, svc, &agentpbv2.WatchModelRequest{InstanceId: id})
	watch1 := receive(t, sent1, func(m *agentpbv2.ModelWatchMessage) bool { return m.GetStarted() != nil }).GetStarted().GetWatchId()

	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	sent2, done2 := watchInBackground(ctx2, svc, &agentpbv2.WatchModelRequest{InstanceId: id})
	receive(t, sent2, func(m *agentpbv2.ModelWatchMessage) bool { return m.GetStarted() != nil })

	if _, err := svc.StopModel(context.Background(), &agentpbv2.StopModelRequest{InstanceId: id, WatchId: watch1}); err != nil {
		t.Fatal(err)
	}
	if err := <-done1; err != nil {
		t.Fatalf("WatchModel (stopped watch) = %v, want a clean end", err)
	}
	for _, msg := range drainSent(sent1) {
		if msg.GetStatus().GetState() == agentpbv2.ModelState_MODEL_STATE_STOPPED {
			t.Fatalf("the ended watch's stream received a STOPPED status: %v", msg)
		}
	}

	list, _ := svc.ListModels(context.Background(), &agentpbv2.ListModelsRequest{})
	if len(list.GetInstances()) != 1 || list.GetInstances()[0].GetInstanceId() != id {
		t.Fatalf("list after stopping one watch = %v, want the instance still listed", list)
	}
	if got := list.GetInstances()[0].GetWatchers(); got != 1 {
		t.Fatalf("watchers after stopping one watch = %d, want 1", got)
	}

	cancel2()
	if err := <-done2; status.Code(err) != codes.Canceled {
		t.Fatalf("second WatchModel after cancel = %v, want Canceled", err)
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
	video := NewVideoService(context.Background(), zap.NewNop(), nil)
	got := ModelCameras{Video: video, Data: m}.List(context.Background())
	if want := []models.Camera{{SourceID: "v4l2:/dev/video0", Name: "C920 USB"}}; !slices.Equal(got, want) {
		t.Fatalf("cameras = %+v, want %+v", got, want)
	}
}

// TestModelCamerasHideRefusedCameras: a camera whose stream the two-plane
// path has refused cannot stream to a model, so the catalog must not offer it.
func TestModelCamerasHideRefusedCameras(t *testing.T) {
	m, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m.SetSourceProvider(func(context.Context) []data.Source {
		return []data.Source{
			{ID: "v4l2:/dev/video0", Kind: "camera", Healthy: true, Detail: "C920 USB"},
			{ID: "v4l2:/dev/video2", Kind: "camera", Healthy: true, Detail: "webcam"},
		}
	})
	video := NewVideoService(context.Background(), zap.NewNop(), nil)
	video.noteTwoPlaneRefusal("v4l2:/dev/video2")
	got := ModelCameras{Video: video, Data: m}.List(context.Background())
	if want := []models.Camera{{SourceID: "v4l2:/dev/video0", Name: "C920 USB"}}; !slices.Equal(got, want) {
		t.Fatalf("cameras = %+v, want %+v", got, want)
	}
}
