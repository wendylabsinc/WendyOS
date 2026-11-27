package agentmodule

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
)

func clonePolicy(t *testing.T, p Policy) Policy {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var copy Policy
	if err = json.Unmarshal(raw, &copy); err != nil {
		t.Fatal(err)
	}
	return copy
}

func TestPolicyAndTrustedClockFailuresRemainLatchedAfterRestoration(t *testing.T) {
	for _, fault := range []string{"configuration", "clock"} {
		t.Run(fault, func(t *testing.T) {
			clock := moduleTestClock()
			var failed atomic.Bool
			m := &Manager{cfg: Config{Policy: basePolicy()}, deps: Dependencies{Clock: clock, PolicyCurrent: func(context.Context) error {
				if failed.Load() {
					return errors.New("test configuration changed")
				}
				return nil
			}}}
			if err := m.policyCurrent(context.Background()); err != nil {
				t.Fatal(err)
			}
			if fault == "configuration" {
				failed.Store(true)
			} else {
				clock.mu.Lock()
				clock.err = trustedtime.ErrUntrusted
				clock.mu.Unlock()
			}
			if err := m.policyCurrent(context.Background()); err == nil {
				t.Fatal("authority fault accepted")
			}
			failed.Store(false)
			clock.mu.Lock()
			clock.err = nil
			clock.mu.Unlock()
			if err := m.policyCurrent(context.Background()); err == nil {
				t.Fatal("restoration cleared observed fault without module restart")
			}
		})
	}
}

func TestConcurrentSuccessfulCheckCannotOvertakePolicyInvalidation(t *testing.T) {
	entered := make(chan struct{})
	resume := make(chan struct{})
	var calls atomic.Uint64
	m := &Manager{cfg: Config{Policy: basePolicy()}, deps: Dependencies{Clock: moduleTestClock(), PolicyCurrent: func(context.Context) error {
		if calls.Add(1) == 1 {
			close(entered)
			<-resume
			return nil
		}
		return ErrDenied
	}}}
	done := make(chan error, 1)
	go func() { done <- m.policyCurrent(context.Background()) }()
	<-entered
	if err := m.policyCurrent(context.Background()); err == nil {
		t.Fatal("failure not observed")
	}
	close(resume)
	if err := <-done; err == nil {
		t.Fatal("in-flight success overtook permanently observed policy failure")
	}
}

func TestCallerCancellationDoesNotInvalidateCommissionedPolicy(t *testing.T) {
	m := &Manager{cfg: Config{Policy: basePolicy()}, deps: Dependencies{Clock: moduleTestClock(), PolicyCurrent: func(context.Context) error { return nil }}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.policyCurrent(ctx); err == nil {
		t.Fatal("cancelled call accepted")
	}
	if err := m.policyCurrent(context.Background()); err != nil {
		t.Fatal("caller cancellation revoked unrelated service authority", err)
	}
}

func TestPolicyRefusesAmbiguousOrUnboundedCommissioning(t *testing.T) {
	base := basePolicy()
	if err := base.validate(); err != nil {
		t.Fatal("baseline policy invalid", err)
	}
	for name, change := range map[string]func(*Policy){
		"unknown-schema":    func(p *Policy) { p.Grants[0].Publish[0].ComponentType = "wendy.unreviewed.v1" },
		"wildcard-stream":   func(p *Policy) { p.Grants[0].Publish[0].Stream = "*" },
		"duplicate-service": func(p *Policy) { p.Grants = append(p.Grants, p.Grants[0]) },
		"duplicate-frame": func(p *Policy) {
			p.Frames = append(p.Frames, world.FrameRef{ID: p.Frames[0].ID, Revision: "conflicting"})
		},
		"empty-frame":               func(p *Policy) { p.Frames = nil },
		"inverted-validity":         func(p *Policy) { p.NotAfter = p.NotBefore },
		"overlong-validity":         func(p *Policy) { p.NotAfter = p.NotBefore.Add(24*time.Hour + time.Second) },
		"uncommissioned-clock":      func(p *Policy) { p.SampleClockDomain = "arbitrary" },
		"physical-simulation-clock": func(p *Policy) { p.SampleClockDomain = "simulation:run" },
		"simulation-utc-clock":      func(p *Policy) { p.ExecutionDomain = "simulation" },
		"bad-certificate-deny-hash": func(p *Policy) { p.DeniedCertificates = []string{"not-sha256"} },
		"duplicate-key-deny-hash":   func(p *Policy) { p.DeniedKeys = []string{strings.Repeat("a", 64), strings.Repeat("a", 64)} },
		"noncanonical-deny-hash":    func(p *Policy) { p.DeniedCertificates = []string{strings.Repeat("A", 64)} },
		"empty-grants":              func(p *Policy) { p.Grants = nil },
	} {
		t.Run(name, func(t *testing.T) {
			p := clonePolicy(t, base)
			change(&p)
			if p.validate() == nil {
				t.Fatal("invalid policy accepted")
			}
		})
	}
	simulation := clonePolicy(t, base)
	simulation.ExecutionDomain = "simulation"
	simulation.SampleClockDomain = "simulation:run-1"
	if err := simulation.validate(); err != nil {
		t.Fatal("explicit simulation domain rejected", err)
	}
}

func TestPolicyMatchesExactServiceScopeAndPublicationPairs(t *testing.T) {
	p := basePolicy()
	m := &Manager{cfg: Config{Policy: p}}
	scope := p.scope(p.Grants[0], 37)
	if !m.permits(scope) {
		t.Fatal("commissioned exact scope denied")
	}
	subset := scope
	subset.Publish = append([]serviceorigin.PublishScope(nil), scope.Publish[:1]...)
	if !m.permits(subset) {
		t.Fatal("commissioned subset denied")
	}
	for name, change := range map[string]func(*serviceorigin.Scope){
		"site": func(s *serviceorigin.Scope) { s.Site = "foreign-site" },
		"device": func(s *serviceorigin.Scope) {
			s.Device = strings.Replace(s.Device, "22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333", 1)
		},
		"app":             func(s *serviceorigin.Scope) { s.App = "com.other.app" },
		"service":         func(s *serviceorigin.Scope) { s.Service = "telemetry-b" },
		"domain":          func(s *serviceorigin.Scope) { s.ExecutionDomain = "simulation" },
		"pair":            func(s *serviceorigin.Scope) { s.Publish[0].Stream = "front" },
		"zero-generation": func(s *serviceorigin.Scope) { s.DeploymentGeneration = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			copy := scope
			copy.Publish = append([]serviceorigin.PublishScope(nil), scope.Publish...)
			change(&copy)
			if m.permits(copy) {
				t.Fatal("foreign scope accepted")
			}
		})
	}
	reordered := scope
	reordered.Publish = []serviceorigin.PublishScope{scope.Publish[2], scope.Publish[0], scope.Publish[1]}
	if !equalScopes(scope, reordered) {
		t.Fatal("scope ordering changed its identity")
	}
	if scope.Publish[0] != p.scope(p.Grants[0], 37).Publish[0] {
		t.Fatal("comparison mutated scope")
	}
}
