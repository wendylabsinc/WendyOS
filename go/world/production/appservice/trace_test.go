package appservice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/rpctrace"
	"github.com/wendylabsinc/wendy/go/world/production/serviceaccess"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type tracedPublisher struct {
	Publisher
	validations *atomic.Int32
}

func (p tracedPublisher) Validate(ctx context.Context) error {
	// Handshakes and idle watchers have no RPC peer context. Count only the
	// actual handler checks, independent of background scheduling.
	if _, ok := peer.FromContext(ctx); ok {
		p.validations.Add(1)
	}
	return p.Publisher.Validate(ctx)
}

func traceSink(t *testing.T, capacity int) *rpctrace.Sink {
	t.Helper()
	s, err := rpctrace.New(rpctrace.Config{Capacity: capacity})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func traceRows(t *testing.T, sink *rpctrace.Sink) []rpctrace.Event {
	t.Helper()
	sink.Close()
	var output bytes.Buffer
	if err := sink.Run(context.Background(), &output); err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(&output)
	var rows []rpctrace.Event
	for {
		var row rpctrace.Event
		if err := decoder.Decode(&row); errors.Is(err, io.EOF) {
			return rows
		} else if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, row)
	}
}

func traceStage(event rpctrace.Event, name string) rpctrace.Measurement {
	for _, stage := range event.Stages {
		if stage.Stage == name {
			return stage
		}
	}
	return rpctrace.Measurement{}
}

func TestTracePreservesAuthorityAndResponseBudgets(t *testing.T) {
	type counts struct{ peer, publisher, callback, finalize, responses int32 }
	var baseline counts
	for _, enabled := range []bool{false, true} {
		name := "disabled"
		if enabled {
			name = "enabled"
		}
		t.Run(name, func(t *testing.T) {
			sink := traceSink(t, 8)
			var peerChecks, validations, calls, finalizations atomic.Int32
			f := setup(t, func(cfg *Config) {
				if enabled {
					cfg.Trace = sink
				}
				// Exactly one request/reply pair fits. A second request must
				// retain the existing ResourceExhausted behavior.
				cfg.Limits.MessagesPerSecond = .0001
				cfg.Limits.MessageBurst = 2
				originalPeer, originalFactory := cfg.PeerCheck, cfg.Factory
				cfg.PeerCheck = func(ctx context.Context, p Peer, scope serviceorigin.Scope) error {
					if _, ok := peer.FromContext(ctx); ok {
						peerChecks.Add(1)
					}
					return originalPeer(ctx, p, scope)
				}
				cfg.Factory = func(ctx context.Context, scope serviceorigin.Scope) (Publisher, error) {
					p, err := originalFactory(ctx, scope)
					return tracedPublisher{p, &validations}, err
				}
				cfg.Query = func(ctx context.Context, _ serviceorigin.Scope, _ serviceaccess.Query, fence *Fence) (serviceaccess.Page, error) {
					calls.Add(1)
					return serviceaccess.Page{SignedRecordsJSON: [][]byte{[]byte("exact-response")}}, fence.Commit(ctx, func(context.Context) error { return nil })
				}
				cfg.FinalizeResponse = func(_ context.Context, _ serviceorigin.Scope, reply proto.Message) (proto.Message, error) {
					finalizations.Add(1)
					return reply, nil
				}
			})
			client, _ := dial(t, f.mount(t, f.scope).SocketPath)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			var responses int32
			got, err := client.Query(ctx, brokerAccessQuery())
			if err != nil || len(got.GetSignedRecordJson()) != 1 || string(got.SignedRecordJson[0]) != "exact-response" {
				t.Fatal("tracing changed the successful response", err)
			}
			responses++
			if got, err := client.Query(ctx, brokerAccessQuery()); got != nil || status.Code(err) != codes.ResourceExhausted {
				t.Fatal("tracing changed the request/reply message charges", err)
			}
			actual := counts{peerChecks.Load(), validations.Load(), calls.Load(), finalizations.Load(), responses}
			if actual != (counts{6, 6, 1, 1, 1}) {
				t.Fatal("unexpected existing authority call count", actual)
			}
			if !enabled {
				baseline = actual
			} else if actual != baseline {
				t.Fatal("tracing changed authority or callback counts", actual, baseline)
			}
			rows := traceRows(t, sink)
			if !enabled && len(rows) != 0 || enabled && len(rows) != 2 {
				t.Fatal("unexpected event count", len(rows))
			}
			if enabled {
				if rows[0].Method != "Query" || rows[0].Code != "OK" || rows[1].Code != "ResourceExhausted" || rows[0].App != f.scope.App || rows[0].Service != f.scope.Service || rows[0].Generation != f.scope.DeploymentGeneration {
					t.Fatal("trace changed method, status or commissioned identity")
				}
				for i, count := range []uint64{4, 2} {
					for _, stage := range []string{"authorize", "peer", "publisher"} {
						if traceStage(rows[i], stage).Count != count {
							t.Fatal("trace count differs from actual authority calls", stage)
						}
					}
				}
				if traceStage(rows[0], "finalize").Count != 1 || traceStage(rows[0], "admission_wait").Count != 1 || traceStage(rows[1], "admission_wait").Count != 0 {
					t.Fatal("trace includes work that did not happen")
				}
			}
		})
	}
}

