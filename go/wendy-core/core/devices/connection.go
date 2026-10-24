// Package devices contains portable device addressing, trust and connection
// decisions. Hosts supply discovery results and own every network connection.
package devices

import (
	"context"
	"errors"
	"time"

	"github.com/wendylabsinc/WendyOS/go/wendy-core/platform"
)

// MTLSProbeTimeout bounds the Agent version probe including the lazy handshake.
const MTLSProbeTimeout = 7 * time.Second
const mtlsCandidateDelay = 250 * time.Millisecond

// Target is a snapshot of the user's selected device and its governing pin.
// Resolve aliases and pin storage once before dialing. Candidates are already
// resolved by the host; their order is preserved. Addr is the primary endpoint
// and the only one eligible for the existing CLI's plaintext fallback.
type Target struct {
	PinKey     string
	PinnedKey  string
	Addr       string
	Candidates []string
	Expected   *platform.DeviceIdentity
}

func (t Target) dialCandidates() []string {
	if len(t.Candidates) > 0 {
		return t.Candidates
	}
	if t.Addr == "" {
		return nil
	}
	return []string{t.Addr}
}

// RefusalKey names the governing pin, including when it is filed under an alias.
func (t Target) RefusalKey() string {
	if t.PinnedKey != "" {
		return t.PinnedKey
	}
	return t.PinKey
}

// ErrIdentityRefused prevents fallback or recovery after a verifier rejection.
var ErrIdentityRefused = errors.New("device identity refused")

// ErrNoAuthenticatedEndpoint means the pin remains intact, but no candidate
// produced an authenticated connection. Removing the pin is not recovery.
var ErrNoAuthenticatedEndpoint = errors.New("no authenticated device endpoint answered")

// IdentityRefusalError preserves the host verifier's typed identity or SPKI error.
type IdentityRefusalError struct {
	Key   string
	Cause error
}

func (e *IdentityRefusalError) Error() string        { return e.Cause.Error() }
func (e *IdentityRefusalError) Unwrap() error        { return e.Cause }
func (e *IdentityRefusalError) Is(target error) bool { return target == ErrIdentityRefused }

// DialOptions contains host cache and diagnostic sinks. They are called from
// concurrent candidate attempts and must be safe for concurrent use. Cache hints
// only reorder credentials; they never establish authority or filter credentials.
type DialOptions struct {
	PreferredOrg     int
	HavePreferredOrg bool
	RememberOrg      func(host string, org int)
	Debugf           func(format string, args ...any)
}

// DialFailure identifies the presentation/recovery decision after a failed walk.
type DialFailure int

const (
	// DialFailed has no certificate or pin diagnosis; Err is the underlying cause.
	DialFailed DialFailure = iota
	// CertificateRejected requires the caller to diagnose the observed org and
	// its complete stored credential snapshot. Plaintext is forbidden.
	CertificateRejected
	// PinnedEndpointUnavailable retains the pin; no authenticated endpoint won.
	PinnedEndpointUnavailable
)

// DialAttempt records a failed endpoint probe in route and credential order.
type DialAttempt struct {
	Address string
	Err     error
}

// DialResult transfers the winning connection to the caller. Failed and late
// connections are closed before Dial returns. Err and LastMTLSError retain the
// original cause; Attempts and AnyCertRejection support accurate CLI diagnostics.
type DialResult struct {
	Connection       platform.DeviceConnection
	Err              error
	LastMTLSError    error
	Failure          DialFailure
	ObservedOrg      int32
	Attempts         []DialAttempt
	AnyCertRejection bool
}

// Dial runs the CLI's two-port, every-credential connection ladder. Identity
// refusals stop new candidates and forbid plaintext; an already-started candidate
// may still authenticate the requested device. Only primary failures decide
// plaintext eligibility, and a governing pin always forbids plaintext.
// credentials includes expired certificates for diagnostics. Core filters them
// only for transport when valid alternatives exist, preserving stored order.
func Dial(ctx context.Context, connector platform.DeviceConnector, target Target, credentials []Credential, options DialOptions) DialResult {
	result := DialResult{}
	walk := &mtlsWalk{target: target, connector: connector, options: options, tlsDebug: options.Debugf != nil}
	candidates := target.dialCandidates()
	if len(credentials) > 0 && len(candidates) > 0 {
		walk.allCerts = OrderCertsByOrg(preferValidCerts(credentials, time.Now()), options.PreferredOrg, options.HavePreferredOrg)
		walk.probeOrder = make([]int, len(walk.allCerts))
		for i := range walk.probeOrder {
			walk.probeOrder[i] = i
		}
		conn, refusal := raceMTLSCandidates(ctx, walk, candidates)
		result.LastMTLSError = walk.lastMTLSErr
		result.AnyCertRejection = walk.anyCertRejection
		for _, a := range walk.attempts {
			result.Attempts = append(result.Attempts, DialAttempt{a.addr, a.err})
		}
		if refusal != nil && (errors.Is(refusal, ErrIdentityRefused) ||
			(!errors.Is(refusal, context.Canceled) && !errors.Is(refusal, context.DeadlineExceeded))) {
			result.Err = refusal
			return result
		}
		if conn != nil {
			result.Connection = conn
			result.LastMTLSError = nil
			return result
		}
		if walk.primaryRejectedOurCert() {
			result.Err = walk.primaryLastErr
			if walk.primaryCertificateErr != nil {
				result.Err = walk.primaryCertificateErr
			}
			result.LastMTLSError = result.Err
			result.ObservedOrg = walk.primaryObservedOrg
			result.Failure = CertificateRejected
			return result
		}
	}
	if err := ctx.Err(); err != nil {
		result.Err = err
		return result
	}
	if target.PinnedKey != "" {
		result.Failure = PinnedEndpointUnavailable
		result.Err = ErrNoAuthenticatedEndpoint
		return result
	}
	result.Connection, result.Err = connector.ConnectDevice(ctx, platform.DeviceConnectRequest{Address: target.Addr})
	return result
}

func hasRefreshReason(err error) bool { return CertificateRefreshReason(err) != "" }
