// Package authenticatedtime obtains nonce-bound Wendy Roughtime quorum evidence
// and advances its interval with a native suspend-inclusive boot clock. It never
// changes the system clock, reads operator credentials, or writes a clock floor.
package authenticatedtime

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/internal/upstreamtime"
	"github.com/wendylabsinc/wendy/go/world/production/internal/upstreamtime/roughtime"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
)

const Source = "wendy-roughtime-quorum"

type Server = roughtime.Server
type Evidence = upstreamtime.RoughtimeEvidence

type Config struct {
	Servers      []Server
	QueryTimeout time.Duration
	MaxHoldover  time.Duration
	DriftPPM     uint64
	MaxWidth     time.Duration
}

type Report struct {
	Source      string               `json:"source"`
	Confidence  string               `json:"confidence"`
	Quorum      int                  `json:"quorum"`
	EvidenceID  string               `json:"evidence_id"`
	Anchor      trustedtime.Interval `json:"anchor"`
	MaxHoldover time.Duration        `json:"max_holdover_ns"`
	MaxWidth    time.Duration        `json:"max_width_ns"`
	DriftPPM    uint64               `json:"drift_ppm"`
	Evidence    []Evidence           `json:"evidence"`
	Attempts    []AttemptDiagnostic  `json:"attempts,omitempty"`
}

// IntervalBoundsError reports authenticated evidence that was refused by the
// caller's configured bounds. It is diagnostic data, never a usable provider.
type IntervalBoundsError struct {
	Reason           string
	Earliest, Latest time.Time
	Width, MaxWidth  time.Duration
	Quorum           int
}

func (e *IntervalBoundsError) Error() string {
	return fmt.Sprintf("authenticated interval refused (%s): quorum=%d interval=[%s,%s] width=%s max_width=%s", e.Reason, e.Quorum, e.Earliest.UTC().Format(time.RFC3339Nano), e.Latest.UTC().Format(time.RFC3339Nano), e.Width, e.MaxWidth)
}

type Provider struct {
	holdover *trustedtime.Holdover
	report   Report
}

var _ trustedtime.Provider = (*Provider)(nil)

// WendyServers returns owned copies of the existing Wendy pinned server list.
// Calling Acquire still requires the caller to select that list explicitly.
func WendyServers() []Server { return cloneServers(roughtime.Servers) }
func cloneServers(in []Server) []Server {
	out := append([]Server(nil), in...)
	for i := range out {
		out[i].PublicKey = append(ed25519.PublicKey(nil), out[i].PublicKey...)
	}
	return out
}

func validate(cfg Config) error {
	if len(cfg.Servers) < 2 || len(cfg.Servers) > 8 || cfg.QueryTimeout <= 0 || cfg.QueryTimeout > 5*time.Second || cfg.MaxHoldover <= 0 || cfg.MaxHoldover > 7*24*time.Hour || cfg.DriftPPM == 0 || cfg.DriftPPM > 1_000_000 || cfg.MaxWidth <= 0 || cfg.MaxWidth > 24*time.Hour {
		return errors.New("explicit bounded time sources and holdover required")
	}
	names, addresses, keys := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, s := range cfg.Servers {
		host, port, err := net.SplitHostPort(s.Address)
		if err != nil || host == "" || port == "" || len(s.Address) > 512 || strings.TrimSpace(s.Address) != s.Address || strings.ContainsAny(s.Address, "\x00\r\n") || s.Name == "" || len(s.Name) > 128 || strings.ContainsAny(s.Name, "\x00\r\n") || len(s.PublicKey) != ed25519.PublicKeySize || names[s.Name] || addresses[strings.ToLower(s.Address)] || keys[string(s.PublicKey)] {
			return errors.New("time sources must have distinct names, addresses and 32-byte keys")
		}
		names[s.Name] = true
		addresses[strings.ToLower(s.Address)] = true
		keys[string(s.PublicKey)] = true
	}
	return nil
}

// Acquire performs fresh network exchanges. A single response, a stale cached
// proof, wall time or simulation time cannot create this Provider. Failures do
// not return a provider. Reacquisition after expiry requires a new signed quorum.
func Acquire(ctx context.Context, cfg Config) (*Provider, error) {
	return acquire(ctx, cfg, readNative)
}

func acquire(ctx context.Context, cfg Config, read func() (trustedtime.Reading, error)) (*Provider, error) {
	return acquireFenced(ctx, cfg, &nativeFence{read: read})
}

