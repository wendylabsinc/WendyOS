package servicejournal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/rpctrace"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	bolt "go.etcd.io/bbolt"
)

type timingClock struct {
	inner trustedtime.Provider
	calls int
}

func (c *timingClock) Now() (trustedtime.Interval, error) { c.calls++; return c.inner.Now() }

func TestTimingSnapshotPreservesChecksCommitsAndBytes(t *testing.T) {
	e := setup(t)
	signed := e.sign(t)
	e.admit(t, signed, world.Applied)
	clock := &timingClock{inner: e.j.cfg.PhysicalClock}
	e.j.cfg.PhysicalClock = clock
	schema, commit := e.j.cfg.AdmitSchema, e.j.commit
	schemaCalls, commitCalls := 0, 0
	e.j.cfg.AdmitSchema = func(ctx context.Context, v serviceorigin.VerifiedRecord) error { schemaCalls++; return schema(ctx, v) }
	e.j.commit = func(fn func(*bolt.Tx) error) error { commitCalls++; return commit(fn) }
	run := func(ctx context.Context) ([]serviceorigin.SignedRecord, [3]int) {
		before := [3]int{clock.calls, schemaCalls, commitCalls}
		got, err := e.j.Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return got, [3]int{clock.calls - before[0], schemaCalls - before[1], commitCalls - before[2]}
	}
	plain, plainCalls := run(context.Background())
	sink, err := rpctrace.New(rpctrace.Config{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, finish := rpctrace.Begin(context.Background(), sink, rpctrace.Labels{Method: rpctrace.MethodQuery, App: e.scope.App, Service: e.scope.Service, Generation: 7})
	traced, tracedCalls := run(ctx)
	finish(rpctrace.OK)
	if !reflect.DeepEqual(plain, traced) || plainCalls != tracedCalls || plainCalls[1] == 0 || plainCalls[2] != 1 {
		t.Fatalf("trace changed snapshot/checks: %v %v", plainCalls, tracedCalls)
	}
	if !e.j.mu.TryLock() {
		t.Fatal("snapshot retained journal lock")
	}
	e.j.mu.Unlock()
	sink.Close()
	var out bytes.Buffer
	if err := sink.Run(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	var event rpctrace.Event
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &event); err != nil {
		t.Fatal(err)
	}
	counts := map[string]uint64{}
	for _, s := range event.Stages {
		counts[s.Stage] = s.Count
	}
	if counts["journal_snapshot"] != 1 || counts["journal_wait"] != 1 || counts["journal_commit"] != 1 || counts["journal_schema"] != uint64(plainCalls[1]) || counts["verify"] != uint64(plainCalls[1]) || counts["replica"] != 3 {
		t.Fatalf("wrong trace: %+v", counts)
	}
}

func TestTimingCanceledSnapshotReleasesJournal(t *testing.T) {
	e := setup(t)
	sink, _ := rpctrace.New(rpctrace.Config{})
	base, cancel := context.WithCancel(context.Background())
	ctx, finish := rpctrace.Begin(base, sink, rpctrace.Labels{Method: rpctrace.MethodQuery, App: e.scope.App, Service: e.scope.Service, Generation: 7})
	cancel()
	got, err := e.j.Snapshot(ctx)
	if !errors.Is(err, context.Canceled) || len(got) != 0 {
		t.Fatal(got, err)
	}
	if !e.j.mu.TryLock() {
		t.Fatal("canceled snapshot retained lock")
	}
	e.j.mu.Unlock()
	finish(rpctrace.Canceled)
	sink.Close()
	var out bytes.Buffer
	if err := sink.Run(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	if sink.Stats().Written != 1 {
		t.Fatal("cancellation lost trace cleanup")
	}
	if _, err = e.j.Snapshot(context.Background()); err != nil {
		t.Fatal("cancellation changed journal usability", err)
	}
}
