package services

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/agent/inference"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

type warnings struct {
	mu       sync.Mutex
	messages []string
}

func (w *warnings) add(message string) {
	w.mu.Lock()
	w.messages = append(w.messages, message)
	w.mu.Unlock()
}
func (w *warnings) matching(text string) (n int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, message := range w.messages {
		if strings.Contains(message, text) {
			n++
		}
	}
	return n
}

func inspectInference(t *testing.T, service *DataService) *data.InferenceStatus {
	t.Helper()
	inspection, err := service.CampaignInspect(context.Background(), &agentpbv2.DataCampaignInspectRequest{Name: "people-all-cameras"})
	if err != nil {
		t.Fatal(err)
	}
	var plan data.Campaign
	if err := json.Unmarshal(inspection.PlanJson, &plan); err != nil {
		t.Fatal(err)
	}
	return plan.InferenceStatus
}

func TestAgentInferenceReportsTheEngineAndWarnsOnFallback(t *testing.T) {
	service, factory, _, _ := newInferenceTestService(t, true)
	logged := &warnings{}
	service.manager.SetWarnLogger(logged.add)
	session := receiveInference(t, factory.sessions)
	building := &data.AcceleratorStatus{Requested: "auto", Active: "cpu", Candidates: []string{"tensorrt", "cpu"},
		Reason: "the TensorRT engine is not ready yet", Engine: &data.EngineStatus{State: "building", StartedAt: time.Now().Add(-90 * time.Second).UTC().Format(time.RFC3339)}}
	session.results <- inference.Result{Type: "accelerator", Accelerator: building, Stats: &data.InferenceStats{FPS: 2, InferMsP50: 40}}
	deadline := time.Now().Add(8 * time.Second)
	var status *data.InferenceStatus
	for time.Now().Before(deadline) {
		if status = inspectInference(t, service); status.Accelerator != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if status.Accelerator == nil || status.Accelerator.Active != "cpu" || status.Stats == nil || status.Stats.InferMsP50 != 40 {
		t.Fatalf("status lacks the engine: %+v", status)
	}
	// The build's age is current at inspect, not as old as the last report.
	if elapsed := status.Accelerator.Engine.ElapsedSeconds; elapsed < 89 || elapsed > 120 {
		t.Fatalf("elapsed %v", elapsed)
	}
	if logged.matching("not tensorrt") != 0 {
		t.Fatal("building on the cpu is not a fallback")
	}

	degraded := &data.AcceleratorStatus{Requested: "auto", Active: "cpu", Degraded: true, Reason: "tensorrt disagrees with the cpu: a class score differs by 0.310 (> 0.05)"}
	for range 3 {
		session.results <- inference.Result{Type: "accelerator", Accelerator: degraded}
	}
	for time.Now().Before(deadline) && logged.matching("not tensorrt") == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if n := logged.matching(`campaign "people-all-cameras" inference runs on cpu, not tensorrt: tensorrt disagrees with the cpu`); n != 1 {
		t.Fatalf("fallback logged %d times: %v", n, logged.messages)
	}
	if status := inspectInference(t, service); !status.Accelerator.Degraded || status.Accelerator.Engine != nil {
		t.Fatalf("degraded status %+v", status.Accelerator)
	}
}

func TestPredictionRecordsNameTheEngine(t *testing.T) {
	job := &campaignInferenceJob{status: data.InferenceStatus{Accelerator: &data.AcceleratorStatus{ModelSHA256: strings.Repeat("7", 64)}}}
	attributes := map[string]any{}
	job.predictionProvenance(attributes, inference.Result{Engine: "tensorrt", Precision: "fp16", InferMs: 5.5})
	if attributes["accelerator"] != "tensorrt" || attributes["precision"] != "fp16" || attributes["infer_ms"] != 5.5 || attributes["model_sha256"] != strings.Repeat("7", 64) {
		t.Fatalf("attributes %v", attributes)
	}
	// An older worker reports none of it, and the record says nothing false.
	attributes = map[string]any{}
	(&campaignInferenceJob{}).predictionProvenance(attributes, inference.Result{})
	if len(attributes) != 0 {
		t.Fatalf("attributes %v", attributes)
	}
}

func TestSnapshotSaysHowLongTheBuildHasWaited(t *testing.T) {
	now := time.Date(2026, 10, 27, 14, 0, 0, 0, time.UTC)
	since := now.Add(-12 * time.Minute).Format(time.RFC3339)
	for _, state := range []string{"waiting_for_cpu", "waiting_for_memory"} {
		status := &data.AcceleratorStatus{Active: "cpu", Engine: &data.EngineStatus{State: state, WaitingSince: since,
			BuildBy: now.Add(18 * time.Minute).Format(time.RFC3339)}}
		engine := acceleratorSnapshot(status, now).Engine
		if engine.ElapsedSeconds != 720 || engine.WaitingSince != since || engine.BuildBy != status.Engine.BuildBy {
			t.Fatalf("%s: %+v", state, engine)
		}
	}
	// A state that is neither a build nor a wait has no age.
	queued := &data.AcceleratorStatus{Engine: &data.EngineStatus{State: "queued", WaitingSince: since, LowPriority: true}}
	if engine := acceleratorSnapshot(queued, now).Engine; engine.ElapsedSeconds != 0 || !engine.LowPriority {
		t.Fatalf("queued: %+v", engine)
	}
}

func TestInferenceHardwareIsDeviceInfo(t *testing.T) {
	hardware := InferenceHardware()
	info := detectGPUInfo()
	if hardware.GPUVendor != info.vendor || hardware.CUDAVersion != info.cudaVersion || hardware.GPUArch != info.gpuArch {
		t.Fatalf("inference hardware %+v differs from device info %+v", hardware, info)
	}
}

// scriptedFactory fails or starts a session, step by step; once out of steps
// it starts sessions that stay up. No step runs before gate is closed.
type scriptedFactory struct {
	gate    chan struct{}
	mu      sync.Mutex
	steps   []any // an error, or an *inferenceTestSession
	started chan *inferenceTestSession
}

func newTestSession() *inferenceTestSession {
	return &inferenceTestSession{results: make(chan inference.Result, 16), inputs: make(chan inference.Input, 16), closed: make(chan struct{})}
}

func (f *scriptedFactory) Start(ctx context.Context, _ data.CampaignInference) (inference.Session, error) {
	select {
	case <-f.gate:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	session := newTestSession()
	if len(f.steps) > 0 {
		step := f.steps[0]
		f.steps = f.steps[1:]
		if err, ok := step.(error); ok {
			return nil, err
		}
		session = step.(*inferenceTestSession)
	}
	select {
	case f.started <- session:
	default:
	}
	return session, nil
}

// A tensorrt campaign on a device without TensorRT cannot start, and was
// logged every 5 s. Each reason is logged once now. A worker that dies after
// it started is logged every time, with the tail of its stderr, which before
// was lost, and its status says the same.
func TestInferenceLogsStartFailuresOnceAndExitsWithTheirStderr(t *testing.T) {
	previous := inferenceRetryDelay
	inferenceRetryDelay = 5 * time.Millisecond
	t.Cleanup(func() { inferenceRetryDelay = previous })
	unavailable := errors.New("accelerator tensorrt is unavailable: this device has an NVIDIA GPU but no TensorRT libraries")
	unfetched := errors.New("accelerator tensorrt is unavailable: the TensorRT Python bindings 10.16.2.10 are not on this device and could not be fetched")
	exit := "model process exited: exit status 1; stderr: Traceback (most recent call last): | RuntimeError: CUDA error 999"
	crashed := newTestSession()
	crashed.err = errors.New(exit)
	close(crashed.results)
	factory := &scriptedFactory{gate: make(chan struct{}), started: make(chan *inferenceTestSession, 8),
		steps: []any{unavailable, unavailable, unavailable, unavailable, unfetched, unfetched, unfetched, crashed}}

	logged := &warnings{}
	var service *DataService
	exitStatus := make(chan *data.InferenceStatus, 1)
	warn := func(message string) {
		logged.add(message)
		if strings.Contains(message, "RuntimeError: CUDA error 999") {
			// The status as the supervisor left it when it logged the exit.
			campaign, err := service.manager.Campaign("people-all-cameras")
			if err == nil {
				select {
				case exitStatus <- service.inference.snapshot(campaign.Name, campaign.Revision):
				default:
				}
			}
		}
	}
	service, _, _ = startInferenceTestService(t, factory, warn, true)
	close(factory.gate) // After service is set, for warn.
	if session := receiveInference(t, factory.started); session != crashed {
		t.Fatal("the crashing session was not the first to start")
	}
	receiveInference(t, factory.started) // The replacement, which stays up.

	if n := logged.matching("no TensorRT libraries; retrying every 5ms, logged again when the reason changes"); n != 1 {
		t.Fatalf("unavailable logged %d times: %q", n, logged.messages)
	}
	if n := logged.matching("could not be fetched; retrying every"); n != 1 {
		t.Fatalf("unfetched logged %d times: %q", n, logged.messages)
	}
	if n := logged.matching(`campaign "people-all-cameras" inference: ` + exit + "; retrying in 5ms"); n != 1 {
		t.Fatalf("exit logged %d times: %q", n, logged.messages)
	}
	status := receiveInference(t, exitStatus)
	if status.State != "error" || status.Error != exit {
		t.Fatalf("status at exit %+v", status)
	}
}