type traceWriteFunc func([]byte) (int, error)

func (f traceWriteFunc) Write(data []byte) (int, error) { return f(data) }

func TestTraceEmitsAfterAdmissionAndHandlerCleanup(t *testing.T) {
	sink := traceSink(t, 8)
	callbackEntered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	var callbackContext context.Context
	var callbackDone, finalized atomic.Bool
	f := setup(t, func(cfg *Config) {
		cfg.Trace = sink
		cfg.Query = func(ctx context.Context, _ serviceorigin.Scope, _ serviceaccess.Query, fence *Fence) (serviceaccess.Page, error) {
			callbackContext = ctx
			close(callbackEntered)
			err := fence.Commit(ctx, func(context.Context) error {
				<-release
				callbackDone.Store(true)
				return nil
			})
			return serviceaccess.Page{}, err
		}
		cfg.FinalizeResponse = func(_ context.Context, _ serviceorigin.Scope, reply proto.Message) (proto.Message, error) {
			finalized.Store(true)
			return reply, nil
		}
	})
	client, _ := dial(t, f.mount(t, f.scope).SocketPath)
	f.manager.mu.Lock()
	s := f.manager.services[bindingKeyForTrace(f.scope)]
	f.manager.mu.Unlock()
	written := make(chan error, 1)
	var recorded rpctrace.Event
	writerCtx, stopWriter := context.WithCancel(context.Background())
	defer stopWriter()
	writerDone := make(chan error, 1)
	go func() {
		writerDone <- sink.Run(writerCtx, traceWriteFunc(func(data []byte) (int, error) {
			var err error
			if json.Unmarshal(data, &recorded) != nil {
				err = errors.New("invalid event")
			}
			if !callbackDone.Load() || !finalized.Load() || callbackContext == nil || callbackContext.Err() == nil || len(s.rpcs) != 0 {
				err = errors.New("trace emitted before handler cleanup")
			}
			if !s.admissionMu.TryLock() {
				err = errors.New("trace writer reached held admission lock")
			} else {
				s.admissionMu.Unlock()
			}
			written <- err
			return len(data), nil
		}))
	}()
	s.admissionMu.Lock()
	var unlockOnce sync.Once
	unlockAdmission := func() { unlockOnce.Do(s.admissionMu.Unlock) }
	defer unlockAdmission()
	result := make(chan error, 1)
	go func() { _, err := client.Query(context.Background(), brokerAccessQuery()); result <- err }()
	finalResponseAwait(t, callbackEntered)
	select {
	case <-written:
		t.Fatal("trace emitted while admission callback was still running")
	default:
	}
	select {
	case <-result:
		unlockAdmission()
		t.Fatal("RPC bypassed held admission lock")
	case <-time.After(25 * time.Millisecond):
	}
	unlockAdmission()
	unblock()
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-written:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("completed RPC trace was not emitted")
	}
	sink.Close()
	if err := <-writerDone; err != nil {
		t.Fatal(err)
	}
	if wait := traceStage(recorded, "admission_wait"); wait.Count != 1 || wait.SumNS < int64(time.Millisecond) || traceStage(recorded, "total").SumNS < wait.SumNS {
		t.Fatal("admission wait did not measure the actual blocked boundary", wait)
	}
}

func bindingKeyForTrace(scope serviceorigin.Scope) string { return scope.App + "\x00" + scope.Service }

