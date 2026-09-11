package services

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
)

// startFakeIngest2 registers an arbitrary DataIngestService implementation on a
// bufconn server and returns a connected client (companion to startFakeIngest,
// which is typed to the concrete fakeIngestServer).
func startFakeIngest2(t *testing.T, srv cloudpb.DataIngestServiceServer) cloudpb.DataIngestServiceClient {
	t.Helper()
	lis := bufconn.Listen(1024 * 1024)
	g := grpc.NewServer()
	cloudpb.RegisterDataIngestServiceServer(g, srv)
	go func() { _ = g.Serve(lis) }()
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() {
		conn.Close()
		g.Stop()
		lis.Close()
	})
	return cloudpb.NewDataIngestServiceClient(conn)
}

// TestDataTransferWorker_ZeroLengthFileUploaded proves that a finalized episode
// containing a zero-length file (for example an empty events.jsonl that a source
// produced no records for) actually streams that file to the cloud. The worker
// has explicit zero-length handling in streamOneFile (send a single empty EOF
// chunk so the server persists the object), but streamFiles must route the file
// there rather than skipping it. A skipped empty file is silent episode data
// loss: the manifest lists it, the server never learns of it.
func TestDataTransferWorker_ZeroLengthFileUploaded(t *testing.T) {
	mgr, root := newTestManager(t)
	files := map[string][]byte{
		"events.jsonl": {},                  // zero-length, must still be uploaded
		"payload.bin":  []byte("real data"), // non-empty companion
	}
	writeFixtureEpisode(t, root, "ep-empty", "", "pending", files)

	srv := newFakeIngestServer()
	client := startFakeIngest(t, srv)
	w := newTestWorker(mgr, client)

	if err := w.runPass(context.Background()); err != nil {
		t.Fatalf("runPass: %v", err)
	}

	if got := uploadState(t, mgr, "ep-empty").State; got != uploadStateUploaded {
		t.Fatalf("state = %q, want uploaded", got)
	}
	if _, ok := srv.received["events.jsonl"]; !ok {
		t.Errorf("zero-length events.jsonl was never streamed to the server (silent loss); a chunk must be sent so the empty object is persisted")
	}
}

// TestDataTransferWorker_PartialResumeOffset proves the classic resume bug is
// absent: when Begin reports a PARTIAL committed offset for a file, the worker
// seeks the local file to that offset and streams the remaining bytes tagged
// with the correct absolute offset, so the reassembled object equals the
// original. Streaming from 0 while telling the server offset=X would corrupt the
// stored object.
func TestDataTransferWorker_PartialResumeOffset(t *testing.T) {
	mgr, root := newTestManager(t)
	full := []byte("0123456789abcdefghijABCDEFGHIJ") // 30 bytes
	const committedOffset = 10
	writeFixtureEpisode(t, root, "ep-partial", "", "uploading", map[string][]byte{"blob.bin": full})

	srv := newFakeIngestServer()
	// Simulate the server already durably holding the first 10 bytes and, on a
	// resume, expecting the worker to append the rest at absolute offset 10.
	srv.presetCommitted["blob.bin"] = committedOffset
	srv.received["blob.bin"] = append([]byte(nil), full[:committedOffset]...)
	srv.committed["blob.bin"] = committedOffset
	client := startFakeIngest(t, srv)
	w := newTestWorker(mgr, client)

	if err := w.runPass(context.Background()); err != nil {
		t.Fatalf("runPass: %v", err)
	}
	if got := uploadState(t, mgr, "ep-partial").State; got != uploadStateUploaded {
		t.Fatalf("state = %q, want uploaded", got)
	}
	if string(srv.received["blob.bin"]) != string(full) {
		t.Errorf("reassembled object = %q, want %q (resume streamed from wrong offset)", srv.received["blob.bin"], full)
	}
}

// TestDataTransferWorker_PacingAbortsOnShutdown proves the bandwidth pacer is
// context-cancellable: with a pathologically small rate the per-chunk sleep would
// otherwise block the worker for days, but cancelling the context returns the
// pass promptly rather than hanging shutdown.
func TestDataTransferWorker_PacingAbortsOnShutdown(t *testing.T) {
	mgr, root := newTestManager(t)
	content := make([]byte, 64*1024)
	writeFixtureEpisode(t, root, "ep-slow", "slow-campaign", "pending", map[string][]byte{"blob.bin": content})
	// 1 byte/sec against 64 KiB = ~18 hours of pacing sleep if not cancellable.
	writeCampaign(t, root, "slow-campaign", "always", "1")

	srv := newFakeIngestServer()
	client := startFakeIngest(t, srv)
	w := newTestWorker(mgr, client)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	done := make(chan struct{})
	go func() {
		_ = w.runPass(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runPass did not return after context cancel; pacing sleep is not abortable (shutdown would hang)")
	}
	// The episode must not be left "uploaded" after an aborted transfer.
	if got := uploadState(t, mgr, "ep-slow").State; got == uploadStateUploaded {
		t.Errorf("state = uploaded after aborted transfer; incomplete upload marked complete")
	}
}

// TestDataTransferWorker_TransientCommitErrorIsRetryable proves a network drop
// during CommitEpisode is classified as retryable, not misclassified as a
// terminal verification failure that would strand a good episode forever.
//
// The drop surfaces as DeadlineExceeded, which is the network rather than the
// episode, so it is a pass-level condition: the episode goes back to pending
// with its attempt count untouched and the worker backs the whole pass off.
// Charging it to the episode is what let a short outage mark a whole backlog
// failed; see TestTransportOutageSpendsNoRetryBudget.
func TestDataTransferWorker_TransientCommitErrorIsRetryable(t *testing.T) {
	mgr, root := newTestManager(t)
	writeFixtureEpisode(t, root, "ep-commiterr", "", "pending", map[string][]byte{"blob.bin": []byte("good bytes")})

	srv := &commitErrorServer{fakeIngestServer: newFakeIngestServer()}
	client := startFakeIngest2(t, srv)
	w := newTestWorker(mgr, client)

	err := w.runPass(context.Background())
	if err == nil {
		t.Fatal("runPass returned nil; a transport failure must surface as a pass failure")
	}
	if transportStalled(err) == nil {
		t.Fatalf("runPass error %v is not classified as a transport stall", err)
	}
	st := uploadState(t, mgr, "ep-commiterr")
	if st.State != uploadStatePending {
		t.Fatalf("state = %q, want pending (transient commit error must be retryable, not failed)", st.State)
	}
	if st.Attempts != 0 {
		t.Errorf("attempts = %d, want 0: the network failing is not the episode's fault", st.Attempts)
	}
}

// commitErrorServer accepts the chunk stream but fails CommitEpisode with a
// transport-style RPC error, standing in for a network drop during commit.
type commitErrorServer struct {
	*fakeIngestServer
}

func (s *commitErrorServer) CommitEpisode(context.Context, *cloudpb.CommitEpisodeRequest) (*cloudpb.CommitEpisodeResponse, error) {
	return nil, context.DeadlineExceeded
}
