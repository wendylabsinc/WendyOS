package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

type tErr string

func (e tErr) Error() string    { return string(e) }
func errForTest(s string) error { return tErr(s) }
func keyMsg(s string) tea.KeyMsg {
	// minimal: only ctrl+c is asserted; map it explicitly.
	if s == "ctrl+c" {
		return tea.KeyMsg{Type: tea.KeyCtrlC}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func applyBuild(m BuildStepsModel, msgs ...interface{}) BuildStepsModel {
	for _, msg := range msgs {
		next, _ := m.Update(msg)
		m = next.(BuildStepsModel)
	}
	return m
}

func TestBuildStepsModelTracksTally(t *testing.T) {
	m := NewBuildStepsModel("Building image...")
	m = applyBuild(m,
		BuildStepMsg{ID: "#6", Kind: BuildVertexStep, Display: "[1/6] FROM", Status: BuildStepRunning},
		BuildStepMsg{ID: "#6", Kind: BuildVertexStep, Display: "[1/6] FROM", Status: BuildStepCached},
		BuildStepMsg{ID: "#9", Kind: BuildVertexStep, Display: "[4/6] RUN", Status: BuildStepRunning},
		BuildStepMsg{ID: "#9", Kind: BuildVertexStep, Display: "[4/6] RUN", Status: BuildStepDone, Dur: time.Second},
	)
	if got := m.Tally(); got.Cached != 1 || got.Rebuilt != 1 {
		t.Fatalf("tally = %+v, want {1 1}", got)
	}
}

func TestBuildStepsModelViewShowsActiveStep(t *testing.T) {
	m := NewBuildStepsModel("Building image...")
	m = applyBuild(m, BuildStepMsg{ID: "#9", Kind: BuildVertexStep, Display: "[4/6] RUN pip install", Status: BuildStepRunning})
	if v := m.View(); !strings.Contains(v, "[4/6] RUN pip install") {
		t.Fatalf("view missing active step:\n%s", v)
	}
}

func TestBuildStepsModelAllDoneQuitsAndKeepsErr(t *testing.T) {
	m := NewBuildStepsModel("Building image...")
	wantErr := errForTest("boom")
	next, cmd := m.Update(BuildAllDoneMsg{Err: wantErr})
	m = next.(BuildStepsModel)
	if cmd == nil {
		t.Fatal("expected a quit command")
	}
	if m.Err() != wantErr {
		t.Fatalf("Err() = %v, want %v", m.Err(), wantErr)
	}
}

func TestBuildStepsModelCtrlCCancels(t *testing.T) {
	m := NewBuildStepsModel("Building image...")
	next, _ := m.Update(keyMsg("ctrl+c"))
	m = next.(BuildStepsModel)
	if m.Err() != ErrCancelled {
		t.Fatalf("Err() = %v, want ErrCancelled", m.Err())
	}
}

func TestBuildStepsModelShowsProgressDetailUnderRunningStep(t *testing.T) {
	m := NewBuildStepsModel("Building image...")
	m = applyBuild(m, BuildStepMsg{
		ID: "#9", Kind: BuildVertexStep, Display: "[4/6] RUN swift build",
		Status: BuildStepRunning, Detail: "[525/1027] 51%  Compiling WendyKit",
	})
	v := m.View()
	if !strings.Contains(v, "[525/1027] 51%  Compiling WendyKit") {
		t.Fatalf("view missing progress detail:\n%s", v)
	}

	// Byte counters render alongside the tool's own progress.
	m = applyBuild(m, BuildStepMsg{
		ID: "#3", Kind: BuildVertexPull, Display: "pull nvidia/l4t-base",
		Status: BuildStepRunning, Bytes: ByteProgress{Current: 5_240_000, Total: 27_090_000, Rate: 3_100_000},
	})
	if v := m.View(); !strings.Contains(v, "19%  5.2MB/27.1MB  3.1MB/s") {
		t.Fatalf("view missing byte progress:\n%s", v)
	}
}

func TestBuildStepsModelClearsDetailWhenStepFinishes(t *testing.T) {
	m := NewBuildStepsModel("Building image...")
	m = applyBuild(m,
		BuildStepMsg{ID: "#9", Kind: BuildVertexStep, Display: "[4/6] RUN x", Status: BuildStepRunning},
		BuildStepMsg{ID: "#9", Kind: BuildVertexStep, Display: "[4/6] RUN x", Status: BuildStepRunning, Detail: "[1/2] 50%  Compiling"},
		BuildStepMsg{ID: "#9", Kind: BuildVertexStep, Display: "[4/6] RUN x", Status: BuildStepDone, Dur: time.Second},
	)
	if v := m.View(); strings.Contains(v, "Compiling") {
		t.Fatalf("finished step should not keep its detail line:\n%s", v)
	}
}

// A step BuildKit reopens (a lazily pulled base image) runs again on screen with
// its progress, finishes with the later duration, and is still one step in the
// tally.
func TestBuildStepsModelReopensFinishedStep(t *testing.T) {
	const from = "[base-nvidia-jetson 1/5] FROM docker.io/dustynv/pytorch"
	m := NewBuildStepsModel("Building image...")
	m = applyBuild(m,
		BuildStepMsg{ID: "#5", Kind: BuildVertexStep, Display: from, Status: BuildStepRunning},
		BuildStepMsg{ID: "#5", Kind: BuildVertexStep, Display: from, Status: BuildStepDone, Dur: 2 * time.Second},
		BuildStepMsg{ID: "#5", Kind: BuildVertexStep, Display: from, Status: BuildStepRunning,
			Detail: "unpacking layer 2", Bytes: ByteProgress{Current: 3_100_000_000, Total: 7_800_000_000}},
	)
	v := m.View()
	if strings.Contains(v, "✓") || !strings.Contains(v, "unpacking layer 2  39%  3.1GB/7.8GB") {
		t.Fatalf("reopened step should be running with its progress:\n%s", v)
	}
	m = applyBuild(m, BuildStepMsg{ID: "#5", Kind: BuildVertexStep, Display: from, Status: BuildStepDone, Dur: 533700 * time.Millisecond})
	if v := m.View(); !strings.Contains(v, "8m53.7s") || strings.Contains(v, "unpacking") {
		t.Fatalf("want the later duration and no detail:\n%s", v)
	}
	if got := m.Tally(); got.Cached != 0 || got.Rebuilt != 1 {
		t.Fatalf("tally = %+v, want the step counted once", got)
	}

	// Cached and then pulled lazily: counted by how it finished.
	m = applyBuild(m,
		BuildStepMsg{ID: "#6", Kind: BuildVertexStep, Display: "[1/2] FROM alpine", Status: BuildStepCached},
		BuildStepMsg{ID: "#6", Kind: BuildVertexStep, Display: "[1/2] FROM alpine", Status: BuildStepRunning},
		BuildStepMsg{ID: "#6", Kind: BuildVertexStep, Display: "[1/2] FROM alpine", Status: BuildStepDone, Dur: time.Second},
	)
	if got := m.Tally(); got.Cached != 0 || got.Rebuilt != 2 {
		t.Fatalf("tally = %+v, want {0 2}", got)
	}
}

// A slow step visibly ticks. The elapsed time is whole seconds, absent for the
// first second, and a reopened step counts from its reopen.
func TestBuildStepsModelShowsElapsedOnRunningStep(t *testing.T) {
	start := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	now := start
	m := NewBuildStepsModel("Building image...").WithoutHints()
	m.now = func() time.Time { return now }
	step := BuildStepMsg{ID: "#9", Kind: BuildVertexStep, Display: "[4/6] RUN pip install", Status: BuildStepRunning}
	m = applyBuild(m, step)

	now = start.Add(400 * time.Millisecond)
	if v := m.View(); strings.Contains(v, "0s") {
		t.Fatalf("a step under a second old should show no elapsed time:\n%s", v)
	}
	now = start.Add(65*time.Second + 400*time.Millisecond)
	if v := m.View(); !strings.Contains(v, "[4/6] RUN pip install "+bsDim.Render("1m5s")) {
		t.Fatalf("want 1m5s beside the running step:\n%s", v)
	}

	done := step
	done.Status, done.Dur = BuildStepDone, 65*time.Second
	m = applyBuild(m, done, step)
	now = now.Add(3 * time.Second)
	if v := m.View(); !strings.Contains(v, bsDim.Render("3s")) || strings.Contains(v, "1m") {
		t.Fatalf("reopened step should count from the reopen:\n%s", v)
	}
}

// A long build must not scroll its own running steps off screen: finished rows
// are elided, running ones are always kept.
func TestBuildStepsModelElidesOldFinishedRowsButKeepsRunning(t *testing.T) {
	m := NewBuildStepsModel("Building image...")
	for i := range 20 {
		id := fmt.Sprintf("#%d", i)
		m = applyBuild(m,
			BuildStepMsg{ID: id, Kind: BuildVertexStep, Display: fmt.Sprintf("[%d/25] RUN done-%d", i, i), Status: BuildStepRunning},
			BuildStepMsg{ID: id, Kind: BuildVertexStep, Display: fmt.Sprintf("[%d/25] RUN done-%d", i, i), Status: BuildStepDone, Dur: time.Second},
		)
	}
	m = applyBuild(m, BuildStepMsg{
		ID: "#99", Kind: BuildVertexStep, Display: "[21/25] RUN swift build",
		Status: BuildStepRunning, Detail: "[525/1027] 51%  Compiling",
	})
	v := m.View()
	if !strings.Contains(v, "[21/25] RUN swift build") || !strings.Contains(v, "[525/1027] 51%  Compiling") {
		t.Fatalf("running step must always be visible:\n%s", v)
	}
	if !strings.Contains(v, "earlier steps") {
		t.Fatalf("want an elision marker for dropped finished rows:\n%s", v)
	}
	if strings.Contains(v, "done-0 ") || strings.Contains(v, "RUN done-0\n") {
		t.Fatalf("oldest finished row should have been elided:\n%s", v)
	}
}
