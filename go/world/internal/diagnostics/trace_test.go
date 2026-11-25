package diagnostics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const testNonce = "0123456789abcdef0123456789abcdef"

func trace(t *testing.T, requests, events int) *Collector {
	t.Helper()
	c, e := New(testNonce, Limits{100, 100, requests, events})
	if e != nil {
		t.Fatal(e)
	}
	return c
}
func TestTraceLateCanceledCompletionAndLimits(t *testing.T) {
	c := trace(t, 1, 2)
	now := time.Unix(1000, 0)
	c.now = func() time.Time { return now }
	if _, r := c.Start(context.Background(), Ack); r != nil {
		t.Fatal("unarmed capture")
	}
	if e := c.Arm(testNonce); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ctx, r := c.Start(ctx, Ack)
	span := Begin(ctx, VerifierLock)
	if _, d := c.Start(ctx, Challenge); d != nil {
		t.Fatal("request cap")
	}
	Begin(ctx, ReplayLock)
	if Begin(ctx, ReplaySync) != nil {
		t.Fatal("event cap")
	}
	now = now.Add(120 * time.Millisecond)
	cancel()
	span.End(errors.New("secret should never be serialized"))
	r.Finish(401)
	if _, late := c.Start(context.Background(), Evaluate); late != nil {
		t.Fatal("late admission")
	}
	snap := c.Snapshot()
	if snap.State != "draining" || !snap.Requests[0].Canceled || !snap.Events[0].Canceled || !snap.Events[0].Failed || snap.Counters.UnfinishedEvents != 1 || snap.Counters.DroppedRequests != 1 || snap.Counters.DroppedEvents != 1 || snap.Counters.OutsideWindow != 1 {
		t.Fatalf("%+v", snap)
	}
	now = now.Add(100 * time.Millisecond)
	if Begin(ctx, TaskCommit) != nil {
		t.Fatal("post-grace span")
	}
	r.Finish(200)
	snap = c.Snapshot()
	if snap.State != "complete" || snap.Counters.LateCompletions != 1 {
		t.Fatal(snap)
	}
	b, _ := json.Marshal(snap)
	if strings.Contains(string(b), "secret") {
		t.Fatal("error text leak")
	}
	if c.Arm(testNonce) == nil {
		t.Fatal("rearm")
	}
}
func TestTraceConcurrentExplicitCorrelation(t *testing.T) {
	c := trace(t, 100, 100)
	fixed := time.Now()
	c.now = func() time.Time { return fixed }
	if e := c.Arm(testNonce); e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, r := c.Start(context.Background(), RepairRound)
			s := Begin(ctx, VerifierHold)
			s.End()
			r.Finish(0)
		}()
	}
	wg.Wait()
	snap := c.Snapshot()
	if len(snap.Requests) != 32 || len(snap.Events) != 32 || snap.Counters.UnfinishedEvents != 0 {
		t.Fatal(snap)
	}
	seen := map[uint64]bool{}
	for _, e := range snap.Events {
		if e.RequestID == 0 || seen[e.RequestID] {
			t.Fatal("misattribution")
		}
		seen[e.RequestID] = true
	}
	if Begin(context.Background(), VerifierHold) != nil {
		t.Fatal("guessed correlation")
	}
}
func TestWriterOneShotFreshBoundedTrigger(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{filepath.Join(dir, "trace.json"), filepath.Join(dir, "trigger.json"), testNonce, 20, 20, 16, 64}
	w, e := Open(cfg)
	if e != nil {
		t.Fatal(e)
	}
	waiting := readSnapshot(t, cfg.Path)
	if waiting.State != "waiting" {
		t.Fatal(waiting)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	if e = os.WriteFile(cfg.TriggerPath, []byte(`{"version":1,"run_nonce":"`+testNonce+`"}`), 0600); e != nil {
		t.Fatal(e)
	}
	go func() { done <- w.Run(ctx) }()
	select {
	case e = <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("writer hung")
	}
	final := readSnapshot(t, cfg.Path)
	if final.State != "complete" || final.ArmedAtUnixMS == 0 {
		t.Fatal(final)
	}
	before, _ := os.ReadFile(cfg.Path)
	_, r := w.Collector().Start(context.Background(), Ack)
	r.Finish(200)
	after, _ := os.ReadFile(cfg.Path)
	if string(before) != string(after) {
		t.Fatal("final file changed")
	}
	if w.Run(ctx) == nil {
		t.Fatal("writer reran")
	}
	if _, e = Open(cfg); e == nil {
		t.Fatal("existing output reused")
	}
}
func readSnapshot(t *testing.T, p string) Snapshot {
	t.Helper()
	b, e := os.ReadFile(p)
	if e != nil {
		t.Fatal(e)
	}
	var s Snapshot
	if e = json.Unmarshal(b, &s); e != nil {
		t.Fatal(e)
	}
	return s
}
func TestTriggerRejectsSpecialMalformedAndMismatchedFiles(t *testing.T) {
	for _, kind := range []string{"fifo", "symlink", "large", "duplicate", "wrong_nonce"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			cfg := Config{filepath.Join(dir, "trace"), filepath.Join(dir, "trigger"), testNonce, 100, 100, 1, 1}
			w, e := Open(cfg)
			if e != nil {
				t.Fatal(e)
			}
			switch kind {
			case "fifo":
				e = syscall.Mkfifo(cfg.TriggerPath, 0600)
			case "symlink":
				e = os.Symlink("absent", cfg.TriggerPath)
			case "large":
				e = os.WriteFile(cfg.TriggerPath, []byte(strings.Repeat("x", 257)), 0600)
			case "duplicate":
				e = os.WriteFile(cfg.TriggerPath, []byte(`{"version":1,"version":1,"run_nonce":"`+testNonce+`"}`), 0600)
			default:
				e = os.WriteFile(cfg.TriggerPath, []byte(`{"version":1,"run_nonce":"00000000000000000000000000000000"}`), 0600)
			}
			if e != nil {
				t.Fatal(e)
			}
			done := make(chan error, 1)
			go func() { _, e := w.trigger(); done <- e }()
			select {
			case e = <-done:
				if e == nil {
					t.Fatal("accepted invalid trigger")
				}
			case <-time.After(time.Second):
				t.Fatal("special file blocked")
			}
		})
	}
}
func TestTraceFullBoundFitsExport(t *testing.T) {
	c, e := New(testNonce, Limits{3000, 3000, 2048, 32768})
	if e != nil {
		t.Fatal(e)
	}
	if e = c.Arm(testNonce); e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 2048; i++ {
		ctx, r := c.Start(context.Background(), Evaluate)
		for j := 0; j < 16; j++ {
			s := Begin(ctx, SupervisorCheck)
			s.End()
		}
		r.Finish(503)
	}
	b, e := json.Marshal(c.Snapshot())
	if e != nil || len(b) > MaxSnapshotBytes {
		t.Fatal(fmt.Sprint(len(b)), e)
	}
}
