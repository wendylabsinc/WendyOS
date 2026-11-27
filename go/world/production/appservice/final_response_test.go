package appservice

import (
	"bytes"
	"context"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/serviceaccess"
	"github.com/wendylabsinc/wendy/go/world/production/serviceevidence"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"google.golang.org/protobuf/proto"
)

func finalResponseAwait(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("RPC did not reach the expected final response barrier")
	}
}

func TestFinalResponseRunsAfterLastBlockingPeerCheck(t *testing.T) {
	for _, expire := range []bool{false, true} {
		name := "still-retained"
		if expire {
			name = "expires-at-peer-barrier"
		}
		t.Run(name, func(t *testing.T) {
			entered, resume := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(resume) }) }
			defer unblock()
			var callbackDone, peerDone, finalizerCalled atomic.Bool
			var clock *testClock
			deadline := time.Date(2026, 11, 11, 12, 0, 2, 0, time.UTC).UnixMilli()
			data := []byte("retained private response")
			f := setup(t, func(cfg *Config) {
				original := cfg.PeerCheck
				cfg.PeerCheck = func(ctx context.Context, peer Peer, scope serviceorigin.Scope) error {
					if err := original(ctx, peer, scope); err != nil {
						return err
					}
					if callbackDone.Load() {
						close(entered)
						select {
						case <-resume:
							peerDone.Store(true)
						case <-ctx.Done():
							return ctx.Err()
						}
					}
					return nil
				}
				cfg.GetEvidence = func(ctx context.Context, _ serviceorigin.Scope, _ serviceaccess.Source, q serviceevidence.GetRequest, fence *Fence) (serviceevidence.GetResponse, error) {
					if err := fence.Commit(ctx, func(context.Context) error { return nil }); err != nil {
						return serviceevidence.GetResponse{}, err
					}
					callbackDone.Store(true)
					return serviceevidence.GetResponse{Reference: serviceevidence.Reference{SHA256: q.SHA256, Size: int64(len(data)), ContentType: "image/jpeg", RetainUntilUnixMS: deadline}, Data: data}, nil
				}
				cfg.FinalizeResponse = func(_ context.Context, _ serviceorigin.Scope, response proto.Message) (proto.Message, error) {
					finalizerCalled.Store(true)
					window, err := clock.Now()
					if !peerDone.Load() || err != nil || response.(*appv1.GetEvidenceReply).RetainUntilUnixMs <= window.Latest.UnixMilli() {
						return nil, ErrAdmission
					}
					return response, nil
				}
			})
			clock = f.clock
			client, _ := dial(t, f.mount(t, f.scope).SocketPath)
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			type result struct {
				out *appv1.GetEvidenceReply
				err error
			}
			done := make(chan result, 1)
			go func() {
				out, err := client.GetEvidence(ctx, &appv1.GetEvidenceRequest{Source: brokerAccessQuery().Sources[0], Sha256: strings.Repeat("a", 64), Length: int32(len(data))})
				done <- result{out, err}
			}()
			finalResponseAwait(t, entered)
			if finalizerCalled.Load() {
				t.Fatal("final response hook ran before the final peer check finished")
			}
			if expire {
				clock.advance(3 * time.Second)
			}
			unblock()
			select {
			case got := <-done:
				if !finalizerCalled.Load() {
					t.Fatal("final response hook was skipped")
				}
				if expire {
					if got.err == nil || got.out != nil {
						t.Fatal("expired bytes escaped the post-peer response check")
					}
				} else if got.err != nil || !bytes.Equal(got.out.GetData(), data) {
					t.Fatalf("current response was lost or changed: %v", got.err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("evidence RPC did not finish")
			}
		})
	}
}

func TestFinalResponseCanFilterButCannotGrowOrExposeRejectedReply(t *testing.T) {
	for _, mode := range []string{"filter", "error", "nil", "typed-nil", "grow"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t, func(cfg *Config) {
				cfg.Query = func(ctx context.Context, _ serviceorigin.Scope, _ serviceaccess.Query, fence *Fence) (serviceaccess.Page, error) {
					return serviceaccess.Page{SignedRecordsJSON: [][]byte{[]byte("old-proof"), []byte("current-proof")}}, fence.Commit(ctx, func(context.Context) error { return nil })
				}
				cfg.FinalizeResponse = func(_ context.Context, _ serviceorigin.Scope, reply proto.Message) (proto.Message, error) {
					switch mode {
					case "filter":
						return &appv1.QueryReply{SignedRecordJson: reply.(*appv1.QueryReply).SignedRecordJson[1:]}, nil
					case "error":
						return reply, ErrAdmission
					case "nil":
						return nil, nil
					case "typed-nil":
						return (*appv1.QueryReply)(nil), nil
					default:
						return &appv1.QueryReply{SignedRecordJson: [][]byte{bytes.Repeat([]byte("x"), 1024)}}, nil
					}
				}
			})
			client, _ := dial(t, f.mount(t, f.scope).SocketPath)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			got, err := client.Query(ctx, brokerAccessQuery())
			if mode == "filter" {
				if err != nil || len(got.GetSignedRecordJson()) != 1 || string(got.SignedRecordJson[0]) != "current-proof" {
					t.Fatalf("filtered original response was not preserved: %v", err)
				}
			} else if err == nil || got != nil {
				t.Fatal("rejected or enlarged final response escaped")
			}
		})
	}
}

// This explicit unit seam controls process death during the finalizer. Other
// Linux tests retain real SO_PEERCRED/SO_PEERPIDFD and actual client processes.
type finalResponsePin struct{ dead atomic.Bool }

func (p *finalResponsePin) Alive() error {
	if p.dead.Load() {
		return ErrPeer
	}
	return nil
}
func (*finalResponsePin) Close() error { return nil }

func TestFinalResponseRechecksPinnedProcessAfterFinalizer(t *testing.T) {
	entered, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(resume) }) }
	defer unblock()
	f := setup(t, func(cfg *Config) {
		cfg.Query = func(ctx context.Context, _ serviceorigin.Scope, _ serviceaccess.Query, fence *Fence) (serviceaccess.Page, error) {
			return serviceaccess.Page{SignedRecordsJSON: [][]byte{[]byte("must remain private")}}, fence.Commit(ctx, func(context.Context) error { return nil })
		}
		cfg.FinalizeResponse = func(ctx context.Context, _ serviceorigin.Scope, reply proto.Message) (proto.Message, error) {
			close(entered)
			select {
			case <-resume:
				return reply, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	})
	pin := &finalResponsePin{}
	f.manager.peerPin = func(*net.UnixConn) (peerPin, error) { return pin, nil }
	client, _ := dial(t, f.mount(t, f.scope).SocketPath)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	type result struct {
		out *appv1.QueryReply
		err error
	}
	done := make(chan result, 1)
	go func() { out, err := client.Query(ctx, brokerAccessQuery()); done <- result{out, err} }()
	finalResponseAwait(t, entered)
	pin.dead.Store(true)
	unblock()
	select {
	case got := <-done:
		if got.err == nil || got.out != nil {
			t.Fatal("dead original process received a finalized response")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("query did not finish after finalizer release")
	}
}