func acquireFenced(ctx context.Context, cfg Config, fence *nativeFence) (*Provider, error) {
	if ctx == nil {
		return nil, errors.New("context required")
	}
	cfg.Servers = cloneServers(cfg.Servers)
	if err := validate(cfg); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	checkedRead := fence.now
	initial, err := checkedRead()
	if err != nil {
		return nil, err
	}
	queryCtx, cancel := context.WithTimeout(ctx, cfg.QueryTimeout)
	defer cancel()
	consensus, err := upstreamtime.QueryConsensus(queryCtx, cfg.Servers, func() (int64, error) { r, e := checkedRead(); return int64(r.Elapsed), e })
	if callerErr := ctx.Err(); callerErr != nil {
		return nil, callerErr
	}
	// Check the native fence even on failed exchanges, before turning a weak
	// quorum into a retryable result. A boot/rollback fault is never availability.
	final, clockErr := checkedRead()
	if clockErr != nil {
		return nil, clockErr
	}
	elapsed := final.Elapsed - initial.Elapsed
	if elapsed < 0 || elapsed > cfg.QueryTimeout+time.Second {
		return nil, errors.New("time collection exceeded boot-time budget")
	}
	if err != nil {
		return nil, err
	}
	// A usable quorum may arrive before another source exhausts the collection
	// deadline. Match Wendy FreshTime: preserve it unless the caller cancelled.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if consensus.Quorum < 2 {
		valid := 0
		for _, evidence := range consensus.Evidence {
			if evidence.Error == "" {
				valid++
			}
		}
		return nil, &QuorumError{Quorum: consensus.Quorum, VerifiedResponses: valid, Sources: len(consensus.Evidence)}
	}
	if (consensus.Confidence != upstreamtime.ConfidenceDegraded && consensus.Confidence != upstreamtime.ConfidenceVerified) || consensus.UpperOffsetNanos < consensus.LowerOffsetNanos {
		return nil, errors.New("invalid authenticated consensus")
	}
	// Widen for oscillator drift over the whole collection, including waiting
	// for unavailable servers. No symmetric network-delay assumption is made.
	drift := driftBound(elapsed, cfg.DriftPPM)
	lo, ok1 := addNanos(int64(final.Elapsed), consensus.LowerOffsetNanos)
	hi, ok2 := addNanos(int64(final.Elapsed), consensus.UpperOffsetNanos)
	if !ok1 || !ok2 {
		return nil, errors.New("authenticated interval overflow")
	}
	anchor := trustedtime.Interval{Earliest: time.Unix(0, lo).Add(-drift), Latest: time.Unix(0, hi).Add(drift), BootID: final.BootID, Monotonic: final.Elapsed, Domain: "physical", Authenticated: true, Source: Source}
	reason := ""
	if anchor.Validate() != nil {
		reason = "invalid interval"
	} else if anchor.Earliest.Before(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)) || anchor.Latest.After(time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)) {
		reason = "plausibility range"
	} else if anchor.Latest.Sub(anchor.Earliest) > cfg.MaxWidth {
		reason = "width"
	}
	if reason != "" {
		return nil, &IntervalBoundsError{Reason: reason, Earliest: anchor.Earliest, Latest: anchor.Latest, Width: anchor.Latest.Sub(anchor.Earliest), MaxWidth: cfg.MaxWidth, Quorum: consensus.Quorum}
	}
	b, err := json.Marshal(consensus.Evidence)
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(b)
	id := hex.EncodeToString(hash[:])
	holdover, err := trustedtime.NewHoldover(trustedtime.HoldoverConfig{Anchor: trustedtime.VerifiedAnchor{Interval: anchor, EvidenceID: id}, Read: checkedRead, MaxHoldover: cfg.MaxHoldover, DriftPPM: cfg.DriftPPM, MaxWidth: cfg.MaxWidth})
	if err != nil {
		return nil, err
	}
	p := &Provider{holdover: holdover, report: Report{Source: Source, Confidence: consensus.Confidence, Quorum: consensus.Quorum, EvidenceID: id, Anchor: anchor, MaxHoldover: cfg.MaxHoldover, MaxWidth: cfg.MaxWidth, DriftPPM: cfg.DriftPPM, Evidence: consensus.Evidence}}
	return p, nil
}
func driftBound(elapsed time.Duration, ppm uint64) time.Duration {
	ns := uint64(elapsed)
	return time.Duration((ns/1_000_000)*ppm + (ns%1_000_000*ppm+999_999)/1_000_000)
}
func addNanos(a, b int64) (int64, bool) {
	sum := a + b
	return sum, (b >= 0 && sum >= a) || (b < 0 && sum < a)
}
func (p *Provider) Now() (trustedtime.Interval, error) {
	if p == nil || p.holdover == nil {
		return trustedtime.Interval{}, trustedtime.ErrUntrusted
	}
	return p.holdover.Now()
}

// Report is acquisition evidence, not current-time authority. Its byte slices
// are owned copies, and changing a report cannot change the provider's anchor.
func (p *Provider) Report() Report {
	if p == nil {
		return Report{}
	}
	out := p.report
	out.Evidence = append([]Evidence(nil), p.report.Evidence...)
	out.Attempts = append([]AttemptDiagnostic(nil), p.report.Attempts...)
	for i := range out.Evidence {
		out.Evidence[i].Nonce = append([]byte(nil), out.Evidence[i].Nonce...)
		out.Evidence[i].RawResponse = append([]byte(nil), out.Evidence[i].RawResponse...)
	}
	return out
}
func nativeError(err error) error { return fmt.Errorf("native boot clock unavailable: %w", err) }
