package data

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// ModelInputLedgerFile is the episode-relative path of the model-input ledger.
// One JSON object per line, one line per sample handed to a model subscriber.
const ModelInputLedgerFile = "model_inputs.jsonl"

// maxSampleRefs bounds the sample references one record may carry. Multi-input
// models exist (stereo pairs, camera plus lidar), but a record naming hundreds
// of samples is a bug or an attempt to inflate the ledger.
const maxSampleRefs = 32

// ErrTooManySampleRefs marks a record whose input references exceed
// maxSampleRefs.
var ErrTooManySampleRefs = errors.New("too many input sample references")

// newModelIO is the manifest's reconstruction contract, written into every
// episode so a consumer never has to guess how inputs and outcomes join.
func newModelIO() ModelIO {
	return ModelIO{
		OutcomeLog: "events.jsonl",
		// app_id is part of the join, not decoration: sources are shared, so two
		// apps can be delivered the same (source_id, sample_id). Joining without
		// app_id would let one app's prediction be paired with another app's
		// input, which is exactly the mis-attribution a training set must not
		// inherit. The manifest's reference counters are a per-episode range
		// check and cannot catch that; the ledger's app_id can.
		JoinKeys:       []string{"app_id", "source_id", "sample_id"},
		PayloadLocator: "a ledger entry's payload bytes are the ones this episode's own capture wrote for the same (source_id, sample_id); for camera sources that is the cameras/<source>/index.jsonl entry with the matching sample_id, which names its segment file and byte offset. The index entry's byte_size is authoritative for how many bytes to read and may be smaller than the ledger entry's payload_bytes: a frame that opens a segment has any bytes before its first parameter set trimmed. A ledger entry with no matching index entry means the capture policy did not keep that sample's payload.",
	}
}

