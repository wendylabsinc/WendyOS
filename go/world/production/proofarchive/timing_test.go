package proofarchive

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/rpctrace"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	bolt "go.etcd.io/bbolt"
)

type archiveTimingClock struct {
	inner trustedtime.Provider
	calls int
}

func (c *archiveTimingClock) Now() (trustedtime.Interval, error) { c.calls++; return c.inner.Now() }

type archiveTimingCounts struct{ Clock, Authorization, Schema, Commit int }

func timingContext(t *testing.T, parent context.Context, enabled bool) (context.Context, func(rpctrace.Code) []rpctrace.Event) {
	t.Helper()
	var sink *rpctrace.Sink
	if enabled {
		var err error
		sink, err = rpctrace.New(rpctrace.Config{})
		if err != nil {
			t.Fatal(err)
		}
	}
	ctx, finish := rpctrace.Begin(parent, sink, rpctrace.Labels{Method: rpctrace.MethodPublish, App: "com.example.trace", Service: "publisher", Generation: 7})
	if !enabled && ctx != parent {
		t.Fatal("nil trace replaced context")
	}
	return ctx, func(code rpctrace.Code) []rpctrace.Event {
		finish(code)
		if sink == nil {
			return nil
		}
		sink.Close()
		var raw bytes.Buffer
		if err := sink.Run(context.Background(), &raw); err != nil {
			t.Fatal(err)
		}
		var out []rpctrace.Event
		for _, line := range bytes.Split(bytes.TrimSpace(raw.Bytes()), []byte("\n")) {
			if len(line) > 0 {
				var e rpctrace.Event
				if err := json.Unmarshal(line, &e); err != nil {
					t.Fatal(err)
				}
				out = append(out, e)
			}
		}
		if len(out) != 1 || out[0].Refusal != nil || !out[0].Inclusive {
			t.Fatal("unexpected trace shape", len(out))
		}
		for _, secret := range [][]byte{[]byte("PRIVATE_TEST_ERROR"), []byte("spiffe://"), []byte("claims"), []byte("envelope"), []byte(".db")} {
			if bytes.Contains(raw.Bytes(), secret) {
				t.Fatal("archive trace copied private data")
			}
		}
		return out
	}
}

func archiveStageCounts(events []rpctrace.Event) map[string]uint64 {
	out := map[string]uint64{}
	for _, e := range events {
		for _, s := range e.Stages {
			out[s.Stage] += s.Count
		}
	}
	return out
}

