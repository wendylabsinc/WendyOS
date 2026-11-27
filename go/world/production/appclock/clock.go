// Package appclock obtains authenticated physical time and exact source time
// from a commissioned, service-scoped Wendy agent socket. It has no wall-clock
// fallback, Internet time acquisition, or cached source-time extrapolation.
package appclock

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"google.golang.org/grpc"
)

var (
	ErrConfig      = errors.New("invalid commissioned app clock")
	ErrUnavailable = errors.New("agent clock unavailable")
	ErrFaulted     = errors.New("app clock fault requires recommissioning")
	ErrClosed      = errors.New("app clock closed")
)

// ClockClient must use the agent-owned private Unix socket mounted for this
// service. A nonce and echoed identity do not authenticate an arbitrary server.
type ClockClient interface {
	Clock(context.Context, *appv1.ClockRequest, ...grpc.CallOption) (*appv1.ClockReply, error)
	PhysicalClock(context.Context, *appv1.ClockRequest, ...grpc.CallOption) (*appv1.PhysicalClockReply, error)
}

type Config struct {
	Tenant, Site, Device, App, Service string
	ExecutionDomain, SampleClockDomain string
	// Zero learns the nonzero generation from the first valid socket response.
	// The commissioned mount establishes that initial deployment identity.
	ExpectedGeneration uint64
	// Empty learns the incarnation from the first valid response. Any later
	// agent/module restart requires a new Provider, including on the same host.
	ExpectedServerBootID   string
	MaxRoundTrip, MaxWidth time.Duration
	DriftPPM               uint64
}

// Observation contains a conservative physical interval at local receipt and
// the exact source instant returned by the agent. SampleTime does not advance
// with physical elapsed time, including while a simulation is paused.
type Observation struct {
	Physical             trustedtime.Interval
	SampleTime           time.Time
	SampleClockDomain    string
	ServerBootID         string
	DeploymentGeneration uint64
	RoundTrip            time.Duration
}

// PhysicalObservation contains fresh authority time and the reader identity.
// It carries no source sample time and grants no reusable authority lease.
type PhysicalObservation struct {
	Physical             trustedtime.Interval
	ServerBootID         string
	DeploymentGeneration uint64
	RoundTrip            time.Duration
}

type Provider struct {
	cfg                    Config
	client                 ClockClient
	read                   func() (trustedtime.Reading, error)
	entropy                io.Reader
	ctx                    context.Context
	cancel                 context.CancelFunc
	gate                   chan struct{}
	fault, seen, localSeen bool
	local                  trustedtime.Reading
	remote                 trustedtime.Interval
	last                   PhysicalObservation
	sampleSeen             bool
	sampleLast             time.Time
}

var _ trustedtime.Provider = (*Provider)(nil)

func New(client ClockClient, cfg Config) (*Provider, error) {
	return newProvider(client, cfg, readNative, rand.Reader)
}

