package simclock

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
)

type testPhysical struct {
	mu  sync.Mutex
	w   trustedtime.Interval
	err error
}

func (p *testPhysical) Now() (trustedtime.Interval, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.w, p.err
}
func (p *testPhysical) advance(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.w.Earliest = p.w.Earliest.Add(d)
	p.w.Latest = p.w.Latest.Add(d)
	p.w.Monotonic += d
}

type fixture struct {
	t        *testing.T
	mu       sync.Mutex
	physical *testPhysical
	server   *httptest.Server
	provider *Provider
	cfg      Config
	sim      int64
	seq      uint64
	mutate   func(*response)
	status   int
	raw      string
	last     response
}

func setup(t *testing.T) *fixture {
	t.Helper()
	base := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	f := &fixture{t: t, physical: &testPhysical{w: trustedtime.Interval{Earliest: base.Add(-time.Millisecond), Latest: base.Add(time.Millisecond), BootID: "host-boot", Domain: "physical", Authenticated: true, Source: "test-authenticated-anchor"}}, sim: int64(time.Second)}
	f.cfg = Config{Token: strings.Repeat("r", 48), RunID: "clock-run", BootID: "scene-boot", PackageSHA256: strings.Repeat("a", 64), Domain: "simulation:clock-run", Frame: world.FrameRef{ID: "office-floor", Revision: "survey-1"}, Anchor: base.Add(-time.Hour), Physical: f.physical, RequestTimeout: 200 * time.Millisecond}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Method != "GET" || r.URL.Path != "/v1/clock" || r.Header.Get("Authorization") != "Bearer "+f.cfg.Token {
			http.Error(w, "denied", 401)
			return
		}
		if f.status != 0 {
			w.WriteHeader(f.status)
			return
		}
		if f.raw != "" {
			_, _ = w.Write([]byte(f.raw))
			return
		}
		f.seq++
		physical, _ := f.physical.Now()
		ns := f.sim
		v := response{Schema: Schema, RunID: f.cfg.RunID, BootID: f.cfg.BootID, PackageSHA256: f.cfg.PackageSHA256, Frame: f.cfg.Frame, Domain: f.cfg.Domain, AnchorNS: f.cfg.Anchor.UnixNano(), SimulationNS: &ns, CaptureNS: physical.Earliest.Add(time.Millisecond).UnixNano(), Sequence: f.seq, Nonce: r.Header.Get(NonceHeader)}
		if f.mutate != nil {
			f.mutate(&v)
		}
		f.last = v
		_ = json.NewEncoder(w).Encode(v)
	}))
	f.cfg.URL = f.server.URL
	var err error
	f.provider, err = New(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.provider.Close(); f.server.Close() })
	return f
}

func (f *fixture) change(fn func(*response)) { f.mu.Lock(); defer f.mu.Unlock(); f.mutate = fn }
func (f *fixture) nowOK() time.Time {
	f.t.Helper()
	v, err := f.provider.Now()
	if err != nil {
		f.t.Fatal(err)
	}
	return v
}
func (f *fixture) wantError(want error) {
	f.t.Helper()
	v, err := f.provider.Now()
	if !v.IsZero() || !errors.Is(err, want) {
		f.t.Fatalf("Now = %v, %v; want zero, %v", v, err, want)
	}
}

func TestFixedAnchorAndPausedSource(t *testing.T) {
	f := setup(t)
	first := f.nowOK()
	if !first.Equal(f.cfg.Anchor.Add(time.Second)) {
		t.Fatal("source clock rebased")
	}
	f.physical.advance(2 * time.Second)
	paused := f.nowOK()
	if !paused.Equal(first) {
		t.Fatal("physical elapsed time advanced paused simulation")
	}
	f.mu.Lock()
	if f.last.Sequence != 2 {
		t.Fatal("clock observation sequence did not advance")
	}
	f.sim += int64(50 * time.Millisecond)
	f.mu.Unlock()
	if !f.nowOK().Equal(first.Add(50 * time.Millisecond)) {
		t.Fatal("source advancement lost")
	}
}

func TestSourceSilenceDoesNotServeCachedTime(t *testing.T) {
	f := setup(t)
	f.nowOK()
	f.mu.Lock()
	f.status = 503
	f.mu.Unlock()
	f.wantError(ErrUnavailable)
	f.mu.Lock()
	f.status = 0
	f.mu.Unlock()
	f.nowOK()
	f.server.Close()
	f.wantError(ErrUnavailable)
}

func TestStaleAndFuturePhysicalObservations(t *testing.T) {
	for _, offset := range []time.Duration{-time.Second, time.Second} {
		t.Run(offset.String(), func(t *testing.T) {
			f := setup(t)
			f.change(func(r *response) { r.CaptureNS += int64(offset) })
			f.wantError(ErrUnavailable)
			f.change(nil)
			f.nowOK()
		})
	}
	t.Run("uncertainty_consumes_age", func(t *testing.T) {
		f := setup(t)
		f.physical.mu.Lock()
		f.physical.w.Earliest = f.physical.w.Earliest.Add(-8 * time.Second)
		f.physical.w.Latest = f.physical.w.Latest.Add(8 * time.Second)
		f.physical.mu.Unlock()
		f.wantError(ErrUnavailable)
	})
}

