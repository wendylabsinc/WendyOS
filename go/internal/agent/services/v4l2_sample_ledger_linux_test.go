//go:build linux && regression_sample_ledger && regression_campaign_runtime

package services

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/agent/ipcam"
	sharedenv "github.com/wendylabsinc/wendy/go/internal/shared/env"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"go.uber.org/zap"
)

// The source is generated H264, not a physical camera. The producer seam feeds
// the production hub; both real capture and the OUTPUT pump consume that hub.
// Kernel QBUF/DQBUF, example decoding/inference/socket framing, recording and
// payload lookup are actual paths. This does not invent application deliveries
// in model_inputs.jsonl or claim OCI grants/physical firmware/model accuracy.
func TestV4L2ExamplePredictionJoinsRecordedCameraPayload(t *testing.T) {
	if os.Getenv("WENDY_V4L2_SAMPLE_LEDGER") != "1" {
		t.Skip("requires owned hosted kernel fixture")
	}
	if os.Getenv("GITHUB_ACTIONS") != "true" || os.Getenv("RUNNER_ENVIRONMENT") != "github-hosted" || os.Geteuid() != 0 {
		t.Fatal("requires disposable hosted Linux runner")
	}
	const node = "/dev/video96"
	const source = "v4l2:/dev/video1890"
	if _, err := os.Lstat(node); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned node must be absent: %v", err)
	}
	version, err := os.ReadFile("/sys/module/v4l2loopback/version")
	if err != nil || strings.TrimSpace(string(version)) != "0.15.4" {
		t.Fatalf("require already loaded owned module 0.15.4: %s %v", version, err)
	}
	fixture := os.Getenv("WENDY_V4L2_LEDGER_FIXTURE")
	python := os.Getenv("WENDY_V4L2_LEDGER_PYTHON")
	journey := os.Getenv("WENDY_V4L2_LEDGER_JOURNEY")
	if fixture == "" || python == "" || journey == "" {
		t.Fatal("fixture paths are required")
	}
	var files map[string]string
	raw, err := os.ReadFile(filepath.Join(fixture, "files.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &files); err != nil {
		t.Fatal(err)
	}
	for name, digest := range files {
		if filepath.Base(name) != name {
			t.Fatal("nonlocal fixture name")
		}
		raw, err := os.ReadFile(filepath.Join(fixture, name))
		if err != nil || fmt.Sprintf("%x", sha256.Sum256(raw)) != digest {
			t.Fatalf("fixture hash mismatch: %s %v", name, err)
		}
	}
	red, err := os.ReadFile(filepath.Join(fixture, "red.h264"))
	if err != nil {
		t.Fatal(err)
	}
	blue, err := os.ReadFile(filepath.Join(fixture, "blue.h264"))
	if err != nil {
		t.Fatal(err)
	}
	if len(red) == 0 || bytes.Equal(red, blue) {
		t.Fatal("distinct generated scenes required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	work, err := os.MkdirTemp(os.Getenv("WENDY_V4L2_LEDGER_WORK"), "sample-ledger-")
	if err != nil {
		t.Fatal(err)
	}
	// The outer guardian owns failure/watchdog cleanup; leave evidence under its
	// owned directory, including partial episodes, until group drain is proven.
	loop := ipcam.NewLoopback(ctx, zap.NewNop(), nil, nil, nil)
	if err := loop.EnsureAuxNode(ctx, 96, os.Getenv("WENDY_V4L2_LEDGER_LABEL")); err != nil {
		t.Fatal(err)
	}
	defer func() {
		loop.Shutdown()
		loop.RemoveAuxNode(96)
		if _, err := os.Lstat(node); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("owned V4L2 node remains: %v", err)
		}
	}()
	writer, err := openLoopbackFrameWriterPlatform(node)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	video := campaignRuntimeVideo(func() ([]string, error) { return []string{"/dev/video1890"}, nil }, func(string) (string, error) { return "Generated sample ledger source", nil })
	sequence := new(atomic.Uint64)
	sequence.Store((1 << 63) + 1234567)
	video.sampleSeqs["/dev/video1890"] = sequence
	var producers sync.WaitGroup
	video.startProducer = func(producerCtx context.Context, h *deviceHub, path string, _ *agentpb.StreamVideoRequest) {
		producers.Add(1)
		go func() {
			defer producers.Done()
			defer close(h.done)
			defer func() {
				h.mu.Lock()
				for _, sub := range h.subs {
					if !sub.closed {
						sub.closed = true
						close(sub.ch)
					}
				}
				h.mu.Unlock()
			}()
			ticker := time.NewTicker(40 * time.Millisecond)
			defer ticker.Stop()
			index := 0
			for {
				select {
				case <-producerCtx.Done():
					return
				case <-ticker.C:
					payload := red
					if index%2 == 1 {
						payload = blue
					}
					index++
					h.produce(&videoFrame{data: payload, codec: agentpb.VideoCodec_VIDEO_CODEC_H264, auAligned: true, tsNs: uint64(time.Now().UnixNano()), nativeClock: "GENERATED_SCENE"})
				}
			}
		}()
	}
	defer func() { video.Shutdown(); producers.Wait() }()
	manager, err := data.NewManager(filepath.Join(work, "episodes"))
	if err != nil {
		t.Fatal(err)
	}
	manager.SetQuota(256<<20, 128<<20)
	service := NewDataService(manager)
	service.SetVideoService(video)
	service.adHocDrain = 0
	episode, err := service.Start(ctx, &agentpbv2.DataStartRequest{Name: "sample-ledger", Sources: []string{source, "applications"}})
	if err != nil {
		t.Fatal(err)
	}
	active := true
	defer func() {
		if active {
			_, _ = service.Stop(context.Background(), &agentpbv2.DataStopRequest{})
		}
	}()
	hub, subID, frames, err := video.joinHub(ctx, "/dev/video1890", &agentpb.StreamVideoRequest{DeviceId: 1890})
	if err != nil {
		t.Fatal(err)
	}
	pump := newHubLoopbackPump(zap.NewNop(), source, node)
	pumpCtx, stopPump := context.WithCancel(ctx)
	pumpDone := make(chan error, 1)
	go func() { pumpDone <- pump.pump(pumpCtx, hub, subID, frames, writer) }()
	defer func() {
		stopPump()
		select {
		case <-pumpDone:
		case <-time.After(3 * time.Second):
			t.Error("sample pump did not join")
		}
		hub.unsubscribe(subID)
	}()
	waitForBindings(t, pump, 2)
	socketCtx, stopSocket := context.WithCancel(ctx)
	defer stopSocket()
	socketRoot, err := os.MkdirTemp("/tmp", "vsl-")
	if err != nil {
		t.Fatal(err)
	}
	oldRoot := AppDataSocketRootPath
	AppDataSocketRootPath = socketRoot
	defer func() { AppDataSocketRootPath = oldRoot }()
	sockets := NewAppDataSocketManager(socketCtx, zap.NewNop(), manager)
	var ownedSockets []*appDataSocket
	defer func() {
		sockets.stopAll()
		// Only the owned direct child can authenticate, and it has been joined
		// before this cleanup. Wait for the real socket handlers to retire.
		deadline := time.Now().Add(2 * time.Second)
		for {
			sockets.mu.Lock()
			live := 0
			for _, socket := range ownedSockets {
				live += socket.live
			}
			sockets.mu.Unlock()
			if live == 0 {
				_ = os.RemoveAll(socketRoot)
				break
			}
			if time.Now().After(deadline) {
				t.Errorf("retaining socket root after incomplete drain: %s", socketRoot)
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	var owner atomic.Int32
	// Keep real SO_PEERCRED. Only the exact Popen child may resolve to this
	// fixture app scope. Cgroup membership/OCI authorization is not claimed.
	sockets.cgroupOfPID = func(pid int32) (string, error) {
		if pid != owner.Load() {
			return "", fmt.Errorf("not owned model child")
		}
		return fmt.Sprintf("0::/system.slice/%s-sh.wendy.sample-ledger.scope\n", sharedenv.SystemdServiceName()), nil
	}
	directory, err := sockets.Ensure("sh.wendy.sample-ledger", "")
	if err != nil {
		t.Fatal(err)
	}
	sockets.mu.Lock()
	for _, socket := range sockets.sockets {
		ownedSockets = append(ownedSockets, socket)
	}
	sockets.mu.Unlock()
	report := filepath.Join(work, "predictions.json")
	permit := filepath.Join(work, "permit")
	commandCtx, stopCommand := context.WithTimeout(ctx, 20*time.Second)
	defer stopCommand()
	command := exec.CommandContext(commandCtx, python, journey, "consume", "--fixture", fixture, "--node", node, "--source", source, "--socket", filepath.Join(directory, DataSocketFilename), "--report", report, "--permit", permit)
	command.WaitDelay = time.Second
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	owner.Store(int32(command.Process.Pid))
	if err := os.WriteFile(permit, []byte(strconv.Itoa(command.Process.Pid)), 0600); err != nil {
		stopCommand()
		_ = command.Wait()
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatalf("actual model reader failed: %v\n%s", err, output.String())
	}
	if !strings.Contains(output.String(), "V4L2_EXAMPLE_PREDICTIONS_RECORDED 12") {
		t.Fatalf("reader proof missing: %s", output.String())
	}
	t.Log(output.String())
	if _, err := service.Stop(ctx, &agentpbv2.DataStopRequest{}); err != nil {
		t.Fatal(err)
	}
	active = false
	manifest, failures, err := manager.Inspect(episode.GetId(), true)
	if err != nil || len(failures) > 0 {
		t.Fatalf("sealed episode validation: %v %v", err, failures)
	}
	if manifest.State != "complete" || manifest.ModelIO.Predictions != 12 {
		t.Fatalf("actual predictions not sealed: %+v", manifest.ModelIO)
	}
	verifyCtx, stopVerify := context.WithTimeout(ctx, 10*time.Second)
	defer stopVerify()
	verify := exec.CommandContext(verifyCtx, python, journey, "verify", "--episode", filepath.Join(work, "episodes", episode.GetId()), "--source", source, "--report", report)
	verify.WaitDelay = time.Second
	result, err := verify.CombinedOutput()
	if err != nil {
		t.Fatalf("kernel sample-ID join failed: %v\n%s", err, result)
	}
	if !bytes.Contains(result, []byte("V4L2_SAMPLE_CAMERA_INDEX_JOIN_VERIFIED 12")) {
		t.Fatalf("join proof missing: %s", result)
	}
	t.Log(string(result))
}
