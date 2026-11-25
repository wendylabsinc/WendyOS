package appservice

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"reflect"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/world/production/rpctrace"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"google.golang.org/grpc/peer"
)

type refusalContext struct {
	context.Context
	calls *[]string
	err   error
}

func (c *refusalContext) Err() error {
	*c.calls = append(*c.calls, "context")
	return c.err
}

type refusalPin struct {
	calls       *[]string
	n           int
	failAt      int
	reason      rpctrace.RefusalReason
	interrupted uint8
}

func (p *refusalPin) Alive() error { err, _ := p.aliveDiagnostic(); return err }
func (p *refusalPin) Close() error { return nil }
func (p *refusalPin) aliveDiagnostic() (error, pinObservation) {
	*p.calls = append(*p.calls, "pin")
	p.n++
	if p.n == p.failAt {
		return ErrPeer, pinObservation{reason: p.reason, interruptions: p.interrupted}
	}
	return nil, pinObservation{interruptions: p.interrupted}
}

type refusalPublisher struct {
	Publisher
	calls    *[]string
	done     chan struct{}
	validate func() error
}

func (p refusalPublisher) Done() <-chan struct{} {
	*p.calls = append(*p.calls, "publisher_done")
	return p.done
}
func (p refusalPublisher) Validate(context.Context) error {
	*p.calls = append(*p.calls, "publisher_validate")
	return p.validate()
}

func TestRefusalPreservesAuthorizationOrderAndErrors(t *testing.T) {
	base := []string{"context", "pin", "credentials", "scope", "context", "pin"}
	const privateErrorText = "private error text must not be exported"
	opaque := errors.New(privateErrorText)
	for _, mode := range []string{"success", "canceled", "deadline", "service-closed", "invalid-binding", "pin-closed", "pin-dead", "pin-event", "pin-syscall", "pin-storm", "credentials", "scope", "after-context", "after-closed", "after-pin", "peer-changed", "publisher-done", "publisher-refused", "publisher-context"} {
		t.Run(mode, func(t *testing.T) {
			var baseline []string
			for _, enabled := range []bool{false, true} {
				calls := []string{}
				ctx := &refusalContext{Context: context.Background(), calls: &calls}
				pin := &refusalPin{calls: &calls, reason: rpctrace.ReasonPinDead}
				publisher := refusalPublisher{calls: &calls, done: make(chan struct{}), validate: func() error { return nil }}
				s := &service{manager: &Manager{}, publisher: publisher}
				// This isolated predicate fixture does not start listeners. The
				// real broker response tests exercise its actual asynchronous stop.
				s.stopOnce.Do(func() {})
				conn := &trackedConn{service: s, pin: pin}
				expectedPeer := Peer{PID: 11, UID: 12, GID: 13}
				s.manager.peerCredentials = func(*net.UnixConn) (Peer, error) {
					calls = append(calls, "credentials")
					if mode == "credentials" {
						return Peer{}, opaque
					}
					return expectedPeer, nil
				}
				s.manager.cfg.PeerCheck = func(context.Context, Peer, serviceorigin.Scope) error {
					calls = append(calls, "scope")
					switch mode {
					case "scope":
						return opaque
					case "after-context":
						ctx.err = context.Canceled
					case "after-closed":
						s.closed.Store(true)
					}
					return nil
				}
				wantError := ErrPeer
				wantPoint, wantReason := "peer_before_scope", ""
				wantCalls := append(append([]string(nil), base...), "context", "publisher_done", "publisher_validate")
				auth := serviceAuthInfo{service: s, conn: conn, peer: expectedPeer}
				switch mode {
				case "success":
					wantError = nil
				case "canceled":
					ctx.err = context.Canceled
					wantReason = "context_canceled"
					wantCalls = base[:1]
				case "deadline":
					ctx.err = context.DeadlineExceeded
					wantReason = "context_deadline"
					wantCalls = base[:1]
				case "service-closed":
					s.closed.Store(true)
					wantReason = "service_closed"
					wantCalls = base[:1]
				case "invalid-binding":
					conn.pin = nil
					wantReason = "invalid_binding"
					wantCalls = base[:1]
				case "pin-closed":
					pin.failAt = 1
					pin.reason = rpctrace.ReasonPinClosed
					wantReason = "pin_closed"
					wantCalls = base[:2]
				case "pin-dead":
					pin.failAt = 1
					wantReason = "pin_dead"
					wantCalls = base[:2]
				case "pin-event":
					pin.failAt = 1
					pin.reason = rpctrace.ReasonPinEvent
					wantReason = "pin_event"
					wantCalls = base[:2]
				case "pin-syscall":
					pin.failAt = 1
					pin.reason = rpctrace.ReasonPinSyscall
					wantReason = "pin_syscall"
					wantCalls = base[:2]
				case "pin-storm":
					pin.failAt = 1
					pin.reason = rpctrace.ReasonPinInterruptLimit
					pin.interrupted = 8
					wantReason = "pin_interrupt_limit"
					wantCalls = base[:2]
				case "credentials":
					wantReason = "credential_lookup"
					wantCalls = base[:3]
				case "scope":
					wantReason = "scoped_peer_refused"
					wantCalls = base[:4]
				case "after-context":
					wantPoint, wantReason = "peer_after_scope", "context_canceled"
					wantCalls = base[:5]
				case "after-closed":
					wantPoint, wantReason = "peer_after_scope", "service_closed"
					wantCalls = base[:5]
				case "after-pin":
					pin.failAt = 2
					wantPoint, wantReason = "peer_after_scope", "pin_dead"
					wantCalls = base
				case "peer-changed":
					auth.peer.UID++
					wantPoint, wantReason = "authorize", "peer_changed"
					wantCalls = base
				case "publisher-done":
					close(publisher.done)
					wantError = ErrClosed
					wantPoint, wantReason = "authority", "publisher_done"
					wantCalls = wantCalls[:8]
				case "publisher-refused":
					publisher.validate = func() error { return opaque }
					wantError = opaque
					wantPoint, wantReason = "authority", "publisher_refused"
					wantCalls = append(wantCalls, "context")
				case "publisher-context":
					publisher.validate = func() error { ctx.err = context.DeadlineExceeded; return opaque }
					wantError = context.DeadlineExceeded
					wantPoint, wantReason = "authority", "context_deadline"
					wantCalls = append(wantCalls, "context", "context")
				}
				s.publisher = publisher
				request := peer.NewContext(ctx, &peer.Peer{AuthInfo: auth})
				sink := traceSink(t, 4)
				var active *rpctrace.Sink
				if enabled {
					active = sink
				}
				request, finish := rpctrace.Begin(request, active, rpctrace.Labels{Method: rpctrace.MethodPutEvidence, App: "app", Service: "service", Generation: 1})
				err := s.authorize(request)
				// Literal identity, not just errors.Is, is part of this contract.
				switch err {
				case wantError:
				default:
					t.Fatalf("changed returned error: %v", err)
				}
				finish(rpctrace.Unavailable)
				if !reflect.DeepEqual(calls, wantCalls) {
					t.Fatalf("%s enabled=%v calls=%v want=%v", mode, enabled, calls, wantCalls)
				}
				if !enabled {
					baseline = append([]string(nil), calls...)
					continue
				}
				if !reflect.DeepEqual(calls, baseline) {
					t.Fatal("diagnostics changed authorization calls")
				}
				rows := traceRows(t, sink)
				if len(rows) != 1 {
					t.Fatal("missing bounded refusal event")
				}
				if mode == "success" {
					if rows[0].Refusal != nil {
						t.Fatal("successful checks recorded a refusal")
					}
				} else if rows[0].Refusal == nil || rows[0].Refusal.Point != wantPoint || rows[0].Refusal.Reason != wantReason {
					t.Fatalf("wrong first predicate: %+v", rows[0].Refusal)
				}
				raw, _ := json.Marshal(rows)
				if strings.Contains(string(raw), privateErrorText) {
					t.Fatal("raw error escaped")
				}
			}
		})
	}
}

