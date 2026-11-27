package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func repairPage(components ...world.Component) []Publication {
	page := make([]Publication, len(components))
	for i, c := range components {
		page[i] = Publication{Source: c.Key.Source, Component: c}
	}
	return page
}

func TestRepairBatchOneCommitAndIsolation(t *testing.T) {
	path, cfg, _, c := fixture(t)
	s := open(t, path, cfg)
	apply(t, s, c)
	if _, err := s.NextEpoch(c.Key.Source); err != nil {
		t.Fatal(err)
	}
	second := c
	second.Key.Entity = "second"
	c.Version.Sequence++
	page := repairPage(c, second)
	writes := 0
	s.write = func(path string, data []byte) error { writes++; return writeAtomic(path, data) }
	var retained BatchPublicationState
	results, err := s.ApplyPreparedBatch(page, func(current BatchPublicationState) error {
		if len(current.Records) != 1 || current.Epochs[c.Key.Source] != 2 {
			t.Fatal("incorrect preparation state")
		}
		retained = current
		current.Records[0].Payload[0] = '!'
		current.Epochs[c.Key.Source] = 999
		page[0].Component.Payload[0] = '!'
		page[1].Source.Stream = "changed"
		return nil
	})
	if err != nil || !reflect.DeepEqual(results, []world.Result{world.Applied, world.Applied}) || writes != 1 {
		t.Fatal(results, err, writes)
	}
	retained.Records[0].Version.Sequence = 999
	retained.Epochs[c.Key.Source] = 1000
	saved := savedPublication(t, path)
	if len(saved.World.Entries) != 2 || saved.Epochs[0].Value != 2 {
		t.Fatal("callback changed committed state")
	}
	for _, entry := range saved.World.Entries {
		if !json.Valid(entry.Component.Payload) || entry.Component.Version.Sequence > 2 || entry.Component.Key.Source.Stream != "pose" {
			t.Fatal("candidate alias escaped")
		}
	}
}

func TestRepairBatchBoundsBeforeObservation(t *testing.T) {
	for _, kind := range []string{"empty", "count", "bytes", "malformed", "nil_callback", "source"} {
		t.Run(kind, func(t *testing.T) {
			path, cfg, now, c := fixture(t)
			s := open(t, path, cfg)
			before := savedPublication(t, path)
			*now = now.Add(time.Second)
			page := repairPage(c)
			callback := func(BatchPublicationState) error { t.Fatal("preflight called callback"); return nil }
			switch kind {
			case "empty":
				page = nil
			case "count":
				page = make([]Publication, MaxRepairRecords+1)
			case "bytes":
				page[0].Component.Payload = json.RawMessage(`"` + strings.Repeat("x", maxRepairComponentBytes) + `"`)
			case "malformed":
				page[0].Component.Payload = json.RawMessage(`{`)
			case "nil_callback":
				callback = nil
			case "source":
				page[0].Source = world.Source{}
			}
			s.write = func(string, []byte) error { t.Fatal("preflight wrote state"); return nil }
			if result, err := s.ApplyPreparedBatch(page, callback); err == nil || result != nil {
				t.Fatal(result, err)
			}
			if !reflect.DeepEqual(before, savedPublication(t, path)) {
				t.Fatal("preflight advanced checkpoint")
			}
		})
	}
}

