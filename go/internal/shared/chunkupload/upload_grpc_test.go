package chunkupload

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	_ "google.golang.org/grpc/encoding/gzip"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// grpcProbeServer is a minimal WriteChunks implementation for driving Upload's
// failure paths over a real gRPC transport, rather than the in-process
// fakeClient/fakeStream. It exists because grpc-go's own status codes,
// EOF-shaped stream deaths, and flow control only show up over a real
// transport; fakeClient's Send/CloseAndRecv return whatever error a test
// injects, which can't reproduce those shapes.
type grpcProbeServer struct {
	agentpb.UnimplementedWendyContainerServiceServer

	streamsOpened atomic.Int64
	received      atomic.Int64

	// failStream/failAfterMsgs/failErr: the Nth stream (1-based, in open
	// order) fails with failErr after receiving failAfterMsgs messages.
	failStream    int64
	failAfterMsgs int
	failErr       error
	perMsgDelay   time.Duration

	onMsg func(total int64)
}

func (p *grpcProbeServer) WriteChunks(stream grpc.ClientStreamingServer[agentpb.WriteChunksRequest, agentpb.WriteChunksResponse]) error {
	id := p.streamsOpened.Add(1)
	n := 0
	for {
		_, err := stream.Recv()
		if err == io.EOF {
			return stream.SendAndClose(&agentpb.WriteChunksResponse{})
		}
		if err != nil {
			return err
		}
		n++
		total := p.received.Add(1)
		if p.onMsg != nil {
			p.onMsg(total)
		}
		if id == p.failStream && n >= p.failAfterMsgs {
			return p.failErr
		}
		if p.perMsgDelay > 0 {
			time.Sleep(p.perMsgDelay)
		}
	}
}

// startGRPCProbeServer starts ps behind a bufconn listener and dials it. The
// window sizes mirror the agent's production mTLS server (mtls/server.go), so
// these tests see the same flow-control behavior a real device would apply.
func startGRPCProbeServer(t *testing.T, ps *grpcProbeServer) (agentpb.WendyContainerServiceClient, *grpc.Server) {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer(grpc.InitialWindowSize(8<<20), grpc.InitialConnWindowSize(16<<20))
	agentpb.RegisterWendyContainerServiceServer(srv, ps)
	go func() { _ = srv.Serve(lis) }()
	cc, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cc.Close(); srv.Stop(); lis.Close() })
	return agentpb.NewWendyContainerServiceClient(cc), srv
}

// cliRetryable mirrors the CLI's retryableTunnelError (cli/commands/
// chunkresume.go), which chunkupload cannot import: cli/commands depends on
// chunkupload, not the other way around. Kept in sync by hand; the two are
// exercised together end to end by
// TestPushLayersResumingTunnelDropsResumesWithSeveralStreamsOpen in cli/commands.
func cliRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	for cur := err; cur != nil; cur = errors.Unwrap(cur) {
		if status.Code(cur) == codes.Unavailable {
			return true
		}
	}
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// TestUploadResourceExhaustedWinsOverSiblingsMidBatch covers Review Focus #2
// over a real gRPC transport: one stream is refused by the device while its
// three siblings are still mid-batch. Upload must return the ResourceExhausted
// status (not a sibling's context-cancelled error), and every stream slot must
// be released once it returns.
func TestUploadResourceExhaustedWinsOverSiblingsMidBatch(t *testing.T) {
	for i := 0; i < 5; i++ {
		ps := &grpcProbeServer{
			failStream:    2,
			failAfterMsgs: 3,
			failErr:       status.Error(codes.ResourceExhausted, "chunk staging exceeds the device limit"),
			perMsgDelay:   200 * time.Microsecond,
		}
		cs, _ := startGRPCProbeServer(t, ps)
		src, refs := layerFixture(t, 2000, 300)
		err := Upload(context.Background(), cs, src, refs, Options{Layer: "sha256:x", BatchChunks: 256, Streams: 4, Compressor: Gzip})
		if status.Code(err) != codes.ResourceExhausted {
			t.Fatalf("iter %d: error = %v, want ResourceExhausted", i, err)
		}
		if n := len(streamSlots); n != 0 {
			t.Fatalf("iter %d: %d stream slots still held after Upload returned", i, n)
		}
	}
}

// TestUploadConnectionDropMidUploadIsRetryable covers Review Focus #4: an
// abrupt server Stop() mid-transfer, with 4 streams open, must surface as an
// error the CLI's retry loop treats as a resumable tunnel drop (Unavailable or
// an EOF-shaped stream death) rather than something it gives up on, and must
// release every stream slot.
func TestUploadConnectionDropMidUploadIsRetryable(t *testing.T) {
	for i := 0; i < 5; i++ {
		ps := &grpcProbeServer{perMsgDelay: 100 * time.Microsecond}
		var once sync.Once
		var srvRef *grpc.Server
		ready := make(chan struct{})
		ps.onMsg = func(total int64) {
			if total == 700 {
				once.Do(func() {
					<-ready
					go srvRef.Stop() // abrupt: closes every connection
				})
			}
		}
		cs, srv := startGRPCProbeServer(t, ps)
		srvRef = srv
		close(ready)
		src, refs := layerFixture(t, 3000, 300)
		start := time.Now()
		err := Upload(context.Background(), cs, src, refs, Options{Layer: "sha256:x", BatchChunks: 256, Streams: 4, Compressor: Gzip})
		if err == nil {
			t.Fatalf("iter %d: Upload succeeded despite the dropped connection", i)
		}
		if !cliRetryable(err) {
			t.Fatalf("iter %d: error %v (code %v) would not be resumed by the CLI", i, err, status.Code(err))
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Fatalf("iter %d: Upload took %s to report the drop", i, d)
		}
		if n := len(streamSlots); n != 0 {
			t.Fatalf("iter %d: %d stream slots still held after Upload returned", i, n)
		}
		t.Logf("iter %d: %v", i, err)
	}
}

// TestUploadParentCancelReleasesSlots covers the remaining Task 4 gap Important
// 1 raises: a caller-cancelled context must surface as gRPC Canceled (which
// the CLI must NOT resume — a user cancel is not a transport drop) and must
// still release every stream slot.
func TestUploadParentCancelReleasesSlots(t *testing.T) {
	ps := &grpcProbeServer{perMsgDelay: 500 * time.Microsecond}
	cs, _ := startGRPCProbeServer(t, ps)
	src, refs := layerFixture(t, 3000, 300)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for ps.received.Load() < 300 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	start := time.Now()
	err := Upload(ctx, cs, src, refs, Options{Layer: "sha256:x", Streams: 4, Compressor: Gzip})
	t.Logf("cancel -> %v after %s", err, time.Since(start))
	if err == nil {
		t.Fatal("want an error")
	}
	if cliRetryable(err) {
		t.Fatalf("a user cancel must not be resumed, got %v", err)
	}
	if n := len(streamSlots); n != 0 {
		t.Fatalf("%d stream slots still held", n)
	}
}
