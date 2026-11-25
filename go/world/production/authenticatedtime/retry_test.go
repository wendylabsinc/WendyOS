package authenticatedtime

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/internal/upstreamtime/roughtime"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
)

// The gate drops selected requests or forwards their unchanged random challenge
// to a real Ed25519 test server. No signing or response reconstruction occurs.
type challengeGate struct {
	mu     sync.Mutex
	nonces [][]byte
}

func (g *challengeGate) observed() [][]byte {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([][]byte, len(g.nonces))
	for i := range out {
		out[i] = bytes.Clone(g.nonces[i])
	}
	return out
}
func gated(t *testing.T, name string, accept func(int) bool) (Server, *challengeGate) {
	t.Helper()
	up := server(t, name, "", testTime)
	upstreamAddress := up.Address
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g := &challengeGate{}
	done := make(chan struct{})
	t.Cleanup(func() { _ = pc.Close(); <-done })
	go func() {
		defer close(done)
		buf := make([]byte, 8192)
		for {
			n, remote, e := pc.ReadFrom(buf)
			if e != nil {
				return
			}
			if n < 12 {
				continue
			}
			m, e := roughtime.DecodeMessage(buf[12:n])
			if e != nil {
				continue
			}
			g.mu.Lock()
			g.nonces = append(g.nonces, bytes.Clone(m[roughtime.TagNONC]))
			count := len(g.nonces)
			g.mu.Unlock()
			if !accept(count) {
				continue
			}
			conn, e := net.Dial("udp", upstreamAddress)
			if e != nil {
				return
			}
			_ = conn.SetDeadline(time.Now().Add(time.Second))
			_, e = conn.Write(buf[:n])
			if e == nil {
				n, e = conn.Read(buf)
			}
			_ = conn.Close()
			if e == nil {
				_, _ = pc.WriteTo(buf[:n], remote)
			}
		}
	}()
	up.Address = pc.LocalAddr().String()
	return up, g
}
func quickConfig(a, b Server) Config {
	c := config(a, b)
	c.QueryTimeout = 150 * time.Millisecond
	return c
}
func startupFailure(t *testing.T, err error, n int) *StartupError {
	t.Helper()
	var e *StartupError
	if !errors.As(err, &e) || len(e.Attempts) != n {
		t.Fatalf("wrong startup diagnostics: %v", err)
	}
	return e
}

