package agentmodule

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/appservice"
	"github.com/wendylabsinc/wendy/go/world/production/rpctrace"
	"github.com/wendylabsinc/wendy/go/world/production/serviceaccess"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
)

// This adapter checks module dispatch. The private provisioning adapter has
// separate tests for its real shared lock and exclusive mutation lifecycle.
type observedClockIdentity struct {
	Identity
	ordinary, clocks atomic.Int32
	before           func(bool)
}

func (i *observedClockIdentity) WithCurrent(token uint64, run func() error) error {
	i.ordinary.Add(1)
	if i.before != nil {
		i.before(false)
	}
	return i.Identity.WithCurrent(token, run)
}
func (i *observedClockIdentity) WithCurrentClock(token uint64, run func() error) error {
	i.clocks.Add(1)
	if i.before != nil {
		i.before(true)
	}
	return i.Identity.WithCurrent(token, run)
}

func TestClockIdentityDispatchFallbackAndTrace(t *testing.T) {
	for _, optional := range []bool{false, true} {
		for _, refuse := range []bool{false, true} {
			name := map[bool]string{false: "fallback", true: "shared"}[optional] + map[bool]string{false: "-entered", true: "-refused"}[refuse]
			t.Run(name, func(t *testing.T) {
				sink, err := rpctrace.New(rpctrace.Config{})
				if err != nil {
					t.Fatal(err)
				}
				base, cancel := context.WithCancel(context.Background())
				defer cancel()
				ctx, finish := rpctrace.Begin(base, sink, rpctrace.Labels{Method: rpctrace.MethodPhysicalClock, App: "com.example.clock", Service: "reader", Generation: 1})
				underlying := &timingIdentity{refuse: refuse}
				observed := &observedClockIdentity{Identity: underlying}
				var identity Identity = underlying
				if optional {
					identity = observed
				}
				m := &Manager{deps: Dependencies{Identity: identity}}
				calls := 0
				err = m.withCurrentClock(ctx, 1, func() error { calls++; cancel(); return ctx.Err() })
				want := context.Canceled
				if refuse {
					want = ErrDenied
				}
				if !errors.Is(err, want) || underlying.calls != 1 || calls != map[bool]int{false: 1, true: 0}[refuse] || observed.ordinary.Load() != 0 || observed.clocks.Load() != map[bool]int32{false: 0, true: 1}[optional] {
					t.Fatal("clock fence changed call count, refusal or cancellation", err)
				}
				if !underlying.mu.TryLock() {
					t.Fatal("callback retained its fence")
				}
				underlying.mu.Unlock()
				finish(rpctrace.Canceled)
				sink.Close()
				var raw bytes.Buffer
				if err := sink.Run(context.Background(), &raw); err != nil {
					t.Fatal(err)
				}
				var event rpctrace.Event
				if err := json.Unmarshal(bytes.TrimSpace(raw.Bytes()), &event); err != nil {
					t.Fatal(err)
				}
				counts := map[string]uint64{}
				for _, stage := range event.Stages {
					counts[stage.Stage] = stage.Count
				}
				if counts["identity_wait"] != 1 || counts["identity_held"] != map[bool]uint64{false: 1, true: 0}[refuse] || counts["total"] != 1 || len(counts) != map[bool]int{false: 3, true: 2}[refuse] {
					t.Fatal("clock fence altered trace semantics", counts)
				}
				// An optional capability must not route ordinary operations to it.
				if optional {
					_ = m.withCurrent(context.Background(), 1, func() error { return nil })
					if observed.ordinary.Load() != 1 || observed.clocks.Load() != 1 || underlying.calls != 2 {
						t.Fatal("ordinary operation entered shared clock fence")
					}
				}
			})
		}
	}
}

type serialClockProvider struct {
	window trustedtime.Interval
	calls  int
	active atomic.Int32
	max    atomic.Int32
}

func (p *serialClockProvider) Now() (trustedtime.Interval, error) {
	active := p.active.Add(1)
	defer p.active.Add(-1)
	for old := p.max.Load(); active > old && !p.max.CompareAndSwap(old, active); old = p.max.Load() {
	}
	// Deliberately unprotected fields exercise Manager.clockMu under -race.
	p.calls++
	p.window.Monotonic++
	p.window.Earliest = p.window.Earliest.Add(time.Nanosecond)
	p.window.Latest = p.window.Latest.Add(time.Nanosecond)
	return p.window, nil
}

func TestClockIdentityConcurrentPhysicalReadsKeepMonotonicFaultState(t *testing.T) {
	w, _ := moduleTestClock().Now()
	provider := &serialClockProvider{window: w}
	m := &Manager{deps: Dependencies{Clock: provider}}
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			for range 8 {
				if _, err := m.physicalNow(); err != nil {
					t.Error(err)
				}
			}
		})
	}
	wg.Wait()
	if provider.calls != 256 || provider.max.Load() != 1 {
		t.Fatal("concurrent module calls overlapped the physical provider")
	}
	provider.window.Earliest = w.Earliest.Add(-time.Second)
	if _, err := m.physicalNow(); !errors.Is(err, ErrDenied) {
		t.Fatal("regression was accepted")
	}
	for range 8 {
		wg.Go(func() {
			if _, err := m.physicalNow(); !errors.Is(err, ErrDenied) {
				t.Error("clock fault reopened")
			}
		})
	}
	wg.Wait()
	if provider.calls != 257 {
		t.Fatal("faulted clock consulted its provider again")
	}
}

