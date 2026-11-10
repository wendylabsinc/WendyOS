package cloudrelay

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/asyncdiag"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/relaypb"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const renewalDeadlineDiagnostic = "cloud presence renewal deadline; retaining valid presence"

type blockedRenewalDiagnosticCore struct {
	zapcore.Core
	message string
	enabled bool
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	mu      sync.Mutex
	first   []zapcore.Field
}

func (c *blockedRenewalDiagnosticCore) block() { c.once.Do(func() { close(c.entered) }); <-c.release }
func (c *blockedRenewalDiagnosticCore) Enabled(level zapcore.Level) bool {
	if c.enabled && level == zap.WarnLevel {
		c.block()
	}
	return true
}
func (c *blockedRenewalDiagnosticCore) Check(e zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	return ce.AddCore(e, c)
}
func (c *blockedRenewalDiagnosticCore) Write(e zapcore.Entry, fields []zapcore.Field) error {
	if e.Message != c.message {
		return nil
	}
	if !c.enabled {
		c.block()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.first == nil {
		c.first = append([]zapcore.Field(nil), fields...)
	}
	return nil
}
func flushCloudDiagnostics(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := asyncdiag.Flush(ctx); err != nil {
		t.Fatalf("diagnostics did not drain after releasing test sink: %v", err)
	}
}
func newBlockedRenewalDiagnosticCore(message string, enabled bool) *blockedRenewalDiagnosticCore {
	return &blockedRenewalDiagnosticCore{Core: zap.NewNop().Core(), message: message, enabled: enabled, entered: make(chan struct{}), release: make(chan struct{})}
}
func diagnosticOfferAck(t *testing.T, r *renewalRelay, offer *pb.SessionOffer) {
	t.Helper()
	renewalSend(t, r, offerMessage(offer))
	select {
	case request := <-r.requests:
		if ack := request.GetOfferAcknowledgement(); ack == nil || ack.OfferId != offer.OfferId {
			t.Error("wrong offer acknowledgement while diagnostic sink blocked")
		}
	case <-time.After(600 * time.Millisecond):
		t.Error("blocked diagnostic sink held live offer acknowledgement")
	}
}

func TestRenewalDiagnosticSinkCannotBlockOffersExpiryOrCancel(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		sink := "Warn"
		if enabled {
			sink = "Enabled"
		}
		for _, parentCancel := range []bool{false, true} {
			boundary := "expiry"
			if parentCancel {
				boundary = "cancel"
			}
			t.Run(sink+"/"+boundary, func(t *testing.T) {
				core := newBlockedRenewalDiagnosticCore(renewalDeadlineDiagnostic, enabled)
				r, _, cancel, done := startRenewalRelay(t, func(_ context.Context, call int, l *pb.IssuePresenceLeaseResponse) (*pb.IssuePresenceLeaseResponse, error) {
					if call > 1 && l == nil {
						return nil, status.Error(codes.DeadlineExceeded, "fixture renewal deadline")
					}
					return l, nil
				}, renewalOptions{lifetime: 4 * time.Second, prepare: func(a *Agent) { a.Logger = zap.New(core) }})
				finished := false
				t.Cleanup(func() {
					close(core.release)
					cancel()
					if !finished {
						renewalWait(t, done)
					}
					flushCloudDiagnostics(t)
				})
				renewalWait(t, core.entered)
				diagnosticOfferAck(t, r, r.sessionOffer("offer-with-blocked-renewal-log", nil, nil))
				_, initial := r.initialLeaseSnapshot()
				if parentCancel {
					cancel()
					select {
					case err := <-done:
						finished = true
						if !errors.Is(err, context.Canceled) {
							t.Errorf("wrong cancellation: %v", err)
						}
					case <-time.After(350 * time.Millisecond):
						t.Error("blocked diagnostic sink held parent cancellation")
					}
				} else {
					finished = waitPresenceExpiry(t, done, initial.Exp)
				}
			})
		}
	}
}