func TestRepairBatchDenialsPreserveExactFloorAndAtomicity(t *testing.T) {
	for _, kind := range []string{"callback", "invalid_second", "apply_rollback", "second_apply_rollback", "checkpoint_rollback"} {
		t.Run(kind, func(t *testing.T) {
			path, cfg, now, c := fixture(t)
			base := *now
			after, calls := false, 0
			cfg.Now = func() time.Time {
				if after && (kind == "second_apply_rollback" || kind == "checkpoint_rollback") {
					calls++
					limit := 1
					if kind == "checkpoint_rollback" {
						limit = 2
					}
					if calls <= limit {
						return base.Add(20 * time.Second)
					}
					return base.Add(5 * time.Second)
				}
				return *now
			}
			s := open(t, path, cfg)
			apply(t, s, c)
			*now = base.Add(10 * time.Second)
			c.Version.Sequence++
			c.Timing.SampleTime = *now
			second := c
			second.Key.Entity = "second"
			if kind == "invalid_second" {
				second.TTL = 0
			}
			denied := errors.New("policy denied")
			writes := 0
			s.write = func(path string, data []byte) error { writes++; return writeAtomic(path, data) }
			result, err := s.ApplyPreparedBatch(repairPage(c, second), func(BatchPublicationState) error {
				after = true
				if kind != "invalid_second" {
					*now = base.Add(5 * time.Second)
				}
				if kind == "callback" {
					return denied
				}
				return nil
			})
			if err == nil || result != nil || writes != 1 {
				t.Fatal(result, err, writes)
			}
			if kind == "callback" && !errors.Is(err, denied) {
				t.Fatal(err)
			}
			if strings.Contains(kind, "rollback") && !errors.Is(err, world.ErrClockRegression) {
				t.Fatal(err)
			}
			saved := savedPublication(t, path)
			if !saved.World.CapturedAt.Equal(base.Add(10*time.Second)) || len(saved.World.Entries) != 1 || saved.World.Entries[0].Component.Version.Sequence != 1 {
				t.Fatal("partial page or lost denial floor")
			}
			s.Close()
			after = false
			*now = base.Add(6 * time.Second)
			if reopened, err := Open(path, cfg); !errors.Is(err, world.ErrClockRegression) {
				if reopened != nil {
					reopened.Close()
				}
				t.Fatal("denial floor lost", err)
			}
		})
	}
}

func TestRepairSnapshotRetainsExpiryTombstonesAndHighWater(t *testing.T) {
	path, cfg, now, c := fixture(t)
	base := *now
	s := open(t, path, cfg)
	apply(t, s, c)
	*now = now.Add(100 * time.Millisecond)
	c.Timing.AgeHint = 900 * time.Millisecond
	tomb := c
	tomb.Key.Entity = "tomb"
	tomb.Tombstone = true
	tomb.Payload = nil
	tomb.Version.Sequence = 3
	expired := c
	expired.Key.Entity = "expired"
	expired.Version.Sequence = 4
	expired.Timing.AgeHint = time.Second
	result, err := s.ApplyPreparedBatch(repairPage(c, tomb, expired), func(BatchPublicationState) error { return nil })
	if err != nil || !reflect.DeepEqual(result, []world.Result{world.Duplicate, world.Applied, world.Applied}) {
		t.Fatal(result, err)
	}
	cp, err := s.RepairSnapshot()
	if err != nil || !reflect.DeepEqual(cp, savedPublication(t, path).World) {
		t.Fatal("snapshot differs from durable checkpoint", err)
	}
	if len(cp.Entries) != 3 {
		t.Fatal("retained entries missing")
	}
	for _, entry := range cp.Entries {
		if entry.Component.Key == c.Key && !entry.ExpiresAt.Equal(base.Add(200*time.Millisecond)) {
			t.Fatal("duplicate deadline extended")
		}
	}
	cp.Frames[0].Revision = "mutated"
	cp.Entries[0].Component.Key.Entity = "mutated"
	s.Close()
	*now = base.Add(200 * time.Millisecond)
	s = open(t, path, cfg)
	oldTomb := tomb
	oldTomb.Version.Sequence = 1
	oldExpired := expired
	oldExpired.Version.Sequence = 1
	c.Timing.AgeHint = 0
	result, err = s.ApplyPreparedBatch(repairPage(c, oldTomb, oldExpired), func(BatchPublicationState) error { return nil })
	if err != nil || !reflect.DeepEqual(result, []world.Result{world.Duplicate, world.Obsolete, world.Obsolete}) {
		t.Fatal(result, err)
	}
	if live, err := s.Snapshot(); err != nil || len(live) != 0 {
		t.Fatal("retained state resurrected", live, err)
	}
}

func TestRepairSnapshotExactCommittedClockAndCopy(t *testing.T) {
	path, cfg, now, c := fixture(t)
	s := open(t, path, cfg)
	apply(t, s, c)
	*now = now.Add(50 * time.Millisecond)
	writes := 0
	s.write = func(path string, data []byte) error {
		writes++
		if err := writeAtomic(path, data); err != nil {
			return err
		}
		*now = now.Add(50 * time.Millisecond)
		return nil
	}
	cp, err := s.RepairSnapshot()
	if err != nil || writes != 1 || !reflect.DeepEqual(cp, savedPublication(t, path).World) {
		t.Fatal("snapshot sampled uncommitted clock", cp.CapturedAt, err, writes)
	}
	floor := cp.CapturedAt
	cp.Entries[0].Component.Payload[0] = '!'
	if !json.Valid(savedPublication(t, path).World.Entries[0].Component.Payload) {
		t.Fatal("snapshot aliases persisted state")
	}
	s.Close()
	*now = floor.Add(-time.Nanosecond)
	if reopened, err := Open(path, cfg); !errors.Is(err, world.ErrClockRegression) {
		if reopened != nil {
			reopened.Close()
		}
		t.Fatal("snapshot floor lost", err)
	}
}

