package rpctrace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"
)

func labels() Labels {
	return Labels{Method: MethodPhysicalClock, App: "com.wendy.trace-test", Service: "reader", Generation: 7}
}
func sink(t *testing.T, cfg Config) *Sink {
	t.Helper()
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func drain(t *testing.T, s *Sink) ([]Event, []byte) {
	t.Helper()
	s.Close()
	var out bytes.Buffer
	if err := s.Run(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	raw := out.Bytes()
	var events []Event
	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var event Event
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	return events, raw
}
func measured(t *testing.T, event Event, name string) Measurement {
	t.Helper()
	for _, stage := range event.Stages {
		if stage.Stage == name {
			return stage
		}
	}
	t.Fatalf("missing stage %q", name)
	return Measurement{}
}

func TestNilTraceDoesNotChangeContextOrAllocate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	allocs := testing.AllocsPerRun(1000, func() {
		next, finish := Begin(ctx, nil, labels())
		if next != ctx {
			t.Fatal("nil tracing changed caller context")
		}
		Start(next, StagePeer)()
		Add(next, StageSchema, time.Second)
		finish(Canceled)
	})
	if allocs != 0 {
		t.Fatalf("nil trace allocated: %g", allocs)
	}
	var absent *Sink
	absent.Close()
	if absent.Stats() != (Stats{}) {
		t.Fatal("nil stats")
	}
	var zero Sink
	zero.Close()
	if err := zero.Run(ctx, nil); !errors.Is(err, ErrWriter) {
		t.Fatal("zero-value sink started writer")
	}
}

func TestInclusiveCountersFixedShapeAndIdempotence(t *testing.T) {
	s := sink(t, Config{})
	ctx, finish := Begin(context.Background(), s, labels())
	Add(ctx, StageAuthorize, 10*time.Millisecond)
	Add(ctx, StagePeer, 3*time.Millisecond)
	Add(ctx, StagePeer, time.Millisecond)
	Add(ctx, StageSchema, -time.Second)
	Add(ctx, StageTotal, time.Hour)
	Add(ctx, Stage(255), time.Second)
	stop := Start(ctx, StageVerify)
	stop()
	stop()
	finish(OK)
	Add(ctx, StagePeer, time.Hour)
	finish(Internal)
	events, raw := drain(t, s)
	if len(events) != 1 {
		t.Fatalf("events: %d", len(events))
	}
	e := events[0]
	if e.Version != 2 || e.Sequence != 1 || !e.Inclusive || e.Code != "OK" || e.Method != "PhysicalClock" || e.App != labels().App || e.Service != labels().Service || e.Generation != 7 {
		t.Fatalf("unexpected event: %+v", e)
	}
	p := measured(t, e, "peer")
	if p.Count != 2 || p.SumNS != int64(4*time.Millisecond) || p.MaxNS != int64(3*time.Millisecond) {
		t.Fatalf("nested counts: %+v", p)
	}
	if measured(t, e, "authorize").SumNS != int64(10*time.Millisecond) || measured(t, e, "verify").Count != 1 || measured(t, e, "total").Count != 1 || len(e.Stages) != 4 {
		t.Fatal("inclusive accounting changed")
	}
	stats := s.Stats()
	if stats.Started != 1 || stats.Finished != 1 || stats.Written != 1 || stats.Active != 0 || stats.EnqueuedBytes != uint64(len(raw)) || stats.WrittenBytes != uint64(len(raw)) {
		t.Fatalf("stats: %+v", stats)
	}
}

func TestConcurrentCountersAndSaturatingDuration(t *testing.T) {
	s := sink(t, Config{})
	ctx, finish := Begin(context.Background(), s, labels())
	var group sync.WaitGroup
	for range 32 {
		group.Go(func() {
			for range 100 {
				Add(ctx, StageSchema, 10*time.Nanosecond)
			}
		})
	}
	group.Wait()
	Add(ctx, StagePeer, time.Duration(math.MaxInt64))
	Add(ctx, StagePeer, time.Second)
	finish(Code(math.MaxUint32))
	events, _ := drain(t, s)
	if len(events) != 1 || events[0].Code != "Unknown" {
		t.Fatal("unknown numeric status escaped fixed code set")
	}
	v := measured(t, events[0], "schema")
	if v.Count != 3200 || v.SumNS != 32000 || v.MaxNS != 10 {
		t.Fatalf("lost concurrent observations: %+v", v)
	}
	v = measured(t, events[0], "peer")
	if v.Count != 2 || v.SumNS != math.MaxInt64 || v.MaxNS != math.MaxInt64 {
		t.Fatalf("duration overflow: %+v", v)
	}
}

func TestInvalidConfigurationAndLabelsAreBounded(t *testing.T) {
	for _, cfg := range []Config{{Capacity: -1}, {Capacity: MaxCapacity + 1}, {MaxEvents: -1}, {MaxEvents: MaxEvents + 1}, {MaxBytes: -1}, {MaxBytes: MaxBytes + 1}} {
		if s, err := New(cfg); !errors.Is(err, ErrConfig) || s != nil {
			t.Fatalf("accepted invalid config %+v", cfg)
		}
	}
	s := sink(t, Config{})
	bad := []Labels{labels(), labels(), labels(), labels(), labels(), labels(), labels()}
	bad[0].Method = 0
	bad[1].Generation = 0
	bad[2].App = "/private/credential"
	bad[3].Service = "reader\nsecret"
	bad[4].App = strings.Repeat("a", 257)
	bad[5].Service = strings.Repeat("b", 129)
	bad[6].Service = "rèader"
	ctx := context.Background()
	for _, l := range bad {
		next, finish := Begin(ctx, s, l)
		if next != ctx {
			t.Fatal("invalid trace changed caller context")
		}
		finish(OK)
	}
	_, finish := Begin(nil, s, labels())
	finish(OK)
	outer, first := Begin(ctx, s, labels())
	inner, second := Begin(outer, s, labels())
	if inner != outer {
		t.Fatal("nested begin replaced collector")
	}
	second(Internal)
	first(OK)
	events, raw := drain(t, s)
	if len(events) != 1 || s.Stats().DroppedInvalid != uint64(len(bad)+2) || bytes.Contains(raw, []byte("credential")) || bytes.Contains(raw, []byte("secret")) {
		t.Fatalf("invalid data emitted: %+v", s.Stats())
	}
}

func TestCancelledContextStillRecordsExistingWork(t *testing.T) {
	s := sink(t, Config{})
	ctx, cancel := context.WithCancel(context.Background())
	ctx, finish := Begin(ctx, s, labels())
	stop := Start(ctx, StageJournalSnapshot)
	cancel()
	stop()
	finish(Canceled)
	events, _ := drain(t, s)
	if ctx.Err() != context.Canceled || len(events) != 1 || events[0].Code != "Canceled" || measured(t, events[0], "journal_snapshot").Count != 1 {
		t.Fatal("trace affected cancellation or lost completed span")
	}
}
