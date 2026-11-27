package appclock

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
)

type clockFunc func(context.Context, *appv1.ClockRequest) (*appv1.ClockReply, error)

func (f clockFunc) Clock(ctx context.Context, r *appv1.ClockRequest, _ ...grpc.CallOption) (*appv1.ClockReply, error) {
	return f(ctx, r)
}

func (f clockFunc) PhysicalClock(ctx context.Context, r *appv1.ClockRequest, _ ...grpc.CallOption) (*appv1.PhysicalClockReply, error) {
	v, err := f(ctx, r)
	return physicalReply(v), err
}
func physicalReply(r *appv1.ClockReply) *appv1.PhysicalClockReply {
	if r == nil {
		return nil
	}
	return &appv1.PhysicalClockReply{Nonce: bytes.Clone(r.Nonce), Physical: r.Physical, Tenant: r.Tenant, Site: r.Site, Device: r.Device, App: r.App, Service: r.Service, ExecutionDomain: r.ExecutionDomain, DeploymentGeneration: r.DeploymentGeneration, ServerBootId: r.ServerBootId}
}

type clockFixture struct {
	mu                           sync.Mutex
	cfg                          Config
	reading                      trustedtime.Reading
	readErr                      error
	reply                        *appv1.ClockReply
	delay                        time.Duration
	mutate                       func(*appv1.ClockReply)
	rpcErr                       error
	calls                        int
	combinedCalls, physicalCalls int
	mutatePhysical               func(*appv1.PhysicalClockReply)
	nonces                       [][]byte
}