func newProvider(client ClockClient, cfg Config, read func() (trustedtime.Reading, error), entropy io.Reader) (*Provider, error) {
	scope := serviceorigin.Scope{Tenant: cfg.Tenant, Site: cfg.Site, Device: cfg.Device, App: cfg.App, Service: cfg.Service, DeploymentGeneration: max(cfg.ExpectedGeneration, 1), ExecutionDomain: cfg.ExecutionDomain}
	if client == nil || read == nil || entropy == nil || scope.Validate() != nil || cfg.MaxRoundTrip < time.Millisecond || cfg.MaxRoundTrip > appv1.MaxClockResponseAge || cfg.MaxWidth <= 0 || cfg.MaxWidth > 24*time.Hour || cfg.DriftPPM == 0 || cfg.DriftPPM > 1_000_000 || !domainValid(cfg) {
		return nil, ErrConfig
	}
	if cfg.ExpectedServerBootID != "" {
		id, err := hex.DecodeString(cfg.ExpectedServerBootID)
		if err != nil || len(id) != 16 || hex.EncodeToString(id) != cfg.ExpectedServerBootID {
			return nil, ErrConfig
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Provider{cfg: cfg, client: client, read: read, entropy: entropy, ctx: ctx, cancel: cancel, gate: make(chan struct{}, 1)}, nil
}

func domainValid(c Config) bool {
	if c.ExecutionDomain == "physical" {
		return c.SampleClockDomain == "utc"
	}
	s := c.SampleClockDomain
	if !strings.HasPrefix(s, "simulation:") || len(s) <= len("simulation:") || len(s) > 256 || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r <= 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// Close cancels pending calls and gate waiters. The injected client connection
// belongs to the caller and is not closed here.
func (p *Provider) Close() error {
	if p != nil {
		p.cancel()
	}
	return nil
}

func (p *Provider) Now() (trustedtime.Interval, error) {
	o, err := p.PhysicalObserve(context.Background())
	return o.Physical, err
}

// SampleNow returns a fresh source time for TTL checks. Simulation remains the
// exact observed instant. UTC uses the transit-expanded physical upper bound.
func (p *Provider) SampleNow() (time.Time, error) {
	o, err := p.Observe(context.Background())
	if err != nil {
		return time.Time{}, err
	}
	if o.SampleClockDomain == "utc" {
		return o.Physical.Latest, nil
	}
	return o.SampleTime, nil
}

func (p *Provider) fail() (Observation, error) {
	p.fault = true
	return Observation{}, ErrFaulted
}

func (p *Provider) readLocal() (trustedtime.Reading, error) {
	r, err := p.read()
	if err != nil || r.BootID == "" || len(r.BootID) > 256 || r.Elapsed < 0 || p.localSeen && (r.BootID != p.local.BootID || r.Elapsed < p.local.Elapsed) {
		p.fault = true
		return trustedtime.Reading{}, ErrFaulted
	}
	p.localSeen, p.local = true, r
	return r, nil
}

// Observe makes one fresh challenge-bound RPC. It accounts for the entire
// local suspend-inclusive interval, with no symmetric-delay assumption. RPC
// errors return no cached value; identity, replay and clock faults latch.
func (p *Provider) Observe(ctx context.Context) (Observation, error) {
	return p.observe(ctx, false)
}

// PhysicalObserve makes a fresh physical-only RPC. It uses the same identity,
// raw physical continuity and receipt-bound checks as Observe, without asking
// for simulation time. Unsupported endpoints fail; there is no fallback.
func (p *Provider) PhysicalObserve(ctx context.Context) (PhysicalObservation, error) {
	o, err := p.observe(ctx, true)
	if err != nil {
		return PhysicalObservation{}, err
	}
	return PhysicalObservation{Physical: o.Physical, ServerBootID: o.ServerBootID, DeploymentGeneration: o.DeploymentGeneration, RoundTrip: o.RoundTrip}, nil
}

func (p *Provider) observe(ctx context.Context, physicalOnly bool) (Observation, error) {
	if p == nil {
		return Observation{}, ErrClosed
	}
	if ctx == nil {
		return Observation{}, ErrConfig
	}
	callCtx, cancel := context.WithTimeout(ctx, p.cfg.MaxRoundTrip)
	defer cancel()
	stop := context.AfterFunc(p.ctx, cancel)
	defer stop()
	select {
	case p.gate <- struct{}{}:
		defer func() { <-p.gate }()
	case <-callCtx.Done():
		return Observation{}, p.unavailable(callCtx)
	}
	if p.ctx.Err() != nil {
		return Observation{}, ErrClosed
	}
	if p.fault {
		return Observation{}, ErrFaulted
	}
	if callCtx.Err() != nil {
		return Observation{}, p.unavailable(callCtx)
	}
	start, err := p.readLocal()
	if err != nil {
		return Observation{}, err
	}
	nonce := make([]byte, appv1.ClockNonceSize)
	if _, err := io.ReadFull(p.entropy, nonce); err != nil {
		return p.fail()
	}
	request := &appv1.ClockRequest{Nonce: bytes.Clone(nonce)}
	opts := []grpc.CallOption{grpc.MaxCallRecvMsgSize(4096), grpc.MaxCallSendMsgSize(128), grpc.MaxRetryRPCBufferSize(0)}
	var combined *appv1.ClockReply
	var physical *appv1.PhysicalClockReply
	var rpcErr error
	if physicalOnly {
		physical, rpcErr = p.client.PhysicalClock(callCtx, request, opts...)
	} else {
		combined, rpcErr = p.client.Clock(callCtx, request, opts...)
	}
	// Read even after an RPC error so suspend, reboot and regression cannot be
	// hidden by a transport failure followed by a successful response.
	end, err := p.readLocal()
	if err != nil {
		return Observation{}, err
	}
	if p.ctx.Err() != nil {
		return Observation{}, ErrClosed
	}
	if rpcErr != nil || callCtx.Err() != nil {
		return Observation{}, p.unavailable(callCtx)
	}
	if end.Elapsed-start.Elapsed > p.cfg.MaxRoundTrip {
		return Observation{}, ErrUnavailable
	}
	var w trustedtime.Interval
	var identity replyIdentity
	var sample time.Time
	var domain string
	if physicalOnly {
		w, err = appv1.DecodePhysicalClockReply(physical)
		if err == nil {
			identity = physicalIdentity(physical)
		}
	} else {
		w, err = appv1.DecodeClockReply(combined)
		if err == nil {
			identity = combinedIdentity(combined)
			sample, domain = time.Unix(0, combined.SampleUnixNs), combined.SampleClockDomain
		}
	}
	if err != nil || !bytes.Equal(identity.nonce, nonce) || !p.matches(identity) || !physicalOnly && domain != p.cfg.SampleClockDomain {
		return p.fail()
	}
	if p.seen && (identity.serverBoot != p.last.ServerBootID || identity.generation != p.last.DeploymentGeneration || w.BootID != p.remote.BootID || w.Source != p.remote.Source || w.Monotonic < p.remote.Monotonic || w.Earliest.Before(p.remote.Earliest) || w.Latest.Before(p.remote.Latest)) {
		return p.fail()
	}
	if !physicalOnly && p.sampleSeen && sample.Before(p.sampleLast) {
		return p.fail()
	}
	// Include decoding and pin validation before granting usable authority.
	end, err = p.readLocal()
	if err != nil {
		return Observation{}, err
	}
	if p.ctx.Err() != nil {
		return Observation{}, ErrClosed
	}
	if callCtx.Err() != nil {
		return Observation{}, p.unavailable(callCtx)
	}
	elapsed := end.Elapsed - start.Elapsed
	if elapsed > p.cfg.MaxRoundTrip {
		return Observation{}, ErrUnavailable
	}
	drift := driftBound(elapsed, p.cfg.DriftPPM)
	advance := elapsed + drift // both bounded by MaxClockResponseAge above
	hi := w.Latest.UnixNano()
	if hi > math.MaxInt64-int64(advance) {
		return p.fail()
	}
	adjusted := w
	adjusted.Latest = time.Unix(0, hi+int64(advance))
	// A shorter later RTT can narrow valid uncertainty. Keep an upper envelope
	// so downstream expiry guards never mistake that for a clock rollback.
	if p.seen && adjusted.Latest.Before(p.last.Physical.Latest) {
		adjusted.Latest = p.last.Physical.Latest
	}
	adjusted.BootID, adjusted.Monotonic = end.BootID, end.Elapsed
	adjusted.Source = "wendy-agent-private-socket"
	if adjusted.Validate() != nil || adjusted.Latest.Sub(adjusted.Earliest) > p.cfg.MaxWidth {
		return p.fail()
	}
	o := Observation{Physical: adjusted, SampleTime: sample, SampleClockDomain: domain, ServerBootID: identity.serverBoot, DeploymentGeneration: identity.generation, RoundTrip: elapsed}
	p.remote, p.last, p.seen = w, PhysicalObservation{Physical: adjusted, ServerBootID: identity.serverBoot, DeploymentGeneration: identity.generation, RoundTrip: elapsed}, true
	if !physicalOnly {
		p.sampleSeen, p.sampleLast = true, sample
	}
	return o, nil
}

type replyIdentity struct {
	nonce                                                           []byte
	tenant, site, device, app, service, executionDomain, serverBoot string
	generation                                                      uint64
}

func combinedIdentity(r *appv1.ClockReply) replyIdentity {
	return replyIdentity{nonce: r.Nonce, tenant: r.Tenant, site: r.Site, device: r.Device, app: r.App, service: r.Service, executionDomain: r.ExecutionDomain, serverBoot: r.ServerBootId, generation: r.DeploymentGeneration}
}
func physicalIdentity(r *appv1.PhysicalClockReply) replyIdentity {
	return replyIdentity{nonce: r.Nonce, tenant: r.Tenant, site: r.Site, device: r.Device, app: r.App, service: r.Service, executionDomain: r.ExecutionDomain, serverBoot: r.ServerBootId, generation: r.DeploymentGeneration}
}
func (p *Provider) matches(r replyIdentity) bool {
	c := p.cfg
	return r.tenant == c.Tenant && r.site == c.Site && r.device == c.Device && r.app == c.App && r.service == c.Service && r.executionDomain == c.ExecutionDomain && (c.ExpectedGeneration == 0 || r.generation == c.ExpectedGeneration) && (c.ExpectedServerBootID == "" || r.serverBoot == c.ExpectedServerBootID)
}

func (p *Provider) unavailable(ctx context.Context) error {
	if p.ctx.Err() != nil {
		return ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(ErrUnavailable, err)
	}
	return ErrUnavailable
}

func driftBound(elapsed time.Duration, ppm uint64) time.Duration {
	ns := uint64(elapsed)
	return time.Duration(ns/1_000_000*ppm + (ns%1_000_000*ppm+999_999)/1_000_000)
}
