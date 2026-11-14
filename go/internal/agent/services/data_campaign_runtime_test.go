//go:build regression_campaign_runtime

package services

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/camera"
	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/agent/inference"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Build only the in-memory video service state. NewVideoService also loads
// host camera registries/controls and retires old loopback nodes; this fixture
// must never read or mutate those host resources.
func campaignRuntimeVideo(glob func() ([]string, error), name func(string) (string, error)) *VideoService {
	ctx, cancel := context.WithCancel(context.Background())
	return &VideoService{ctx: ctx, cancel: cancel, logger: zap.NewNop(), hubs: map[string]*deviceHub{}, sampleSeqs: map[string]*atomic.Uint64{}, globDevices: glob, readDeviceName: name,
		hasVideoCapture: func(string) bool { return true }, classifyTransport: func(string) (camera.Transport, string) { return camera.TransportUnknown, "" },
		enumerateLibcamera: func(context.Context) (map[string]string, error) { return nil, nil }, readStableNames: func() map[string]stableNames { return nil },
		findCameraSource: func(context.Context, string) (uint64, bool) { return 0, false }, isJetson: func() bool { return false }}
}

// Observe the real process results without changing or supplying predictions.
type campaignRuntimeFactory struct {
	factory  inference.ManagedFactory
	results  chan inference.Result
	sessions atomic.Int32
	closed   atomic.Int32
}

func (f *campaignRuntimeFactory) Start(ctx context.Context, config data.CampaignInference) (inference.Session, error) {
	real, err := f.factory.Start(ctx, config)
	if err != nil {
		return nil, err
	}
	f.sessions.Add(1)
	s := &campaignRuntimeSession{Session: real, results: make(chan inference.Result), done: make(chan struct{})}
	go func() {
		defer close(s.done)
		defer close(s.results)
		for result := range real.Results() {
			select {
			case f.results <- result:
			case <-ctx.Done():
				return
			}
			select {
			case s.results <- result:
			case <-ctx.Done():
				return
			}
		}
	}()
	s.onClose = func() { f.closed.Add(1) }
	return s, nil
}

type campaignRuntimeSession struct {
	inference.Session
	results chan inference.Result
	done    chan struct{}
	once    sync.Once
	onClose func()
}

func (s *campaignRuntimeSession) Results() <-chan inference.Result { return s.results }
func (s *campaignRuntimeSession) Close() error {
	err := s.Session.Close()
	<-s.done
	s.once.Do(s.onClose)
	return err
}

