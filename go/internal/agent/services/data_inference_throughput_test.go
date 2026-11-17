package services

import (
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/agent/inference"
)

var (
	// A Transformers model on a Jetson Orin Nano's CPU: 9.1 s a frame.
	slowStats = &data.InferenceStats{FPS: 0.11, InferMsP50: 9104, InferMsP95: 9377, FramesScored: 12}
	fastStats = &data.InferenceStats{FPS: 0.98, InferMsP50: 212, InferMsP95: 240, FramesScored: 80}
	cpuOnly   = &data.AcceleratorStatus{Requested: "auto", Active: "cpu", Candidates: []string{"cpu"}}
)

const slowSummary = "model scores 0.11 frames/s of the 1 requested (about 9.1 s per frame on cpu)"

func TestThroughputFlagsAModelSlowerThanItsRateOnceItLasts(t *testing.T) {
	start := time.Now()
	at := func(seconds int) time.Time { return start.Add(time.Duration(seconds) * time.Second) }
	pace := &inferencePace{}
	for _, seconds := range []int{0, 10, 20} {
		throughput, changed := pace.judge(at(seconds), 1, 1, slowStats, cpuOnly)
		if changed || throughput.ModelTooSlow {
			t.Fatalf("flagged after %d s: %+v", seconds, throughput)
		}
		// The figures show from the first report, flagged or not.
		if throughput.RequestedFPS != 1 || throughput.AchievedFPS != 0.11 || throughput.Summary != slowSummary {
			t.Fatalf("throughput %+v", throughput)
		}
	}
	if throughput, changed := pace.judge(at(30), 1, 1, slowStats, cpuOnly); !changed || !throughput.ModelTooSlow {
		t.Fatalf("not flagged after 30 s: %+v", throughput)
	}
	if throughput, changed := pace.judge(at(40), 1, 1, slowStats, cpuOnly); changed || !throughput.ModelTooSlow {
		t.Fatalf("flag did not hold: %+v", throughput)
	}

	// It clears the same way: once the model has kept up for as long.
	for _, seconds := range []int{50, 60, 70} {
		if throughput, changed := pace.judge(at(seconds), 1, 1, fastStats, cpuOnly); changed || !throughput.ModelTooSlow {
			t.Fatalf("cleared after %d s: %+v", seconds-50, throughput)
		}
	}
	throughput, changed := pace.judge(at(80), 1, 1, fastStats, cpuOnly)
	if !changed || throughput.ModelTooSlow {
		t.Fatalf("not cleared after 30 s: %+v", throughput)
	}
	if want := "model scores 0.98 frames/s of the 1 requested (about 212 ms per frame on cpu)"; throughput.Summary != want {
		t.Fatalf("summary %q, want %q", throughput.Summary, want)
	}
}

func TestThroughputIgnoresASlowSpell(t *testing.T) {
	start := time.Now()
	pace := &inferencePace{}
	// Slow, fast, then slow for 25 s: no 30 s of it in a row. Then nothing
	// scored, as while a camera is away, and slow for 25 s again.
	steps := []struct {
		seconds int
		stats   *data.InferenceStats
	}{{0, slowStats}, {10, fastStats}, {20, slowStats}, {45, slowStats}, {50, nil}, {60, slowStats}, {85, slowStats}}
	for _, step := range steps {
		throughput, changed := pace.judge(start.Add(time.Duration(step.seconds)*time.Second), 1, 1, step.stats, cpuOnly)
		if changed || throughput != nil && throughput.ModelTooSlow {
			t.Fatalf("flagged at %d s: %+v", step.seconds, throughput)
		}
	}
	if throughput, changed := pace.judge(start.Add(90*time.Second), 1, 1, slowStats, cpuOnly); !changed || !throughput.ModelTooSlow {
		t.Fatalf("not flagged after 30 s in a row: %+v", throughput)
	}
}

