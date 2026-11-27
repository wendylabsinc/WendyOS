package livefusion

import (
	"context"
	"errors"
	"sync"
	"time"

	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
)

var ErrClosed = errors.New("fusion session closed or tick budget exhausted")

type TickResult struct {
	Result
	Tick int `json:"tick"`
}

type Summary struct {
	Version         int    `json:"version"`
	RunID           string `json:"run_id"`
	Status          string `json:"status"`
	Ticks           int    `json:"ticks"`
	Published       int    `json:"published"`
	UnknownOutcomes int    `json:"unknown_outcomes,omitempty"`
}

// Session preserves source versions, authenticated clocks and original capture
// candidates between observations. An exact processed tuple is never renewed.
// The caller supplies a bounded lifetime and owns the API connection.
type Session struct {
	mu           sync.Mutex
	cfg          Config
	api          appv1.WorldAppServiceClient
	state        *session
	certificates *wendycert.DeviceVerifier
	window       *candidateWindow
	native       bool
	mediaCache   *nativeMediaCache
	attempted    map[string]struct{}
	ticks        int
	fault        error
}

func New(c Config, api appv1.WorldAppServiceClient) (*Session, error) {
	c, err := c.normalized()
	if err != nil || api == nil {
		return nil, ErrConfig
	}
	return &Session{cfg: c, api: api}, nil
}

// NewNative trusts admitted reads from the application's private World socket.
// The caller must obtain api through its service-scoped native broker, not an
// arbitrary gRPC endpoint. Publish still performs authoritative input admission.
func NewNative(c Config, api appv1.WorldAppServiceClient) (*Session, error) {
	s, err := New(c, api)
	if err == nil {
		s.native = true
		s.mediaCache = &nativeMediaCache{}
		s.attempted = make(map[string]struct{})
	}
	return s, err
}

func (s *Session) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fault = ErrClosed
	s.mediaCache = nil
	if s.state != nil {
		return s.state.clock.Close()
	}
	return nil
}

func (s *Session) Step(ctx context.Context) (out TickResult, err error) {
	if s == nil {
		return out, ErrClosed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out = TickResult{Result: Result{Version: 1, RunID: s.cfg.RunID, Status: "failed"}, Tick: s.ticks + 1}
	if ctx == nil || s.fault != nil || s.ticks >= s.cfg.MaxTicks {
		return out, ErrClosed
	}
	s.ticks++
	c := s.cfg
	c.MaxAttempts = 1
	out.Result, err = run(ctx, c, s.api, s)
	if err != nil && !s.continueUnknown(ctx, out.Result, err) {
		s.fault = err
	}
	return out, err
}

// RunContinuous emits bounded receipts without retaining media history. A
// publication with an uncertain acknowledgment ends the session without retry.
func RunContinuous(ctx context.Context, c Config, api appv1.WorldAppServiceClient, emit func(TickResult) error) (out Summary, err error) {
	return runContinuous(ctx, c, api, emit, New)
}

// RunNativeContinuous is the ordinary native application path. Inputs are
// broker-admitted; the app verifies its own signed publication acknowledgment.
// Unavailable/deadline publication outcomes remain unknown. Their inputs are
// retired, and only a new inference digest may produce another publication.
func RunNativeContinuous(ctx context.Context, c Config, api appv1.WorldAppServiceClient, emit func(TickResult) error) (out Summary, err error) {
	return runContinuous(ctx, c, api, emit, NewNative)
}

func runContinuous(ctx context.Context, c Config, api appv1.WorldAppServiceClient, emit func(TickResult) error, create func(Config, appv1.WorldAppServiceClient) (*Session, error)) (out Summary, err error) {
	if ctx == nil || emit == nil {
		return out, ErrConfig
	}
	s, err := create(c, api)
	if err != nil {
		return out, err
	}
	defer s.Close()
	ctx, cancel := context.WithTimeout(ctx, time.Duration(s.cfg.DurationMS)*time.Millisecond)
	defer cancel()
	out = Summary{Version: 1, RunID: c.RunID, Status: "failed"}
	for i := 0; i < s.cfg.MaxTicks; i++ {
		started := time.Now()
		r, e := s.Step(ctx)
		out.Ticks++
		if r.LocallyVerified {
			out.Published++
		}
		if errors.Is(e, ErrUncertain) {
			out.UnknownOutcomes++
		}
		if emitErr := emit(r); emitErr != nil {
			return out, emitErr
		}
		if e != nil && !s.continueUnknown(ctx, r.Result, e) {
			if errors.Is(e, ErrUncertain) {
				out.Status = "unknown_outcome"
			}
			return out, e
		}
		if i+1 == s.cfg.MaxTicks {
			break
		}
		// Keep the configured start-to-start cadence. Slow evaluations and
		// receipt emission already consume the interval; do not add another
		// full polling delay after them. The loop remains strictly serial.
		if err := ctx.Err(); err != nil {
			return out, err
		}
		delay := time.Duration(s.cfg.PollIntervalMS)*time.Millisecond - time.Since(started)
		if delay <= 0 {
			continue
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return out, ctx.Err()
		case <-timer.C:
		}
	}
	out.Status = "complete"
	if out.UnknownOutcomes > 0 {
		out.Status = "complete_with_unknown_outcomes"
	}
	return out, nil
}