func TestRepairBatchAbsoluteCapSurvivesCallbackDelayAndRestart(t *testing.T) {
	path, cfg, now, c := fixture(t)
	base := *now
	s := open(t, path, cfg)
	apply(t, s, c)
	page := repairPage(c)
	page[0].ExpiresAt = base.Add(200 * time.Millisecond)
	result, err := s.ApplyPreparedBatch(page, func(BatchPublicationState) error {
		*now = base.Add(150 * time.Millisecond)
		page[0].ExpiresAt = base.Add(time.Hour) // Input alias must not extend it.
		return nil
	})
	if err != nil || result[0] != world.Duplicate {
		t.Fatal(result, err)
	}
	cp, err := s.RepairSnapshot()
	if err != nil || !cp.Entries[0].ExpiresAt.Equal(base.Add(200*time.Millisecond)) {
		t.Fatal("transit delay extended cap", cp, err)
	}
	s.Close()
	*now = base.Add(200 * time.Millisecond)
	s = open(t, path, cfg)
	if live, err := s.Snapshot(); err != nil || len(live) != 0 {
		t.Fatal("cap resurrected after restart", live, err)
	}
	newer := c
	newer.Version.Sequence++
	apply(t, s, newer)
	old := repairPage(c)
	old[0].ExpiresAt = base
	if result, err := s.ApplyPreparedBatch(old, func(BatchPublicationState) error { return nil }); err != nil || result[0] != world.Obsolete {
		t.Fatal(result, err)
	}
	if _, live, err := s.Get(c.Key); err != nil || !live {
		t.Fatal("obsolete cap expired newer record", err)
	}
}

func TestRepairBatchClampRollbackKeepsStagedFloor(t *testing.T) {
	path, cfg, now, c := fixture(t)
	base := *now
	after, calls := false, 0
	cfg.Now = func() time.Time {
		if after {
			calls++
			if calls == 1 {
				return base.Add(20 * time.Second)
			}
			return base.Add(5 * time.Second)
		}
		return *now
	}
	s := open(t, path, cfg)
	apply(t, s, c)
	*now = base.Add(10 * time.Second)
	c.Version.Sequence++
	c.Timing.SampleTime = *now
	page := repairPage(c)
	page[0].ExpiresAt = base.Add(30 * time.Second)
	result, err := s.ApplyPreparedBatch(page, func(BatchPublicationState) error { after = true; return nil })
	if result != nil || !errors.Is(err, world.ErrClockRegression) {
		t.Fatal(result, err)
	}
	cp := savedPublication(t, path).World
	if !cp.CapturedAt.Equal(base.Add(10*time.Second)) || cp.Entries[0].Component.Version.Sequence != 1 {
		t.Fatal("clamp rollback lost staged floor")
	}
}

func TestRepairPersistenceFailuresPoisonWithoutResults(t *testing.T) {
	for _, kind := range []string{"batch_before", "batch_after", "denial", "snapshot_before", "snapshot_after"} {
		t.Run(kind, func(t *testing.T) {
			path, cfg, _, c := fixture(t)
			s := open(t, path, cfg)
			failed := errors.New("injected writer failure")
			writes := 0
			s.write = func(path string, data []byte) error {
				writes++
				if strings.HasSuffix(kind, "after") {
					if err := writeAtomic(path, data); err != nil {
						return err
					}
				}
				return failed
			}
			if strings.HasPrefix(kind, "snapshot") {
				if cp, err := s.RepairSnapshot(); !errors.Is(err, failed) || cp.Version != 0 {
					t.Fatal(cp, err)
				}
			} else {
				result, err := s.ApplyPreparedBatch(repairPage(c), func(BatchPublicationState) error {
					if kind == "denial" {
						return errors.New("policy refusal")
					}
					return nil
				})
				if !errors.Is(err, failed) || result != nil {
					t.Fatal(result, err)
				}
			}
			if _, err := s.RepairSnapshot(); !errors.Is(err, ErrFaulted) || writes != 1 {
				t.Fatal(err, writes)
			}
			s.Close()
			s = open(t, path, cfg)
			records, err := s.Records()
			expected := 0
			if kind == "batch_after" {
				expected = 1
			}
			if err != nil || len(records) != expected {
				t.Fatal("reopen state", records, err)
			}
		})
	}
}

