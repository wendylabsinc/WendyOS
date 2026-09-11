package data

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/timesync"
)

// TestClockAgreementWithUnboundedSystemClock is the guard on a status that
// lied. A device whose system clock is not synchronized reports an unbounded
// observation, whose offset interval is [0, 0] because there is no interval to
// report. Comparing that against a real Roughtime consensus put every episode
// such a device recorded at system_clock_status "conflict", which reads as
// evidence that the two clocks disagree when in fact only one of them spoke.
func TestClockAgreementWithUnboundedSystemClock(t *testing.T) {
	bounded := UTCObservation{Confidence: "system_reported", OffsetLowerNanos: 900, OffsetUpperNanos: 1100}
	unbounded := UTCObservation{Confidence: "unbounded"}
	consensus := timesync.Consensus{Confidence: "bounded", LowerOffsetNanos: 1000, UpperOffsetNanos: 2000}
	disjoint := timesync.Consensus{Confidence: "bounded", LowerOffsetNanos: 5000, UpperOffsetNanos: 6000}
	noConsensus := timesync.Consensus{Confidence: "unbounded"}

	for _, tc := range []struct {
		name      string
		system    UTCObservation
		consensus timesync.Consensus
		want      string
	}{
		{"bounded system, overlapping consensus", bounded, consensus, ClockStatusAgreement},
		{"bounded system, disjoint consensus", bounded, disjoint, ClockStatusConflict},
		{"unbounded system, consensus present", unbounded, consensus, ClockStatusRoughtimeOnly},
		{"unbounded system, no consensus", unbounded, noConsensus, ClockStatusSystemReported},
		// The fourth combination's mirror: a bounded system clock with no
		// consensus to judge it against is still just the system's own claim.
		{"bounded system, no consensus", bounded, noConsensus, ClockStatusSystemReported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := clockAgreement(tc.system, tc.consensus); got != tc.want {
				t.Fatalf("clockAgreement = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRecoveryKeepsPersistedPayloadRetention pins the honesty of the recovered
// model-input accounting. SourceCapture is excluded from the manifest JSON, so
// a manifest read back off disk carries no capture policy; rebuilding the
// retention class from that nil policy relabelled a snapshot or rate-capped
// source as "captured_subject_to_drop_accounting", telling a training pipeline
// the episode holds every payload the model saw when it holds a fraction.
func TestRecoveryKeepsPersistedPayloadRetention(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "20260101T000000Z-abcdef0123456789.partial")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	const note = "capture mode snapshot keeps less than the model consumed; ledger entries with no matching capture-index entry have no payload bytes in this episode"
	// A live mid-episode manifest: the ModelInputs block was written with the
	// correct class while the capture policy was still in memory.
	manifest := Manifest{
		Version: ManifestVersion, ID: "20260101T000000Z-abcdef0123456789", State: "recording",
		BootID: bootID(), CanonicalClock: "CLOCK_BOOTTIME", ModelIO: newModelIO(),
		Sources: []SourceStats{{
			Source: Source{ID: "v4l2:/dev/video0", Kind: "camera", ClockDomain: "CLOCK_BOOTTIME", Healthy: true},
			ModelInputs: &SourceModelInputs{
				SourceID: "v4l2:/dev/video0", Delivered: 3, FirstSampleID: 1, LastSampleID: 3,
				PayloadRetention: RetentionPolicySubset, Note: note,
			},
		}},
	}
	manifest.ModelIO.InputLedger = ModelInputLedgerFile
	if err := writeManifest(dir, manifest); err != nil {
		t.Fatal(err)
	}
	var ledger strings.Builder
	for i := 1; i <= 5; i++ {
		b, err := json.Marshal(ModelInput{AppID: "sh.wendy.model", SourceID: "v4l2:/dev/video0", SampleID: uint64(i), PayloadBytes: 64})
		if err != nil {
			t.Fatal(err)
		}
		ledger.Write(b)
		ledger.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(dir, ModelInputLedgerFile), []byte(ledger.String()), 0o640); err != nil {
		t.Fatal(err)
	}

	if _, err := NewManager(root); err != nil {
		t.Fatal(err)
	}
	recovered := readRecoveredManifest(t, root, manifest.ID)
	if recovered.State != "interrupted" {
		t.Fatalf("state = %q, want interrupted", recovered.State)
	}
	if len(recovered.Sources) != 1 || recovered.Sources[0].ModelInputs == nil {
		t.Fatalf("model-input accounting missing after recovery: %+v", recovered.Sources)
	}
	stats := recovered.Sources[0].ModelInputs
	if stats.PayloadRetention != RetentionPolicySubset {
		t.Fatalf("payload_retention = %q, want %q: the persisted class was rebuilt from a capture policy the manifest cannot carry",
			stats.PayloadRetention, RetentionPolicySubset)
	}
	if stats.Note != note {
		t.Fatalf("note = %q, want the persisted one", stats.Note)
	}
	// The counters are still recomputed from the ledger, which is the whole
	// reason reconciliation runs at all.
	if stats.Delivered != 5 || recovered.ModelIO.SamplesDelivered != 5 {
		t.Fatalf("delivered = %d / %d, want 5 from the five ledger lines", stats.Delivered, recovered.ModelIO.SamplesDelivered)
	}
}

// TestRecoveryLeavesCompleteManifestAlone covers the crash that lands between
// writing the sealed manifest and renaming the directory. The episode is
// complete: every file was checksummed and listed. Relabelling it interrupted
// and blaming a reboot invents a failure that never happened.
func TestRecoveryLeavesCompleteManifestAlone(t *testing.T) {
	root := t.TempDir()
	manager, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	started, err := manager.Start(StartOptions{Name: "sealed", Sources: []string{"applications"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Stop(AdHocEpisodeKey); err != nil {
		t.Fatal(err)
	}
	// Undo just the rename, which is exactly the state a crash between the
	// manifest write and the rename leaves behind.
	final := filepath.Join(root, started.ID)
	partial := final + ".partial"
	if err := os.Rename(final, partial); err != nil {
		t.Fatal(err)
	}

	if _, err := NewManager(root); err != nil {
		t.Fatal(err)
	}
	recovered := readRecoveredManifest(t, root, started.ID)
	if recovered.State != "complete" {
		t.Fatalf("state = %q, want complete: a fully sealed episode was relabelled by recovery", recovered.State)
	}
	if recovered.Interruption != "" {
		t.Fatalf("interruption = %q, want none", recovered.Interruption)
	}
	if len(recovered.RecoveryActions) != 0 {
		t.Fatalf("recovery_actions = %v, want none: nothing was repaired", recovered.RecoveryActions)
	}
}

// TestRecoveryQuarantinesUnreadableManifest covers a partial directory whose
// manifest cannot be parsed. Skipping it silently left it on disk forever,
// invisible to the quota and retried on every agent start.
func TestRecoveryQuarantinesUnreadableManifest(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "20260101T000000Z-0badc0de0badc0de.partial")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte("{not json"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte("{}\n"), 0o640); err != nil {
		t.Fatal(err)
	}

	manager, err := NewManager(root)
	if err != nil {
		t.Fatalf("one damaged partial stopped the agent from starting: %v", err)
	}
	quarantined := filepath.Join(root, "20260101T000000Z-0badc0de0badc0de"+unrecoverableSuffix)
	if _, err := os.Stat(quarantined); err != nil {
		t.Fatalf("the unreadable partial was not quarantined: %v", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the .partial directory is still there and will be retried on every start")
	}
	// The warning survives NewManager, where no logger can be attached yet.
	var warnings []string
	manager.SetWarnLogger(func(message string) { warnings = append(warnings, message) })
	if !containsSubstring(warnings, "could not be recovered") {
		t.Fatalf("recovery said nothing about quarantining the episode: %v", warnings)
	}
}

// TestRecoveryDeletesPartialWithNoManifest covers the directory a crash
// between Mkdir and the first manifest write leaves behind. It holds no
// episode, so there is nothing to quarantine and nothing to salvage.
func TestRecoveryDeletesPartialWithNoManifest(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "20260101T000000Z-1111111111111111.partial")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := NewManager(root); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("the manifest-less directory survived recovery: %v", entries)
	}
}

// TestQuotaCountsPartialAndQuarantinedBytes pins that bytes the store cannot
// evict are still bytes. Skipping .partial directories entirely meant an
// episode mid-recording, and a quarantined one that can never be read, were
// both invisible to the quota, so the store could exceed it without anything
// noticing.
func TestQuotaCountsPartialAndQuarantinedBytes(t *testing.T) {
	root := t.TempDir()
	partial := filepath.Join(root, "20260101T000000Z-2222222222222222.partial")
	quarantined := filepath.Join(root, "20260101T000000Z-3333333333333333"+unrecoverableSuffix)
	for _, dir := range []string{partial, quarantined} {
		if err := os.Mkdir(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "payload.bin"), make([]byte, 4096), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	scan, err := scanEpisodeStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if scan.used < 8192 {
		t.Fatalf("used = %d, want at least the 8192 bytes in the two directories", scan.used)
	}
	if scan.partialBytes < 4096 {
		t.Fatalf("partialBytes = %d, want at least the 4096 bytes of the recording episode", scan.partialBytes)
	}
	// The recording episode must never be an eviction candidate; the
	// quarantined one must be, or one damaged episode wedges the quota.
	var sawQuarantined bool
	for _, c := range scan.candidates {
		if c.path == partial {
			t.Fatal("an episode that is still recording was offered as an eviction candidate")
		}
		if c.path == quarantined {
			sawQuarantined = true
			if c.tier != 0 || !c.quarantined {
				t.Fatalf("quarantined candidate has tier %d, quarantined=%v; want tier 0 and true", c.tier, c.quarantined)
			}
		}
	}
	if !sawQuarantined {
		t.Fatal("the quarantined directory can never be evicted, so its bytes leak forever")
	}
}

// TestPreRollFlushSyncsOnce pins the cost of starting an episode. The flush
// used to reopen events.jsonl and fsync it once per buffered record, so a full
// pre-roll ring forced hundreds of separate disk barriers onto the start path
// with the manager lock held, before the first frame was captured.
func TestPreRollFlushSyncsOnce(t *testing.T) {
	var syncs atomic.Int64
	original := syncFile
	syncFile = func(f *os.File) error {
		syncs.Add(1)
		return f.Sync()
	}
	t.Cleanup(func() { syncFile = original })

	manager, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now, err := readBootTime()
	if err != nil {
		t.Fatal(err)
	}
	const records = 25
	for i := 0; i < records; i++ {
		if _, err := manager.RecordApplication("com.example.app", ApplicationRecord{
			Version: 1, Type: "event", Name: "ready", ClientBootNanos: now, ClientBootID: bootID(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	syncs.Store(0)
	started, err := manager.Start(StartOptions{Sources: []string{"applications"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := syncs.Load(); got != 1 {
		t.Fatalf("starting an episode with %d pre-roll records cost %d fsyncs, want 1", records, got)
	}
	// All of them still reached the episode, durably.
	b, err := os.ReadFile(filepath.Join(manager.root, started.ID+".partial", "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(strings.TrimSpace(string(b)), "\n") + 1; lines != records {
		t.Fatalf("events.jsonl holds %d lines, want %d", lines, records)
	}
}

// TestStartCallsSourceProviderWithoutTheLock is the deadlock guard. Sources()
// has always called the provider outside m.mu because a provider is entitled
// to call back into the manager; Start called it while holding the lock, so
// any provider that logged a warning or asked for the active session hung the
// agent for good. It also fixes a provider that shells out under the lock.
func TestStartCallsSourceProviderWithoutTheLock(t *testing.T) {
	manager, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager.SetWarnLogger(func(string) {})
	var deadline bool
	manager.SetSourceProvider(func(ctx context.Context) []Source {
		// Every one of these takes m.mu. Under the old code the first would
		// deadlock and this test would time out rather than fail.
		manager.Warnf("provider is discovering sources")
		manager.Status()
		manager.ActiveSession(AdHocEpisodeKey)
		if _, ok := ctx.Deadline(); ok {
			deadline = true
		}
		return []Source{{ID: "probe:0", Kind: "camera", ClockDomain: "CLOCK_BOOTTIME", Healthy: true}}
	})

	done := make(chan error, 1)
	go func() {
		_, startErr := manager.Start(StartOptions{Sources: []string{"probe:0"}})
		done <- startErr
	}()
	select {
	case startErr := <-done:
		if startErr != nil {
			t.Fatalf("Start: %v", startErr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Start deadlocked: the source provider was called with the manager lock held")
	}
	if !deadline {
		t.Fatal("the source provider was called with an unbounded context; an adapter that hangs stalls the episode forever")
	}
}

// TestRecoveryToleratesOversizedOutcomeLine covers a single application record
// longer than the recovery line limit. It used to abort reconciliation, which
// aborted NewManager, which stopped the agent from recording anything at all
// until somebody deleted the directory by hand.
func TestRecoveryToleratesOversizedOutcomeLine(t *testing.T) {
	root := t.TempDir()
	manager, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	started, err := manager.Start(StartOptions{Name: "oversized", Sources: []string{"applications"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.RecordApplication("sh.wendy.model", ApplicationRecord{
		Version: 1, Type: "prediction", Model: "detector", Value: 1,
		Inputs: []SampleRef{{SourceID: "applications", SampleID: 1}},
	}); err != nil {
		t.Fatal(err)
	}
	partial := filepath.Join(root, started.ID+".partial")
	events, err := os.OpenFile(filepath.Join(partial, "events.jsonl"), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	oversized, err := json.Marshal(storedApplicationRecord{ApplicationRecord: ApplicationRecord{
		Version: 1, Type: "prediction", Model: "detector",
		Attributes: map[string]any{"blob": strings.Repeat("x", maxLedgerLineBytes+1)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := events.Write(append(oversized, '\n')); err != nil {
		t.Fatal(err)
	}
	if err := events.Close(); err != nil {
		t.Fatal(err)
	}

	recoverer, err := NewManager(root)
	if err != nil {
		t.Fatalf("one oversized record stopped the agent from starting: %v", err)
	}
	recovered := readRecoveredManifest(t, root, started.ID)
	if recovered.State != "interrupted" {
		t.Fatalf("state = %q, want interrupted", recovered.State)
	}
	if len(recovered.ModelIO.RecoveryNotes) == 0 {
		t.Fatal("the manifest says nothing about the line recovery could not read, so its counters are silently low")
	}
	if !containsSubstring(recovered.ModelIO.RecoveryNotes, "longer than the") {
		t.Fatalf("recovery notes do not name the over-long line: %v", recovered.ModelIO.RecoveryNotes)
	}
	var warnings []string
	recoverer.SetWarnLogger(func(message string) { warnings = append(warnings, message) })
	if !containsSubstring(warnings, "longer than the") {
		t.Fatalf("the operator was told nothing about the unreadable line: %v", warnings)
	}
}

// TestRecoveryQuarantinesEpisodeWithSymlink covers the other per-partial
// failure that used to fail NewManager outright: sealFiles refuses to
// checksum a symlink, correctly, but the whole agent paid for it.
func TestRecoveryQuarantinesEpisodeWithSymlink(t *testing.T) {
	root := t.TempDir()
	manager, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	started, err := manager.Start(StartOptions{Name: "symlinked", Sources: []string{"applications"}})
	if err != nil {
		t.Fatal(err)
	}
	partial := filepath.Join(root, started.ID+".partial")
	if err := os.Symlink("/etc/passwd", filepath.Join(partial, "sneaky")); err != nil {
		t.Skipf("this filesystem does not support symlinks: %v", err)
	}

	if _, err := NewManager(root); err != nil {
		t.Fatalf("one episode holding a symlink stopped the agent from starting: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, started.ID+unrecoverableSuffix)); err != nil {
		t.Fatalf("the unsealable episode was not quarantined: %v", err)
	}
}

// TestTruncateJSONLScansBackwards covers the bounded backwards scan. The
// previous implementation read the whole file and then copied it into a
// string, so recovering a large log allocated twice its size on a device that
// has a few hundred megabytes of memory.
func TestTruncateJSONLScansBackwards(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name    string
		content string
		want    string
	}{
		{"torn tail", "{\"a\":1}\n{\"b\":2}\n{\"c\":", "{\"a\":1}\n{\"b\":2}\n"},
		{"clean tail", "{\"a\":1}\n", "{\"a\":1}\n"},
		{"no newline at all", "{\"a\":", ""},
		{"empty", "", ""},
		// Longer than one chunk with the last newline in an earlier chunk,
		// which is the case the backwards scan has to loop for.
		{"tail longer than a chunk", "{\"a\":1}\n" + strings.Repeat("x", truncateJSONLChunk+512), "{\"a\":1}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(dir, strings.ReplaceAll(tc.name, " ", "_")+".jsonl")
			if err := os.WriteFile(p, []byte(tc.content), 0o640); err != nil {
				t.Fatal(err)
			}
			truncateJSONL(p)
			got, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Fatalf("truncateJSONL left %q, want %q", got, tc.want)
			}
		})
	}
}

// TestCampaignRejectsBlankSelector covers the selector that validated as
// absent and resolved as present. Validation trimmed before testing while
// resolution did not, so a blank camera selector beside a real audio one
// passed validation, then matched every camera on the device by empty
// substring and dropped the microphone entirely.
func TestCampaignRejectsBlankSelector(t *testing.T) {
	plan := []byte(`version: 1
name: blank-selector
sources:
  - camera: "   "
  - audio: default
capture:
  buffer: 0s
  after_trigger: 5s
  triggers:
    - event: go
upload: {when: manual}
export: {annotation: cvat}
`)
	_, err := ParseCampaign(plan)
	if err == nil {
		t.Fatal("a whitespace-only camera selector was accepted")
	}
	if !strings.Contains(err.Error(), "sources[0].camera") {
		t.Fatalf("error does not name the blank selector: %v", err)
	}
}

// TestCampaignRevisionIgnoresUnrelatedStructFields is the guard against a
// fleet-wide phantom plan change. The digest used to be taken over a
// marshalled Campaign struct, so an agent release that added any field to that
// struct changed the revision of every already-deployed campaign on every
// device that took the upgrade. The digest now covers an enumerated list of
// author-declared plan fields, so a field that is not on the list, at its zero
// value or not, cannot move it.
func TestCampaignRevisionIgnoresUnrelatedStructFields(t *testing.T) {
	campaign, err := ParseCampaign([]byte(drainDigestPinYAML))
	if err != nil {
		t.Fatal(err)
	}
	before := campaign.Revision

	// Stand in for the next release's new struct field by setting fields that
	// are not part of the author-declared plan. Under the struct-marshalling
	// digest each of these moved the revision.
	mutated := campaign
	mutated.State = "disarmed"
	mutated.DeployedUnixNanos = time.Now().UnixNano()
	mutated.Warnings = []string{"a warning the next release adds"}
	mutated.Notify = nil
	digest, err := json.Marshal(mutated.planDigestInput())
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := json.Marshal(campaign.planDigestInput())
	if err != nil {
		t.Fatal(err)
	}
	if string(digest) != string(baseline) {
		t.Fatalf("deploy-time state reached the revision digest:\n got %s\nwant %s", digest, baseline)
	}

	// Reparsing the same text must give the same revision, which is the
	// property operators rely on to tell a redeploy from an edit.
	again, err := ParseCampaign([]byte(drainDigestPinYAML))
	if err != nil {
		t.Fatal(err)
	}
	if again.Revision != before {
		t.Fatalf("the same plan hashed to %s and then %s", before, again.Revision)
	}

	// A real plan change still moves it.
	edited, err := ParseCampaign([]byte(strings.Replace(drainDigestPinYAML, "buffer: 1s", "buffer: 2s", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if edited.Revision == before {
		t.Fatal("editing capture.buffer did not change the revision")
	}
}

// TestCampaignRejectsUnparseableSecondDocument covers the second YAML document
// that failed to parse. Testing the trailing decode for a nil error treated
// "this does not parse" as "there is nothing here", so a plan with a malformed
// second document deployed with that document silently dropped.
func TestCampaignRejectsUnparseableSecondDocument(t *testing.T) {
	plan := []byte(`version: 1
name: two-docs
sources:
  - telemetry: true
capture:
  buffer: 0s
  after_trigger: 5s
  triggers:
    - event: go
upload: {when: manual}
export: {annotation: cvat}
---
: : {{{
`)
	_, err := ParseCampaign(plan)
	if err == nil {
		t.Fatal("a second, unparseable YAML document was silently dropped")
	}
	if !strings.Contains(err.Error(), "exactly one document") {
		t.Fatalf("error does not explain the rejection: %v", err)
	}
}

// TestEpisodeDirRejectsDotSegments covers "." and "..", which survive safeName
// untouched and, joined onto the store root, name the store itself and its
// parent directory.
func TestEpisodeDirRejectsDotSegments(t *testing.T) {
	manager, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{".", ".."} {
		if _, err := manager.episodeDir(id); !errors.Is(err, ErrInvalidEpisodeID) {
			t.Fatalf("episodeDir(%q) = %v, want ErrInvalidEpisodeID", id, err)
		}
		if _, _, err := manager.Inspect(id, false); !errors.Is(err, ErrInvalidEpisodeID) {
			t.Fatalf("Inspect(%q) = %v, want ErrInvalidEpisodeID", id, err)
		}
	}
}

// TestConcurrentDeployOfTheSameCampaign covers two deploys racing on what used
// to be one fixed temporary filename. Both wrote into it and the file renamed
// into place held one plan's bytes overlaid with the other's, which is still
// valid JSON and therefore undetectable downstream.
func TestConcurrentDeployOfTheSameCampaign(t *testing.T) {
	manager, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	plan := func(annotation string, triggers int) []byte {
		var b strings.Builder
		b.WriteString("version: 1\nname: racer\nsources:\n  - telemetry: true\ncapture:\n  buffer: 0s\n  after_trigger: 5s\n  triggers:\n")
		for i := 0; i < triggers; i++ {
			b.WriteString("    - event: e" + strings.Repeat("0", i) + strings.Repeat("long", 200) + "\n")
		}
		b.WriteString("upload: {when: manual}\nexport: {annotation: " + annotation + "}\n")
		return []byte(b.String())
	}
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 8; i++ {
		annotation, triggers := "cvat", 1
		if i%2 == 1 {
			annotation, triggers = "labelstudio", 6
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := manager.DeployCampaign(plan(annotation, triggers)); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent deploy failed: %v", err)
	}
	// Whichever deploy won, the file on disk must be exactly one of the two
	// plans and not a blend of both.
	stored, err := manager.Campaign("racer")
	if err != nil {
		t.Fatalf("the stored plan is unreadable, so the deploys interleaved: %v", err)
	}
	switch {
	case stored.Export.Annotation == "cvat" && len(stored.Capture.Triggers) == 1:
	case stored.Export.Annotation == "labelstudio" && len(stored.Capture.Triggers) == 6:
	default:
		t.Fatalf("the stored plan is a blend of two deploys: annotation %q with %d triggers",
			stored.Export.Annotation, len(stored.Capture.Triggers))
	}
	if _, err := os.Stat(filepath.Join(manager.campaignDir(), "racer.json.tmp")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a fixed temporary filename is still in use")
	}
}

// TestRoughtimeEvidenceGoesToASidecar pins that the manifest stops growing
// with the episode. Raw Roughtime evidence (a nonce and the signed response
// bytes per server) used to be appended to the manifest on every consensus
// round and rewritten in full each time, so a long episode ended with a
// manifest dominated by clock evidence.
func TestRoughtimeEvidenceGoesToASidecar(t *testing.T) {
	root := t.TempDir()
	manager, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	// Every round renders to the same number of bytes, so any growth in the
	// manifest is structural (a round that was appended rather than replaced)
	// rather than a wider integer.
	manager.SetConsensusProvider(func(context.Context) (timesync.Consensus, error) {
		return timesync.Consensus{
			Confidence: "bounded", LowerOffsetNanos: 1_000_000, UpperOffsetNanos: 1_000_500,
			Quorum: 3, ObservedUnixNanos: 1_700_000_000_000_000_000,
			Evidence: []timesync.RoughtimeEvidence{{
				Server: "example", Address: "roughtime.example:2002", Included: true,
				Nonce: make([]byte, 32), RawResponse: make([]byte, 8192),
			}},
		}, nil
	})
	started, err := manager.Start(StartOptions{Name: "clocked", Sources: []string{"applications"}, RequireUTCUncertainty: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, started.ID+".partial")
	manifestSize := func() int64 {
		info, statErr := os.Stat(filepath.Join(dir, "manifest.json"))
		if statErr != nil {
			t.Fatal(statErr)
		}
		return info.Size()
	}
	// Drive further rounds the way the in-episode clock sampler does. The
	// manifest keeps the opening round and the latest one, so it reaches its
	// steady size at the second round and must not move after that however
	// many more land.
	manager.mu.Lock()
	episode := manager.active[AdHocEpisodeKey]
	manager.mu.Unlock()
	if episode == nil {
		t.Fatal("the episode is not active")
	}
	manager.queryAndAttachConsensus(context.Background(), episode, false)
	steady := manifestSize()
	const extraRounds = 12
	for i := 0; i < extraRounds; i++ {
		manager.queryAndAttachConsensus(context.Background(), episode, false)
	}
	// The only thing in the manifest that may still move with the round count
	// is the decimal width of roughtime_rounds itself. Anything beyond that is
	// evidence accumulating in the manifest again; one round of the evidence
	// used to cost over eight kilobytes.
	const counterDigits = 16
	if got := manifestSize(); got-steady > counterDigits {
		t.Fatalf("the manifest grew from %d to %d bytes over %d further consensus rounds", steady, got, extraRounds)
	}

	sidecar, err := os.ReadFile(filepath.Join(dir, RoughtimeEvidenceFile))
	if err != nil {
		t.Fatalf("the evidence sidecar is missing, so the rounds cannot be reverified: %v", err)
	}
	rows := strings.Count(strings.TrimSpace(string(sidecar)), "\n") + 1
	if rows != extraRounds+2 {
		t.Fatalf("the sidecar holds %d rows, want %d", rows, extraRounds+2)
	}

	// The manifest keeps the bounds and drops the bytes; the sidecar keeps
	// both, and the sealed episode lists and checksums it.
	stopped, err := manager.Stop(AdHocEpisodeKey)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.RoughtimeEvidenceLog != RoughtimeEvidenceFile {
		t.Fatalf("roughtime_evidence_log = %q, want %q", stopped.RoughtimeEvidenceLog, RoughtimeEvidenceFile)
	}
	if stopped.RoughtimeRounds != uint64(extraRounds+3) {
		t.Fatalf("roughtime_rounds = %d, want %d (the seal adds one more)", stopped.RoughtimeRounds, extraRounds+3)
	}
	if len(stopped.Roughtime) > 2 {
		t.Fatalf("the manifest kept %d consensus rounds, want at most the opening one and the latest", len(stopped.Roughtime))
	}
	for _, c := range stopped.Roughtime {
		if len(c.Evidence) != 0 {
			t.Fatal("raw Roughtime evidence is still in the manifest")
		}
		if c.LowerOffsetNanos == 0 || c.UpperOffsetNanos == 0 {
			t.Fatalf("the manifest kept no usable bounds: %+v", c)
		}
	}
	var listed bool
	for _, f := range stopped.Files {
		if f.Path == RoughtimeEvidenceFile {
			listed = true
			if f.SHA256 == "" || f.Size == 0 {
				t.Fatalf("the sidecar was listed without a checksum: %+v", f)
			}
		}
	}
	if !listed {
		t.Fatal("the sidecar is not in the sealed file list, so it is neither checksummed nor uploaded")
	}
	if _, failures, err := manager.Inspect(stopped.ID, true); err != nil || len(failures) != 0 {
		t.Fatalf("verifying the sealed episode: %v %v", err, failures)
	}
}

// TestTelemetryWriteFailureIsVisible covers the 1 Hz sampler swallowing a
// write error. Rows vanished silently on a full disk, and the manifest went on
// saying the telemetry source had drop accounting "unavailable", so an episode
// missing an hour of telemetry looked exactly like a complete one.
func TestTelemetryWriteFailureIsVisible(t *testing.T) {
	root := t.TempDir()
	manager, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	warnings := make(chan string, 8)
	manager.SetWarnLogger(func(message string) {
		select {
		case warnings <- message:
		default:
		}
	})
	started, err := manager.Start(StartOptions{Name: "telemetry", Sources: []string{"telemetry"}})
	if err != nil {
		t.Fatal(err)
	}
	// Make the append fail the way a full disk does: the sampler opens
	// telemetry.jsonl for append on every tick, so removing it is enough.
	if err := os.Remove(filepath.Join(root, started.ID+".partial", "telemetry.jsonl")); err != nil {
		t.Fatal(err)
	}
	select {
	case message := <-warnings:
		if !strings.Contains(message, "telemetry row") {
			t.Fatalf("unexpected warning: %s", message)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a failing telemetry write said nothing at all")
	}
	stopped, err := manager.Stop(AdHocEpisodeKey)
	if err != nil {
		t.Fatal(err)
	}
	for _, stats := range stopped.Sources {
		if stats.Source.ID != "telemetry" {
			continue
		}
		if stats.Drops == nil || *stats.Drops == 0 {
			t.Fatalf("the lost rows are not counted as drops: %+v", stats)
		}
		if stats.DropAccounting != "exact" {
			t.Fatalf("drop_accounting = %q, want exact: the loss is counted precisely", stats.DropAccounting)
		}
		return
	}
	t.Fatal("the episode has no telemetry source")
}

// TestCaptureStopIsStampedBeforeTheDrain covers episode length. The post-seal
// drain is a window for late application records, not recording time, but
// stopped_episode_nanos was stamped after it, so every drained episode
// reported itself as having recorded for the whole drain longer than it did.
func TestCaptureStopIsStampedBeforeTheDrain(t *testing.T) {
	manager, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const drain = 300 * time.Millisecond
	if _, err := manager.Start(StartOptions{Name: "drained", Sources: []string{"applications"}, DrainDuration: drain}); err != nil {
		t.Fatal(err)
	}

	// While the drain runs, Status must say the episode is finishing rather
	// than that the device is idle.
	statuses := make(chan *Manifest, 1)
	go func() {
		time.Sleep(drain / 3)
		statuses <- manager.Status()
	}()
	stopped, err := manager.Stop(AdHocEpisodeKey)
	if err != nil {
		t.Fatal(err)
	}
	during := <-statuses
	if during == nil {
		t.Fatal("Status reported no episode while Stop was still draining")
	}
	if during.State != EpisodeStateDraining {
		t.Fatalf("Status during the drain reported state %q, want %q", during.State, EpisodeStateDraining)
	}

	if stopped.CaptureStoppedEpisodeNS == 0 {
		t.Fatal("capture_stopped_episode_nanos was never stamped")
	}
	if stopped.CaptureStoppedEpisodeNS >= stopped.StoppedEpisodeNS {
		t.Fatalf("capture stop %d is not before the seal at %d", stopped.CaptureStoppedEpisodeNS, stopped.StoppedEpisodeNS)
	}
	// The gap between them is the drain, which is what BI was previously
	// counting as recording time.
	if gap := stopped.StoppedEpisodeNS - stopped.CaptureStoppedEpisodeNS; gap < int64(drain/2) {
		t.Fatalf("the seal is only %dns after capture stopped, want about the %s drain", gap, drain)
	}
	// A sealed episode never leaves the draining state on disk.
	if stopped.State != "complete" {
		t.Fatalf("state = %q, want complete", stopped.State)
	}
}

func readRecoveredManifest(t *testing.T, root, id string) Manifest {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, id, "manifest.json"))
	if err != nil {
		t.Fatalf("the episode was not recovered: %v", err)
	}
	var mf Manifest
	if err := json.Unmarshal(raw, &mf); err != nil {
		t.Fatal(err)
	}
	return mf
}

func containsSubstring(haystack []string, needle string) bool {
	for _, s := range haystack {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}
