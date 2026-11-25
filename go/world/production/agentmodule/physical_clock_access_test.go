package agentmodule

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/appservice"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"google.golang.org/protobuf/proto"
)

func modulePhysicalReply(scope serviceorigin.Scope, w trustedtime.Interval, incarnation string) *appv1.PhysicalClockReply {
	return &appv1.PhysicalClockReply{Nonce: moduleClockRequest().Nonce, Physical: &appv1.PhysicalClock{EarliestUnixNs: w.Earliest.UnixNano(), LatestUnixNs: w.Latest.UnixNano(), BootId: w.BootID, MonotonicNs: int64(w.Monotonic), Source: w.Source}, Tenant: scope.Tenant, Site: scope.Site, Device: scope.Device, App: scope.App, Service: scope.Service, ExecutionDomain: scope.ExecutionDomain, DeploymentGeneration: scope.DeploymentGeneration, ServerBootId: incarnation}
}

func TestPhysicalClockAccessNeverAcquiresUnavailableSourceClock(t *testing.T) {
	for _, mode := range []string{"failed", "blocked"} {
		t.Run(mode, func(t *testing.T) {
			f := setupModule(t, accessConfig)
			source := accessSource("telemetry-a", "wendy.sensor.scalar.v1", "gas")
			client := moduleDial(t, accessMount(t, f, source).SocketPath)
			var calls atomic.Int32
			release := make(chan struct{})
			defer close(release)
			f.manager.deps.SampleNow = func() time.Time {
				calls.Add(1)
				if mode == "blocked" {
					<-release
				}
				return time.Time{}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			request := moduleClockRequest()
			first, err := client.PhysicalClock(ctx, request)
			if err != nil || calls.Load() != 0 || !bytes.Equal(first.Nonce, request.Nonce) || first.App != "com.example.fusion" || first.Service != "reader" || first.DeploymentGeneration != 7 || first.Device != f.config.Policy.Device {
				t.Fatal("physical clock acquired source time or lost exact reader scope", err)
			}
			if _, err = appv1.DecodePhysicalClockReply(first); err != nil {
				t.Fatal(err)
			}
			finalReadAdvance(f.clock, 100*time.Millisecond)
			second, err := client.PhysicalClock(ctx, request)
			if err != nil || second.Physical.EarliestUnixNs <= first.Physical.EarliestUnixNs || second.ServerBootId != first.ServerBootId || calls.Load() != 0 {
				t.Fatal("physical RPC reused an old sample or consulted source", err)
			}
			if mode == "failed" {
				if out, err := client.Clock(ctx, request); err == nil || out != nil || calls.Load() != 1 {
					t.Fatal("combined Clock stopped enforcing its source sample", err)
				}
				if out, err := client.PhysicalClock(ctx, request); err != nil || out == nil || calls.Load() != 1 {
					t.Fatal("source failure disabled independent physical authority", err)
				}
			}
		})
	}
}

func TestPhysicalClockAccessRefusesFinalAuthorityChanges(t *testing.T) {
	for _, mode := range []string{"old-response", "clock-regression", "policy", "identity", "replacement", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			f := setupModule(t, accessConfig)
			source := accessSource("telemetry-a", "wendy.sensor.scalar.v1", "gas")
			client := moduleDial(t, accessMount(t, f, source).SocketPath)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := client.PhysicalClock(ctx, moduleClockRequest()); err != nil {
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
				if scope.Service == "reader" && calls.Add(1) == 4 {
					close(entered)
					select {
					case <-resume:
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				return nil
			}
			type result struct {
				out *appv1.PhysicalClockReply
				err error
			}
			done := make(chan result, 1)
			go func() { out, err := client.PhysicalClock(ctx, moduleClockRequest()); done <- result{out, err} }()
			finalReadAwait(t, entered)
			switch mode {
			case "old-response":
				finalReadAdvance(f.clock, appv1.MaxClockResponseAge+time.Nanosecond)
			case "clock-regression":
				finalReadAdvance(f.clock, -time.Millisecond)
			case "policy":
				f.policyOK.Store(false)
			case "identity":
				f.identity.invalidate()
			case "replacement":
				f.manager.Release("com.example.fusion", "reader", 7)
			case "cancel":
				cancel()
			}
			unblock()
			select {
			case got := <-done:
				if got.err == nil || got.out != nil {
					t.Fatal("final authority failure returned physical time")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("physical clock RPC did not finish")
			}
		})
	}
}

type finalPhysicalSequence struct {
	values []trustedtime.Interval
	calls  int
	hook   func(int)
}

func finalReaderManager(clock trustedtime.Provider, policy Policy, scope serviceorigin.Scope, incarnation string) *Manager {
	identity := &moduleIdentity{}
	identity.token.Store(1)
	return &Manager{cfg: Config{Policy: policy}, deps: Dependencies{Clock: clock, Identity: identity, PolicyCurrent: func(context.Context) error { return nil }, SampleNow: func() time.Time { return moduleTestNow }}, clockIncarnation: incarnation, bindings: map[string]*binding{bindingKey(scope.App, scope.Service): {scope: scope, token: 1, authorityFrom: policy.NotBefore, authorityUntil: policy.NotAfter}}}
}

func (c *finalPhysicalSequence) Now() (trustedtime.Interval, error) {
	i := c.calls
	c.calls++
	if c.hook != nil {
		c.hook(c.calls)
	}
	if i >= len(c.values) {
		i = len(c.values) - 1
	}
	return c.values[i], nil
}

func TestClockFinalPhysicalReadEnforcesPolicyWindowWithoutRewritingReply(t *testing.T) {
	for _, kind := range []string{"combined", "physical"} {
		for _, mode := range []string{"valid", "not-after", "not-before-regression"} {
			t.Run(kind+"/"+mode, func(t *testing.T) {
				baseClock := moduleTestClock()
				w, _ := baseClock.Now()
				final := w
				policy := basePolicy()
				policy.NotAfter = w.Latest.Add(10 * time.Millisecond)
				if mode == "not-after" {
					final.Latest = policy.NotAfter
					final.Monotonic += 10 * time.Millisecond
				}
				if mode == "not-before-regression" {
					final.Earliest = policy.NotBefore.Add(-time.Nanosecond)
				}
				clock := &finalPhysicalSequence{values: []trustedtime.Interval{w, final}}
				scope := policy.scope(policy.Grants[0], 1)
				m := finalReaderManager(clock, policy, scope, strings.Repeat("a", 32))
				physical := modulePhysicalReply(scope, w, m.clockIncarnation)
				var reply proto.Message = physical
				if kind == "combined" {
					reply = &appv1.ClockReply{Nonce: physical.Nonce, Physical: physical.Physical, Tenant: scope.Tenant, Site: scope.Site, Device: scope.Device, App: scope.App, Service: scope.Service, ExecutionDomain: scope.ExecutionDomain, DeploymentGeneration: scope.DeploymentGeneration, ServerBootId: m.clockIncarnation, SampleClockDomain: "utc", SampleUnixNs: moduleTestNow.UnixNano()}
				}
				before := proto.Clone(reply)
				out, err := m.finalizeResponse(context.Background(), scope, reply)
				if clock.calls != 2 {
					t.Fatal("did not exercise final physical observation", clock.calls)
				}
				if !proto.Equal(reply, before) {
					t.Fatal("finalizer rewrote original sample bytes")
				}
				if mode == "valid" {
					if err != nil || !proto.Equal(out, before) {
						t.Fatal("live policy refused", err)
					}
				} else if err == nil || out != nil || !m.policyFailed.Load() {
					t.Fatal("final physical read crossed policy/clock boundary without refusing", err)
				}
			})
		}
	}
}

func TestPhysicalClockFinalizerRejectsNotYetValidPolicy(t *testing.T) {
	clock := moduleTestClock()
	w, _ := clock.Now()
	policy := basePolicy()
	policy.NotBefore = w.Earliest.Add(time.Nanosecond)
	m := &Manager{cfg: Config{Policy: policy}, deps: Dependencies{Clock: clock}}
	if m.clockIntervalCurrent(serviceorigin.Scope{}, w) || !m.policyFailed.Load() {
		t.Fatal("final clock interval preceded policy NotBefore")
	}
}

func TestEveryReadResponseChecksFinalReaderWindowAndReplacement(t *testing.T) {
	for _, kind := range []string{"query", "proof", "get-evidence", "put-evidence", "combined-clock", "physical-clock"} {
		for _, mode := range []string{"valid", "delegation-expiry", "delegation-not-before", "policy-expiry", "replacement", "identity"} {
			t.Run(kind+"/"+mode, func(t *testing.T) {
				base := moduleTestClock()
				w, _ := base.Now()
				final := w
				final.Latest = final.Latest.Add(10 * time.Millisecond)
				final.Monotonic += 10 * time.Millisecond
				policy := basePolicy()
				if mode == "policy-expiry" {
					policy.NotAfter = final.Latest
				}
				scope := policy.scope(policy.Grants[0], 1)
				values := []trustedtime.Interval{w, final}
				if kind == "query" {
					values = []trustedtime.Interval{w, w, final}
				}
				clock := &finalPhysicalSequence{values: values}
				m := finalReaderManager(clock, policy, scope, strings.Repeat("a", 32))
				b := m.bindings[bindingKey(scope.App, scope.Service)]
				if mode == "delegation-expiry" {
					b.authorityUntil = final.Latest
				}
				if mode == "delegation-not-before" {
					b.authorityFrom = final.Earliest.Add(time.Nanosecond)
				}
				clock.hook = func(call int) {
					if call != len(values) {
						return
					}
					switch mode {
					case "replacement":
						replacement := *b
						replacement.scope.DeploymentGeneration++
						m.bindings[bindingKey(scope.App, scope.Service)] = &replacement
					case "identity":
						m.deps.Identity.(*moduleIdentity).token.Store(2)
					}
				}
				physical := modulePhysicalReply(scope, w, m.clockIncarnation)
				var reply proto.Message
				switch kind {
				case "query":
					reply = &appv1.QueryReply{}
				case "proof":
					reply = &appv1.GetRecordProofReply{Historical: true, RetainUntilUnixMs: moduleTestNow.Add(time.Hour).UnixMilli()}
				case "get-evidence":
					reply = &appv1.GetEvidenceReply{RetainUntilUnixMs: moduleTestNow.Add(time.Hour).UnixMilli()}
				case "put-evidence":
					reply = &appv1.PutEvidenceReply{RetainUntilUnixMs: moduleTestNow.Add(time.Hour).UnixMilli()}
				case "combined-clock":
					reply = &appv1.ClockReply{Nonce: physical.Nonce, Physical: physical.Physical, Tenant: scope.Tenant, Site: scope.Site, Device: scope.Device, App: scope.App, Service: scope.Service, ExecutionDomain: scope.ExecutionDomain, DeploymentGeneration: scope.DeploymentGeneration, ServerBootId: m.clockIncarnation, SampleClockDomain: "utc", SampleUnixNs: moduleTestNow.UnixNano()}
				case "physical-clock":
					reply = physical
				}
				before := proto.Clone(reply)
				out, err := m.finalizeResponse(context.Background(), scope, reply)
				if clock.calls != len(values) {
					t.Fatalf("final guard added or skipped a clock: %d != %d", clock.calls, len(values))
				}
				if !proto.Equal(reply, before) {
					t.Fatal("read response bytes changed")
				}
				if mode == "valid" {
					if err != nil || out == nil {
						t.Fatal("current reader refused", err)
					}
				} else if err == nil || out != nil {
					t.Fatal("expired/replaced reader received response", err)
				}
			})
		}
	}
}