// The evidence file is a structural digest surrogate, not cryptographic proof.
// These process exits test ordering and completed fsyncs, not power loss.
func TestRepairBatchAbruptExit(t *testing.T) {
	const pathEnv, phaseEnv = "WENDY_REPAIR_EXIT_PATH", "WENDY_REPAIR_EXIT_PHASE"
	const exitCode = 73
	if path := os.Getenv(pathEnv); path != "" {
		phase := os.Getenv(phaseEnv)
		if phase != "before_world" && phase != "after_world" {
			t.Fatal("invalid phase")
		}
		seed := savedPublication(t, path).World
		now := seed.CapturedAt
		cfg := world.Config{Tenant: seed.Tenant, Site: seed.Site, ClockDomain: seed.ClockDomain, Frames: seed.Frames, MaxEntries: 100, Now: func() time.Time { return now }}
		s := open(t, path, cfg)
		now = now.Add(100 * time.Millisecond)
		duplicate := seed.Entries[0].Component
		duplicate.Timing.AgeHint = 900 * time.Millisecond
		added := duplicate
		added.Key.Entity = "second"
		added.Version.Sequence = 2
		page := repairPage(duplicate, added)
		for i := range page {
			page[i].ExpiresAt = seed.CapturedAt.Add(180 * time.Millisecond)
		}
		results, err := s.ApplyPreparedBatch(page, func(current BatchPublicationState) error {
			writePublicationEvidence(t, path+".repair-evidence", append(current.Records, duplicate, added))
			if phase == "before_world" {
				os.Exit(exitCode)
			}
			return nil
		})
		if err != nil || !reflect.DeepEqual(results, []world.Result{world.Duplicate, world.Applied}) {
			t.Fatal(results, err)
		}
		os.Exit(exitCode)
	}
	for _, phase := range []string{"before_world", "after_world"} {
		t.Run(phase, func(t *testing.T) {
			path, cfg, now, c := fixture(t)
			base := *now
			s := open(t, path, cfg)
			apply(t, s, c)
			s.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRepairBatchAbruptExit$")
			cmd.Env = append(os.Environ(), pathEnv+"="+path, phaseEnv+"="+phase)
			output, err := cmd.CombinedOutput()
			var exited *exec.ExitError
			if ctx.Err() != nil || !errors.As(err, &exited) || exited.ExitCode() != exitCode {
				t.Fatalf("child failed before expected exit: %v %s", err, output)
			}
			saved := savedPublication(t, path).World
			floor, expiry, count := base, base.Add(time.Second), 1
			if phase == "after_world" {
				floor, expiry, count = base.Add(100*time.Millisecond), base.Add(180*time.Millisecond), 2
			}
			if !saved.CapturedAt.Equal(floor) || len(saved.Entries) != count {
				t.Fatal("partial page or wrong floor")
			}
			data, err := os.ReadFile(path + ".repair-evidence")
			if err != nil {
				t.Fatal(err)
			}
			var evidence []string
			if err := json.Unmarshal(data, &evidence); err != nil {
				t.Fatal(err)
			}
			if len(evidence) != 2 {
				t.Fatal("missing durable proof superset")
			}
			for _, entry := range saved.Entries {
				covered := false
				for _, digest := range evidence {
					covered = covered || digest == publicationEvidenceDigest(t, entry.Component)
				}
				if !covered || !entry.ExpiresAt.Equal(expiry) {
					t.Fatal("missing coverage or expiry extended")
				}
			}
			*now = floor.Add(-time.Millisecond)
			if reopened, err := Open(path, cfg); !errors.Is(err, world.ErrClockRegression) {
				if reopened != nil {
					reopened.Close()
				}
				t.Fatal(err)
			}
			*now = expiry
			s = open(t, path, cfg)
			if records, err := s.Snapshot(); err != nil || len(records) != 0 {
				t.Fatal("expired page resurrected", records, err)
			}
		})
	}
}