// RecordModelInput records one sample handed to a model subscriber into every
// active episode. The payload bytes are deliberately not copied: the episode's
// own capture already writes them (the subscriber and the capture consume the
// same producer), so the ledger stores the identity of the sample and the
// capture's index resolves it to bytes. Duplicating the bytes would double the
// storage cost of every episode and reintroduce the possibility of the two
// copies disagreeing.
//
// Callers record a sample only after it has actually been handed to the model,
// so the ledger never claims a delivery that did not happen.
func (m *Manager) RecordModelInput(input ModelInput) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var errs []error
	for _, a := range m.active {
		// The summary counters are folded into the in-memory manifest and
		// written when the episode is sealed, exactly like the per-source
		// capture counters: rewriting manifest.json once per sample would mean
		// a full serialize-and-rename at sensor rates. The ledger line itself
		// is on disk immediately, so a crashed episode still carries what the
		// model consumed (see recoverPartials).
		if err := a.recordModelInput(input); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// recordModelInput appends one ledger line and folds the sample into this
// episode's per-source model-input accounting.
func (a *activeEpisode) recordModelInput(input ModelInput) error {
	if err := a.openModelInputLedger(); err != nil {
		return err
	}
	input.EpisodeNanos = input.BootNanos - a.manifest.RequestBootNanos
	b, err := json.Marshal(input)
	if err != nil {
		return err
	}
	if _, err := a.modelInputs.Write(append(b, '\n')); err != nil {
		return err
	}
	stats := a.sourceModelInputs(input.SourceID)
	if stats.Delivered == 0 {
		stats.FirstSampleID = input.SampleID
	}
	stats.Delivered++
	stats.SubscriberDrops += input.DroppedBefore
	stats.LastSampleID = input.SampleID
	a.manifest.ModelIO.SamplesDelivered++
	a.manifest.ModelIO.InputLedger = ModelInputLedgerFile
	return nil
}

// openModelInputLedger creates the ledger on first use. Episodes that no model
// consumed keep no empty ledger, and the manifest's input_ledger field stays
// absent, so the presence of the file is itself the honest signal.
func (a *activeEpisode) openModelInputLedger() error {
	if a.modelInputs != nil {
		return nil
	}
	f, err := os.OpenFile(filepath.Join(a.dir, ModelInputLedgerFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return err
	}
	a.modelInputs = f
	return nil
}

// sourceModelInputs returns the mutable accounting record for one source,
// creating it on first delivery. A source the episode captures gets its record
// on its SourceStats entry (beside the capture's own counters); a source the
// episode does not capture gets one in the manifest's uncaptured list, so the
// episode never implies it holds payload bytes it does not have.
func (a *activeEpisode) sourceModelInputs(sourceID string) *SourceModelInputs {
	for i := range a.manifest.Sources {
		stats := &a.manifest.Sources[i]
		if stats.Source.ID != sourceID {
			continue
		}
		if stats.ModelInputs == nil {
			retention, note := capturedRetention(stats.Source.Capture)
			stats.ModelInputs = &SourceModelInputs{SourceID: sourceID, PayloadRetention: retention, Note: note}
		}
		return stats.ModelInputs
	}
	for i := range a.manifest.ModelIO.Uncaptured {
		if a.manifest.ModelIO.Uncaptured[i].SourceID == sourceID {
			return &a.manifest.ModelIO.Uncaptured[i]
		}
	}
	a.manifest.ModelIO.Uncaptured = append(a.manifest.ModelIO.Uncaptured, SourceModelInputs{
		SourceID:         sourceID,
		PayloadRetention: RetentionNotCaptured,
		Note:             "a model consumed this source during the episode, but the episode does not capture it: the ledger records which samples the model saw and this episode holds none of their payload bytes",
	})
	return &a.manifest.ModelIO.Uncaptured[len(a.manifest.ModelIO.Uncaptured)-1]
}

// capturedRetention classifies what an episode retains for a captured source
// whose samples a model consumed, using the capture policy's own vocabulary.
func capturedRetention(capture *SourceCapture) (string, string) {
	if capture == nil {
		return RetentionCapturePolicy, ""
	}
	if mode := capture.EffectiveMode(); mode == "snapshot" || mode == "fragment" || mode == "threshold" {
		return RetentionPolicySubset, "capture mode " + mode + " keeps less than the model consumed; ledger entries with no matching capture-index entry have no payload bytes in this episode"
	}
	if capture.Rate > 0 {
		return RetentionPolicySubset, "a capture rate cap keeps less than the model consumed; ledger entries with no matching capture-index entry have no payload bytes in this episode"
	}
	return RetentionCapturePolicy, ""
}

// noteApplicationRecord folds one recorded application record into the
// episode's model input/outcome accounting: how many predictions the episode
// holds, how many of them named their inputs, and how many references point
// outside what this episode delivered.
func (a *activeEpisode) noteApplicationRecord(record ApplicationRecord) {
	if record.Type != "prediction" {
		return
	}
	a.manifest.ModelIO.Predictions++
	if len(record.Inputs) == 0 {
		return
	}
	a.manifest.ModelIO.PredictionsWithInputs++
	for _, ref := range record.Inputs {
		if !a.deliveredSample(ref) {
			a.manifest.ModelIO.ReferencesOutsideDelivered++
		}
	}
}

// deliveredSample reports whether a reference falls inside the range of
// sample identifiers this episode handed to a model for that source. It is a
// range check, not set membership: keeping every delivered identifier would
// grow without bound, and a reference outside the range is the case that
// actually cannot be reconstructed.
func (a *activeEpisode) deliveredSample(ref SampleRef) bool {
	var stats *SourceModelInputs
	for i := range a.manifest.Sources {
		if a.manifest.Sources[i].Source.ID == ref.SourceID {
			stats = a.manifest.Sources[i].ModelInputs
			break
		}
	}
	if stats == nil {
		for i := range a.manifest.ModelIO.Uncaptured {
			if a.manifest.ModelIO.Uncaptured[i].SourceID == ref.SourceID {
				stats = &a.manifest.ModelIO.Uncaptured[i]
				break
			}
		}
	}
	if stats == nil || stats.Delivered == 0 {
		return false
	}
	return ref.SampleID >= stats.FirstSampleID && ref.SampleID <= stats.LastSampleID
}

// reconcileModelIO rebuilds an interrupted episode's model input/outcome
// accounting from the files that survived on disk.
//
// The counters are folded in memory and written only at seal, so an interrupted
// episode reaches recovery with every one of them at zero while its ledger holds
// thousands of lines. Leaving them there would publish a manifest that says
// samples_delivered=0 next to a populated model_inputs.jsonl, and a consumer has
// no reason to disbelieve a number. A note saying "the ledger is authoritative"
// does not repair a field that reads zero. Both files are already walked during
// recovery to truncate torn tails, so recomputing from them costs one more pass
// and makes the manifest true instead of merely annotated.
//
// A torn final line is skipped rather than guessed at: truncateJSONL has already
// removed it, and any residue that will not parse is not evidence of anything.
func reconcileModelIO(dir string, mf *Manifest) (bool, error) {
	inputs, err := reconcileModelInputs(dir, mf)
	if err != nil {
		return false, err
	}
	outcomes, err := reconcileOutcomes(dir, mf)
	if err != nil {
		return false, err
	}
	return inputs || outcomes, nil
}

// reconcileModelInputs recounts the model-input ledger into the manifest's
// per-source and episode-wide delivery counters.
func reconcileModelInputs(dir string, mf *Manifest) (bool, error) {
	f, err := os.Open(filepath.Join(dir, ModelInputLedgerFile))
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	defer f.Close()

	mf.ModelIO.InputLedger = ModelInputLedgerFile
	mf.ModelIO.SamplesDelivered = 0
	mf.ModelIO.Uncaptured = nil
	for i := range mf.Sources {
		mf.Sources[i].ModelInputs = nil
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLedgerLineBytes)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var input ModelInput
		if err := json.Unmarshal(line, &input); err != nil {
			continue
		}
		stats := manifestModelInputs(mf, input.SourceID)
		if stats.Delivered == 0 {
			stats.FirstSampleID = input.SampleID
		}
		stats.Delivered++
		stats.SubscriberDrops += input.DroppedBefore
		stats.LastSampleID = input.SampleID
		mf.ModelIO.SamplesDelivered++
	}
	if err := scanner.Err(); err != nil {
		return false, err
	}
	return true, nil
}

// reconcileOutcomes recounts the prediction records and their input references.
func reconcileOutcomes(dir string, mf *Manifest) (bool, error) {
	name := mf.ModelIO.OutcomeLog
	if name == "" {
		name = "events.jsonl"
	}
	f, err := os.Open(filepath.Join(dir, name))
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	defer f.Close()

	mf.ModelIO.Predictions = 0
	mf.ModelIO.PredictionsWithInputs = 0
	mf.ModelIO.ReferencesOutsideDelivered = 0

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLedgerLineBytes)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var record ApplicationRecord
		if err := json.Unmarshal(line, &record); err != nil {
			continue
		}
		if record.Type != "prediction" {
			continue
		}
		mf.ModelIO.Predictions++
		if len(record.Inputs) == 0 {
			continue
		}
		mf.ModelIO.PredictionsWithInputs++
		for _, ref := range record.Inputs {
			if !manifestDeliveredSample(mf, ref) {
				mf.ModelIO.ReferencesOutsideDelivered++
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return false, err
	}
	return true, nil
}

// maxLedgerLineBytes bounds one JSONL line during recovery. A line longer than
// this is not something this package wrote, so refusing to buffer it is the
// correct response rather than a limitation.
const maxLedgerLineBytes = 1 << 20

// manifestModelInputs is sourceModelInputs against a manifest being recovered
// rather than a live episode: the same placement rule (beside the capture's own
// counters for a captured source, in the uncaptured list otherwise) so a
// recovered manifest is shaped exactly like a sealed one.
func manifestModelInputs(mf *Manifest, sourceID string) *SourceModelInputs {
	for i := range mf.Sources {
		stats := &mf.Sources[i]
		if stats.Source.ID != sourceID {
			continue
		}
		if stats.ModelInputs == nil {
			retention, note := capturedRetention(stats.Source.Capture)
			stats.ModelInputs = &SourceModelInputs{SourceID: sourceID, PayloadRetention: retention, Note: note}
		}
		return stats.ModelInputs
	}
	for i := range mf.ModelIO.Uncaptured {
		if mf.ModelIO.Uncaptured[i].SourceID == sourceID {
			return &mf.ModelIO.Uncaptured[i]
		}
	}
	mf.ModelIO.Uncaptured = append(mf.ModelIO.Uncaptured, SourceModelInputs{
		SourceID:         sourceID,
		PayloadRetention: RetentionNotCaptured,
		Note:             "a model consumed this source during the episode, but the episode does not capture it: the ledger records which samples the model saw and this episode holds none of their payload bytes",
	})
	return &mf.ModelIO.Uncaptured[len(mf.ModelIO.Uncaptured)-1]
}

// manifestDeliveredSample is deliveredSample against a recovered manifest.
func manifestDeliveredSample(mf *Manifest, ref SampleRef) bool {
	var stats *SourceModelInputs
	for i := range mf.Sources {
		if mf.Sources[i].Source.ID == ref.SourceID {
			stats = mf.Sources[i].ModelInputs
			break
		}
	}
	if stats == nil {
		for i := range mf.ModelIO.Uncaptured {
			if mf.ModelIO.Uncaptured[i].SourceID == ref.SourceID {
				stats = &mf.ModelIO.Uncaptured[i]
				break
			}
		}
	}
	if stats == nil || stats.Delivered == 0 {
		return false
	}
	return ref.SampleID >= stats.FirstSampleID && ref.SampleID <= stats.LastSampleID
}

// ValidateSampleRefs checks the input references on an application record.
func ValidateSampleRefs(refs []SampleRef) error {
	if len(refs) > maxSampleRefs {
		return ErrTooManySampleRefs
	}
	for _, ref := range refs {
		if ref.SourceID == "" {
			return errors.New("input reference is missing source_id")
		}
	}
	return nil
}