// A timeout can replay an offer for a lease Cloud never returned. Refusing
// that unverified binding is diagnostic-only and must not stall the owner.
func TestRenewalDeferredRefusalDiagnosticCannotBlockExpiry(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		sink := "Warn"
		if enabled {
			sink = "Enabled"
		}
		t.Run(sink, func(t *testing.T) {
			core := newBlockedRenewalDiagnosticCore("refused cloud relay offer", enabled)
			started, fail := make(chan struct{}), make(chan struct{})
			var issueOnce sync.Once
			r, _, cancel, done := startRenewalRelay(t, func(ctx context.Context, call int, l *pb.IssuePresenceLeaseResponse) (*pb.IssuePresenceLeaseResponse, error) {
				if call > 1 && l == nil {
					issueOnce.Do(func() { close(started) })
					select {
					case <-fail:
						return nil, status.Error(codes.DeadlineExceeded, "fixture unknown issued lease")
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}
				return l, nil
			}, renewalOptions{lifetime: 4 * time.Second, prepare: func(a *Agent) { a.Logger = zap.New(core) }})
			finished := false
			t.Cleanup(func() {
				close(core.release)
				cancel()
				if !finished {
					renewalWait(t, done)
				}
				flushCloudDiagnostics(t)
			})
			renewalWait(t, started)
			unknown := r.sessionOffer("deferred-unverified-binding", nil, func(g *claims) { g.Presence = "not-returned-by-cloud" })
			renewalSend(t, r, offerMessage(unknown))
			// The stream's ordered valid acknowledgement proves the earlier unknown
			// binding has reached its bounded deferral before the RPC deadline.
			diagnosticOfferAck(t, r, r.sessionOffer("before-deadline-barrier", nil, nil))
			close(fail)
			renewalWait(t, core.entered)
			diagnosticOfferAck(t, r, r.sessionOffer("after-replay-warning", nil, nil))
			_, initial := r.initialLeaseSnapshot()
			finished = waitPresenceExpiry(t, done, initial.Exp)
		})
	}
}

func TestRenewalDiagnosticCapturesImmutableScalarSnapshot(t *testing.T) {
	core := newBlockedRenewalDiagnosticCore(renewalDeadlineDiagnostic, false)
	r, _, cancel, done := startRenewalRelay(t, func(_ context.Context, call int, l *pb.IssuePresenceLeaseResponse) (*pb.IssuePresenceLeaseResponse, error) {
		if call > 1 && l == nil {
			return nil, status.Error(codes.DeadlineExceeded, "fixture scalar error")
		}
		return l, nil
	}, renewalOptions{prepare: func(a *Agent) { a.Logger = zap.New(core) }})
	finished := false
	t.Cleanup(func() {
		select {
		case <-core.release:
		default:
			close(core.release)
		}
		cancel()
		if !finished {
			renewalWait(t, done)
		}
		flushCloudDiagnostics(t)
	})
	renewalWait(t, core.entered)
	// Retry scheduling and offers proceed while the first diagnostic retains its
	// original retry/error fields in the bounded process dispatcher.
	diagnosticOfferAck(t, r, r.sessionOffer("snapshot-barrier", nil, nil))
	cancel()
	renewalWait(t, done)
	finished = true
	close(core.release)
	flushCloudDiagnostics(t)
	core.mu.Lock()
	defer core.mu.Unlock()
	fields := map[string]zapcore.Field{}
	for _, f := range core.first {
		fields[f.Key] = f
	}
	if f := fields["retry"]; f.Type != zapcore.DurationType || time.Duration(f.Integer) != time.Second {
		t.Error("retry diagnostic did not capture the original scalar delay")
	}
	if f := fields["error"]; f.Type != zapcore.StringType || f.Interface != nil || !strings.Contains(f.String, "fixture scalar error") {
		t.Error("diagnostic retained a mutable error or omitted its scalar snapshot")
	}
}