func TestThroughputDoesNotBlameTheModelForMissingFrames(t *testing.T) {
	start := time.Now()
	pace := &inferencePace{}
	// Half the frames the rate asks for, from a model that needs 120 ms for
	// one: the cameras deliver fewer, and the model has time to spare.
	starved := &data.InferenceStats{FPS: 0.5, InferMsP50: 120, InferMsP95: 140, FramesScored: 40}
	for seconds := 0; seconds <= 120; seconds += 10 {
		throughput, changed := pace.judge(start.Add(time.Duration(seconds)*time.Second), 1, 1, starved, cpuOnly)
		if changed || throughput.ModelTooSlow {
			t.Fatalf("flagged at %d s: %+v", seconds, throughput)
		}
		if want := "model scores 0.5 frames/s of the 1 requested (about 120 ms per frame on cpu)"; throughput.Summary != want {
			t.Fatalf("summary %q, want %q", throughput.Summary, want)
		}
	}
	// Nothing scored lately: nothing to compare.
	for _, stats := range []*data.InferenceStats{nil, {FramesScored: 3}, {FPS: 1, FramesScored: 3}} {
		if throughput, changed := pace.judge(start, 1, 1, stats, cpuOnly); throughput != nil || changed {
			t.Fatalf("compared %+v: %+v", stats, throughput)
		}
	}
	if throughput, _ := pace.judge(start, 1, 0, slowStats, cpuOnly); throughput != nil {
		t.Fatalf("compared with no camera being scored: %+v", throughput)
	}
}

func TestThroughputWaitsForTheTensorRTEngine(t *testing.T) {
	start := time.Now()
	at := func(seconds int) time.Time { return start.Add(time.Duration(seconds) * time.Second) }
	pace := &inferencePace{}
	slow := &data.InferenceStats{FPS: 2.1, InferMsP50: 470, InferMsP95: 520, FramesScored: 300}
	// The CPU stands in while the engine builds: its speed is shown, not judged.
	building := &data.AcceleratorStatus{Requested: "auto", Active: "cpu", Candidates: []string{"tensorrt", "cpu"},
		Reason: "the TensorRT engine is not ready yet", Engine: &data.EngineStatus{State: "building"}}
	for seconds := 0; seconds <= 120; seconds += 10 {
		throughput, changed := pace.judge(at(seconds), 10, 1, slow, building)
		if changed || throughput.ModelTooSlow {
			t.Fatalf("flagged during the build at %d s: %+v", seconds, throughput)
		}
		if want := "model scores 2.1 frames/s of the 10 requested (about 470 ms per frame on cpu)"; throughput.Summary != want {
			t.Fatalf("summary %q, want %q", throughput.Summary, want)
		}
	}
	// The engine failed, so the CPU is what the campaign has.
	failed := &data.AcceleratorStatus{Requested: "auto", Active: "cpu", Candidates: []string{"tensorrt", "cpu"}, Degraded: true,
		Reason: "the TensorRT engine build failed", Engine: &data.EngineStatus{State: "failed"}}
	pace.judge(at(130), 10, 1, slow, failed)
	if throughput, changed := pace.judge(at(160), 10, 1, slow, failed); !changed || !throughput.ModelTooSlow {
		t.Fatalf("not flagged on the cpu after the engine failed: %+v", throughput)
	}
	// TensorRT keeps up, and says what it runs on.
	ready := &data.AcceleratorStatus{Requested: "auto", Active: "tensorrt", Candidates: []string{"tensorrt", "cpu"}, Engine: &data.EngineStatus{State: "ready"}}
	quick := &data.InferenceStats{FPS: 9.97, InferMsP50: 5.53, InferMsP95: 6.1, FramesScored: 900}
	pace.judge(at(170), 10, 1, quick, ready)
	throughput, changed := pace.judge(at(200), 10, 1, quick, ready)
	if !changed || throughput.ModelTooSlow {
		t.Fatalf("not cleared on tensorrt: %+v", throughput)
	}
	if want := "model scores 10 frames/s of the 10 requested (about 5.5 ms per frame on tensorrt)"; throughput.Summary != want {
		t.Fatalf("summary %q, want %q", throughput.Summary, want)
	}
}

