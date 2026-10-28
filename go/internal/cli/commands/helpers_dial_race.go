package commands

import (
	"context"
	"crypto/x509"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
)

const mtlsCandidateDelay = 250 * time.Millisecond

var dialMTLSCandidateFn = func(ctx context.Context, walk *mtlsWalk, candidate string, isPrimary bool) (*grpcclient.AgentConnection, error) {
	return walk.dialAddr(ctx, candidate, isPrimary)
}

type concurrentPinChecker struct {
	mu    sync.Mutex
	store certs.PinChecker
}

func (p *concurrentPinChecker) CheckAndUpdate(leaf *x509.Certificate, displayName string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.store.CheckAndUpdate(leaf, displayName)
}

type mtlsCandidateResult struct {
	index int
	walk  *mtlsWalk
	conn  *grpcclient.AgentConnection
	err   error
}

// raceMTLSCandidates staggers authenticated attempts in route preference order.
// A failed attempt starts the next address immediately. A success cancels the
// remaining attempts, then joins them so no connection or refusal is lost. A
// refusal stops new attempts but lets an already started attempt authenticate.
func raceMTLSCandidates(ctx context.Context, walk *mtlsWalk, candidates []string) (*grpcclient.AgentConnection, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	raceCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	pins := walk.pins
	if pins != nil {
		pins = &concurrentPinChecker{store: pins}
	}
	// Snapshot the hook before starting workers; all workers finish before the
	// caller returns and a test can restore it.
	dial := dialMTLSCandidateFn
	results := make(chan mtlsCandidateResult, len(candidates))
	completed := make([]*mtlsWalk, len(candidates))
	next, running := 0, 0
	startNext := func() {
		index := next
		next++
		running++
		candidateWalk := &mtlsWalk{
			target: walk.target, allCerts: walk.allCerts, pins: pins,
			probeOrder: slices.Clone(walk.probeOrder), tlsDebug: walk.tlsDebug,
		}
		go func() {
			conn, err := dial(raceCtx, candidateWalk, candidates[index], index == 0)
			results <- mtlsCandidateResult{index: index, walk: candidateWalk, conn: conn, err: err}
		}()
	}
	startNext()
	timer := time.NewTimer(mtlsCandidateDelay)
	defer timer.Stop()
	var winner *grpcclient.AgentConnection
	var refusal error
	stopping := false
	parentDone := ctx.Done()
	for running > 0 {
		var launch <-chan time.Time
		if !stopping && next < len(candidates) && running < maxDialCandidates {
			launch = timer.C
		}
		select {
		case result := <-results:
			running--
			completed[result.index] = result.walk
			if result.err != nil && (errors.Is(result.err, errDeviceIdentityRefused) ||
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
			if !stopping && next < len(candidates) && running < maxDialCandidates {
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
			w.primaryObservedTenant = candidate.primaryObservedTenant
			w.primaryLastErr = candidate.primaryLastErr
			w.primaryCertificateErr = candidate.primaryCertificateErr
		}
	}
}
