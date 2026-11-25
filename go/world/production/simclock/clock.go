// Package simclock reads a commissioned simulation source clock. It never
// supplies certificate validity time and never extrapolates simulation time.
package simclock

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/jws"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
)

const Schema = "wendy.sim.clock.v1"
const NonceHeader = "X-Wendy-Clock-Nonce"
const MaxResponseBytes = 4096

var (
	ErrConfig      = errors.New("invalid commissioned simulation clock")
	ErrUnavailable = errors.New("simulation source clock unavailable")
	ErrFaulted     = errors.New("simulation source clock fault requires recommissioning")
	ErrClosed      = errors.New("simulation source clock closed")
)

type Config struct {
	// URL is an explicit HTTP(S) origin, with no credentials, query or path.
	// HTTP is appropriate only inside the commissioned isolated simulation.
	URL, Token, RunID, BootID, PackageSHA256, Domain string
	Frame                                            world.FrameRef
	Anchor                                           time.Time
	Physical                                         trustedtime.Provider
	// MaxAge bounds the clock observation against authenticated physical time,
	// including its uncertainty. RequestTimeout bounds local elapsed time too.
	MaxAge, RequestTimeout time.Duration
}

type response struct {
	Schema        string         `json:"schema_version"`
	RunID         string         `json:"run_id"`
	BootID        string         `json:"boot_id"`
	PackageSHA256 string         `json:"package_sha256"`
	Frame         world.FrameRef `json:"frame"`
	Domain        string         `json:"clock_domain"`
	AnchorNS      int64          `json:"anchor_unix_ns"`
	SimulationNS  *int64         `json:"simulation_time_ns"`
	CaptureNS     int64          `json:"observed_unix_ns"`
	Sequence      uint64         `json:"observation_sequence"`
	Nonce         string         `json:"nonce"`
}

type Provider struct {
	mu                      sync.Mutex
	cfg                     Config
	client                  *http.Client
	ctx                     context.Context
	cancel                  context.CancelFunc
	fault                   bool
	seen                    bool
	sequence                uint64
	simulationNS, captureNS int64
	physicalSeen            bool
	physical                trustedtime.Interval
}

func tokenValid(s string) bool {
	if len(s) < 32 || len(s) > 256 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

func New(cfg Config) (*Provider, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") || u.RawPath != "" {
		return nil, ErrConfig
	}
	host, port, err := net.SplitHostPort(u.Host)
	n, e := strconv.Atoi(port)
	if err != nil || e != nil || host == "" || n < 1 || n > 65535 || strings.ContainsAny(host, "\r\n%") {
		return nil, ErrConfig
	}
	digest, err := hex.DecodeString(cfg.PackageSHA256)
	anchorNS := cfg.Anchor.UnixNano()
	if !tokenValid(cfg.Token) || cfg.RunID == "" || len(cfg.RunID) > 256 || cfg.BootID == "" || len(cfg.BootID) > 256 || cfg.Domain != "simulation:"+cfg.RunID || cfg.Frame.Validate() != nil || err != nil || len(digest) != 32 || strings.ToLower(cfg.PackageSHA256) != cfg.PackageSHA256 || cfg.Physical == nil || cfg.Anchor.IsZero() || anchorNS <= 0 || !time.Unix(0, anchorNS).Equal(cfg.Anchor) {
		return nil, ErrConfig
	}
	if cfg.MaxAge == 0 {
		cfg.MaxAge = 5 * time.Second
	}
	if cfg.RequestTimeout == 0 {
		cfg.RequestTimeout = time.Second
	}
	if cfg.MaxAge < time.Millisecond || cfg.MaxAge > 10*time.Second || cfg.RequestTimeout < time.Millisecond || cfg.RequestTimeout > 3*time.Second || cfg.RequestTimeout > cfg.MaxAge {
		return nil, ErrConfig
	}
	cfg.URL = strings.TrimSuffix(cfg.URL, "/")
	cfg.Anchor = cfg.Anchor.Round(0).UTC()
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: cfg.RequestTimeout}).DialContext, DisableCompression: true, MaxResponseHeaderBytes: 4096, ResponseHeaderTimeout: cfg.RequestTimeout, MaxIdleConnsPerHost: 1}
	ctx, cancel := context.WithCancel(context.Background())
	return &Provider{cfg: cfg, ctx: ctx, cancel: cancel, client: &http.Client{Transport: transport, Timeout: cfg.RequestTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return ErrUnavailable }}}, nil
}

// Close cancels an in-flight HTTP request without waiting for its mutex. A
// closed provider cannot be reopened, even if the source becomes available.
func (p *Provider) Close() error {
	p.cancel()
	p.client.CloseIdleConnections()
	return nil
}