func TestRefusalResponseAndWatcherHistoryAreDiagnosticOnly(t *testing.T) {
	calls := []string{}
	pin := &refusalPin{calls: &calls, failAt: 1, reason: rpctrace.ReasonPinClosed}
	s := &service{manager: &Manager{}}
	s.watcherReason.Store(uint32(rpctrace.ReasonPublisherDone))
	conn := &trackedConn{service: s, pin: pin}
	conn.watcherReason.Store(uint32(rpctrace.ReasonPinDead))
	ctx := peer.NewContext(context.Background(), &peer.Peer{AuthInfo: serviceAuthInfo{service: s, conn: conn}})
	sink := traceSink(t, 4)
	ctx, finish := rpctrace.Begin(ctx, sink, rpctrace.Labels{Method: rpctrace.MethodQuery, App: "app", Service: "reader", Generation: 1})
	err := s.responseAlive(ctx)
	switch err {
	case ErrPeer:
	default:
		t.Fatal("response error changed")
	}
	finish(rpctrace.Unavailable)
	rows := traceRows(t, sink)
	if len(rows) != 1 || rows[0].Refusal == nil || *rows[0].Refusal != (rpctrace.Refusal{Point: "response", Reason: "pin_closed", WatcherReason: "pin_dead"}) || !reflect.DeepEqual(calls, []string{"pin"}) {
		t.Fatal("response repeated checks or lost watcher observation")
	}
	// Retained causes alone never deny a fresh pin result.
	pin.failAt = 0
	s.publisher = refusalPublisher{calls: &calls, done: make(chan struct{}), validate: func() error { return nil }}
	if err := s.responseAlive(ctx); err != nil {
		t.Fatal("watcher history became authority")
	}
}
