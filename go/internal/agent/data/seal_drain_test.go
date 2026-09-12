package data

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestSealDrainFilesLateRecordsIntoTheirOwnEpisode pins the post-seal drain.
//
// This is the asynchronous scoring case: an application consumes samples from
// an episode, spends time computing a prediction, and writes the prediction
// only after capture has stopped. Before the drain existed that record was
// acknowledged as "buffered", a documented success, and then flushed into the
// NEXT episode with a negative offset that presented it as that episode's
// pre-trigger context. The prediction was filed against the wrong recording,
// silently, with a plausible-looking timestamp.
//
// With the drain the episode stays open for its configured window, so the late
// record is acknowledged "recorded" and lands in the episode it belongs to.
func TestSealDrainFilesLateRecordsIntoTheirOwnEpisode(t *testing.T) {
	root := t.TempDir()
	m, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}

	started, err := m.Start(StartOptions{Name: "scored", Sources: []string{"applications"}, DrainDuration: time.Second})
	if err != nil {
		t.Fatal(err)
	}

	// A record written while the episode is live is recorded into it.
	duringState, err := m.RecordApplication("com.example.scorer", ApplicationRecord{
		Version: 1, Type: "event", Name: "during-episode",
	})
	if err != nil {
		t.Fatal(err)
	}
	if duringState != "recorded" {
		t.Fatalf("record written during the episode: state=%q, want %q", duringState, "recorded")
	}

	// Stop runs on its own goroutine so the test can write into the drain
	// window. Waiting on the episode's own done channel is what makes this
	// deterministic rather than timing-dependent: that channel closes only
	// after Stop has cancelled the sampler, which means Stop has reached the
	// drain and is sleeping in it. No wall-clock sleep synchronises anything
	// here.
	m.mu.Lock()
	a := m.active[AdHocEpisodeKey]
	m.mu.Unlock()
	if a == nil {
		t.Fatal("episode is not active immediately after Start")
	}
	type stopResult struct {
		manifest Manifest
		err      error
	}
	stopped := make(chan stopResult, 1)
	begin := time.Now()
	go func() {
		manifest, stopErr := m.Stop(AdHocEpisodeKey)
		stopped <- stopResult{manifest, stopErr}
	}()
	<-a.done

	// The late prediction. Same app, same episode's data, and the write lands
	// after capture has shut down but inside the drain.
	lateState, err := m.RecordApplication("com.example.scorer", ApplicationRecord{
		Version: 1, Type: "prediction", Name: "after-seal", Model: "detector",
	})
	if err != nil {
		t.Fatal(err)
	}
	if lateState != "recorded" {
		t.Fatalf("record written inside the drain: state=%q, want %q", lateState, "recorded")
	}

	result := <-stopped
	if result.err != nil {
		t.Fatal(result.err)
	}
	if result.manifest.State != "complete" {
		t.Fatalf("episode did not seal cleanly: state=%q", result.manifest.State)
	}
	// The window above is only a window because Stop waits in it. Asserting the
	// wait directly means removing the drain fails this test deterministically,
	// rather than leaving the outcome to whichever goroutine the scheduler
	// happens to favour.
	if elapsed := time.Since(begin); elapsed < 900*time.Millisecond {
		t.Fatalf("Stop returned after %s, want at least the configured 1s drain", elapsed)
	}

	dir, err := m.episodeDir(started.ID)
	if err != nil {
		t.Fatal(err)
	}
	names := recordNames(t, readEvents(t, dir))
	if !containsName(names, "during-episode") || !containsName(names, "after-seal") {
		t.Fatalf("sealed episode holds %v, want both during-episode and after-seal", names)
	}

	manifest, failures, err := m.Inspect(started.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(failures) != 0 {
		t.Fatalf("sealed episode failed verification after the drain write: %v", failures)
	}
	for _, source := range manifest.Sources {
		if source.Source.ID == "applications" && source.Count != uint64(len(names)) {
			t.Fatalf("manifest applications count=%d, but %d record(s) are on disk", source.Count, len(names))
		}
	}

	// Past the drain the episode really is sealed, and a record is buffered for
	// whatever starts next rather than being added to it.
	afterDrainState, err := m.RecordApplication("com.example.scorer", ApplicationRecord{
		Version: 1, Type: "prediction", Name: "after-drain", Model: "detector",
	})
	if err != nil {
		t.Fatal(err)
	}
	if afterDrainState != "buffered" {
		t.Fatalf("record written after the drain: state=%q, want %q", afterDrainState, "buffered")
	}
	firstEpisodeRaw := readEvents(t, dir)
	if containsName(recordNames(t, firstEpisodeRaw), "after-drain") {
		t.Fatal("a record written after the drain reached the sealed episode")
	}

	// The ring is deliberately not consumed by a flush, so a following episode
	// receives the whole pre-trigger window: the record written after the drain
	// and the one written inside it alike. Both arrive marked as flushed, which
	// is what lets a consumer tell replayed pre-trigger context apart from a
	// record this episode observed live.
	second, err := m.Start(StartOptions{Name: "unrelated", Sources: []string{"applications"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Stop(AdHocEpisodeKey); err != nil {
		t.Fatal(err)
	}
	secondDir, err := m.episodeDir(second.ID)
	if err != nil {
		t.Fatal(err)
	}
	secondRaw := readEvents(t, secondDir)
	secondNames := recordNames(t, secondRaw)
	if !containsName(secondNames, "after-drain") || !containsName(secondNames, "after-seal") {
		t.Fatalf("second episode holds %v, want both after-drain and after-seal from the pre-roll ring", secondNames)
	}
	for i, stored := range decodeRecords(t, secondRaw) {
		if !stored.PrerollFlushed {
			t.Fatalf("second episode line %d (%s) is not marked preroll_flushed", i, stored.Name)
		}
		if stored.EpisodeNanos >= 0 {
			t.Fatalf("second episode line %d (%s) has offset %d, want a negative pre-trigger offset", i, stored.Name, stored.EpisodeNanos)
		}
	}
	// A live-recorded line must stay byte-identical to what it was before the
	// provenance key existed, so the key is absent rather than false.
	for i, raw := range jsonLines(firstEpisodeRaw) {
		var fields map[string]json.RawMessage
		if err = json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		if _, present := fields["preroll_flushed"]; present {
			t.Fatalf("live-recorded line %d carries preroll_flushed: %s", i, raw)
		}
	}
}

// TestInterruptDrainsLateRecordsLikeStop pins that an episode finalized after a
// failed adapter start honours the same drain. An application's outstanding
// verdict is no less its own for the episode having ended badly.
func TestInterruptDrainsLateRecordsLikeStop(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	started, err := m.Start(StartOptions{Name: "interrupted", Sources: []string{"applications"}, DrainDuration: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	a := m.active[AdHocEpisodeKey]
	m.mu.Unlock()
	if a == nil {
		t.Fatal("episode is not active immediately after Start")
	}
	done := make(chan error, 1)
	go func() {
		_, interruptErr := m.Interrupt(AdHocEpisodeKey, "camera adapter failed to start")
		done <- interruptErr
	}()
	<-a.done

	state, err := m.RecordApplication("com.example.scorer", ApplicationRecord{
		Version: 1, Type: "prediction", Name: "after-interrupt", Model: "detector",
	})
	if err != nil {
		t.Fatal(err)
	}
	if state != "recorded" {
		t.Fatalf("record written inside the interrupt drain: state=%q, want %q", state, "recorded")
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	dir, err := m.episodeDir(started.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !containsName(recordNames(t, readEvents(t, dir)), "after-interrupt") {
		t.Fatal("the interrupted episode did not receive the record written during its drain")
	}
}

// TestDrainSkippedWithoutApplicationsSource pins that an episode which never
// captures application records does not pay for a drain it has nothing to wait
// for. This is what keeps telemetry-only episodes, and the many tests that use
// them, fast.
func TestDrainSkippedWithoutApplicationsSource(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Start(StartOptions{Name: "telemetry-only", Sources: []string{"telemetry"}, DrainDuration: 5 * time.Second}); err != nil {
		t.Fatal(err)
	}
	begin := time.Now()
	if _, err = m.Stop(AdHocEpisodeKey); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(begin); elapsed >= time.Second {
		t.Fatalf("telemetry-only episode took %s to stop, want well under 1s: the drain must not apply", elapsed)
	}
}

// TestFlushPreRollAdmitsOnReceiptAndStampIntersection pins the flush fence. A
// ring entry reaches an episode only when both its agent receipt and its
// presentation stamp fall inside the requested window. The stamp alone is
// client-supplied, so testing it alone let an accepted client pick which later
// episode windows it appeared in.
func TestFlushPreRollAdmitsOnReceiptAndStampIntersection(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err = os.WriteFile(filepath.Join(dir, "events.jsonl"), nil, 0o640); err != nil {
		t.Fatal(err)
	}
	const origin = int64(1_000_000_000_000)
	second := time.Second.Nanoseconds()
	outside := (10 * time.Minute).Nanoseconds()

	for _, entry := range []struct {
		name    string
		receipt int64
		stamp   int64
	}{
		// Both timestamps inside the window. The stamp is deliberately not the
		// receipt so the resulting offset proves which one the arithmetic uses.
		{"both-in", origin - second, origin - 2*second},
		// Received before the window opened, forward-dated to look present.
		{"receipt-too-old", origin - outside, origin - second},
		// Received inside the window, back-dated out of it.
		{"stamp-back-dated", origin - second, origin - outside},
		// Received inside the window, stamped into the episode's future.
		{"stamp-after-origin", origin - second, origin + second},
		// Received after the episode began, back-dated into its pre-roll.
		{"receipt-after-origin", origin + second, origin - second},
	} {
		stored := storedApplicationRecord{
			ApplicationRecord:     ApplicationRecord{Version: 1, Type: "event", Name: entry.name},
			AppID:                 "com.example.app",
			AgentReceiptBootNanos: entry.receipt,
		}
		encoded, marshalErr := json.Marshal(stored)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		m.preRoll = append(m.preRoll, bufferedRecord{bootNanos: entry.stamp, receiptNanos: entry.receipt, encoded: encoded})
	}

	count, earliest, err := m.flushPreRoll(dir, origin, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("flushed %d record(s), want exactly the one whose receipt and stamp are both in the window", count)
	}
	records := decodeRecords(t, readEvents(t, dir))
	if len(records) != 1 || records[0].Name != "both-in" {
		t.Fatalf("flushed %+v, want only both-in", recordNames(t, readEvents(t, dir)))
	}
	if !records[0].PrerollFlushed {
		t.Fatal("flushed record is not marked preroll_flushed")
	}
	if records[0].EpisodeNanos != -2*second {
		t.Fatalf("offset=%d, want %d derived from the presentation stamp", records[0].EpisodeNanos, -2*second)
	}
	if earliest == nil || *earliest != -2*second {
		t.Fatalf("earliest offset=%v, want %d", earliest, -2*second)
	}
}

// TestBackdatedClientStampCannotPlantIntoNarrowWindow pins that a client cannot
// place a record into an episode's pre-roll by choosing its own timestamp. The
// record here is genuinely accepted, so its stamp is the client's, and a narrow
// buffer must still exclude it.
func TestBackdatedClientStampCannotPlantIntoNarrowWindow(t *testing.T) {
	restore := stubBootIDForAcceptance(t, "0f2a1c3e-test-boot-id")
	defer restore()

	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now, err := readBootTime()
	if err != nil {
		t.Fatal(err)
	}
	// Back-date by four minutes, which stays inside the five minute acceptance
	// band and far outside the 50ms buffer below. Where the boot clock has not
	// been running that long yet, back-date by half of it instead: the stamp
	// still has to be non-negative to be accepted at all.
	backdate := (4 * time.Minute).Nanoseconds()
	if now-backdate < 0 {
		backdate = now / 2
	}
	stamp := now - backdate
	state, err := m.RecordApplication("com.example.app", ApplicationRecord{
		Version: 1, Type: "event", Name: "back-dated",
		ClientBootNanos: stamp,
		ClientBootID:    "0f2a1c3e-test-boot-id",
	})
	if err != nil {
		t.Fatal(err)
	}
	if state != "buffered" {
		t.Fatalf("state=%q, want buffered", state)
	}
	// Without an accepted client stamp there would be no back-dating to fence
	// off, so the premise is checked rather than assumed.
	if len(m.preRoll) != 1 {
		t.Fatalf("pre-roll ring holds %d entries, want 1", len(m.preRoll))
	}
	buffered := decodeRecords(t, append(append([]byte(nil), m.preRoll[0].encoded...), '\n'))[0]
	if !buffered.ClientTimestampAccepted || m.preRoll[0].bootNanos != stamp {
		t.Fatalf("the client stamp was not accepted, so this test would not exercise the fence: accepted=%v stamp=%d want=%d",
			buffered.ClientTimestampAccepted, m.preRoll[0].bootNanos, stamp)
	}

	started, err := m.Start(StartOptions{Sources: []string{"applications"}, PreRollDuration: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Stop(AdHocEpisodeKey); err != nil {
		t.Fatal(err)
	}
	dir, err := m.episodeDir(started.ID)
	if err != nil {
		t.Fatal(err)
	}
	if names := recordNames(t, readEvents(t, dir)); len(names) != 0 {
		t.Fatalf("a record back-dated four minutes reached a 50ms pre-roll window: %v", names)
	}
}

// TestRecordsNeverAcceptedWhenBootIDUnavailable pins the boot identity check.
// bootID reports the literal "unavailable" when the kernel boot identity cannot
// be read, so an unguarded equality let a client that sends "unavailable" match
// on exactly the hosts where the agent has nothing to check against, and have
// its arbitrary timestamp trusted.
func TestRecordsNeverAcceptedWhenBootIDUnavailable(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	started, err := m.Start(StartOptions{Sources: []string{"applications"}})
	if err != nil {
		t.Fatal(err)
	}

	restore := stubBootIDForAcceptance(t, "unavailable")
	arbitrary := int64(42)
	if _, err = m.RecordApplication("com.example.app", ApplicationRecord{
		Version: 1, Type: "event", Name: "agent-has-no-boot-id",
		ClientBootNanos: arbitrary, ClientBootID: "unavailable",
	}); err != nil {
		t.Fatal(err)
	}
	restore()

	// Against a healthy agent identity, neither an empty nor an "unavailable"
	// client value may match.
	restore = stubBootIDForAcceptance(t, "0f2a1c3e-test-boot-id")
	defer restore()
	for _, claimed := range []string{"", "unavailable"} {
		if _, err = m.RecordApplication("com.example.app", ApplicationRecord{
			Version: 1, Type: "event", Name: "claims-" + claimed,
			ClientBootNanos: arbitrary, ClientBootID: claimed,
		}); err != nil {
			t.Fatal(err)
		}
	}

	if _, err = m.Stop(AdHocEpisodeKey); err != nil {
		t.Fatal(err)
	}
	dir, err := m.episodeDir(started.ID)
	if err != nil {
		t.Fatal(err)
	}
	records := decodeRecords(t, readEvents(t, dir))
	if len(records) != 3 {
		t.Fatalf("recorded %d line(s), want 3", len(records))
	}
	for _, stored := range records {
		if stored.ClientTimestampAccepted {
			t.Fatalf("%s: client_timestamp_accepted=true, want false", stored.Name)
		}
		if stored.EpisodeNanos != stored.AgentReceiptBootNanos-started.RequestBootNanos {
			t.Fatalf("%s: offset=%d is not derived from the agent receipt (%d - %d)",
				stored.Name, stored.EpisodeNanos, stored.AgentReceiptBootNanos, started.RequestBootNanos)
		}
		if stored.EpisodeNanos == arbitrary-started.RequestBootNanos {
			t.Fatalf("%s: the arbitrary client stamp was trusted", stored.Name)
		}
	}
}

// stubBootIDForAcceptance replaces the boot identity RecordApplication checks
// against and returns the restore function. Tests using it must not run in
// parallel with each other.
func stubBootIDForAcceptance(t *testing.T, id string) func() {
	t.Helper()
	previous := readBootIDForAcceptance
	readBootIDForAcceptance = func() string { return id }
	return func() { readBootIDForAcceptance = previous }
}

func readEvents(t *testing.T, dir string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func jsonLines(raw []byte) [][]byte {
	var out [][]byte
	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		out = append(out, line)
	}
	return out
}

func decodeRecords(t *testing.T, raw []byte) []storedApplicationRecord {
	t.Helper()
	var out []storedApplicationRecord
	for _, line := range jsonLines(raw) {
		var stored storedApplicationRecord
		if err := json.Unmarshal(line, &stored); err != nil {
			t.Fatalf("bad JSONL line %q: %v", line, err)
		}
		out = append(out, stored)
	}
	return out
}

func recordNames(t *testing.T, raw []byte) []string {
	t.Helper()
	var names []string
	for _, stored := range decodeRecords(t, raw) {
		names = append(names, stored.Name)
	}
	return names
}

func containsName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// TestPreRollLostIsThisEpisodesLossNotTheAgentsTotal pins the meaning of the
// manifest's pre_roll_lost.
//
// The field was set from a counter that ran for the manager's whole lifetime
// and was never reset, so every episode published the agent's running total
// since start as its own loss. It also counted entries that simply aged out of
// the five minute ring, which are not losses at all: an entry past the window
// is no longer pre-roll for any episode that could start now. A status field
// that lies is worse than no status field, and this one lied twice.
func TestPreRollLostIsThisEpisodesLossNotTheAgentsTotal(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now, err := readBootTime()
	if err != nil {
		t.Fatal(err)
	}
	// One record that aged out of the ring long ago, and one dropped by the
	// byte budget while it was still inside its window. Only the second is a
	// loss, and only for an episode whose window covers it.
	aged := now - (10 * time.Minute).Nanoseconds()
	budgeted := now - time.Second.Nanoseconds()
	// One pass per reason, so each is attributed on its own. First the entry
	// that is simply past the window: it is pre-roll for nobody and its
	// departure costs nothing.
	m.preRoll = []bufferedRecord{{bootNanos: aged, receiptNanos: aged, encoded: []byte(`{"name":"aged-out"}`)}}
	m.preRollBytes = len(m.preRoll[0].encoded)
	m.evictPreRoll(now)
	if len(m.preRollEvicted) != 0 {
		t.Fatalf("an entry that aged out was counted as a loss: %v", m.preRollEvicted)
	}
	// Then the entry the byte budget takes while it is still inside its
	// window. That one WOULD have reached the next episode and did not.
	m.preRoll = []bufferedRecord{{bootNanos: budgeted, receiptNanos: budgeted, encoded: []byte(`{"name":"budget-evicted"}`)}}
	m.preRollBytes = preRollLimit + 1
	m.evictPreRoll(now)
	m.preRollBytes = 0

	if len(m.preRollEvicted) != 1 {
		t.Fatalf("recorded %d in-window eviction(s), want exactly the one the byte budget dropped", len(m.preRollEvicted))
	}

	started, err := m.Start(StartOptions{Sources: []string{"applications"}, DrainDuration: 0})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Stop(AdHocEpisodeKey); err != nil {
		t.Fatal(err)
	}
	manifest, _, err := m.Inspect(started.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.PreRollLost != 1 {
		t.Fatalf("pre_roll_lost = %d, want 1: the aged-out entry was never this episode's to lose", manifest.PreRollLost)
	}

	// A second episode must not inherit the first one's number. Nothing has
	// been lost since, so its own loss is zero.
	second, err := m.Start(StartOptions{
		Sources:         []string{"applications"},
		PreRollDuration: 100 * time.Millisecond,
		DrainDuration:   0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Stop(AdHocEpisodeKey); err != nil {
		t.Fatal(err)
	}
	secondManifest, _, err := m.Inspect(second.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if secondManifest.PreRollLost != 0 {
		t.Fatalf("second episode's pre_roll_lost = %d, want 0: its 100ms window does not reach the eviction a second before it, "+
			"and the first episode's loss is not its own", secondManifest.PreRollLost)
	}
}