func (p *Provider) fail() (time.Time, error) { p.fault = true; return time.Time{}, ErrFaulted }

func (p *Provider) physicalNow() (trustedtime.Interval, error) {
	w, err := p.cfg.Physical.Now()
	if err != nil {
		return trustedtime.Interval{}, ErrUnavailable
	}
	if w.Validate() != nil {
		p.fault = true
		return trustedtime.Interval{}, ErrFaulted
	}
	if p.physicalSeen && (w.BootID != p.physical.BootID || w.Monotonic < p.physical.Monotonic || w.Earliest.Before(p.physical.Earliest) || w.Latest.Before(p.physical.Latest)) {
		p.fault = true
		return trustedtime.Interval{}, ErrFaulted
	}
	p.physicalSeen, p.physical = true, w
	return w, nil
}

// Now obtains a fresh nonce-bound source observation for every call. Paused
// physics may return the same simulation instant with a new source sequence;
// it never advances that instant using elapsed physical time. No cached value
// is returned after source silence, HTTP failure or physical clock failure.
// Identity changes, replay and regression latch a fault until recommissioning.
func (p *Provider) Now() (time.Time, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ctx.Err() != nil {
		return time.Time{}, ErrClosed
	}
	if p.fault {
		return time.Time{}, ErrFaulted
	}
	before, err := p.physicalNow()
	if err != nil {
		return time.Time{}, err
	}
	started := time.Now() // only local monotonic elapsed; never authority time
	var nonceBytes [32]byte
	if _, err := rand.Read(nonceBytes[:]); err != nil {
		return time.Time{}, ErrUnavailable
	}
	nonce := hex.EncodeToString(nonceBytes[:])
	ctx, cancel := context.WithTimeout(p.ctx, p.cfg.RequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.cfg.URL+"/v1/clock", nil)
	if err != nil {
		return time.Time{}, ErrUnavailable
	}
	req.Header.Set("Authorization", "Bearer "+p.cfg.Token)
	req.Header.Set(NonceHeader, nonce)
	req.Header.Set("Accept", "application/json")
	r, err := p.client.Do(req)
	if err != nil {
		if p.ctx.Err() != nil {
			return time.Time{}, ErrClosed
		}
		return time.Time{}, ErrUnavailable // never return token-bearing request errors
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		return time.Time{}, ErrUnavailable
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, MaxResponseBytes+1))
	if err != nil || len(raw) > MaxResponseBytes {
		return time.Time{}, ErrUnavailable
	}
	after, err := p.physicalNow()
	if err != nil {
		return time.Time{}, err
	}
	if p.ctx.Err() != nil {
		return time.Time{}, ErrClosed
	}
	if ctx.Err() != nil || time.Since(started) > p.cfg.RequestTimeout || after.Monotonic-before.Monotonic > p.cfg.RequestTimeout {
		return time.Time{}, ErrUnavailable
	}
	var sample response
	if jws.DecodeStrict(raw, &sample) != nil || sample.Schema != Schema || sample.Nonce != nonce || sample.RunID != p.cfg.RunID || sample.BootID != p.cfg.BootID || sample.PackageSHA256 != p.cfg.PackageSHA256 || sample.Frame != p.cfg.Frame || sample.Domain != p.cfg.Domain || sample.AnchorNS != p.cfg.Anchor.UnixNano() || sample.SimulationNS == nil || *sample.SimulationNS < 0 || sample.Sequence == 0 || sample.Sequence > 1<<53-1 || sample.CaptureNS <= 0 {
		return p.fail()
	}
	if p.seen && (sample.Sequence <= p.sequence || *sample.SimulationNS < p.simulationNS || sample.CaptureNS < p.captureNS) {
		return p.fail()
	}
	capture := time.Unix(0, sample.CaptureNS)
	// The response was sampled after our nonce was created. Its source wall
	// marker must overlap that authenticated physical interval and remain fresh
	// at the conservative upper bound; uncertainty consumes the age budget.
	if capture.Before(before.Earliest) || capture.After(after.Latest) || after.Latest.Sub(capture) > p.cfg.MaxAge {
		return time.Time{}, ErrUnavailable
	}
	now := p.cfg.Anchor.Add(time.Duration(*sample.SimulationNS))
	if now.Before(p.cfg.Anchor) || now.UnixNano() < p.cfg.Anchor.UnixNano() || !time.Unix(0, now.UnixNano()).Equal(now) {
		return p.fail()
	}
	p.seen, p.sequence, p.simulationNS, p.captureNS = true, sample.Sequence, *sample.SimulationNS, sample.CaptureNS
	return now, nil
}
