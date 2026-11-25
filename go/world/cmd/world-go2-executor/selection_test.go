package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/wendylabsinc/wendy/go/world/internal/go2executor"
)

func selectedZero(capture, sequence uint64) *go2executor.SelectionDiagnostic {
	target := [3]float64{}
	return &go2executor.SelectionDiagnostic{TaskID: "task", RunID: "run", SpecSHA256: "digest", ExecutorEpoch: 1, CaptureSequence: capture, CommandSequence: sequence, Stage: "joint-continuation", Candidate: [3]float64{.3, 0, 0}, PriorCount: 2, Priors: [2][3]float64{{.3, 0, 0}, {}}, CandidateResult: "unknown", ZeroResult: "clear", Outcome: "zero_after_unknown", Selected: &target}
}
func TestSelectionLogsExactCaptureCopyAndUnattemptedSkip(t *testing.T) {
	l := observationLogState{}
	s := go2executor.State{CaptureSequence: 10}
	selection := selectedZero(10, 1)
	command := commandObservation(1, 10)
	command.Selection = selectedZero(10, 1)
	events := l.events(s, true, command, "task", "run", 1, selection)
	if len(events) != 1 || events[0]["event"] != "executor_observation" || events[0]["selection_observation_join"] != "exact_capture_sequence" || events[0]["command"] != command {
		t.Fatal("matching selection added a row or lost identity")
	}
	selection.Selected[0] = 9
	selection.Priors[0][0] = 9
	recorded := events[0]["selection"].(*go2executor.SelectionDiagnostic)
	if recorded.Selected[0] != 0 || recorded.Priors[0][0] != .3 {
		t.Fatal("selection event aliases caller")
	}
	// Same original capture is evaluated again but cannot be dispatched twice.
	selection = selectedZero(10, 0)
	events = l.events(s, true, command, "task", "run", 1, selection)
	if len(events) != 1 || events[0]["event"] != "executor_selection" || events[0]["command"] != nil || events[0]["selection"].(*go2executor.SelectionDiagnostic).CommandSequence != 0 {
		t.Fatal("skip fabricated dispatch/ACK or discarded selection")
	}
	if again := l.events(s, true, command, "task", "run", 1, selection); len(again) != 0 {
		t.Fatal("unchanged skip emitted repeatedly")
	}
	// A selection from an older capture never joins a newer observation.
	s.CaptureSequence = 11
	selection = selectedZero(10, 2)
	events = l.events(s, true, command, "task", "run", 1, selection)
	if len(events) != 2 || events[0]["selection"] != nil || events[1]["selection_observation_join"] != "separate_event_no_current_capture_join" {
		t.Fatal("stale selection joined newer capture")
	}
	var out bytes.Buffer
	for _, event := range events {
		if _, e := writeObservation(&out, event, maxObservationLogBytes); e != nil {
			t.Fatal(e)
		}
	}
	if bytes.Contains(out.Bytes(), []byte("bridge_publication_receipt")) {
		t.Fatal("selection-only event invented receipt")
	}
}
func TestSelectionLogScopeCapsAndBoundedJSON(t *testing.T) {
	for _, kind := range []string{"task", "run", "epoch", "zero-capture"} {
		l := observationLogState{}
		s := selectedZero(1, 0)
		switch kind {
		case "task":
			s.TaskID = "other"
		case "run":
			s.RunID = "other"
		case "epoch":
			s.ExecutorEpoch++
		case "zero-capture":
			s.CaptureSequence = 0
		}
		if events := l.events(go2executor.State{}, false, nil, "task", "run", 1, s); len(events) != 0 {
			t.Fatalf("accepted wrong %s", kind)
		}
	}
	l := observationLogState{}
	limits := 0
	total := 0
	for i := uint64(1); i <= maxCommandEvents+5; i++ {
		events := l.events(go2executor.State{}, false, nil, "task", "run", 1, selectedZero(i, 0))
		for _, e := range events {
			if e["event"] == "executor_diagnostic_limit" {
				limits++
				if e["stream"] != "selections" || e["task_id"] != "task" || e["run_id"] != "run" || e["executor_epoch"] != uint64(1) {
					t.Fatal("limit lost scope")
				}
			}
			b, err := json.Marshal(e)
			if err != nil || len(b) > maxObservationJSONBytes {
				t.Fatal("selection event exceeds bound")
			}
			total += len(b)
		}
	}
	if l.selections != maxCommandEvents || limits != 1 || total >= maxObservationLogBytes {
		t.Fatal("selection stream lacks bounded retention")
	}
}
