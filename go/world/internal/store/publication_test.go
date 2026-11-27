package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"sort"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/runtimeobs"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

// This surrogate checks only structural proof-before-world ordering. It is not
// a signed origin proof; node integration tests exercise actual signed records.
func publicationEvidenceDigest(t *testing.T, component world.Component) string {
	t.Helper()
	component.Timing.AgeHint = 0
	encoded, err := json.Marshal(component)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func writePublicationEvidence(t *testing.T, path string, records []world.Component) {
	t.Helper()
	unique := map[string]bool{}
	for _, record := range records {
		unique[publicationEvidenceDigest(t, record)] = true
	}
	digests := make([]string, 0, len(unique))
	for digest := range unique {
		digests = append(digests, digest)
	}
	sort.Strings(digests)
	data, err := json.Marshal(digests)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(path, data); err != nil {
		t.Fatal(err)
	}
}

func TestApplyPreparedAbruptExitPreservesWorldEvidenceAndExpiry(t *testing.T) {
	const pathEnv = "WENDY_PUBLICATION_EXIT_TEST_PATH"
	const phaseEnv = "WENDY_PUBLICATION_EXIT_TEST_PHASE"
	const deliberateExit = 73
	if path := os.Getenv(pathEnv); path != "" {
		phase := os.Getenv(phaseEnv)
		if phase != "before_world" && phase != "after_world" && phase != "duplicate" {
			t.Fatal("invalid subprocess test phase")
		}
		initial := savedPublication(t, path)
		if len(initial.World.Entries) != 1 {
			t.Fatal("missing subprocess seed")
		}
		now := initial.World.CapturedAt
		cfg := world.Config{Tenant: initial.World.Tenant, Site: initial.World.Site,
			ClockDomain: initial.World.ClockDomain, Frames: initial.World.Frames,
			MaxEntries: 100, Now: func() time.Time { return now }}
		s, err := Open(path, cfg)
		if err != nil {
			t.Fatal(err)
		}
		now = now.Add(100 * time.Millisecond)
		candidate := initial.World.Entries[0].Component
		expected := world.Applied
		if phase == "duplicate" {
			candidate.Timing.AgeHint = 900 * time.Millisecond
			expected = world.Duplicate
		} else {
			candidate.Version.Sequence++
			candidate.Payload = json.RawMessage(`{"x":2}`)
		}
		result, err := s.ApplyPrepared(candidate.Key.Source, candidate, func(current PublicationState) error {
			writePublicationEvidence(t, path+".evidence-test.json", append(current.Records, candidate))
			if phase == "before_world" {
				// Exit with the store lock held: no Close, callback return,
				// deferred cleanup or world commit may be required for recovery.
				os.Exit(deliberateExit)
			}
			return nil
		})
		if err != nil || result != expected {
			t.Fatal("subprocess transaction failed", result, err)
		}
		// The acknowledged world write must already be durable, including a
		// duplicate's shortened deadline. This does not simulate power loss.
		os.Exit(deliberateExit)
	}
	for _, phase := range []string{"before_world", "after_world", "duplicate"} {
		t.Run(phase, func(t *testing.T) {
			path, cfg, now, original := fixture(t)
			base := *now
			s := open(t, path, cfg)
			apply(t, s, original)
			writePublicationEvidence(t, path+".evidence-test.json", []world.Component{original})
			s.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestApplyPreparedAbruptExitPreservesWorldEvidenceAndExpiry$")
			cmd.Env = append(os.Environ(), pathEnv+"="+path, phaseEnv+"="+phase)
			output, err := cmd.CombinedOutput()
			var exited *exec.ExitError
			if ctx.Err() != nil || !errors.As(err, &exited) || exited.ExitCode() != deliberateExit {
				t.Fatalf("child did not reach deliberate exit: %v; %s", err, output)
			}
			saved := savedPublication(t, path)
			if len(saved.World.Entries) != 1 {
				t.Fatal("wrong recovered world cardinality")
			}
			floor := base.Add(100 * time.Millisecond)
			sequence := uint64(2)
			expires := base.Add(time.Second)
			if phase == "before_world" {
				// An interrupted, unacknowledged transaction need not commit
				// its staged observation floor; the previous world is intact.
				floor, sequence = base, 1
			} else if phase == "duplicate" {
				sequence, expires = 1, base.Add(200*time.Millisecond)
			}
			entry := saved.World.Entries[0]
			if !saved.World.CapturedAt.Equal(floor) || entry.Component.Version.Sequence != sequence || !entry.ExpiresAt.Equal(expires) {
				t.Fatal("wrong recovered checkpoint, version or expiry", saved.World.CapturedAt, entry.Component.Version, entry.ExpiresAt)
			}
			data, err := os.ReadFile(path + ".evidence-test.json")
			if err != nil {
				t.Fatal(err)
			}
			var evidence []string
			if err = json.Unmarshal(data, &evidence); err != nil {
				t.Fatal(err)
			}
			expectedEvidence := 2
			if phase == "duplicate" {
				expectedEvidence = 1
			}
			if len(evidence) != expectedEvidence {
				t.Fatal("callback evidence superset was not durable")
			}
			covered := false
			for _, digest := range evidence {
				covered = covered || digest == publicationEvidenceDigest(t, entry.Component)
			}
			if !covered {
				t.Fatal("recovered world has no durable evidence surrogate")
			}
			*now = floor.Add(-time.Millisecond)
			if reopened, err := Open(path, cfg); !errors.Is(err, world.ErrClockRegression) {
				if reopened != nil {
					reopened.Close()
				}
				t.Fatal("recovered clock floor not enforced", err)
			}
			*now = expires.Add(-time.Millisecond)
			s = open(t, path, cfg)
			if _, live, err := s.Get(original.Key); err != nil || !live {
				t.Fatal("record expired before its durable deadline", err)
			}
			s.Close()
			*now = expires
			s = open(t, path, cfg)
			if _, live, err := s.Get(original.Key); err != nil || live {
				t.Fatal("record revived beyond durable deadline", err)
			}
		})
	}
}

func savedPublication(t *testing.T, path string) state {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var e envelope
	if err = decode(data, &e); err != nil {
		t.Fatal(err)
	}
	var saved state
	if err = decode(e.State, &saved); err != nil {
		t.Fatal(err)
	}
	return saved
}

func TestApplyPreparedOneWriteAfterCallbackAndCopyIsolation(t *testing.T) {
	path, cfg, _, c := fixture(t)
	s := open(t, path, cfg)
	apply(t, s, c)
	if _, err := s.NextEpoch(c.Key.Source); err != nil {
		t.Fatal(err)
	}
	metrics := runtimeobs.New()
	s.SetMetrics(metrics)
	c.Version.Sequence = 2
	c.Payload = json.RawMessage(`{"x":2}`)
	prepared := false
	s.write = func(path string, data []byte) error {
		if !prepared {
			t.Fatal("world write preceded proof preparation")
		}
		return writeAtomic(path, data)
	}
	result, err := s.ApplyPrepared(c.Key.Source, c, func(current PublicationState) error {
		if current.Epoch != 2 || len(current.Records) != 1 || current.Records[0].Version.Sequence != 1 {
			t.Fatal("wrong publication snapshot", current)
		}
		current.Records[0].Payload[5] = '8'
		current.Records[0].Version.Sequence = 99
		c.Payload[5] = '9'
		prepared = true
		return nil
	})
	if err != nil || result != world.Applied {
		t.Fatal(result, err)
	}
	if count, failed, _ := metricCounts(t, metrics); count != 1 || failed != 0 {
		t.Fatal("publication was not one durable write", count, failed)
	}
	saved := savedPublication(t, path)
	if got := saved.World.Entries[0].Component; got.Version.Sequence != 2 || string(got.Payload) != `{"x":2}` {
		t.Fatal("callback mutated transaction", got)
	}
}

func TestApplyPreparedDenialsKeepStagedFloorThroughClockRollback(t *testing.T) {
	for _, kind := range []string{"callback", "apply", "final_checkpoint"} {
		t.Run(kind, func(t *testing.T) {
			path, cfg, clock, c := fixture(t)
			base := *clock
			afterCallback := false
			lateCalls := 0
			cfg.Now = func() time.Time {
				if afterCallback && kind == "final_checkpoint" {
					lateCalls++
					if lateCalls == 1 {
						return base.Add(20 * time.Second)
					}
					return base.Add(5 * time.Second)
				}
				return *clock
			}
			s := open(t, path, cfg)
			apply(t, s, c)
			*clock = base.Add(10 * time.Second)
			c.Version.Sequence = 2
			c.Timing.SampleTime = *clock
			denial := errors.New("test policy refusal")
			_, err := s.ApplyPrepared(c.Key.Source, c, func(PublicationState) error {
				afterCallback = true
				*clock = base.Add(5 * time.Second)
				if kind == "callback" {
					return denial
				}
				return nil
			})
			if kind == "callback" && !errors.Is(err, denial) {
				t.Fatal(err)
			}
			if kind != "callback" && !errors.Is(err, world.ErrClockRegression) {
				t.Fatal(err)
			}
			saved := savedPublication(t, path)
			if !saved.World.CapturedAt.Equal(base.Add(10*time.Second)) || saved.World.Entries[0].Component.Version.Sequence != 1 {
				t.Fatal("denial lost exact pre-callback floor or applied candidate", saved.World.CapturedAt)
			}
			s.Close()
			afterCallback = false
			*clock = base.Add(6 * time.Second)
			if reopened, err := Open(path, cfg); err == nil {
				reopened.Close()
				t.Fatal("rollback permitted after denial restart")
			}
			*clock = base.Add(10 * time.Second)
			s = open(t, path, cfg)
			if savedPublication(t, path).World.Entries[0].Component.Version.Sequence != 1 {
				t.Fatal("denied publication resumed")
			}
		})
	}
}

func TestApplyPreparedDuplicateShortensExpiryAndObsoleteStillPrepares(t *testing.T) {
	path, cfg, now, c := fixture(t)
	s := open(t, path, cfg)
	apply(t, s, c)
	*now = now.Add(100 * time.Millisecond)
	c.Timing.AgeHint = 900 * time.Millisecond
	calls := 0
	prepare := func(PublicationState) error { calls++; return nil }
	if result, err := s.ApplyPrepared(c.Key.Source, c, prepare); err != nil || result != world.Duplicate {
		t.Fatal(result, err)
	}
	s.Close()
	*now = now.Add(50 * time.Millisecond)
	s = open(t, path, cfg)
	if _, ok, err := s.Get(c.Key); err != nil || !ok {
		t.Fatal("shortened track expired early", err)
	}
	*now = now.Add(50 * time.Millisecond)
	if _, ok, err := s.Get(c.Key); err != nil || ok {
		t.Fatal("duplicate age shortening lost on reopen", err)
	}
	c.Version.Sequence = 2
	c.Timing.SampleTime = *now
	c.Timing.AgeHint = 0
	apply(t, s, c)
	c.Version.Sequence = 1
	metrics := runtimeobs.New()
	s.SetMetrics(metrics)
	if result, err := s.ApplyPrepared(c.Key.Source, c, prepare); err != nil || result != world.Obsolete {
		t.Fatal(result, err)
	}
	if count, _, _ := metricCounts(t, metrics); count != 1 || calls != 2 {
		t.Fatal("obsolete skipped preparation/checkpoint", calls, count)
	}
	if got := savedPublication(t, path).World.Entries[0].Component.Version.Sequence; got != 2 {
		t.Fatal("obsolete replaced high water", got)
	}
}

func TestApplyPreparedWriteFailuresPoisonWithoutRetry(t *testing.T) {
	for _, kind := range []string{"before_write", "after_replace", "denial"} {
		t.Run(kind, func(t *testing.T) {
			path, cfg, _, c := fixture(t)
			s := open(t, path, cfg)
			apply(t, s, c)
			c.Version.Sequence = 2
			injected := errors.New("test persistence failure")
			writes := 0
			s.write = func(path string, data []byte) error {
				writes++
				if kind == "after_replace" {
					if err := writeAtomic(path, data); err != nil {
						t.Fatal(err)
					}
				}
				return injected
			}
			_, err := s.ApplyPrepared(c.Key.Source, c, func(PublicationState) error {
				if kind == "denial" {
					return errors.New("policy denial must not mask persistence failure")
				}
				return nil
			})
			if !errors.Is(err, injected) || writes != 1 {
				t.Fatal("write error masked or retried", err, writes)
			}
			if _, err := s.Records(); !errors.Is(err, ErrFaulted) {
				t.Fatal("uncertain store not poisoned", err)
			}
			s.Close()
			s = open(t, path, cfg)
			expected := uint64(1)
			if kind == "after_replace" {
				expected = 2
			}
			if got := savedPublication(t, path).World.Entries[0].Component.Version.Sequence; got != expected {
				t.Fatal("wrong durable state after reopen", got)
			}
		})
	}
}