func TestTimingArchivePreservesBytesChecksAndCommits(t *testing.T) {
	e := setup(t)
	cfg := e.cfg
	cfg.Path = filepath.Join(filepath.Dir(cfg.Path), "comparison.db")
	other, err := Create(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	raw := e.raw(t, false, "gas")
	_, ref, err := e.a.decodeEnvelope(raw)
	if err != nil {
		t.Fatal(err)
	}
	claim := e.raw(t, true, "claim", ref)
	type outcome struct {
		Ref     Reference
		Pin     PinReceipt
		Checked PinReceipt
		Proof   HistoricalProof
		Rows    map[string][]byte
	}
	var outputs []outcome
	var counters []archiveTimingCounts
	var events []rpctrace.Event
	for i, a := range []*Archive{e.a, other} {
		counter := archiveTimingCounts{}
		clock := &archiveTimingClock{inner: e.clock}
		a.cfg.Clock = clock
		e.authHook = func() { counter.Authorization++ }
		startSchemas := e.pinCalls
		a.commit = func(fn func(*bolt.Tx) error) error { counter.Commit++; return a.db.Update(fn) }
		ctx, finish := timingContext(t, context.Background(), i == 1)
		if err := a.Prune(ctx); err != nil {
			t.Fatal(err)
		}
		out := outcome{}
		out.Ref, err = a.RememberAccepted(ctx, raw)
		if err != nil {
			t.Fatal(err)
		}
		out.Pin, err = a.Pin(ctx, PinRequest{ClaimEnvelope: claim, Inputs: []Reference{ref}, RetainUntilUnixMS: e.clock.w.Earliest.Add(time.Hour).UnixMilli()})
		if err != nil {
			t.Fatal(err)
		}
		out.Checked, err = a.CheckPin(ctx, out.Pin.Claim)
		if err != nil {
			t.Fatal(err)
		}
		out.Proof, err = a.Get(ctx, ref.Source(), ref)
		if err != nil {
			t.Fatal(err)
		}
		if err = a.Prune(ctx); err != nil {
			t.Fatal(err)
		}
		if !a.mu.TryLock() {
			t.Fatal("archive lock survived operation")
		}
		a.mu.Unlock()
		rows := map[string][]byte{}
		if err = a.db.View(func(tx *bolt.Tx) error {
			for _, key := range [][]byte{floorKey, usageKey} {
				rows[string(metaBucket)+string(key)] = bytes.Clone(tx.Bucket(metaBucket).Get(key))
			}
			for _, name := range [][]byte{proofBucket, pinBucket, rememberBucket, pinWaterBucket} {
				if err := tx.Bucket(name).ForEach(func(k, v []byte) error { rows[string(name)+string(k)] = bytes.Clone(v); return nil }); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		out.Rows = rows
		counter.Clock = clock.calls
		counter.Schema = e.pinCalls - startSchemas
		outputs = append(outputs, out)
		counters = append(counters, counter)
		recorded := finish(rpctrace.OK)
		if i == 1 {
			events = recorded
		} else if len(recorded) != 0 {
			t.Fatal("nil trace emitted")
		}
	}
	if !reflect.DeepEqual(outputs[0], outputs[1]) || counters[0] != counters[1] || counters[0].Authorization == 0 || counters[0].Schema == 0 || counters[0].Commit == 0 {
		t.Fatal("trace changed archive behavior", counters)
	}
	if !bytes.Equal(outputs[0].Proof.Envelope, raw) {
		t.Fatal("original envelope rewritten")
	}
	counts := archiveStageCounts(events)
	if counts["archive_prune"] != 2 || counts["archive_remember"] != 1 || counts["archive_usage"] != uint64(counters[1].Commit) || counts["archive_commit"] != uint64(counters[1].Commit) || counts["archive_verify"] == 0 || counts["total"] != 1 {
		t.Fatal("spans missed existing work", counts, counters)
	}
}

func TestTimingArchiveFailureAndCancellationParity(t *testing.T) {
	for _, mode := range []string{"denied", "pre-cancel", "post-cancel", "uncertain-commit", "clock-failure", "corrupt-prune"} {
		t.Run(mode, func(t *testing.T) {
			type outcome struct {
				Counts  archiveTimingCounts
				Error   string
				Faulted bool
				Proofs  int
			}
			var outcomes []outcome
			var events []rpctrace.Event
			for _, enabled := range []bool{false, true} {
				e := setup(t)
				raw := e.raw(t, false, "gas")
				if mode == "corrupt-prune" {
					remember(t, e, raw)
					if err := e.a.db.Update(func(tx *bolt.Tx) error {
						k, _ := tx.Bucket(proofBucket).Cursor().First()
						return tx.Bucket(proofBucket).Put(k, []byte("PRIVATE_TEST_ERROR"))
					}); err != nil {
						t.Fatal(err)
					}
				}
				counter := archiveTimingCounts{}
				clock := &archiveTimingClock{inner: e.clock}
				e.a.cfg.Clock = clock
				e.authHook = func() { counter.Authorization++ }
				parent, cancel := context.WithCancel(context.Background())
				ctx, finish := timingContext(t, parent, enabled)
				e.a.commit = func(fn func(*bolt.Tx) error) error {
					counter.Commit++
					if err := e.a.db.Update(fn); err != nil {
						return err
					}
					switch mode {
					case "post-cancel":
						cancel()
					case "uncertain-commit":
						return errors.New("PRIVATE_TEST_ERROR")
					}
					return nil
				}
				switch mode {
				case "denied":
					e.denySource = true
				case "pre-cancel":
					cancel()
				case "clock-failure":
					e.clock.fail = true
				}
				var err error
				if mode == "corrupt-prune" {
					err = e.a.Prune(ctx)
				} else {
					var ref Reference
					ref, err = e.a.RememberAccepted(ctx, raw)
					if ref != (Reference{}) {
						t.Fatal("failed operation returned reference")
					}
				}
				if err == nil {
					t.Fatal("injected fault did not fail", mode)
				}
				proofs := 0
				if e2 := e.a.db.View(func(tx *bolt.Tx) error { proofs = tx.Bucket(proofBucket).Stats().KeyN; return nil }); e2 != nil {
					t.Fatal(e2)
				}
				counter.Clock = clock.calls
				counter.Schema = e.pinCalls
				outcomes = append(outcomes, outcome{counter, err.Error(), e.a.fault != nil, proofs})
				if !e.a.mu.TryLock() {
					t.Fatal("failed operation retained lock")
				}
				e.a.mu.Unlock()
				code := rpctrace.Unavailable
				if mode == "pre-cancel" || mode == "post-cancel" {
					code = rpctrace.Canceled
				}
				got := finish(code)
				if enabled {
					events = got
				}
				cancel()
			}
			if !reflect.DeepEqual(outcomes[0], outcomes[1]) {
				t.Fatal("tracing changed refusal or durable outcome", outcomes)
			}
			counts := archiveStageCounts(events)
			if counts["archive_commit"] != uint64(outcomes[1].Counts.Commit) || counts["total"] != 1 {
				t.Fatal("failure cleanup commit not measured", counts, outcomes)
			}
			if mode == "corrupt-prune" {
				if counts["archive_prune"] != 1 {
					t.Fatal(counts)
				}
			} else if counts["archive_remember"] != 1 {
				t.Fatal(counts)
			}
		})
	}
}