func TestStartupFreshQuorumSucceedsWithoutResponseReuse(t *testing.T) {
	a, ga := gated(t, "a", func(int) bool { return true })
	b, gb := gated(t, "b", func(n int) bool { return n >= 2 })
	clock := testClock()
	var delays []time.Duration
	p, err := acquireStartup(context.Background(), quickConfig(a, b), clock.read, func(_ context.Context, d time.Duration) error { delays = append(delays, d); return nil })
	if err != nil {
		t.Fatal(err)
	}
	r := p.Report()
	if len(r.Attempts) != 2 || r.Attempts[0].Outcome != "insufficient_quorum" || r.Attempts[0].Quorum != 1 || r.Attempts[1].Outcome != "accepted" || r.Quorum != 2 || len(delays) != 1 || delays[0] != 250*time.Millisecond {
		t.Fatal("unexpected acquisition", r.Attempts, delays)
	}
	for _, g := range []*challengeGate{ga, gb} {
		n := g.observed()
		if len(n) != 2 || len(n[0]) != 32 || bytes.Equal(n[0], n[1]) {
			t.Fatal("fresh challenges missing")
		}
		found := false
		for _, e := range r.Evidence {
			if bytes.Equal(e.Nonce, n[1]) {
				found = true
			}
			if bytes.Equal(e.Nonce, n[0]) {
				t.Fatal("first response reused")
			}
		}
		if !found {
			t.Fatal("latest challenge absent")
		}
	}
	r.Attempts[0].Outcome = "mutated"
	if p.Report().Attempts[0].Outcome != "insufficient_quorum" {
		t.Fatal("report aliases provider")
	}
	clock.set(trustedtime.Reading{BootID: "other", Elapsed: 11 * time.Second})
	if _, err = p.Now(); !errors.Is(err, trustedtime.ErrUntrusted) {
		t.Fatal("successful retry lost native fence")
	}
}
func TestStartupNeverCombinesAlternatingSingleResponses(t *testing.T) {
	a, ga := gated(t, "a", func(n int) bool { return n%2 == 1 })
	b, gb := gated(t, "b", func(n int) bool { return n%2 == 0 })
	var delays []time.Duration
	p, err := acquireStartup(context.Background(), quickConfig(a, b), testClock().read, func(_ context.Context, d time.Duration) error { delays = append(delays, d); return nil })
	if p != nil {
		t.Fatal("mixed attempts became authority")
	}
	e := startupFailure(t, err, 3)
	var q *QuorumError
	if !errors.As(err, &q) || q.Quorum != 1 || len(delays) != 2 || delays[0] != 250*time.Millisecond || delays[1] != 500*time.Millisecond {
		t.Fatal("retry bounds or cause lost", err)
	}
	for _, d := range e.Attempts {
		if d.Outcome != "insufficient_quorum" || d.Quorum != 1 || d.VerifiedResponses != 1 {
			t.Fatal(d)
		}
	}
	for _, g := range []*challengeGate{ga, gb} {
		n := g.observed()
		if len(n) != 3 {
			t.Fatal("attempt bound")
		}
		for i := range n {
			for j := 0; j < i; j++ {
				if bytes.Equal(n[i], n[j]) {
					t.Fatal("nonce reuse")
				}
			}
		}
	}
	if strings.Contains(err.Error(), "127.0.0.1") || strings.Contains(err.Error(), "signature") {
		t.Fatal("raw diagnostic leaked")
	}
}
func TestStartupTerminalFailuresDoNotRetry(t *testing.T) {
	for _, mode := range []string{"no-response", "width", "configuration", "native-before"} {
		t.Run(mode, func(t *testing.T) {
			a, ga := gated(t, "a", func(int) bool { return mode != "no-response" })
			b, gb := gated(t, "b", func(int) bool { return mode != "no-response" })
			cfg := quickConfig(a, b)
			clock := testClock()
			waits := 0
			if mode == "width" {
				cfg.MaxWidth = time.Millisecond
			}
			if mode == "configuration" {
				cfg.MaxWidth = 0
			}
			if mode == "native-before" {
				clock.err = errors.New("test native failure")
			}
			p, err := acquireStartup(context.Background(), cfg, clock.read, func(context.Context, time.Duration) error { waits++; return nil })
			if p != nil || err == nil || waits != 0 {
				t.Fatal("terminal retry", err, waits)
			}
			want := 1
			if mode == "configuration" || mode == "native-before" {
				want = 0
			}
			if len(ga.observed()) != want || len(gb.observed()) != want {
				t.Fatal("unexpected exchange")
			}
			if mode == "width" {
				var bounds *IntervalBoundsError
				if !errors.As(err, &bounds) {
					t.Fatal("width error hidden", err)
				}
			}
			if mode == "native-before" && !errors.Is(err, trustedtime.ErrUntrusted) {
				t.Fatal("native cause hidden")
			}
		})
	}
}
func TestStartupNativeFaultIsCheckedBeforeWeakQuorum(t *testing.T) {
	clock := testClock()
	a := server(t, "a", "", testTime)
	b, _ := gated(t, "b", func(int) bool {
		clock.set(trustedtime.Reading{BootID: "changed", Elapsed: 10 * time.Second})
		return false
	})
	waits := 0
	p, err := acquireStartup(context.Background(), quickConfig(a, b), clock.read, func(context.Context, time.Duration) error { waits++; return nil })
	var q *QuorumError
	if p != nil || !errors.Is(err, trustedtime.ErrUntrusted) || errors.As(err, &q) || waits != 0 {
		t.Fatal("native fault became availability", err)
	}
}
func TestStartupFenceSurvivesBackoff(t *testing.T) {
	for _, mode := range []string{"rollback", "boot", "unavailable", "suspend-budget"} {
		t.Run(mode, func(t *testing.T) {
			a, ga := gated(t, "a", func(int) bool { return true })
			b, gb := gated(t, "b", func(n int) bool { return n > 1 })
			clock := testClock()
			p, err := acquireStartup(context.Background(), quickConfig(a, b), clock.read, func(context.Context, time.Duration) error {
				switch mode {
				case "rollback":
					clock.set(trustedtime.Reading{BootID: "test-only-boot", Elapsed: time.Second})
				case "boot":
					clock.set(trustedtime.Reading{BootID: "changed", Elapsed: 10 * time.Second})
				case "unavailable":
					clock.mu.Lock()
					clock.err = errors.New("test fault")
					clock.mu.Unlock()
				case "suspend-budget":
					clock.set(trustedtime.Reading{BootID: "test-only-boot", Elapsed: time.Minute})
				}
				return nil
			})
			if p != nil || err == nil || len(ga.observed()) != 1 || len(gb.observed()) != 1 {
				t.Fatal("fault retried with fresh fence", err)
			}
			startupFailure(t, err, 1)
			if mode == "suspend-budget" {
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal(err)
				}
			} else if !errors.Is(err, trustedtime.ErrUntrusted) {
				t.Fatal(err)
			}
		})
	}
}
func TestStartupCancellationAndBackoffDeadline(t *testing.T) {
	for _, mode := range []string{"before", "during", "backoff"} {
		t.Run(mode, func(t *testing.T) {
			a, ga := gated(t, "a", func(int) bool { return true })
			b, gb := gated(t, "b", func(int) bool { return false })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "before" {
				cancel()
			}
			if mode == "during" {
				timer := time.AfterFunc(10*time.Millisecond, cancel)
				defer timer.Stop()
			}
			wait := func(ctx context.Context, d time.Duration) error { cancel(); return waitStartup(ctx, d) }
			start := time.Now()
			p, err := acquireStartup(ctx, quickConfig(a, b), testClock().read, wait)
			if p != nil || !errors.Is(err, context.Canceled) || time.Since(start) > time.Second {
				t.Fatal("cancel ignored", err)
			}
			want := 1
			if mode == "before" {
				want = 0
			}
			if len(ga.observed()) != want || len(gb.observed()) != want {
				t.Fatal("cancel retried")
			}
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := waitStartup(ctx, time.Second); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("backoff deadline ignored", err)
	}
}
func TestNativeFenceFaultCannotRecover(t *testing.T) {
	clock := testClock()
	f := &nativeFence{read: clock.read}
	if _, err := f.now(); err != nil {
		t.Fatal(err)
	}
	clock.set(trustedtime.Reading{BootID: "test-only-boot", Elapsed: time.Second})
	if _, err := f.now(); !errors.Is(err, trustedtime.ErrUntrusted) {
		t.Fatal(err)
	}
	clock.set(trustedtime.Reading{BootID: "test-only-boot", Elapsed: 11 * time.Second})
	if _, err := f.now(); !errors.Is(err, trustedtime.ErrUntrusted) {
		t.Fatal("fault recovered")
	}
}
