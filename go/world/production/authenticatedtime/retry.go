package authenticatedtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
)

const StartupMaxAttempts = 3

// QuorumError is a completed collection with at least one verified response,
// but fewer than two overlapping responses. It carries no reusable authority.
// Zero-response exchange failures retain their original terminal error.
type QuorumError struct {
	Quorum            int
	VerifiedResponses int
	Sources           int
}

func (e *QuorumError) Error() string {
	return fmt.Sprintf("fresh authenticated quorum required: quorum=%d verified_responses=%d sources=%d", e.Quorum, e.VerifiedResponses, e.Sources)
}

// AttemptDiagnostic is bounded acquisition metadata, not proof or a time lease.
// Outcomes are fixed labels. No endpoint, nonce, response or raw error is stored.
type AttemptDiagnostic struct {
	Attempt           int    `json:"attempt"`
	Outcome           string `json:"outcome"`
	Quorum            int    `json:"quorum"`
	VerifiedResponses int    `json:"verified_responses"`
}

type StartupError struct {
	Attempts []AttemptDiagnostic
	cause    error
}

func (e *StartupError) Error() string {
	n := min(len(e.Attempts), StartupMaxAttempts)
	quorums, verified := make([]int, n), make([]int, n)
	for i := 0; i < n; i++ {
		quorums[i] = min(max(e.Attempts[i].Quorum, 0), 8)
		verified[i] = min(max(e.Attempts[i].VerifiedResponses, 0), 8)
	}
	return fmt.Sprintf("authenticated startup time unavailable: outcome=%s attempts=%d quorums=%v verified_responses=%v", outcome(e.cause), n, quorums, verified)
}
func (e *StartupError) Unwrap() error { return e.cause }

// nativeFence serializes all boot observations across attempts AND subsequent
// holdover. Its first boot ID and monotonic high-water cannot be reset by retry.
// A failed native read, changed boot or regression permanently poisons it.
type nativeFence struct {
	mu    sync.Mutex
	read  func() (trustedtime.Reading, error)
	boot  string
	last  time.Duration
	fault bool
}

func (f *nativeFence) now() (trustedtime.Reading, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fault || f.read == nil {
		return trustedtime.Reading{}, trustedtime.ErrUntrusted
	}
	r, err := f.read()
	if err != nil || r.BootID == "" || r.Elapsed < 0 || f.boot != "" && (r.BootID != f.boot || r.Elapsed < f.last) {
		f.fault = true
		return trustedtime.Reading{}, trustedtime.ErrUntrusted
	}
	f.boot, f.last = r.BootID, r.Elapsed
	return r, nil
}

// AcquireStartup allows at most three independent fresh acquisitions. Each
// attempt retains cfg's exact pins, quorum, exchange timeout and interval bounds.
// Only a completed insufficient quorum is retryable; no responses are combined.
// Fixed backoffs are 250ms and 500ms. The overall deadline is three exchange
// timeouts plus those waits, and never exceeds the caller's deadline. No stores,
// listeners, system-clock writes or persistent authority are involved.
func AcquireStartup(ctx context.Context, cfg Config) (*Provider, error) {
	return acquireStartup(ctx, cfg, readNative, waitStartup)
}

func acquireStartup(ctx context.Context, cfg Config, read func() (trustedtime.Reading, error), wait func(context.Context, time.Duration) error) (*Provider, error) {
	if ctx == nil {
		return nil, errors.New("context required")
	}
	cfg.Servers = cloneServers(cfg.Servers)
	if err := validate(cfg); err != nil {
		return nil, err
	}
	budget := StartupMaxAttempts*cfg.QueryTimeout + 750*time.Millisecond
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	fence := &nativeFence{read: read}
	attempts := make([]AttemptDiagnostic, 0, StartupMaxAttempts)
	fail := func(err error) (*Provider, error) {
		return nil, &StartupError{Attempts: append([]AttemptDiagnostic(nil), attempts...), cause: err}
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	start, err := fence.now()
	if err != nil {
		return fail(err)
	}
	checkBudget := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		now, err := fence.now()
		if err != nil {
			return err
		}
		if now.Elapsed-start.Elapsed > budget {
			return context.DeadlineExceeded
		}
		return nil
	}
	for i := 0; i < StartupMaxAttempts; i++ {
		if err := checkBudget(); err != nil {
			return fail(err)
		}
		p, err := acquireFenced(ctx, cfg, fence)
		if budgetErr := checkBudget(); budgetErr != nil {
			p, err = nil, budgetErr
		}
		d := AttemptDiagnostic{Attempt: i + 1, Outcome: outcome(err)}
		var quorum *QuorumError
		if errors.As(err, &quorum) {
			d.Quorum, d.VerifiedResponses = quorum.Quorum, quorum.VerifiedResponses
		}
		if p != nil && err == nil {
			d.Quorum = p.report.Quorum
			for _, evidence := range p.report.Evidence {
				if evidence.Error == "" {
					d.VerifiedResponses++
				}
			}
		}
		attempts = append(attempts, d)
		if err == nil {
			if e := ctx.Err(); e != nil {
				return fail(e)
			}
			p.report.Attempts = append([]AttemptDiagnostic(nil), attempts...)
			return p, nil
		}
		if quorum == nil || quorum.Quorum != 1 || i+1 == StartupMaxAttempts {
			return fail(err)
		}
		if err = wait(ctx, time.Duration(i+1)*250*time.Millisecond); err != nil {
			return fail(err)
		}
	}
	panic("bounded acquisition loop exhausted")
}

func waitStartup(ctx context.Context, delay time.Duration) error {
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func outcome(err error) string {
	if err == nil {
		return "accepted"
	}
	var q *QuorumError
	var bounds *IntervalBoundsError
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case errors.Is(err, trustedtime.ErrUntrusted):
		return "native_clock_fault"
	case errors.As(err, &bounds):
		return "interval_refused"
	case errors.As(err, &q):
		return "insufficient_quorum"
	default:
		return "acquisition_refused"
	}
}
