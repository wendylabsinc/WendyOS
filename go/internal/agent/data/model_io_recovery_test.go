package data

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInterruptedEpisodeReconcilesModelIOFromDisk is the honesty guard on the
// interrupted-recovery path.
//
// The model input/outcome counters are folded in memory and written only when
// the episode is sealed, so an interrupted episode reaches recovery with all of
// them at zero while its ledger and outcome log are intact on disk. Publishing
// those zeros would be a manifest that states samples_delivered=0 beside a
// populated model_inputs.jsonl, and a consumer has no reason to disbelieve a
// number. Recovery must therefore recompute the counters, not annotate them.
func TestInterruptedEpisodeReconcilesModelIOFromDisk(t *testing.T) {
	root := t.TempDir()
	manager, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := manager.Start(StartOptions{Name: "interrupted"})
	if err != nil {
		t.Fatal(err)
	}
	const delivered = 7
	for i := 1; i <= delivered; i++ {
		input := ModelInput{AppID: "sh.wendy.model", Model: "detector", SourceID: "applications", SampleID: uint64(i), PayloadBytes: 32}
		if i == 4 {
			// One subscriber drop, so the reconciliation is checked on a field
			// that is not simply the line count.
			input.DroppedBefore = 2
		}
		if err := manager.RecordModelInput(input); err != nil {
			t.Fatal(err)
		}
	}
	// A source the episode does not capture must land in the uncaptured list
	// after recovery exactly as it would at seal.
	if err := manager.RecordModelInput(ModelInput{AppID: "sh.wendy.model", SourceID: "v4l2:/dev/video9", SampleID: 11, PayloadBytes: 8}); err != nil {
		t.Fatal(err)
	}
	// Two predictions: one naming a delivered sample, one naming a sample
	// outside the delivered range.
	for _, refs := range [][]SampleRef{
		{{SourceID: "applications", SampleID: 3}},
		{{SourceID: "applications", SampleID: 9999}},
	} {
		if _, err := manager.RecordApplication("sh.wendy.model", ApplicationRecord{
			Version: 1, Type: "prediction", Model: "detector", Value: 1, Inputs: refs,
		}); err != nil {
			t.Fatal(err)
		}
	}

	partial := filepath.Join(root, manifest.ID+".partial")
	if _, err := os.Stat(filepath.Join(partial, ModelInputLedgerFile)); err != nil {
		t.Fatalf("the ledger was not written: %v", err)
	}

	// Abandon the manager without sealing, exactly as an agent crash does, and
	// let a fresh manager over the same root recover the .partial directory.
	if _, err := NewManager(root); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(root, manifest.ID, "manifest.json"))
	if err != nil {
		t.Fatalf("the interrupted episode was not recovered: %v", err)
	}
	var recovered Manifest
	if err := json.Unmarshal(raw, &recovered); err != nil {
		t.Fatal(err)
	}
	if recovered.State != "interrupted" {
		t.Fatalf("state = %q, want interrupted", recovered.State)
	}
	if recovered.ModelIO.InputLedger != ModelInputLedgerFile {
		t.Errorf("input_ledger = %q, want %q", recovered.ModelIO.InputLedger, ModelInputLedgerFile)
	}
	if got, want := recovered.ModelIO.SamplesDelivered, uint64(delivered+1); got != want {
		t.Errorf("samples_delivered = %d, want %d (the ledger holds that many lines)", got, want)
	}
	var applications *SourceModelInputs
	for i := range recovered.Sources {
		if recovered.Sources[i].Source.ID == "applications" {
			applications = recovered.Sources[i].ModelInputs
		}
	}
	if applications == nil {
		t.Fatal("the captured source that fed the model has no model_inputs entry after recovery")
	}
	if applications.Delivered != delivered {
		t.Errorf("applications delivered_to_models = %d, want %d", applications.Delivered, delivered)
	}
	if applications.SubscriberDrops != 2 {
		t.Errorf("applications subscriber_drops = %d, want 2", applications.SubscriberDrops)
	}
	if applications.FirstSampleID != 1 || applications.LastSampleID != delivered {
		t.Errorf("applications sample range = [%d,%d], want [1,%d]", applications.FirstSampleID, applications.LastSampleID, delivered)
	}
	if len(recovered.ModelIO.Uncaptured) != 1 || recovered.ModelIO.Uncaptured[0].SourceID != "v4l2:/dev/video9" {
		t.Errorf("uncaptured_sources = %+v, want the one source the episode does not capture", recovered.ModelIO.Uncaptured)
	} else if recovered.ModelIO.Uncaptured[0].PayloadRetention != RetentionNotCaptured {
		t.Errorf("uncaptured payload_retention = %q, want %q", recovered.ModelIO.Uncaptured[0].PayloadRetention, RetentionNotCaptured)
	}
	if recovered.ModelIO.Predictions != 2 {
		t.Errorf("predictions = %d, want 2", recovered.ModelIO.Predictions)
	}
	if recovered.ModelIO.PredictionsWithInputs != 2 {
		t.Errorf("predictions_with_inputs = %d, want 2", recovered.ModelIO.PredictionsWithInputs)
	}
	if recovered.ModelIO.ReferencesOutsideDelivered != 1 {
		t.Errorf("input_references_outside_delivered_range = %d, want 1", recovered.ModelIO.ReferencesOutsideDelivered)
	}
	var reconciledNote bool
	for _, action := range recovered.RecoveryActions {
		if strings.Contains(action, "recomputed model input/outcome counters") {
			reconciledNote = true
		}
	}
	if !reconciledNote {
		t.Errorf("recovery_actions do not record the reconciliation: %v", recovered.RecoveryActions)
	}
}

// TestInterruptedEpisodeWithoutModelInputsStaysSilent keeps the absence signal
// intact: an episode no model consumed must not gain an input ledger reference
// or a zeroed accounting block just because it was interrupted.
func TestInterruptedEpisodeWithoutModelInputsStaysSilent(t *testing.T) {
	root := t.TempDir()
	manager, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := manager.Start(StartOptions{Name: "no-model"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewManager(root); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, manifest.ID, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var recovered Manifest
	if err := json.Unmarshal(raw, &recovered); err != nil {
		t.Fatal(err)
	}
	if recovered.ModelIO.InputLedger != "" {
		t.Errorf("input_ledger = %q, want empty for an episode no model consumed", recovered.ModelIO.InputLedger)
	}
	if recovered.ModelIO.SamplesDelivered != 0 {
		t.Errorf("samples_delivered = %d, want 0", recovered.ModelIO.SamplesDelivered)
	}
	for i := range recovered.Sources {
		if recovered.Sources[i].ModelInputs != nil {
			t.Errorf("source %s gained a model_inputs block without a model consuming it", recovered.Sources[i].Source.ID)
		}
	}
}
