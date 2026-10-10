package services

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/agent/models"
	"github.com/wendylabsinc/wendy/go/internal/agent/models/modelstest"
	sharedenv "github.com/wendylabsinc/wendy/go/internal/shared/env"
)

// TestModelHostDetectionsReachWatchersThroughTheRealDataSocket wires a real
// AppDataSocketManager to a real models.Supervisor through SetRecordSink,
// exactly as cmd/wendy-agent/main.go does, and speaks the host side of the
// data-socket contract at it over an actual unix socket: a model.status
// ready event, then model.entered and model.left events shaped like
// go/modelhost/fakehost/main.go's status() and detection() send them,
// inputs included (design §6.1).
//
// The supervisor's own tests call Supervisor.PublishApplicationRecord
// directly, which bypasses validateApplicationRecord entirely; this test is
// the one that would have caught "event" records being rejected for naming
// their inputs.
func TestModelHostDetectionsReachWatchersThroughTheRealDataSocket(t *testing.T) {
	capture, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	socketRoot, err := os.MkdirTemp("/tmp", "wendy-model-socket-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(socketRoot)
	oldRoot := AppDataSocketRootPath
	AppDataSocketRootPath = socketRoot
	defer func() { AppDataSocketRootPath = oldRoot }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager := NewAppDataSocketManager(ctx, nil, capture)

	clock := modelstest.NewClock()
	runtime := modelstest.NewRuntime()
	cameras := modelstest.NewCameras(models.Camera{SourceID: "v4l2:/dev/video0", Name: "front door"})
	files := modelstest.NewFiles()
	sup := models.NewSupervisor(models.Config{
		Catalog: modelstest.Catalog(models.EngineONNXRuntime),
		Device:  models.DeviceProfile{Arch: "arm64", GPUArch: "sm_87"},
		Runtime: runtime, Cameras: cameras, Files: files, Root: t.TempDir(), Clock: clock,
	})
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer shutdownCancel()
		sup.Shutdown(shutdownCtx)
	}()
	manager.SetRecordSink(sup.PublishApplicationRecord)

	info, _, err := sup.Start(context.Background(), "coco-detector", "v4l2:/dev/video0")
	if err != nil {
		t.Fatal(err)
	}
	modelstest.Eventually(t, "the host to start", func() bool { return runtime.Running(info.ID) })

	w, _, _, err := sup.Watch(models.WatchRequest{InstanceID: info.ID})
	if err != nil {
		t.Fatal(err)
	}

	appID := models.AppIDPrefix + info.ID
	manager.peerCred = func(net.Conn) (peerCredentials, error) { return peerCredentials{UID: 0, PID: 4242}, nil }
	manager.cgroupOfPID = func(int32) (string, error) {
		return fmt.Sprintf("0::/system.slice/%s-%s.scope\n", sharedenv.SystemdServiceName(), appID), nil
	}

	dir, err := manager.Ensure(appID, "")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("unix", filepath.Join(dir, DataSocketFilename))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	send := func(rec data.ApplicationRecord) dataAck {
		t.Helper()
		body, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		if err := writeDataFrame(conn, json.RawMessage(body)); err != nil {
			t.Fatal(err)
		}
		ackBody, err := readDataFrame(conn)
		if err != nil {
			t.Fatal(err)
		}
		var ack dataAck
		if err := json.Unmarshal(ackBody, &ack); err != nil {
			t.Fatal(err)
		}
		return ack
	}

	status := data.ApplicationRecord{Version: 1, Type: "event", Name: models.RecordStatus, ClientBootID: "unavailable",
		Attributes: map[string]any{"state": models.HostReady, "processed_fps": 10.0, "latency_p50_ms": 1.0}}
	if ack := send(status); ack.State == "rejected" {
		t.Fatalf("model.status ack = %+v", ack)
	}

	entered := data.ApplicationRecord{Version: 1, Type: "event", Name: models.RecordEntered,
		Model: "test-" + models.EngineONNXRuntime, ClientBootID: "unavailable",
		Attributes: map[string]any{"class": "person", "confidence": 0.9, "track_id": 1.0,
			"box": map[string]any{"x": 0.4, "y": 0.2, "width": 0.2, "height": 0.6}},
		Inputs: []data.SampleRef{{SourceID: "v4l2:/dev/video0", SampleID: 1}}}
	if ack := send(entered); ack.State == "rejected" {
		t.Fatalf("model.entered ack = %+v", ack)
	}

	modelstest.Eventually(t, "the instance to become ready", func() bool {
		return instanceState(sup, info.ID) == models.StateReady
	})

	e := nextModelEvent(t, w)
	if e.Type != models.EventEntered || e.Class != "person" || e.SourceID != "v4l2:/dev/video0" || e.SampleID != 1 {
		t.Fatalf("entered event = %+v", e)
	}

	left := data.ApplicationRecord{Version: 1, Type: "event", Name: models.RecordLeft,
		Model: "test-" + models.EngineONNXRuntime, ClientBootID: "unavailable",
		Attributes: map[string]any{"class": "person", "confidence": 0.9, "track_id": 1.0,
			"box": map[string]any{"x": 0.4, "y": 0.2, "width": 0.2, "height": 0.6}},
		Inputs: []data.SampleRef{{SourceID: "v4l2:/dev/video0", SampleID: 2}}}
	if ack := send(left); ack.State == "rejected" {
		t.Fatalf("model.left ack = %+v", ack)
	}
	if e := nextModelEvent(t, w); e.Type != models.EventLeft {
		t.Fatalf("left event = %+v", e)
	}
}

// instanceState looks up id's current state on sup, or 0 if sup no longer
// knows about it.
func instanceState(sup *models.Supervisor, id string) models.State {
	for _, i := range sup.List() {
		if i.ID == id {
			return i.State
		}
	}
	return 0
}

// nextModelEvent reads w's next message, skipping the status snapshots a
// ready report and every later broadcast produce, and fails the test if no
// event arrives within two seconds.
func nextModelEvent(t *testing.T, w *models.Watch) models.Event {
	t.Helper()
	for {
		select {
		case msg, ok := <-w.C:
			if !ok {
				t.Fatal("the watch closed before the event arrived")
			}
			if msg.Event != nil {
				return *msg.Event
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for a watch event")
		}
	}
}
