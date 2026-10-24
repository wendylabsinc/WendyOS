package devices

import (
	"context"
	"errors"
	"net"
	"slices"
	"strconv"
	"time"

	"github.com/wendylabsinc/WendyOS/go/wendy-core/platform"
)

type mtlsAttemptError struct {
	addr string
	err  error
}

func (e mtlsAttemptError) Error() string { return e.addr + ": " + e.err.Error() }

func (e mtlsAttemptError) Unwrap() error { return e.err }

// mtlsWalk carries a candidate's certificate order, failure evidence, and
// diagnostics. Address attempts run independently; raceMTLSCandidates merges
// their evidence after every started attempt has finished.
type mtlsWalk struct {
	target     Target
	allCerts   []Credential
	connector  platform.DeviceConnector
	options    DialOptions
	probeOrder []int
	jumped     bool
	tlsDebug   bool

	// The suppression buckets below describe the PRIMARY candidate (target.Addr)
	// and nothing else. That scoping is load-bearing in both directions.
	//
	// target.Addr is the only address the plaintext rung ever dials, so it is the
	// only address whose TLS posture may decide whether that rung is offered. Let
	// the buckets accumulate across the whole walk instead and two things break:
	// a single unreachable extra candidate contributes a non-cert failure, which
	// un-suppresses the rung and hands out an unauthenticated connection where
	// one address alone would have refused; and a single black-holed candidate
	// (whose TLS handshake times out, which isCertRejectionError counts as a
	// rejection) suppresses the rung for a primary that legitimately earned it.
	// Neither has anything to do with whether dialing target.Addr in the clear is
	// safe.
	//
	// Non-primary candidates are therefore pure routing attempts: they can only
	// ever find a working device, never change the verdict about the primary.
	//
	// primaryOwnPortCertReject — the primary's OWN port was a TLS endpoint that
	// rejected our cert (the tunnel/mTLS-only-discovery case where that port IS
	// already the mTLS port). This includes an explicit clientAuth rejection on
	// the RPC probe, but not a non-TLS preface from a plaintext port.
	primaryOwnPortCertReject bool
	// primaryMTLSPortCertFails / primaryMTLSPortNonCertFails — cert-rejection vs.
	// other failures at the primary's port+1 (the dedicated mTLS port in the
	// normal case).
	primaryMTLSPortCertFails    int
	primaryMTLSPortNonCertFails int
	// The primary's org and failure evidence are kept separately from the
	// walk-wide fields, which a later candidate would have overwritten.
	primaryObservedOrg int32
	primaryLastErr     error
	// Keep explicit certificate failures when a later port is unreachable.
	// This evidence is diagnostic only; the suppression buckets stay unchanged.
	primaryCertificateErr error
	// anyCertRejection records whether ANY candidate rejected our certificate. It
	// decides nothing; it only keeps the unreachable message from claiming that
	// no certificate was ever compared when one was.
	anyCertRejection bool
	// observedDeviceOrg is the org read from the device's server cert on a failed
	// mTLS probe (0 = none), walk-wide because promoteOrgNext consumes it.
	observedDeviceOrg int32
	lastMTLSErr       error
	// attempts logs every failed rung in order, so a refusal can name the
	// addresses actually dialled instead of asserting that "no endpoint
	// answered" and leaving the user to guess where we looked.
	attempts []mtlsAttemptError
}

// primaryRejectedOurCert reports whether the PRIMARY candidate proved itself a
// TLS endpoint that refused our certificate: either its own port did, or every
// failure at its port+1 was a cert rejection rather than plain unreachability.
//
// It decides both whether the plaintext rung is suppressed and whether the
// caller gets chooseRejectionError's actionable diagnosis, and it reads ONLY the
// primary's buckets — the primary being the only address the plaintext rung
// dials. See the buckets' doc on mtlsWalk for what breaks in each direction when
// non-primary candidates are allowed a vote.
func (w *mtlsWalk) primaryRejectedOurCert() bool {
	return w.primaryOwnPortCertReject ||
		(w.primaryMTLSPortCertFails > 0 && w.primaryMTLSPortNonCertFails == 0)
}

func (w *mtlsWalk) recordMTLSErr(addr string, err error, isPrimary bool) {
	if err == nil {
		return
	}
	attempt := mtlsAttemptError{addr: addr, err: err}
	w.lastMTLSErr = attempt
	if isPrimary {
		w.primaryLastErr = attempt
		// An explicit client-credential diagnosis remains actionable even if
		// later sessions fail with generic server-certificate errors.
		if isTLSCertificateError(err) && !hasRefreshReason(w.primaryCertificateErr) {
			w.primaryCertificateErr = attempt
		}
	}
	w.attempts = append(w.attempts, attempt)
}

