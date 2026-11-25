package agentmodule

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/rpctrace"
)

type timingIdentity struct {
	mu     sync.Mutex
	calls  int
	refuse bool
}

func (*timingIdentity) Snapshot(context.Context) (IdentitySnapshot, error) {
	panic("trace added identity snapshot")
}
func (*timingIdentity) Current(uint64) bool { panic("trace added identity check") }
func (i *timingIdentity) WithCurrent(_ uint64, fn func() error) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.calls++
	if i.refuse {
		return ErrDenied
	}
	return fn()
}

func TestTimingPreservesAuthorityCallsAndCancellationCleanup(t *testing.T) {
	for _, trace := range []bool{false, true} {
		for _, refuse := range []bool{false, true} {
			t.Run(map[bool]string{true: "trace", false: "default"}[trace]+map[bool]string{true: "-refused", false: "-entered"}[refuse], func(t *testing.T) {
				var sink *rpctrace.Sink
				if trace {
					var err error
					sink, err = rpctrace.New(rpctrace.Config{})
					if err != nil {
						t.Fatal(err)
					}
				}
				base, cancel := context.WithCancel(context.Background())
				defer cancel()
				ctx, finish := rpctrace.Begin(base, sink, rpctrace.Labels{Method: rpctrace.MethodQuery, App: "com.example.test", Service: "reader", Generation: 7})
				id := &timingIdentity{refuse: refuse}
				configCalls, sampleCalls, bodyCalls := 0, 0, 0
				expected := time.Unix(100, 0)
				m := &Manager{deps: Dependencies{Identity: id, PolicyCurrent: func(got context.Context) error {
					if got != ctx {
						t.Fatal("context replaced")
					}
					configCalls++
					return nil
				}, SampleNow: func() time.Time { sampleCalls++; return expected }}}
				if err := m.configCurrent(ctx); err != nil || m.sampleNow(ctx) != expected {
					t.Fatal("observation changed")
				}
				err := m.withCurrent(ctx, 7, func() error { bodyCalls++; cancel(); return ctx.Err() })
				want := context.Canceled
				if refuse {
					want = ErrDenied
				}
				if !errors.Is(err, want) || id.calls != 1 || configCalls != 1 || sampleCalls != 1 || bodyCalls != map[bool]int{true: 0, false: 1}[refuse] {
					t.Fatalf("calls/error changed: %v %d %d %d %d", err, id.calls, configCalls, sampleCalls, bodyCalls)
				}
				if !id.mu.TryLock() {
					t.Fatal("identity fence leaked after cancellation")
				}
				id.mu.Unlock()
				finish(rpctrace.Canceled)
				if sink != nil {
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
					if counts["config"] != 1 || counts["source_clock"] != 1 || counts["identity_wait"] != 1 || counts["identity_held"] != map[bool]uint64{true: 0, false: 1}[refuse] || counts["total"] != 1 {
						t.Fatalf("unexpected measurements: %+v", counts)
					}
				}
			})
		}
	}
}