func clockIdentityInstall(f *moduleFixture) *observedClockIdentity {
	id := &observedClockIdentity{Identity: f.identity}
	f.manager.deps.Identity = id // before any service, watcher or RPC exists
	return id
}

func TestClockIdentityLinuxOnlyClockRPCsUseOptionalFence(t *testing.T) {
	f := setupModule(t, accessConfig)
	id := clockIdentityInstall(f)
	source := accessSource("telemetry-a", "wendy.sensor.scalar.v1", "gas")
	reader := moduleDial(t, accessMount(t, f, source).SocketPath)
	writer := moduleDial(t, f.mount(t, 0, 11).SocketPath)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := reader.PhysicalClock(ctx, moduleClockRequest()); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Clock(ctx, moduleClockRequest()); err != nil {
		t.Fatal(err)
	}
	if id.clocks.Load() != 2 || id.ordinary.Load() != 0 {
		t.Fatal("clock observations entered data admission")
	}
	modulePublish(t, writer, moduleRequest("gas"))
	if out := accessQuery(t, reader, []serviceaccess.Source{source}, 1, ""); len(out.SignedRecordJson) != 1 {
		t.Fatal("ordinary query lost admitted proof")
	}
	if id.clocks.Load() != 2 || id.ordinary.Load() != 2 {
		t.Fatal("publication or Query used the clock-only fence")
	}
}

func TestClockIdentityLinuxClockCanPassWaitingDataOperation(t *testing.T) {
	f := setupModule(t, accessConfig)
	id := clockIdentityInstall(f)
	source := accessSource("telemetry-a", "wendy.sensor.scalar.v1", "gas")
	entered, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(resume) }) }
	defer unblock()
	id.before = func(clock bool) {
		if !clock {
			close(entered)
			<-resume
		}
	}
	reader := moduleDial(t, accessMount(t, f, source).SocketPath)
	otherService := moduleDial(t, f.mount(t, 0, 11).SocketPath)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dataCtx, cancelData := context.WithCancel(ctx)
	defer cancelData()
	done := make(chan error, 1)
	go func() {
		_, err := reader.Query(dataCtx, &appv1.QueryRequest{Sources: []*appv1.SourceSelector{accessSelector(source)}, Limit: 1})
		done <- err
	}()
	finalReadAwait(t, entered)
	if _, err := otherService.PhysicalClock(ctx, moduleClockRequest()); err != nil {
		t.Fatal("clock RPC waited for the ordinary operation gate", err)
	}
	cancelData()
	unblock()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled data operation returned success")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("data fence was not released")
	}
	if _, err := otherService.Clock(ctx, moduleClockRequest()); err != nil {
		t.Fatal("cancellation leaked the identity fence", err)
	}
}

func TestClockIdentityLinuxSharedReadKeepsFinalAuthorityChecks(t *testing.T) {
	for _, kind := range []string{"physical", "combined"} {
		for _, mode := range []string{"policy", "identity", "replacement", "cancel", "clock-regression", "original-age"} {
			t.Run(kind+"/"+mode, func(t *testing.T) {
				f := setupModule(t, accessConfig)
				id := clockIdentityInstall(f)
				source := accessSource("telemetry-a", "wendy.sensor.scalar.v1", "gas")
				client := moduleDial(t, accessMount(t, f, source).SocketPath)
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				call := func() error {
					if kind == "physical" {
						_, err := client.PhysicalClock(ctx, moduleClockRequest())
						return err
					}
					_, err := client.Clock(ctx, moduleClockRequest())
					return err
				}
				if err := call(); err != nil {
					t.Fatal(err)
				}
				original := f.manager.deps.PeerCheck
				entered, resume := make(chan struct{}), make(chan struct{})
				var calls atomic.Int32
				var once sync.Once
				unblock := func() { once.Do(func() { close(resume) }) }
				defer unblock()
				f.manager.deps.PeerCheck = func(ctx context.Context, peer appservice.Peer, scope serviceorigin.Scope) error {
					if err := original(ctx, peer, scope); err != nil {
						return err
					}
					if calls.Add(1) == 4 {
						close(entered)
						select {
						case <-resume:
						case <-ctx.Done():
							return ctx.Err()
						}
					}
					return nil
				}
				done := make(chan error, 1)
				go func() { done <- call() }()
				finalReadAwait(t, entered)
				switch mode {
				case "policy":
					f.policyOK.Store(false)
				case "identity":
					f.identity.invalidate()
				case "replacement":
					f.manager.Release("com.example.fusion", "reader", 7)
				case "cancel":
					cancel()
				case "clock-regression":
					finalReadAdvance(f.clock, -time.Millisecond)
				case "original-age":
					finalReadAdvance(f.clock, appv1.MaxClockResponseAge+time.Nanosecond)
				}
				unblock()
				select {
				case err := <-done:
					if err == nil {
						t.Fatal("shared clock fence returned stale authority")
					}
				case <-time.After(3 * time.Second):
					t.Fatal("clock RPC did not finish")
				}
				if id.clocks.Load() != 2 || id.ordinary.Load() != 0 {
					t.Fatal("shared path was not exercised exactly once per clock RPC")
				}
			})
		}
	}
}