// dialAddr runs the two-port × every-cert rungs at ONE candidate address.
//
// The three return shapes are the walk's control flow: a live connection (stop,
// success), a refusal (stop this candidate and prevent new candidates or
// plaintext fallback), or (nil, nil) meaning "nothing answered here, try the
// next address". Already-started candidates may still authenticate the target.
func (w *mtlsWalk) dialAddr(ctx context.Context, cand string, isPrimary bool) (platform.DeviceConnection, error) {
	host, portStr, err := net.SplitHostPort(cand)
	if err != nil {
		return nil, nil
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, nil
	}
	// Try the given port first (covers explicit tunnel ports that already point
	// at the mTLS port), then fall back to port+1 (the normal case where
	// discovery returns the plaintext port and mTLS is port+1).
	mtlsAddrs := []string{cand, HostPort(host, port+1)}
	for addrIdx, mtlsAddr := range mtlsAddrs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for pos := range w.probeOrder {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			i := w.probeOrder[pos]
			conn, tlsErr := w.connector.ConnectDevice(ctx, platform.DeviceConnectRequest{Address: mtlsAddr, TLS: true, Credential: w.allCerts[i].Reference, Expected: w.target.Expected})
			if tlsErr != nil {
				w.recordMTLSErr(mtlsAddr, tlsErr, isPrimary)
				var rejection *platform.DevicePeerRejectedError
				if errors.As(tlsErr, &rejection) {
					return nil, &IdentityRefusalError{Key: w.target.RefusalKey(), Cause: rejection.Cause}
				}
				if w.tlsDebug {
					w.options.Debugf("[tls-debug] ConnectWithTLS(%s) error: %v\n", mtlsAddr, tlsErr)
				}
				continue
			}
			// grpc.NewClient is lazy — verify the connection actually works with
			// a fast probe before committing to mTLS. Every candidate is already
			// a literal IP, so this only needs to cover TCP + the TLS handshake;
			// the old 8s budget (which also covered .local mDNS resolution) made
			// an unreachable mTLS port cost 8s before the plaintext fallback.
			probeCtx, cancel := context.WithTimeout(ctx, MTLSProbeTimeout)
			_, probeErr := GetAgentVersion(probeCtx, conn)
			cancel()
			if probeErr == nil {
				if w.options.RememberOrg != nil {
					w.options.RememberOrg(host, w.allCerts[i].OrganizationID)
				}
				return conn, nil
			}
			w.recordMTLSErr(mtlsAddr, probeErr, isPrimary)
			evidence := conn.Evidence()
			if im := evidence.IdentityMismatch; im != nil {
				conn.Close()
				// The device is wrong, so trying another certificate or port at
				// this address cannot help. Stop scheduling new candidates and
				// forbid plaintext, but let already-started candidates finish:
				// another address may still authenticate the requested device
				// after this address was reassigned by DHCP.
				//
				// refusalKey, not PinKey: the pin that produced this constraint
				// may be filed under one of the device's other names, and that
				// is the key `wendy device unpin` has to be handed to clear it.
				return nil, &IdentityRefusalError{Key: w.target.RefusalKey(), Cause: im}
			}
			if pm := evidence.PinMismatch; pm != nil {
				conn.Close()
				// The SPKI store rejected the peer's public key. Apply the same
				// candidate and fallback rules as an identity mismatch. Preserve
				// the typed refusal because gRPC's handshake wrapper otherwise
				// hides the reason and the recovery steps.
				return nil, &IdentityRefusalError{Key: w.target.RefusalKey(), Cause: pm}
			}
			if w.tlsDebug {
				w.options.Debugf("[tls-debug] GetAgentVersion(%s) error: %v\n", mtlsAddr, probeErr)
			}
			if evidence.TCPDialError != nil {
				// The actual dial never reached TCP. Another credential cannot
				// change that, so try the next port without repeating its wait.
				conn.Close()
				if isPrimary && addrIdx == 1 {
					w.primaryMTLSPortNonCertFails++
				}
				break
			}
			if org := evidence.ObservedOrg; org != 0 {
				w.observedDeviceOrg = org
				if isPrimary {
					w.primaryObservedOrg = org
				}
				// The device just named its own org. Try that org's cert next
				// rather than grinding through the remaining ones in their
				// original order. Purely a reordering of certs we already hold —
				// the trust decision stays with BuildServerVerifyConnection
				// (expected-org, ML-DSA chain, SPKI pin), so an org claimed by a
				// hostile server only ever gets shown a cert this loop would
				// have shown it anyway. Each candidate may reorder once, so
				// repeated org claims cannot reshuffle its ladder indefinitely.
				if !w.jumped && PromoteOrgNext(w.probeOrder, pos, w.allCerts, org) {
					w.jumped = true
				}
			}
			conn.Close()
			certRejected := isCertRejectionError(cand, probeErr)
			if certRejected {
				w.anyCertRejection = true
			}
			if !isPrimary {
				continue
			}
			if addrIdx == 0 {
				if certRejected {
					w.primaryOwnPortCertReject = true
				}
			} else {
				if certRejected {
					w.primaryMTLSPortCertFails++
				} else {
					w.primaryMTLSPortNonCertFails++
				}
			}
		}
	}
	return nil, nil
}