func TestTraceBlockedWriterDropsWithoutBlockingRPC(t *testing.T) {
	sink := traceSink(t, 1)
	f := setup(t, func(cfg *Config) {
		cfg.Trace = sink
		cfg.Query = func(ctx context.Context, _ serviceorigin.Scope, _ serviceaccess.Query, fence *Fence) (serviceaccess.Page, error) {
			return serviceaccess.Page{}, fence.Commit(ctx, func(context.Context) error { return nil })
		}
	})
	client, _ := dial(t, f.mount(t, f.scope).SocketPath)
	entered, release := make(chan struct{}), make(chan struct{})
	var once, unblockOnce sync.Once
	unblock := func() { unblockOnce.Do(func() { close(release) }) }
	defer unblock()
	done := make(chan error, 1)
	go func() {
		done <- sink.Run(context.Background(), traceWriteFunc(func(b []byte) (int, error) {
			once.Do(func() { close(entered) })
			<-release
			return len(b), nil
		}))
	}()
	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := client.Query(ctx, brokerAccessQuery())
		cancel()
		if err != nil {
			t.Fatal("trace backpressure changed an RPC", err)
		}
		if i == 0 {
			finalResponseAwait(t, entered)
		}
	}
	stats := sink.Stats()
	if stats.Started != 3 || stats.Finished != 3 || stats.Enqueued != 2 || stats.DroppedQueueFull != 1 || stats.Active != 0 {
		t.Fatal("bounded sink did not drop excess profiling events", stats)
	}
	sink.Close()
	unblock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestTraceStreamCancelFinishesOnlyAfterSlotRelease(t *testing.T) {
	sink := traceSink(t, 8)
	f := setup(t, func(cfg *Config) { cfg.Trace = sink })
	client, _ := dial(t, f.mount(t, f.scope).SocketPath)
	f.manager.mu.Lock()
	s := f.manager.services[bindingKeyForTrace(f.scope)]
	f.manager.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := client.PublishStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(request()); err != nil {
		t.Fatal(err)
	}
	if reply, err := stream.Recv(); err != nil || !reply.GetAccepted() {
		t.Fatal("tracing changed stream publication", err)
	}
	if stats := sink.Stats(); stats.Finished != 0 || stats.Active != 1 {
		t.Fatal("stream produced a terminal event before ending", stats)
	}
	event := make(chan rpctrace.Event, 1)
	done := make(chan error, 1)
	go func() {
		done <- sink.Run(context.Background(), traceWriteFunc(func(raw []byte) (int, error) {
			var row rpctrace.Event
			if json.Unmarshal(raw, &row) != nil || len(s.rpcs) != 0 {
				return 0, errors.New("stream event preceded slot release")
			}
			event <- row
			return len(raw), nil
		}))
	}()
	cancel()
	if _, err := stream.Recv(); status.Code(err) != codes.Canceled {
		t.Fatal("stream cancellation changed", err)
	}
	select {
	case row := <-event:
		// Cancellation may reach the existing pre-Recv authority check or
		// Recv itself. The trace records the actual fixed handler status.
		if row.Method != "PublishStream" || row.Code != "Canceled" && row.Code != "PermissionDenied" || row.App != f.scope.App || row.Generation != f.scope.DeploymentGeneration || traceStage(row, "admission_wait").Count != 1 {
			t.Fatal("stream cancellation produced an incorrect aggregate", row)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("canceled stream did not produce a terminal event")
	}
	sink.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestTraceOnlyUsesGeneratedMethodNamesAndFixedStatus(t *testing.T) {
	methods := []string{
		appv1.WorldAppService_Clock_FullMethodName, appv1.WorldAppService_PhysicalClock_FullMethodName,
		appv1.WorldAppService_Query_FullMethodName, appv1.WorldAppService_GetEvidence_FullMethodName,
		appv1.WorldAppService_PutEvidence_FullMethodName, appv1.WorldAppService_GetRecordProof_FullMethodName,
		appv1.WorldAppService_Publish_FullMethodName, appv1.WorldAppService_PublishStream_FullMethodName,
		appv1.WorldAppService_GetSourceAuthority_FullMethodName,
	}
	seen := map[rpctrace.Method]bool{}
	for _, full := range methods {
		method, ok := traceMethod(full)
		if !ok || method == 0 || seen[method] {
			t.Fatal("generated method does not have a unique fixed label")
		}
		seen[method] = true
	}
	sink := traceSink(t, 8)
	s := &service{manager: &Manager{cfg: Config{Trace: sink}}, scope: serviceorigin.Scope{App: "com.example.camera", Service: "reader", DeploymentGeneration: 1}}
	ctx := context.Background()
	secret := "caller-controlled-token-and-error-must-not-escape"
	if _, err := s.traceUnary(ctx, secret, &grpc.UnaryServerInfo{FullMethod: "/" + secret}, func(got context.Context, _ any) (any, error) {
		if got != ctx {
			t.Fatal("unknown method received a trace context")
		}
		return nil, status.Error(codes.PermissionDenied, secret)
	}); status.Code(err) != codes.PermissionDenied || sink.Stats().Started != 0 {
		t.Fatal("unknown method was traced or changed")
	}
	_, _ = s.traceUnary(ctx, secret, &grpc.UnaryServerInfo{FullMethod: appv1.WorldAppService_Query_FullMethodName}, func(context.Context, any) (any, error) {
		return nil, status.Error(codes.PermissionDenied, secret)
	})
	rows := traceRows(t, sink)
	if len(rows) != 1 || rows[0].Code != "PermissionDenied" {
		t.Fatal("fixed status missing")
	}
	raw, err := json.Marshal(rows)
	if err != nil || bytes.Contains(raw, []byte(secret)) {
		t.Fatal("caller data reached a trace")
	}
}
