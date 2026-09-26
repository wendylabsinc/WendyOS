package main

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentmodels "github.com/wendylabsinc/wendy/go/internal/agent/models"
	"github.com/wendylabsinc/wendy/go/internal/agent/models/modelstest"
	"github.com/wendylabsinc/wendy/go/internal/agent/services"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

func TestLoadModelCatalogDefaultsToBuiltIn(t *testing.T) {
	c, err := loadModelCatalog(zap.NewNop(), "")
	if err != nil || c.Version == "" {
		t.Fatalf("catalog = %+v, %v", c, err)
	}
}

func TestLoadModelCatalogOverride(t *testing.T) {
	sha := strings.Repeat("1", 64)
	variant := func(image string) string {
		return `{"version":"dev","models":[{"id":"fake-detector","kind":"detector","labels":["person"],"variants":[{"id":"v","engine":"onnxruntime","host_image":"` +
			image + `","file":{"url":"https://example.com/m","sha256":"` + sha + `","bytes":1},"input_size":1}]}]}`
	}
	dir := t.TempDir()
	good := filepath.Join(dir, "good.json")
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(good, []byte(variant("ghcr.io/wendylabsinc/wendy-model-host-fake@sha256:"+strings.Repeat("a", 64))), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte(variant("ghcr.io/wendylabsinc/wendy-model-host-fake:latest")), 0o644); err != nil {
		t.Fatal(err)
	}
	if c, err := loadModelCatalog(zap.NewNop(), good); err != nil || c.Version != "dev" {
		t.Fatalf("override = %+v, %v", c, err)
	}
	if _, err := loadModelCatalog(zap.NewNop(), bad); err == nil {
		t.Fatal("an override with a tag-pinned image was accepted")
	}
}

// TestStopModelsLetsTheServersDrain: grpc.Server.GracefulStop waits for every
// open WatchModel stream, so the agent stops its models (stopModels) before
// it drains its servers. Each watch then ends with the agent shutting down,
// not with a camera failure once the video pumps have stopped.
func TestStopModelsLetsTheServersDrain(t *testing.T) {
	sup := agentmodels.NewSupervisor(agentmodels.Config{
		Catalog: modelstest.Catalog(agentmodels.EngineONNXRuntime), Device: agentmodels.DeviceProfile{Arch: "arm64"},
		Runtime: modelstest.NewRuntime(), Cameras: modelstest.NewCameras(agentmodels.Camera{SourceID: "v4l2:/dev/video0"}),
		Files: modelstest.NewFiles(), Root: t.TempDir(), Clock: modelstest.NewClock(),
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		sup.Shutdown(ctx)
	})
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	agentpbv2.RegisterWendyModelServiceServer(srv, services.NewModelService(zap.NewNop(), sup))
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client := agentpbv2.NewWendyModelServiceClient(conn)

	ctx := context.Background()
	started, err := client.StartModel(ctx, &agentpbv2.StartModelRequest{ModelId: "coco-detector", CameraSourceId: "v4l2:/dev/video0"})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := client.WatchModel(ctx, &agentpbv2.WatchModelRequest{InstanceId: started.GetInstance().GetInstanceId()})
	if err != nil {
		t.Fatal(err)
	}
	if first, err := stream.Recv(); err != nil || first.GetStarted() == nil {
		t.Fatalf("first watch message = %v, %v", first, err)
	}
	final := make(chan *agentpbv2.ModelInstance, 1)
	go func() {
		var last *agentpbv2.ModelInstance
		for {
			msg, err := stream.Recv()
			if err != nil {
				if err != io.EOF {
					last = nil
				}
				final <- last
				return
			}
			if s := msg.GetStatus(); s != nil {
				last = s
			}
		}
	}()

	stopModels(sup)
	drained := make(chan struct{})
	go func() {
		srv.GracefulStop()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(3 * time.Second):
		t.Fatal("GracefulStop still waits on an open model watch after stopModels")
	}
	select {
	case last := <-final:
		if last.GetState() != agentpbv2.ModelState_MODEL_STATE_STOPPED || last.GetStateDetail() != "the agent is shutting down" {
			t.Fatalf("the watch ended with %v; want STOPPED, the agent is shutting down", last)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the watch did not end")
	}
}