type mtlsCandidateResult struct {
	index int
	walk  *mtlsWalk
	conn  platform.DeviceConnection
	err   error
}

// raceMTLSCandidates staggers authenticated attempts in route preference order.
// A failed attempt starts the next address immediately. A success cancels the
// remaining attempts, then joins them so no connection or refusal is lost. A
// refusal stops new attempts but lets an already started attempt authenticate.
func raceMTLSCandidates(ctx context.Context, walk *mtlsWalk, candidates []string) (platform.DeviceConnection, error) {
	return raceMTLSCandidatesWithDial(ctx, walk, candidates, (*mtlsWalk).dialAddr)
}

func raceMTLSCandidatesWithDial(ctx context.Context, walk *mtlsWalk, candidates []string, dial func(*mtlsWalk, context.Context, string, bool) (platform.DeviceConnection, error)) (platform.DeviceConnection, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	raceCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	results := make(chan mtlsCandidateResult, len(candidates))
	completed := make([]*mtlsWalk, len(candidates))
	next, running := 0, 0
	startNext := func() {
		index := next
		next++
		running++
		candidateWalk := &mtlsWalk{
			target: walk.target, allCerts: walk.allCerts, connector: walk.connector, options: walk.options,
			probeOrder: slices.Clone(walk.probeOrder), tlsDebug: walk.tlsDebug,
		}
		go func() {
			conn, err := dial(candidateWalk, raceCtx, candidates[index], index == 0)
			results <- mtlsCandidateResult{index: index, walk: candidateWalk, conn: conn, err: err}
		}()
	}
	startNext()
	timer := time.NewTimer(mtlsCandidateDelay)
	defer timer.Stop()
	var winner platform.DeviceConnection
	var refusal error
	stopping := false
	parentDone := ctx.Done()
	for running > 0 {
		var launch <-chan time.Time
		if !stopping && next < len(candidates) && running < MaxDialCandidates {
			launch = timer.C
		}
		select {
		case result := <-results:
			running--
			completed[result.index] = result.walk
			if result.err != nil && (errors.Is(result.err, ErrIdentityRefused) ||
				(!errors.Is(result.err, context.Canceled) && !errors.Is(result.err, context.DeadlineExceeded))) {
				if refusal == nil {
					refusal = result.err
				}
				stopping = true
			}
			if result.conn != nil {
				if winner == nil && result.err == nil && ctx.Err() == nil {
					winner = result.conn
				} else {
					result.conn.Close()
				}
				if result.err == nil {
					stopping = true
					cancel()
				}
			}
			if !stopping && next < len(candidates) && running < MaxDialCandidates {
				startNext()
				timer.Reset(mtlsCandidateDelay)
			}
		case <-launch:
			startNext()
			timer.Reset(mtlsCandidateDelay)
		case <-parentDone:
			stopping = true
			cancel()
			parentDone = nil
		}
	}
	walk.mergeCandidateResults(completed)
	if err := ctx.Err(); err != nil {
		if winner != nil {
			winner.Close()
		}
		// A peer's identity or key mismatch remains actionable even if the
		// parent was canceled while the remaining attempts were finishing.
		if refusal != nil {
			return nil, refusal
		}
		return nil, err
	}
	if winner != nil {
		// Another address may have been reassigned to an unrelated device.
		// Its refusal does not invalidate this authenticated connection.
		return winner, nil
	}
	return nil, refusal
}

// Keep diagnostic order stable even though addresses finish in any order.
// Only the primary address may decide whether plaintext is permitted.
func (w *mtlsWalk) mergeCandidateResults(completed []*mtlsWalk) {
	for index, candidate := range completed {
		if candidate == nil {
			continue
		}
		w.attempts = append(w.attempts, candidate.attempts...)
		w.anyCertRejection = w.anyCertRejection || candidate.anyCertRejection
		if candidate.lastMTLSErr != nil {
			w.lastMTLSErr = candidate.lastMTLSErr
		}
		if index == 0 {
			w.primaryOwnPortCertReject = candidate.primaryOwnPortCertReject
			w.primaryMTLSPortCertFails = candidate.primaryMTLSPortCertFails
			w.primaryMTLSPortNonCertFails = candidate.primaryMTLSPortNonCertFails
			w.primaryObservedOrg = candidate.primaryObservedOrg
			w.primaryLastErr = candidate.primaryLastErr
			w.primaryCertificateErr = candidate.primaryCertificateErr
		}
	}
}