func TestIdentityChangesAndRegressionsLatch(t *testing.T) {
	cases := map[string]func(*response){
		"boot":                    func(r *response) { r.BootID = "new-boot" },
		"run":                     func(r *response) { r.RunID = "new-run" },
		"package":                 func(r *response) { r.PackageSHA256 = strings.Repeat("b", 64) },
		"frame":                   func(r *response) { r.Frame.Revision = "other" },
		"domain":                  func(r *response) { r.Domain = "physical" },
		"anchor":                  func(r *response) { r.AnchorNS++ },
		"sequence_replay":         func(r *response) { r.Sequence = 1 },
		"nonce_replay":            func(r *response) { r.Nonce = strings.Repeat("0", 64) },
		"simulation_rewind":       func(r *response) { *r.SimulationNS = 0 },
		"observation_rewind":      func(r *response) { r.CaptureNS-- },
		"missing_simulation_time": func(r *response) { r.SimulationNS = nil },
		"overflow":                func(r *response) { *r.SimulationNS = 1<<63 - 1 },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			f.nowOK()
			f.change(change)
			f.wantError(ErrFaulted)
			f.change(nil)
			f.wantError(ErrFaulted)
		})
	}
}

func TestExactResponseReplayRejected(t *testing.T) {
	f := setup(t)
	f.nowOK()
	f.mu.Lock()
	b, _ := json.Marshal(f.last)
	f.raw = string(b)
	f.mu.Unlock()
	f.wantError(ErrFaulted)
}

func TestPhysicalTimeFailureAndRollback(t *testing.T) {
	t.Run("unavailable", func(t *testing.T) {
		f := setup(t)
		f.nowOK()
		f.physical.mu.Lock()
		f.physical.err = trustedtime.ErrUntrusted
		f.physical.mu.Unlock()
		f.wantError(ErrUnavailable)
	})
	t.Run("regression", func(t *testing.T) {
		f := setup(t)
		f.physical.advance(time.Second)
		f.nowOK()
		f.physical.advance(-time.Millisecond)
		f.wantError(ErrFaulted)
	})
	t.Run("host_boot", func(t *testing.T) {
		f := setup(t)
		f.nowOK()
		f.physical.mu.Lock()
		f.physical.w.BootID = "other-host"
		f.physical.mu.Unlock()
		f.wantError(ErrFaulted)
	})
	t.Run("unauthenticated", func(t *testing.T) {
		f := setup(t)
		f.physical.mu.Lock()
		f.physical.w.Authenticated = false
		f.physical.mu.Unlock()
		f.wantError(ErrFaulted)
	})
}

func TestBoundedBodyAndStrictJSON(t *testing.T) {
	t.Run("oversize", func(t *testing.T) {
		f := setup(t)
		f.mu.Lock()
		f.raw = strings.Repeat(" ", MaxResponseBytes+1)
		f.mu.Unlock()
		f.wantError(ErrUnavailable)
	})
	t.Run("duplicate", func(t *testing.T) {
		f := setup(t)
		f.mu.Lock()
		f.raw = `{"schema_version":"a","schema_version":"b"}`
		f.mu.Unlock()
		f.wantError(ErrFaulted)
	})
}

func TestRedirectDoesNotForwardReadToken(t *testing.T) {
	var received atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { received.Add(1) }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 302) }))
	defer source.Close()
	f := setup(t)
	cfg := f.cfg
	cfg.URL = source.URL
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	_, err = p.Now()
	if !errors.Is(err, ErrUnavailable) || received.Load() != 0 {
		t.Fatalf("redirect leaked request: %v count=%d", err, received.Load())
	}
}

func TestCloseCancelsPendingRequest(t *testing.T) {
	entered := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-r.Context().Done() }))
	defer s.Close()
	f := setup(t)
	cfg := f.cfg
	cfg.URL = s.URL
	cfg.RequestTimeout = time.Second
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { _, e := p.Now(); result <- e }()
	<-entered
	p.Close()
	select {
	case e := <-result:
		if !errors.Is(e, ErrClosed) {
			t.Fatal(e)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("Close did not cancel request")
	}
	_, err = p.Now()
	if !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
}

func TestSilentSourceAndSuspendElapsedAreBounded(t *testing.T) {
	t.Run("silent_http", func(t *testing.T) {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
		defer s.Close()
		f := setup(t)
		cfg := f.cfg
		cfg.URL, cfg.RequestTimeout = s.URL, 30*time.Millisecond
		p, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer p.Close()
		started := time.Now()
		value, err := p.Now()
		if !value.IsZero() || !errors.Is(err, ErrUnavailable) || time.Since(started) > time.Second {
			t.Fatalf("silent request not bounded: %v %v", value, err)
		}
	})
	t.Run("suspend_inclusive_elapsed", func(t *testing.T) {
		f := setup(t)
		f.change(func(r *response) { f.physical.advance(time.Second) })
		f.wantError(ErrUnavailable)
	})
}

func TestParallelReadsAreSerialized(t *testing.T) {
	f := setup(t)
	var wg sync.WaitGroup
	errorsCh := make(chan error, 12)
	read := func() {
		defer wg.Done()
		for i := 0; i < 4; i++ {
			_, e := f.provider.Now()
			errorsCh <- e
		}
	}
	wg.Add(3)
	go read()
	go read()
	go read()
	wg.Wait()
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.seq != 12 {
		t.Fatal(f.seq)
	}
}

func TestConfigurationRefusesUncommissionedOrCredentialBearingURL(t *testing.T) {
	f := setup(t)
	cases := []func(*Config){func(c *Config) { c.URL = "http://user:secret@localhost:8080" }, func(c *Config) { c.URL += "/other" }, func(c *Config) { c.URL += "?token=x" }, func(c *Config) { c.Token = "" }, func(c *Config) { c.Domain = "physical" }, func(c *Config) { c.BootID = "" }, func(c *Config) { c.Anchor = time.Time{} }, func(c *Config) { c.Physical = nil }}
	for _, change := range cases {
		cfg := f.cfg
		change(&cfg)
		if p, err := New(cfg); err == nil {
			p.Close()
			t.Fatal("invalid configuration accepted")
		}
	}
}