func TestYAMLCampaignManagedModelRecordsAndRestores(t *testing.T) {
	fixtureDir := os.Getenv("WENDY_CAMPAIGN_RUNTIME_FIXTURE")
	if fixtureDir == "" {
		t.Skip("requires generated local model and encoded scene fixture")
	}
	var fixture struct {
		Schema     int               `json:"schema"`
		Root       string            `json:"runtime_root"`
		Model      string            `json:"model"`
		Revision   string            `json:"revision"`
		Files      map[string]string `json:"files"`
		ModelFiles map[string]string `json:"model_files"`
	}
	raw, err := os.ReadFile(filepath.Join(fixtureDir, "fixture.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Schema != 1 || !strings.HasPrefix(fixture.Model, "wendy-regression/camera-") || len(fixture.Revision) != 40 {
		t.Fatal("invalid owned local model fixture")
	}
	verify := func(dir string, files map[string]string) {
		t.Helper()
		for name, want := range files {
			if filepath.Base(name) != name {
				t.Fatal("nonlocal fixture name")
			}
			p := filepath.Join(dir, name)
			info, err := os.Lstat(p)
			if err != nil || !info.Mode().IsRegular() {
				t.Fatalf("fixture must be regular: %s %v", p, err)
			}
			value, err := os.ReadFile(p)
			if err != nil || fmt.Sprintf("%x", sha256.Sum256(value)) != want {
				t.Fatalf("fixture hash changed: %s %v", name, err)
			}
		}
	}
	verify(fixtureDir, fixture.Files)
	verify(filepath.Join(fixture.Root, "models/hub", "models--"+strings.ReplaceAll(fixture.Model, "/", "--"), "snapshots", fixture.Revision), fixture.ModelFiles)
	red, err := os.ReadFile(filepath.Join(fixtureDir, "red.h264"))
	if err != nil {
		t.Fatal(err)
	}
	blue, err := os.ReadFile(filepath.Join(fixtureDir, "blue.h264"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(red, blue) || len(red) == 0 || len(blue) == 0 {
		t.Fatal("camera scenes must differ")
	}
	// Only this owned tiny recording store uses a 128MiB reserve. The normal
	// production default remains 5GiB; the fixture writes less than 1MiB.
	root := t.TempDir()
	signalCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	ctx, cancel := context.WithTimeout(signalCtx, 6*time.Minute)
	defer cancel()
	yaml := []byte(fmt.Sprintf(`version: 1
name: managed-color
sources:
  - camera: "*"
    capture: {mode: continuous}
inference:
  model: %s
  revision: %s
  labels: [fixture_red]
  threshold: 0.9
  rate: 5
  accelerator: cpu
  event: red_seen
  clear_after: 100ms
  cooldown: 1h
  enabled: true
capture:
  buffer: 500ms
  drain: 0s
  after_trigger: 2s
  triggers: [{event: red_seen}]
upload: {when: manual}
export: {annotation: cvat}
`, fixture.Model, fixture.Revision))
	oldIDs := map[string]bool{}
	for boot := 0; boot < 2; boot++ {
		manager, err := data.NewManager(root)
		if err != nil {
			t.Fatal(err)
		}
		var visible atomic.Bool
		visible.Store(true)
		manager.SetQuota(256<<20, 128<<20)
		manager.SetWarnLogger(func(message string) { t.Log("CAMPAIGN_WARNING " + message) })
		video := campaignRuntimeVideo(func() ([]string, error) {
			if !visible.Load() {
				return nil, nil
			}
			return []string{"/dev/video1890"}, nil
		}, func(string) (string, error) { return "Generated color source", nil })
		var positive atomic.Bool
		var producers sync.WaitGroup
		video.startProducer = func(producerCtx context.Context, h *deviceHub, path string, _ *agentpb.StreamVideoRequest) {
			producers.Add(1)
			go func() {
				defer producers.Done()
				defer close(h.done)
				defer func() {
					video.mu.Lock()
					if video.hubs[path] == h {
						delete(video.hubs, path)
					}
					video.mu.Unlock()
					h.mu.Lock()
					for _, s := range h.subs {
						if !s.closed {
							s.closed = true
							close(s.ch)
						}
					}
					h.mu.Unlock()
				}()
				tick := time.NewTicker(50 * time.Millisecond)
				defer tick.Stop()
				for {
					select {
					case <-producerCtx.Done():
						return
					case <-tick.C:
						payload := blue
						if positive.Load() {
							payload = red
						}
						h.produce(&videoFrame{data: payload, codec: agentpb.VideoCodec_VIDEO_CODEC_H264, auAligned: true, tsNs: uint64(time.Now().UnixNano()), nativeNs: time.Now().UnixNano(), nativeClock: "GENERATED_SCENE_RECEIPT"})
					}
				}
			}()
		}
		service := NewDataService(manager)
		service.SetVideoService(video)
		factory := &campaignRuntimeFactory{factory: inference.ManagedFactory{Root: fixture.Root}, results: make(chan inference.Result, 256)}
		stopInference := service.StartCampaignInference(ctx, factory, nil)
		t.Cleanup(func() { stopInference(); video.Shutdown(); producers.Wait() })
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		server := grpc.NewServer()
		agentpbv2.RegisterDataServiceServer(server, service)
		served := make(chan error, 1)
		go func() { served <- server.Serve(listener) }()
		conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			t.Fatal(err)
		}
		client := agentpbv2.NewDataServiceClient(conn)
		var cleanupOnce sync.Once
		cleanup := func() {
			cleanupOnce.Do(func() {
				stopInference()
				for _, key := range manager.ActiveEpisodeKeys() {
					_, _ = service.stopCapture(context.Background(), key)
				}
				if a := service.arming(); a != nil {
					a.Disarm("managed-color")
				}
				video.Shutdown()
				producers.Wait()
				conn.Close()
				server.Stop()
				select {
				case <-served:
				case <-time.After(5 * time.Second):
					t.Error("campaign RPC did not drain")
				}
			})
		}
		t.Cleanup(cleanup)
		if boot == 0 {
			if _, err = client.CampaignDeploy(ctx, &agentpbv2.DataCampaignDeployRequest{CampaignYaml: yaml}); err != nil {
				t.Fatal(err)
			}
		}
		waitPrediction := func(wantPositive bool, minimumGeneration uint64) inference.Result {
			t.Helper()
			deadline := time.NewTimer(90 * time.Second)
			defer deadline.Stop()
			for {
				select {
				case r := <-factory.results:
					if r.Type == "prediction" && r.Generation >= minimumGeneration && r.SourceID == "v4l2:/dev/video1890" && (len(r.Detections) > 0) == wantPositive {
						if r.Width != 64 || r.Height != 64 || r.Engine != "cpu" {
							t.Fatalf("lost real model provenance: %+v", r)
						}
						return r
					}
				case <-ctx.Done():
					t.Fatalf("campaign runtime interrupted: %v", ctx.Err())
				case <-deadline.C:
					inspect, _ := client.CampaignInspect(ctx, &agentpbv2.DataCampaignInspectRequest{Name: "managed-color"})
					t.Fatalf("real managed model did not report scene positive=%v: %v", wantPositive, inspect)
				}
			}
		}
		waitPrediction(false, 0)
		episodes, err := manager.List()
		if err != nil || len(episodes) != boot {
			t.Fatalf("negative camera scene triggered recording: %v %v", episodes, err)
		}
		positive.Store(true)
		positiveResult := waitPrediction(true, 0)
		var manifest data.Manifest
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			if ctx.Err() != nil {
				t.Fatal(ctx.Err())
			}
			episodes, err = manager.List()
			if err != nil {
				t.Fatal(err)
			}
			for _, episode := range episodes {
				if oldIDs[episode.ID] {
					continue
				}
				candidate, failures, inspectErr := manager.Inspect(episode.ID, true)
				if inspectErr == nil && candidate.State == "complete" {
					if len(failures) != 0 {
						t.Fatalf("sealed recording integrity: %v", failures)
					}
					manifest = candidate
					break
				}
			}
			if manifest.ID != "" {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if manifest.ID == "" {
			t.Fatalf("real prediction did not trigger and seal a camera episode: list=%+v active=%+v", episodes, manager.Status())
		}
		if manifest.Trigger.Reason != "event:red_seen" || manifest.Trigger.CampaignName != "managed-color" {
			t.Fatalf("wrong trigger: %+v", manifest.Trigger)
		}
		if manifest.ModelVersions[fixture.Model] != fixture.Revision {
			t.Fatalf("sealed model revision differs from loaded bytes: %v", manifest.ModelVersions)
		}
		if manifest.ModelIO.Predictions == 0 || manifest.ModelIO.SamplesDelivered == 0 {
			t.Fatalf("model outputs/inputs missing from real episode: %+v", manifest.ModelIO)
		}
		media := false
		for _, file := range manifest.Files {
			if file.SourceID == "v4l2:/dev/video1890" && strings.HasSuffix(file.Path, ".h264") && file.Size > 0 {
				reader, _, err := manager.OpenFile(manifest.ID, file.Path, 0)
				if err != nil {
					t.Fatal(err)
				}
				payload, readErr := io.ReadAll(io.LimitReader(reader, 8<<20))
				closeErr := reader.Close()
				if readErr != nil || closeErr != nil || !bytes.Contains(payload, red) {
					t.Fatalf("sealed camera bytes lost actual positive scene: read=%v close=%v", readErr, closeErr)
				}
				media = true
			}
		}
		if !media {
			t.Fatal("camera adapter did not retain encoded model input")
		}

		if boot == 0 {
			visible.Store(false)
			until := time.Now().Add(8 * time.Second)
			lost := false
			for time.Now().Before(until) {
				if ctx.Err() != nil {
					t.Fatal(ctx.Err())
				}
				inspected, err := client.CampaignInspect(ctx, &agentpbv2.DataCampaignInspectRequest{Name: "managed-color"})
				if err != nil {
					t.Fatal(err)
				}
				var plan data.Campaign
				if err = json.Unmarshal(inspected.PlanJson, &plan); err != nil {
					t.Fatal(err)
				}
				if plan.InferenceStatus != nil && len(plan.InferenceStatus.Sources) == 0 {
					lost = true
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
			if !lost {
				t.Fatal("actual camera disappearance was not reconciled")
			}
			visible.Store(true)
			waitPrediction(true, positiveResult.Generation+1)
			// Receiving a second real result means the first was forwarded to
			// the production campaign loop, including its presence decision.
			waitPrediction(true, positiveResult.Generation+1)
			until = time.Now().Add(time.Second)
			for time.Now().Before(until) {
				if ctx.Err() != nil {
					t.Fatal(ctx.Err())
				}
				episodes, err := manager.List()
				if err != nil || len(episodes) != 1 || len(manager.ActiveEpisodeKeys()) != 0 {
					t.Fatalf("camera reconnection lost presence/cooldown and duplicated episode: %v active=%v err=%v", episodes, manager.ActiveEpisodeKeys(), err)
				}
				time.Sleep(20 * time.Millisecond)
			}
			t.Log("CAMPAIGN_RECONNECT_VERIFIED actual new stream generation kept cooldown")
		}
		oldIDs[manifest.ID] = true
		t.Logf("CAMPAIGN_RUNTIME_EPISODE boot=%d id=%s predictions=%d inputs=%d model=%s revision=%s", boot, manifest.ID, manifest.ModelIO.Predictions, manifest.ModelIO.SamplesDelivered, fixture.Model, fixture.Revision)
		if boot == 1 {
			removed, err := client.CampaignRemove(ctx, &agentpbv2.DataCampaignRemoveRequest{Name: "managed-color"})
			if err != nil || len(removed.GetWarnings()) != 0 {
				t.Fatalf("campaign removal did not drain: %v %v", removed, err)
			}
			if factory.closed.Load() != factory.sessions.Load() {
				t.Fatal("removal returned before actual model process closed")
			}
			if _, err = manager.Campaign("managed-color"); !os.IsNotExist(err) {
				t.Fatalf("removed plan remains: %v", err)
			}
		}
		cleanup()
		if factory.sessions.Load() != 1 || factory.closed.Load() != 1 {
			t.Fatalf("managed process lifecycle sessions=%d closed=%d", factory.sessions.Load(), factory.closed.Load())
		}
		video.mu.Lock()
		remaining := len(video.hubs)
		video.mu.Unlock()
		if remaining != 0 {
			t.Fatalf("camera hub retained after shutdown: %d", remaining)
		}
	}
	verify(fixtureDir, fixture.Files)
	t.Log("CAMPAIGN_RUNTIME_VERIFIED real YAML RPC, local HF model/PyAV, camera recorder, sealed provenance, persisted restart and removal")
}