func newFixture(t *testing.T) *clockFixture {
	t.Helper()
	tenant := "fd3b13fb-e19a-4b57-83b7-32d3d332a010"
	cfg := Config{Tenant: tenant, Site: "office", Device: "spiffe://wendy.sh/tenant/" + tenant + "/device/725cadb6-48f4-461f-a358-3611f8ee0b02", App: "com.wendy.world.reader", Service: "fusion", ExecutionDomain: "simulation", SampleClockDomain: "simulation:run-a", ExpectedGeneration: 9, MaxRoundTrip: time.Second, MaxWidth: 3 * time.Second, DriftPPM: 1000}
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	r := &appv1.ClockReply{Physical: &appv1.PhysicalClock{EarliestUnixNs: now.UnixNano(), LatestUnixNs: now.Add(time.Millisecond).UnixNano(), BootId: "remote-host-boot", MonotonicNs: int64(5 * time.Second), Source: "wendy-roughtime-quorum"}, SampleClockDomain: cfg.SampleClockDomain, SampleUnixNs: now.Add(-time.Hour).UnixNano(), Tenant: cfg.Tenant, Site: cfg.Site, Device: cfg.Device, App: cfg.App, Service: cfg.Service, ExecutionDomain: cfg.ExecutionDomain, DeploymentGeneration: cfg.ExpectedGeneration, ServerBootId: strings.Repeat("a", 32)}
	return &clockFixture{cfg: cfg, reading: trustedtime.Reading{BootID: "local-host-boot", Elapsed: time.Second}, reply: r, delay: 20 * time.Millisecond}
}
func (f *clockFixture) read() (trustedtime.Reading, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reading, f.readErr
}
func (f *clockFixture) Clock(_ context.Context, req *appv1.ClockRequest, _ ...grpc.CallOption) (*appv1.ClockReply, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.combinedCalls++
	return f.respond(req)
}
func (f *clockFixture) PhysicalClock(_ context.Context, req *appv1.ClockRequest, _ ...grpc.CallOption) (*appv1.PhysicalClockReply, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.physicalCalls++
	r, err := f.respond(req)
	p := physicalReply(r)
	if p != nil && f.mutatePhysical != nil {
		f.mutatePhysical(p)
	}
	return p, err
}
func (f *clockFixture) respond(req *appv1.ClockRequest) (*appv1.ClockReply, error) {
	f.calls++
	f.nonces = append(f.nonces, bytes.Clone(req.Nonce))
	f.reading.Elapsed += f.delay
	if f.rpcErr != nil {
		return nil, f.rpcErr
	}
	r := proto.Clone(f.reply).(*appv1.ClockReply)
	r.Nonce = bytes.Clone(req.Nonce)
	if f.mutate != nil {
		f.mutate(r)
	}
	return r, nil
}
func (f *clockFixture) provider(t *testing.T) *Provider {
	t.Helper()
	p, err := newProvider(f, f.cfg, f.read, bytes.NewReader(bytes.Repeat([]byte{1, 2, 3, 4, 5, 6, 7, 8, 9}, 4096)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func TestRoundTripDriftAndPausedSource(t *testing.T) {
	f := newFixture(t)
	f.delay = 900 * time.Millisecond
	p := f.provider(t)
	first, err := p.Observe(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	wantLatest := f.reply.Physical.LatestUnixNs + int64(f.delay) + int64(driftBound(f.delay, f.cfg.DriftPPM))
	if first.Physical.Earliest.UnixNano() != f.reply.Physical.EarliestUnixNs || first.Physical.Latest.UnixNano() != wantLatest || first.Physical.BootID != f.reading.BootID || first.Physical.Monotonic != f.reading.Elapsed || first.RoundTrip != f.delay || first.SampleTime.UnixNano() != f.reply.SampleUnixNs {
		t.Fatalf("wrong receipt bounds: %+v", first)
	}
	f.delay = time.Millisecond
	f.reply.Physical.EarliestUnixNs += int64(100 * time.Millisecond)
	f.reply.Physical.LatestUnixNs += int64(100 * time.Millisecond)
	f.reply.Physical.MonotonicNs += int64(100 * time.Millisecond)
	second, err := p.Observe(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !second.Physical.Latest.Equal(first.Physical.Latest) || !second.Physical.Earliest.After(first.Physical.Earliest) || !second.SampleTime.Equal(first.SampleTime) {
		t.Fatalf("RTT jitter or pause changed time incorrectly: %+v", second)
	}
	if bytes.Equal(f.nonces[0], f.nonces[1]) || len(f.nonces[0]) != appv1.ClockNonceSize {
		t.Fatal("challenge was reused")
	}
}

func TestIdentityReplayAndRemoteClockFaultsLatch(t *testing.T) {
	cases := map[string]func(*appv1.ClockReply){
		"nonce":             func(r *appv1.ClockReply) { r.Nonce[0] ^= 1 },
		"tenant":            func(r *appv1.ClockReply) { r.Tenant = "3dc4fd79-fc22-4baf-8047-6eb4ca9c03b9" },
		"site":              func(r *appv1.ClockReply) { r.Site = "other" },
		"device":            func(r *appv1.ClockReply) { r.Device = strings.TrimSuffix(r.Device, "b02") + "b03" },
		"app":               func(r *appv1.ClockReply) { r.App = "com.example.other" },
		"service":           func(r *appv1.ClockReply) { r.Service = "other" },
		"execution":         func(r *appv1.ClockReply) { r.ExecutionDomain = "physical"; r.SampleClockDomain = "utc" },
		"sample-domain":     func(r *appv1.ClockReply) { r.SampleClockDomain = "simulation:other" },
		"generation":        func(r *appv1.ClockReply) { r.DeploymentGeneration++ },
		"zero-generation":   func(r *appv1.ClockReply) { r.DeploymentGeneration = 0 },
		"server-restart":    func(r *appv1.ClockReply) { r.ServerBootId = strings.Repeat("b", 32) },
		"host-restart":      func(r *appv1.ClockReply) { r.Physical.BootId = "new-host-boot" },
		"remote-monotonic":  func(r *appv1.ClockReply) { r.Physical.MonotonicNs-- },
		"remote-earliest":   func(r *appv1.ClockReply) { r.Physical.EarliestUnixNs-- },
		"remote-latest":     func(r *appv1.ClockReply) { r.Physical.LatestUnixNs-- },
		"sample-regression": func(r *appv1.ClockReply) { r.SampleUnixNs-- },
		"time-source":       func(r *appv1.ClockReply) { r.Physical.Source = "different-authenticated-source" },
		"fake-physical":     func(r *appv1.ClockReply) { r.Physical.Source = "simulation" },
		"unknown-top":       func(r *appv1.ClockReply) { r.ProtoReflect().SetUnknown([]byte{0xf8, 0x01, 0x01}) },
		"unknown-physical":  func(r *appv1.ClockReply) { r.Physical.ProtoReflect().SetUnknown([]byte{0xf8, 0x01, 0x01}) },
		"no-physical":       func(r *appv1.ClockReply) { r.Physical = nil },
		"oversized-width":   func(r *appv1.ClockReply) { r.Physical.LatestUnixNs += int64(4 * time.Second) },
		"overflow": func(r *appv1.ClockReply) {
			r.Physical.EarliestUnixNs = math.MaxInt64 - int64(time.Millisecond)
			r.Physical.LatestUnixNs = math.MaxInt64
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			p := f.provider(t)
			if _, err := p.Observe(t.Context()); err != nil {
				t.Fatal(err)
			}
			f.mutate = mutate
			if w, err := p.Observe(t.Context()); !errors.Is(err, ErrFaulted) || w != (Observation{}) {
				t.Fatalf("fault accepted: %+v %v", w, err)
			}
			f.mutate = nil
			calls := f.calls
			if _, err := p.Observe(t.Context()); !errors.Is(err, ErrFaulted) || f.calls != calls {
				t.Fatal("fault recovered or sent another RPC")
			}
		})
	}
}

func TestLocalSuspendRebootRegressionAndFailure(t *testing.T) {
	for _, name := range []string{"reboot", "rollback", "unavailable", "suspend"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			p := f.provider(t)
			if _, err := p.Now(); err != nil {
				t.Fatal(err)
			}
			switch name {
			case "reboot":
				f.reading.BootID = "another-local-boot"
			case "rollback":
				f.reading.Elapsed = 0
			case "unavailable":
				f.readErr = io.ErrUnexpectedEOF
			case "suspend":
				f.delay = 2 * time.Second
			}
			w, err := p.Now()
			want := ErrFaulted
			if name == "suspend" {
				want = ErrUnavailable
			}
			if !errors.Is(err, want) || w != (trustedtime.Interval{}) {
				t.Fatalf("local fault returned authority: %+v %v", w, err)
			}
		})
	}
}

func TestUnavailableNeverReturnsCachedSource(t *testing.T) {
	f := newFixture(t)
	p := f.provider(t)
	if _, err := p.Observe(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.rpcErr = errors.New("private endpoint detail")
	if o, err := p.Observe(t.Context()); !errors.Is(err, ErrUnavailable) || o != (Observation{}) || errors.Is(err, f.rpcErr) {
		t.Fatal("failed RPC returned a stale observation or leaked its error")
	}
	f.rpcErr = nil
	if _, err := p.Now(); err != nil {
		t.Fatalf("transient failure did not recover: %v", err)
	}
}

func TestInitialPinsAndExpectedIncarnation(t *testing.T) {
	f := newFixture(t)
	f.cfg.ExpectedGeneration = 0
	f.cfg.ExpectedServerBootID = f.reply.ServerBootId
	p := f.provider(t)
	o, err := p.Observe(t.Context())
	if err != nil || o.DeploymentGeneration != 9 {
		t.Fatalf("initial private socket pin: %+v %v", o, err)
	}
	f.reply.DeploymentGeneration++
	if _, err := p.Now(); !errors.Is(err, ErrFaulted) {
		t.Fatal("generation changed after initial pin")
	}
	f = newFixture(t)
	f.cfg.ExpectedServerBootID = strings.Repeat("b", 32)
	p = f.provider(t)
	if _, err := p.Now(); !errors.Is(err, ErrFaulted) {
		t.Fatal("wrong commissioned incarnation accepted")
	}
}

func TestSampleNowUsesExactSimulationAndConservativeUTC(t *testing.T) {
	f := newFixture(t)
	if got, err := f.provider(t).SampleNow(); err != nil || got.UnixNano() != f.reply.SampleUnixNs {
		t.Fatalf("simulation time extrapolated: %v %v", got, err)
	}
	f = newFixture(t)
	f.cfg.ExecutionDomain, f.cfg.SampleClockDomain = "physical", "utc"
	f.reply.ExecutionDomain, f.reply.SampleClockDomain = "physical", "utc"
	want := f.reply.Physical.LatestUnixNs + int64(f.delay+driftBound(f.delay, f.cfg.DriftPPM))
	p := f.provider(t)
	if got, err := p.SampleNow(); err != nil || got.UnixNano() != want {
		t.Fatalf("UTC omitted transit: %v %v", got, err)
	}
	f.rpcErr = io.ErrUnexpectedEOF
	if got, err := p.SampleNow(); !errors.Is(err, ErrUnavailable) || !got.IsZero() {
		t.Fatalf("source outage returned cached time: %v %v", got, err)
	}
}

func TestDecodeWorkIncludedAndEntropyFailure(t *testing.T) {
	f := newFixture(t)
	reads := 0
	read := func() (trustedtime.Reading, error) {
		reads++
		r, err := f.read()
		if reads == 3 {
			r.Elapsed += 7 * time.Millisecond
		}
		return r, err
	}
	p, err := newProvider(f, f.cfg, read, bytes.NewReader(bytes.Repeat([]byte{7}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	o, err := p.Observe(t.Context())
	if err != nil || o.RoundTrip != f.delay+7*time.Millisecond {
		t.Fatalf("final read was omitted: %+v %v", o, err)
	}
	p, err = newProvider(f, f.cfg, f.read, bytes.NewReader(nil))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	calls := f.calls
	if _, err := p.Now(); !errors.Is(err, ErrFaulted) || f.calls != calls {
		t.Fatal("missing nonce entropy sent RPC")
	}
}

func TestBoundsConfiguration(t *testing.T) {
	cases := map[string]func(*Config){
		"identity":          func(c *Config) { c.Device = "device" },
		"missing-domain":    func(c *Config) { c.SampleClockDomain = "" },
		"empty-run":         func(c *Config) { c.SampleClockDomain = "simulation:" },
		"domain-whitespace": func(c *Config) { c.SampleClockDomain += " " },
		"physical-domain":   func(c *Config) { c.ExecutionDomain = "physical" },
		"zero-timeout":      func(c *Config) { c.MaxRoundTrip = 0 },
		"long-timeout":      func(c *Config) { c.MaxRoundTrip = time.Second + 1 },
		"zero-width":        func(c *Config) { c.MaxWidth = 0 },
		"long-width":        func(c *Config) { c.MaxWidth = 25 * time.Hour },
		"zero-drift":        func(c *Config) { c.DriftPPM = 0 },
		"high-drift":        func(c *Config) { c.DriftPPM = 1_000_001 },
		"bad-incarnation":   func(c *Config) { c.ExpectedServerBootID = "boot" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			mutate(&f.cfg)
			if p, err := New(f, f.cfg); !errors.Is(err, ErrConfig) || p != nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
	f := newFixture(t)
	if p, err := New(nil, f.cfg); !errors.Is(err, ErrConfig) || p != nil {
		t.Fatal("nil client accepted")
	}
	f.cfg.ExecutionDomain, f.cfg.SampleClockDomain = "physical", "utc"
	f.reply.ExecutionDomain, f.reply.SampleClockDomain = "physical", "utc"
	if _, err := f.provider(t).Now(); err != nil {
		t.Fatal(err)
	}
}

func TestCloseAndDeadlineBoundConcurrentWaiters(t *testing.T) {
	f := newFixture(t)
	f.cfg.MaxRoundTrip = 80 * time.Millisecond
	entered := make(chan struct{})
	client := clockFunc(func(ctx context.Context, _ *appv1.ClockRequest) (*appv1.ClockReply, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	p, err := newProvider(client, f.cfg, f.read, bytes.NewReader(bytes.Repeat([]byte{7}, 128)))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 2)
	go func() { _, err := p.Now(); done <- err }()
	<-entered
	ctx, cancel := context.WithCancel(t.Context())
	go func() { _, err := p.Observe(ctx); done <- err }()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("queued caller not canceled: %v", err)
	}
	_ = p.Close()
	if err := <-done; !errors.Is(err, ErrClosed) {
		t.Fatalf("pending RPC not closed: %v", err)
	}
	if _, err := p.Now(); !errors.Is(err, ErrClosed) {
		t.Fatal("closed provider reopened")
	}
	client = clockFunc(func(ctx context.Context, _ *appv1.ClockRequest) (*appv1.ClockReply, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	p, err = newProvider(client, f.cfg, f.read, bytes.NewReader(bytes.Repeat([]byte{9}, 64)))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if o, err := p.Observe(t.Context()); !errors.Is(err, context.DeadlineExceeded) || o != (Observation{}) {
		t.Fatalf("unbounded or stale deadline: %+v %v", o, err)
	}
}

type socketClockServer struct {
	appv1.UnimplementedWorldAppServiceServer
	fixture   *clockFixture
	oversized bool
}

func (s *socketClockServer) Clock(ctx context.Context, r *appv1.ClockRequest) (*appv1.ClockReply, error) {
	reply, err := s.fixture.Clock(ctx, r)
	if reply != nil && s.oversized {
		reply.Physical.Source = strings.Repeat("x", 8192)
	}
	return reply, err
}

func (s *socketClockServer) PhysicalClock(ctx context.Context, r *appv1.ClockRequest) (*appv1.PhysicalClockReply, error) {
	reply, err := s.fixture.PhysicalClock(ctx, r)
	if reply != nil && s.oversized {
		reply.Physical.Source = strings.Repeat("x", 8192)
	}
	return reply, err
}

func TestRealUnixRPCAndNativeBootClock(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("native suspend-inclusive clock unsupported")
	}
	r1, err := readNative()
	if err != nil {
		t.Fatal(err)
	}
	r2, err := readNative()
	if err != nil || r1.BootID == "" || r1.BootID != r2.BootID || r2.Elapsed < r1.Elapsed {
		t.Fatalf("native clock: %+v %+v %v", r1, r2, err)
	}
	for _, oversized := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "bounded-receive"}[oversized], func(t *testing.T) {
			f := newFixture(t)
			dir, err := os.MkdirTemp("/tmp", "world-appclock-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(dir) })
			path := filepath.Join(dir, "clock.sock")
			listener, err := net.Listen("unix", path)
			if err != nil {
				t.Fatal(err)
			}
			server := grpc.NewServer()
			appv1.RegisterWorldAppServiceServer(server, &socketClockServer{fixture: f, oversized: oversized})
			go server.Serve(listener)
			t.Cleanup(server.Stop)
			conn, err := grpc.NewClient("passthrough:///clock", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", path)
			}), grpc.WithDisableRetry())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = conn.Close() })
			p, err := New(appv1.NewWorldAppServiceClient(conn), f.cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = p.Close() })
			o, err := p.Observe(t.Context())
			if oversized {
				if !errors.Is(err, ErrUnavailable) || o != (Observation{}) {
					t.Fatal("oversized response accepted")
				}
				if w, e := p.PhysicalObserve(t.Context()); !errors.Is(e, ErrUnavailable) || w != (PhysicalObservation{}) {
					t.Fatal("oversized physical response accepted")
				}
				return
			}
			if err != nil || o.Physical.BootID != r1.BootID || o.Physical.Monotonic < r1.Elapsed || o.SampleTime.UnixNano() != f.reply.SampleUnixNs {
				t.Fatalf("real RPC: %+v %v", o, err)
			}
			physical, err := p.PhysicalObserve(t.Context())
			if err != nil || physical.Physical.BootID != r1.BootID || f.physicalCalls != 1 || f.combinedCalls != 1 {
				t.Fatal("real physical-only RPC", err)
			}
		})
	}
}
