package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func fixture(t *testing.T) (string, world.Config, *time.Time, world.Component) {
	t.Helper()
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	cfg := world.Config{Tenant: "tenant", Site: "office", ClockDomain: "test", Now: func() time.Time { return now }, MaxEntries: 100, Frames: []world.FrameRef{{ID: "floor", Revision: "1"}}}
	c := world.Component{Key: world.Key{Tenant: "tenant", Site: "office", Entity: "robot", Type: "pose", Source: world.Source{Device: "device", App: "app", Service: "tracker", Stream: "pose"}}, Version: world.Version{Epoch: 1, Sequence: 1}, Frame: cfg.Frames[0], Timing: world.Timing{SampleTime: now, ClockDomain: "test", ErrorKnown: true}, TTL: time.Second, Provenance: world.Provenance{Origin: world.Measured, AppVersion: "v1"}, Payload: json.RawMessage(`{"x":1}`)}
	return filepath.Join(t.TempDir(), "replica.json"), cfg, &now, c
}
func open(t *testing.T, path string, cfg world.Config) *Store {
	t.Helper()
	s, err := Open(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
func apply(t *testing.T, s *Store, c world.Component) world.Result {
	t.Helper()
	result, err := s.Apply(c.Key.Source, c)
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func TestReopenPreservesExpiredRegistersTombstonesAndFrames(t *testing.T) {
	path, cfg, now, c := fixture(t)
	s := open(t, path, cfg)
	apply(t, s, c)
	other := c
	other.Key.Source.Stream = "other"
	other.TTL = time.Hour
	other.Frame.Revision = "2"
	apply(t, s, other)
	tomb := c
	tomb.Version.Sequence = 2
	tomb.Tombstone = true
	tomb.Payload = nil
	apply(t, s, tomb)
	if err := s.SetFrame(other.Frame); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(2 * time.Second)
	s = open(t, path, cfg)
	if got := apply(t, s, c); got != world.Obsolete {
		t.Fatal(got)
	}
	if _, ok, err := s.Get(c.Key); err != nil || ok {
		t.Fatalf("tombstone revived: %v %v", ok, err)
	}
	if _, ok, err := s.Get(other.Key); err != nil || !ok {
		t.Fatalf("frame not restored: %v %v", ok, err)
	}
	records, err := s.Records()
	if err != nil || len(records) != 2 {
		t.Fatalf("records %v %v", records, err)
	}
	for _, v := range records {
		if v.Key == c.Key && (!v.Tombstone || v.Timing.AgeHint < v.TTL) {
			t.Fatal("expired tombstone lost")
		}
	}
}
func TestShortenedExpirySurvivesReopenAndPeerTransfer(t *testing.T) {
	path, cfg, now, c := fixture(t)
	s := open(t, path, cfg)
	apply(t, s, c)
	*now = now.Add(100 * time.Millisecond)
	c.Timing.AgeHint = 900 * time.Millisecond
	apply(t, s, c)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(50 * time.Millisecond)
	s = open(t, path, cfg)
	// The sender's original hint is stripped from the duplicate.
	c.Timing.AgeHint = 0
	apply(t, s, c)
	records, err := s.Records()
	if err != nil {
		t.Fatal(err)
	}
	peer, err := world.NewReplica(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := peer.Apply(c.Key.Source, records[0]); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(50 * time.Millisecond)
	if _, ok, err := s.Get(c.Key); err != nil || ok {
		t.Fatalf("local expiry extended: %v %v", ok, err)
	}
	if _, ok, err := peer.Get(c.Key); err != nil || ok {
		t.Fatalf("peer expiry extended: %v %v", ok, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = open(t, path, cfg)
	apply(t, s, c)
	if _, ok, err := s.Get(c.Key); err != nil || ok {
		t.Fatalf("reopened expiry revived: %v %v", ok, err)
	}
}
func TestObservedReadClockSurvivesReopen(t *testing.T) {
	path, cfg, now, c := fixture(t)
	s := open(t, path, cfg)
	apply(t, s, c)
	*now = now.Add(2 * time.Second)
	if _, err := s.Snapshot(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(-time.Second)
	if _, err := Open(path, cfg); !errors.Is(err, world.ErrClockRegression) {
		t.Fatalf("rollback admitted: %v", err)
	}
	*now = now.Add(time.Second)
	s = open(t, path, cfg)
	if values, err := s.Snapshot(); err != nil || len(values) != 0 {
		t.Fatalf("stale value revived %v %v", values, err)
	}
}
func TestExclusiveLockReleasedOnCloseAndFailedOpen(t *testing.T) {
	path, cfg, _, _ := fixture(t)
	s := open(t, path, cfg)
	if _, err := Open(path, cfg); !errors.Is(err, ErrLocked) {
		t.Fatalf("concurrent writer admitted: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, cfg); err == nil {
		t.Fatal("truncation accepted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	_ = open(t, path, cfg)
}
func TestCorruptionAndScopeChangesFailClosed(t *testing.T) {
	path, cfg, _, c := fixture(t)
	s := open(t, path, cfg)
	apply(t, s, c)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved envelope
	if err := json.Unmarshal(original, &saved); err != nil {
		t.Fatal(err)
	}
	saved.State = []byte(`{}`)
	bad, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bad, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, cfg); err == nil {
		t.Fatal("checksum mismatch accepted")
	}
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	cfg.Site = "another"
	if _, err := Open(path, cfg); err == nil {
		t.Fatal("wrong site accepted")
	}
}
func TestFailedWriteDoesNotAcknowledgeOrExposeMutation(t *testing.T) {
	path, cfg, _, c := fixture(t)
	s := open(t, path, cfg)
	apply(t, s, c)
	s.write = func(string, []byte) error { return errors.New("disk full") }
	next := c
	next.Version.Sequence = 2
	next.Payload = json.RawMessage(`{"x":2}`)
	if _, err := s.Apply(next.Key.Source, next); err == nil {
		t.Fatal("failed write acknowledged")
	}
	// The old replica remains internally unchanged, but external reads fail
	// until reopen because a generic persistence error can be ambiguous.
	got, ok, err := s.replica.Get(c.Key)
	if err != nil || !ok || got.Version.Sequence != 1 {
		t.Fatalf("failed mutation exposed internally: %v %v", got, err)
	}
	if _, err := s.Snapshot(); !errors.Is(err, ErrFaulted) {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = open(t, path, cfg)
	got, ok, err = s.Get(c.Key)
	if err != nil || !ok || got.Version.Sequence != 1 {
		t.Fatalf("failed mutation persisted: %v %v", got, err)
	}
}
func TestPostRenameFailureRequiresReopen(t *testing.T) {
	path, cfg, _, c := fixture(t)
	s := open(t, path, cfg)
	s.write = func(path string, data []byte) error {
		if err := writeAtomic(path, data); err != nil {
			return err
		}
		return errors.New("directory sync outcome uncertain")
	}
	if _, err := s.Apply(c.Key.Source, c); err == nil {
		t.Fatal("ambiguous write acknowledged")
	}
	if _, err := s.Records(); !errors.Is(err, ErrFaulted) {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = open(t, path, cfg)
	if _, ok, err := s.Get(c.Key); err != nil || !ok {
		t.Fatalf("durable ambiguous result not recovered %v %v", ok, err)
	}
}
func TestEpochsPersistAndExceedObservedPublisherEpoch(t *testing.T) {
	path, cfg, _, c := fixture(t)
	s := open(t, path, cfg)
	if got, err := s.CurrentEpoch(c.Key.Source); err != nil || got != 0 {
		t.Fatal(got, err)
	}
	if got, err := s.NextEpoch(c.Key.Source); err != nil || got != 1 {
		t.Fatal(got, err)
	}
	c.Version.Epoch = 20
	apply(t, s, c)
	if got, err := s.CurrentEpoch(c.Key.Source); err != nil || got != 1 {
		t.Fatal(got, err)
	}
	if got, err := s.NextEpoch(c.Key.Source); err != nil || got != 21 {
		t.Fatal(got, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = open(t, path, cfg)
	if got, err := s.CurrentEpoch(c.Key.Source); err != nil || got != 21 {
		t.Fatal(got, err)
	}
	if got, err := s.NextEpoch(c.Key.Source); err != nil || got != 22 {
		t.Fatal(got, err)
	}
}
func TestConcurrentWritesAreDurable(t *testing.T) {
	path, cfg, _, c := fixture(t)
	s := open(t, path, cfg)
	var wg sync.WaitGroup
	jobs := make(chan int, 20)
	for i := 0; i < 20; i++ {
		jobs <- i
	}
	close(jobs)
	worker := func() {
		defer wg.Done()
		for i := range jobs {
			item := c
			item.Key.Entity = fmt.Sprint(i)
			if _, err := s.Apply(item.Key.Source, item); err != nil {
				t.Error(err)
			}
		}
	}
	wg.Add(2)
	go worker()
	go worker()
	wg.Wait()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = open(t, path, cfg)
	got, err := s.Snapshot()
	if err != nil || len(got) != 20 {
		t.Fatal(len(got), err)
	}
}
func TestClosedStoreRejectsOperations(t *testing.T) {
	path, cfg, _, c := fixture(t)
	s := open(t, path, cfg)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply(c.Key.Source, c); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	if _, err := s.NextEpoch(c.Key.Source); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
}

func TestAcknowledgedWriteSurvivesProcessExit(t *testing.T) {
	const childPath = "WENDY_WORLD_STORE_TEST_PATH"
	path, cfg, _, c := fixture(t)
	if inherited := os.Getenv(childPath); inherited != "" {
		s, err := Open(inherited, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Apply(c.Key.Source, c); err != nil {
			t.Fatal(err)
		}
		// No Close or deferred cleanup runs. The acknowledged write must already
		// be durable and the OS must release the process lock on exit.
		os.Exit(0)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestAcknowledgedWriteSurvivesProcessExit$")
	cmd.Env = append(os.Environ(), childPath+"="+path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child: %v %s", err, output)
	}
	s := open(t, path, cfg)
	if value, ok, err := s.Get(c.Key); err != nil || !ok || value.Version != c.Version {
		t.Fatalf("acknowledged write lost: %v %v %v", value, ok, err)
	}
}

func TestRealClockDeadlineSurvivesRepeatedReopen(t *testing.T) {
	for _, serializedSample := range []bool{false, true} {
		t.Run(fmt.Sprint(serializedSample), func(t *testing.T) {
			path, cfg, _, component := fixture(t)
			cfg.Now = time.Now
			component.Timing.SampleTime = time.Now()
			if serializedSample {
				component.Timing.SampleTime = component.Timing.SampleTime.Round(0)
			}
			component.TTL = time.Minute
			s := open(t, path, cfg)
			apply(t, s, component)
			for i := 0; i < 5; i++ {
				if _, err := s.Snapshot(); err != nil {
					t.Fatal(err)
				}
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				s = open(t, path, cfg)
				if _, live, err := s.Get(component.Key); err != nil || !live {
					t.Fatalf("reopen %d: %v %v", i, live, err)
				}
			}
		})
	}
}