func TestThroughputCountsEveryCameraBeingScored(t *testing.T) {
	start := time.Now()
	pace := &inferencePace{}
	// 800 ms a frame is enough for one camera at rate 1, not for two.
	stats := &data.InferenceStats{FPS: 1.2, InferMsP50: 800, InferMsP95: 900, FramesScored: 60}
	if throughput, _ := pace.judge(start, 1, 1, stats, cpuOnly); throughput.RequestedFPS != 1 {
		t.Fatalf("one camera: %+v", throughput)
	}
	if throughput, changed := pace.judge(start.Add(time.Minute), 1, 1, stats, cpuOnly); changed || throughput.ModelTooSlow {
		t.Fatalf("one camera flagged: %+v", throughput)
	}
	pace.judge(start.Add(2*time.Minute), 1, 2, stats, cpuOnly)
	throughput, changed := pace.judge(start.Add(3*time.Minute), 1, 2, stats, cpuOnly)
	if !changed || !throughput.ModelTooSlow || throughput.RequestedFPS != 2 {
		t.Fatalf("two cameras not flagged: %+v", throughput)
	}
	if want := "model scores 1.2 frames/s of the 2 requested (1 for each of 2 cameras; about 800 ms per frame on cpu)"; throughput.Summary != want {
		t.Fatalf("summary %q, want %q", throughput.Summary, want)
	}
	// A rate below one frame a second, on three cameras.
	if throughput, _ := (&inferencePace{}).judge(start, 0.1, 3, slowStats, cpuOnly); throughput.RequestedFPS != 0.3 ||
		throughput.Summary != "model scores 0.11 frames/s of the 0.3 requested (0.1 for each of 3 cameras; about 9.1 s per frame on cpu)" {
		t.Fatalf("three cameras: %+v", throughput)
	}
}

// The reported defect: rate 1 and a model that scores one frame every 9 s,
// with no warning and no achieved rate anywhere.
func TestAgentInferenceSaysWhenTheModelMissesItsRate(t *testing.T) {
	previous := inferenceThroughputHold
	inferenceThroughputHold = 20 * time.Millisecond
	t.Cleanup(func() { inferenceThroughputHold = previous })
	service, factory, _, _ := newInferenceTestService(t, true)
	logged := &warnings{}
	service.manager.SetWarnLogger(logged.add)
	session := receiveInference(t, factory.sessions)
	input := receiveInference(t, session.inputs)
	receiveInference(t, session.inputs)
	// One of the two cameras is being scored.
	session.results <- inference.Result{Type: "prediction", SourceID: input.SourceID, Generation: input.Generation, InferMs: 9104}

	report := func(stats *data.InferenceStats, tooSlow bool) *data.InferenceStatus {
		t.Helper()
		deadline := time.Now().Add(8 * time.Second)
		for {
			session.results <- inference.Result{Type: "accelerator", Accelerator: cpuOnly, Stats: stats}
			time.Sleep(30 * time.Millisecond)
			status := inspectInference(t, service)
			if status.Throughput != nil && status.Throughput.AchievedFPS == stats.FPS && status.Throughput.ModelTooSlow == tooSlow {
				return status
			}
			if time.Now().After(deadline) {
				t.Fatalf("throughput %+v, want model_too_slow %v at %v frames/s", status.Throughput, tooSlow, stats.FPS)
			}
		}
	}
	status := report(slowStats, true)
	if status.State != "running" || status.Stats.FPS != 0.11 || status.Throughput.RequestedFPS != 1 || status.Throughput.Summary != slowSummary {
		t.Fatalf("status %+v throughput %+v", status, status.Throughput)
	}
	// It stays flagged, and is logged once.
	report(slowStats, true)
	if n := logged.matching(`campaign "people-all-cameras" inference cannot keep up with its rate: ` + slowSummary); n != 1 {
		t.Fatalf("logged %d times: %q", n, logged.messages)
	}

	report(fastStats, false)
	report(fastStats, false)
	if n := logged.matching(`campaign "people-all-cameras" inference keeps up with its rate again: model scores 0.98 frames/s of the 1 requested (about 212 ms per frame on cpu)`); n != 1 {
		t.Fatalf("cleared %d times: %q", n, logged.messages)
	}
	if n := logged.matching("cannot keep up"); n != 1 {
		t.Fatalf("logged %d times: %q", n, logged.messages)
	}
}
