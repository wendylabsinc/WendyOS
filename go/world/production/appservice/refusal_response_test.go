package appservice

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"

	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/rpctrace"
	"github.com/wendylabsinc/wendy/go/world/production/serviceevidence"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// This pin is an explicit predicate seam, not a kernel-liveness substitute in
// production. An unregistered connection keeps the background peer watcher
// from consuming the fault before the exact fourth authorize under test.
// The full Linux broker suite separately exercises real Unix peers and pidfds.
type responseRefusalPin struct {
	armed  *atomic.Bool
	reason rpctrace.RefusalReason
	calls  atomic.Int32
}

func (p *responseRefusalPin) observe() (error, pinObservation) {
	p.calls.Add(1)
	if p.armed.Load() && p.reason != 0 {
		return ErrPeer, pinObservation{reason: p.reason}
	}
	return nil, pinObservation{}
}
func (p *responseRefusalPin) Alive() error                             { err, _ := p.observe(); return err }
func (p *responseRefusalPin) aliveDiagnostic() (error, pinObservation) { return p.observe() }
func (*responseRefusalPin) Close() error                               { return nil }

func TestPutEvidenceFourthAuthorizationRefusalPreservesUnknownWrite(t *testing.T) {
	type scenario struct {
		name, reason, point, watcher      string
		pin                               rpctrace.RefusalReason
		credentials, peerChecks, pinCalls int32
	}
	cases := []scenario{
		{name: "success", credentials: 4, peerChecks: 4, pinCalls: 9},
		{name: "initial-pin", reason: "pin_dead", point: "peer_before_scope", pin: rpctrace.ReasonPinDead, credentials: 3, peerChecks: 3, pinCalls: 7},
		{name: "credential-lookup", reason: "credential_lookup", point: "peer_before_scope", credentials: 4, peerChecks: 3, pinCalls: 7},
		{name: "host-scope", reason: "scoped_peer_refused", point: "peer_before_scope", credentials: 4, peerChecks: 4, pinCalls: 7},
		{name: "context-before-peer", reason: "context_canceled", point: "peer_before_scope", credentials: 3, peerChecks: 3, pinCalls: 6},
		{name: "context-after-scope", reason: "context_canceled", point: "peer_after_scope", credentials: 4, peerChecks: 4, pinCalls: 7},
		{name: "watcher-closed-pin", reason: "pin_closed", point: "peer_before_scope", watcher: "pin_dead", pin: rpctrace.ReasonPinClosed, credentials: 3, peerChecks: 3, pinCalls: 7},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			type counts struct{ credentials, peer, publisher, pins, stores, finalizers int32 }
			var untraced counts
			for _, tracing := range []bool{false, true} {
				name := "without-trace"
				if tracing {
					name = "with-trace"
				}
				t.Run(name, func(t *testing.T) {
					sink := traceSink(t, 8)
					var armed, committed atomic.Bool
					var credentialCalls, peerCalls, validations, stores, finalizers atomic.Int32
					var store *serviceevidence.Store
					var conn *trackedConn
					var owner serviceevidence.Owner
					parent, cancel := context.WithCancel(context.Background())
					defer cancel()
					f := setup(t, func(cfg *Config) {
						if tracing {
							cfg.Trace = sink
						}
						originalPeer, originalFactory := cfg.PeerCheck, cfg.Factory
						cfg.PeerCheck = func(ctx context.Context, p Peer, scope serviceorigin.Scope) error {
							if _, ok := peer.FromContext(ctx); ok {
								peerCalls.Add(1)
							}
							if err := originalPeer(ctx, p, scope); err != nil {
								return err
							}
							if armed.Load() {
								switch test.name {
								case "host-scope":
									return ErrPeer
								case "context-after-scope":
									cancel()
								}
							}
							return nil
						}
						cfg.Factory = func(ctx context.Context, scope serviceorigin.Scope) (Publisher, error) {
							p, err := originalFactory(ctx, scope)
							return tracedPublisher{p, &validations}, err
						}
						cfg.PutEvidence = func(ctx context.Context, scope serviceorigin.Scope, source serviceorigin.PublishScope, request serviceevidence.PutRequest, fence *Fence) (serviceevidence.PutResponse, error) {
							owner = serviceevidence.Owner{Tenant: scope.Tenant, Site: scope.Site, Device: scope.Device, App: scope.App, Service: scope.Service, Component: source.ComponentType, Stream: source.Stream}
							var response serviceevidence.PutResponse
							err := fence.Commit(ctx, func(ctx context.Context) error {
								var err error
								response, err = store.Put(ctx, owner, request)
								if err == nil && response.Complete {
									stores.Add(1)
								}
								return err
							})
							if err != nil {
								return response, err
							}
							committed.Store(true)
							if test.name == "watcher-closed-pin" {
								// Preserve the distinction between an earlier watcher
								// observation and the pin-closed predicate now failing.
								conn.watcherReason.Store(uint32(rpctrace.ReasonPinDead))
							}
							armed.Store(true)
							if test.name == "context-before-peer" {
								cancel()
							}
							return response, nil
						}
						cfg.FinalizeResponse = func(_ context.Context, _ serviceorigin.Scope, reply proto.Message) (proto.Message, error) {
							finalizers.Add(1)
							return reply, nil
						}
					})
					var err error
					store, err = serviceevidence.Open(serviceevidence.Config{Directory: filepath.Join(f.config.Directory, "evidence"), Tenant: f.scope.Tenant, Site: f.scope.Site, Device: f.scope.Device, Clock: f.clock})
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = store.Close() })
					f.mount(t, f.scope)
					f.manager.mu.Lock()
					s := f.manager.services[bindingKeyForTrace(f.scope)]
					f.manager.mu.Unlock()
					pin := &responseRefusalPin{armed: &armed, reason: test.pin}
					conn = &trackedConn{service: s, pin: pin}
					expectedPeer := Peer{PID: int32(os.Getpid()), UID: uint32(os.Geteuid()), GID: uint32(os.Getegid())}
					// The nil UnixConn is deliberate. The existing kernel tests
					// cover its acquisition; here each fault is deterministic.
					f.manager.peerCredentials = func(*net.UnixConn) (Peer, error) {
						credentialCalls.Add(1)
						if armed.Load() && test.name == "credential-lookup" {
							return Peer{}, ErrPeer
						}
						return expectedPeer, nil
					}
					ctx := peer.NewContext(parent, &peer.Peer{AuthInfo: serviceAuthInfo{service: s, conn: conn, peer: expectedPeer}})
					data := []byte("exact evidence written before fourth authorization")
					hash := sha256.Sum256(data)
					digest := hex.EncodeToString(hash[:])
					request := &appv1.PutEvidenceRequest{ComponentType: f.scope.Publish[0].ComponentType, Stream: f.scope.Publish[0].Stream, Sha256: digest, Size: int64(len(data)), Data: data, ContentType: "audio/wav"}
					rpc := &rpcServer{service: s}
					handler := func(ctx context.Context, _ any) (any, error) {
						out, err := rpc.PutEvidence(ctx, request)
						if err != nil {
							return nil, err
						}
						return out, nil
					}
					var reply any
					if tracing {
						reply, err = s.traceUnary(ctx, request, &grpc.UnaryServerInfo{FullMethod: appv1.WorldAppService_PutEvidence_FullMethodName}, handler)
					} else {
						reply, err = handler(ctx, request)
					}
					wantPublisher, wantFinalize := int32(3), int32(0)
					if test.name == "success" {
						wantPublisher, wantFinalize = 4, 1
						out, ok := reply.(*appv1.PutEvidenceReply)
						if err != nil || !ok || !out.Complete || out.Sha256 != digest || out.Received != int64(len(data)) {
							t.Fatalf("successful upload changed: %v", err)
						}
					} else if reply != nil || status.Code(err) != codes.Unavailable {
						t.Fatalf("post-write refusal exposed an acknowledgment: %v", err)
					}
					if !committed.Load() || stores.Load() != 1 {
						t.Fatal("test did not complete its actual fenced store write")
					}
					// Read through a separate context, not the canceled RPC.
					// This is positive evidence of a write despite the refused
					// acknowledgment; an error never implies rollback.
					_, retained, readErr := store.ReadComplete(context.Background(), owner, digest, int64(len(data)), "audio/wav")
					if readErr != nil || !bytes.Equal(retained, data) {
						t.Fatalf("completed bytes missing after response gate: %v", readErr)
					}
					got := counts{credentialCalls.Load(), peerCalls.Load(), validations.Load(), pin.calls.Load(), stores.Load(), finalizers.Load()}
					want := counts{test.credentials, test.peerChecks, wantPublisher, test.pinCalls, 1, wantFinalize}
					if got != want {
						t.Fatalf("incorrect authority boundary counts: got %+v want %+v", got, want)
					}
					if !tracing {
						untraced = got
					} else if got != untraced {
						t.Fatal("tracing changed authority or storage work")
					}
					rows := traceRows(t, sink)
					if !tracing {
						if len(rows) != 0 {
							t.Fatal("nil sink emitted an event")
						}
						return
					}
					if len(rows) != 1 {
						t.Fatalf("unexpected handler event count: %d", len(rows))
					}
					event := rows[0]
					if event.Version != 2 || event.Method != "PutEvidence" || event.App != f.scope.App || event.Service != f.scope.Service || event.Generation != f.scope.DeploymentGeneration || event.Code != status.Code(err).String() {
						t.Fatal("trace labels or status do not describe the invocation")
					}
					var refusal *rpctrace.Refusal
					if test.reason != "" {
						refusal = &rpctrace.Refusal{Point: test.point, Reason: test.reason, WatcherReason: test.watcher}
					}
					if !reflect.DeepEqual(event.Refusal, refusal) || len(event.PinInterruptions) != 0 {
						t.Fatalf("wrong first refusal: %+v", event.Refusal)
					}
					for stage, count := range map[string]uint64{"authorize": 4, "peer": 4, "publisher": uint64(wantPublisher), "admission_wait": 1, "finalize": uint64(wantFinalize), "evidence_floor": 2, "total": 1} {
						if traceStage(event, stage).Count != count {
							t.Fatalf("stage %s count differs from actual work", stage)
						}
					}
					if len(s.rpcs) != 0 || !s.admissionMu.TryLock() {
						t.Fatal("event escaped handler cleanup")
					}
					s.admissionMu.Unlock()
				})
			}
		})
	}
}
