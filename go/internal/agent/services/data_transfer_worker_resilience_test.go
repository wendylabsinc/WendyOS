package services

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
)

// beginErrorServer answers BeginEpisodeUpload with a fixed status and counts
// the calls, standing in for a server the device cannot reach (Unavailable) or
// one that rejects this particular episode (InvalidArgument and friends).
type beginErrorServer struct {
	*fakeIngestServer
	err   error
	calls atomic.Int64
}

func (s *beginErrorServer) BeginEpisodeUpload(context.Context, *cloudpb.BeginEpisodeUploadRequest) (*cloudpb.BeginEpisodeUploadResponse, error) {
	s.calls.Add(1)
	return nil, s.err
}

// TestTransportOutageSpendsNoRetryBudget is the regression for the outage that
// ate a backlog. The ingest client connects lazily, so an offline device does
// not fail when it dials: it fails on each episode's first call, with
// Unavailable. Charged to the episode, five passes at the ten second idle
// cadence, under a minute of outage, marked every queued episode permanently
// failed, and only an agent restart brought them back. The network being down
// must cost wall clock and nothing else.
func TestTransportOutageSpendsNoRetryBudget(t *testing.T) {
	mgr, root := newTestManager(t)
	ids := []string{"ep-a", "ep-b", "ep-c"}
	for _, id := range ids {
		writeFixtureEpisode(t, root, id, "", "pending", map[string][]byte{"blob.bin": []byte("payload")})
	}

	srv := &beginErrorServer{fakeIngestServer: newFakeIngestServer(), err: status.Error(codes.Unavailable, "connection refused")}
	w := newTestWorker(mgr, startFakeIngest2(t, srv))

	// More passes than the retry ceiling: the budget must survive all of them.
	for pass := 0; pass < transferMaxAttempts+2; pass++ {
		err := w.runPass(context.Background())
		if err == nil {
			t.Fatalf("pass %d: runPass returned nil; an unreachable ingest must fail the pass", pass)
		}
		if transportStalled(err) == nil {
			t.Fatalf("pass %d: error %v is not classified as a transport stall", pass, err)
		}
	}
	// One episode per pass: the pass abandons the backlog on the first stall
	// rather than walking it and meeting the same wall on every episode.
	if got := srv.calls.Load(); got != int64(transferMaxAttempts+2) {
		t.Errorf("BeginEpisodeUpload called %d times over %d passes; the pass must stop at the first stall",
			got, transferMaxAttempts+2)
	}
	for _, id := range ids {
		st := uploadState(t, mgr, id)
		if st.State != uploadStatePending {
			t.Errorf("%s: state = %q, want %q; an outage may not fail an episode", id, st.State, uploadStatePending)
		}
		if st.Attempts != 0 {
			t.Errorf("%s: attempts = %d, want 0; an outage may not spend the retry budget", id, st.Attempts)
		}
	}
}

// TestEpisodeSpecificErrorSpendsRetryBudget is the other half of the rule: an
// error the server answered about THIS episode is exactly what the per-episode
// budget is for, and must still count.
func TestEpisodeSpecificErrorSpendsRetryBudget(t *testing.T) {
	for _, code := range []codes.Code{codes.InvalidArgument, codes.Internal, codes.FailedPrecondition} {
		t.Run(code.String(), func(t *testing.T) {
			mgr, root := newTestManager(t)
			writeFixtureEpisode(t, root, "ep-bad", "", "pending", map[string][]byte{"blob.bin": []byte("payload")})

			srv := &beginErrorServer{fakeIngestServer: newFakeIngestServer(), err: status.Error(code, "this episode is wrong")}
			w := newTestWorker(mgr, startFakeIngest2(t, srv))

			if err := w.runPass(context.Background()); err != nil {
				t.Fatalf("runPass: %v; an episode-specific rejection stays with the episode", err)
			}
			st := uploadState(t, mgr, "ep-bad")
			if st.State != uploadStatePending {
				t.Fatalf("state = %q, want %q", st.State, uploadStatePending)
			}
			if st.Attempts != 1 {
				t.Errorf("attempts = %d, want 1", st.Attempts)
			}
			if st.NextAttemptUnixNanos == 0 {
				t.Errorf("no per-episode backoff recorded")
			}
		})
	}
}

// TestTransportStalledClassification pins which codes are the network. Too wide
// and a server that rejects one episode stalls the whole worker forever; too
// narrow and an outage eats the backlog again.
func TestTransportStalledClassification(t *testing.T) {
	for code, want := range map[codes.Code]bool{
		codes.Unavailable:        true,
		codes.DeadlineExceeded:   true,
		codes.Unimplemented:      false,
		codes.Unauthenticated:    false,
		codes.PermissionDenied:   false,
		codes.InvalidArgument:    false,
		codes.Internal:           false,
		codes.FailedPrecondition: false,
		codes.ResourceExhausted:  false,
		codes.Unknown:            false,
	} {
		if got := transportStalled(status.Error(code, "x")) != nil; got != want {
			t.Errorf("transportStalled(%s) = %v, want %v", code, got, want)
		}
	}
	if transportStalled(nil) != nil {
		t.Error("transportStalled(nil) must be nil")
	}
	if transportStalled(errors.New("plain")) != nil {
		t.Error("a plain error is not a transport stall")
	}
	if transportStalled(context.DeadlineExceeded) == nil {
		t.Error("a client-side deadline is a transport stall")
	}
	// A wrapped status keeps its classification: the worker wraps every RPC
	// failure with the stage it happened in before returning it.
	if transportStalled(errors.Join(errors.New("begin"), status.Error(codes.Unavailable, "x"))) == nil {
		t.Error("a wrapped Unavailable must still classify as a transport stall")
	}
}

// TestPassBackoffGrowsAndIsCapped pins the pass-level backoff policy that
// replaces the per-episode attempt charge for transport failures: exponential
// from one second, jittered over the upper half of the window, capped at a
// minute.
func TestPassBackoffGrowsAndIsCapped(t *testing.T) {
	var sawSpread bool
	var first time.Duration
	for attempt := 0; attempt < 10; attempt++ {
		want := transferBackoffBase << attempt
		if want > transferMaxBackoff || want <= 0 {
			want = transferMaxBackoff
		}
		for i := 0; i < 64; i++ {
			got := passBackoff(attempt)
			if got < want/2 || got > want {
				t.Fatalf("passBackoff(%d) = %v, want within [%v, %v]", attempt, got, want/2, want)
			}
			if attempt == 0 {
				if first == 0 {
					first = got
				} else if got != first {
					sawSpread = true
				}
			}
		}
	}
	if !sawSpread {
		t.Error("passBackoff never varied; without jitter a fleet retries in lockstep the second the link returns")
	}
}

// TestPeriodicRequeueRearmsFailedEpisodes proves the worker recovers from an
// outage that outlived the retry budget without an agent restart.
// EpisodesAwaitingUpload returns only pending and uploading episodes, so a
// failed one is invisible to every later pass; re-arming only at process start
// meant the backlog stayed dead until somebody restarted the agent.
func TestPeriodicRequeueRearmsFailedEpisodes(t *testing.T) {
	mgr, root := newTestManager(t)
	srv := newFakeIngestServer()
	w := newTestWorker(mgr, startFakeIngest(t, srv))
	w.ingestHost = "ingest.test"

	tick := make(chan time.Time, 1)
	w.newTicker = func(time.Duration) (<-chan time.Time, func()) { return tick, func() {} }
	// Keep the loop off real time, but do not spin: the pass does disk work.
	w.wait = func(ctx context.Context, _ time.Duration) { waitFor(ctx, time.Millisecond) }

	var passes atomic.Int64
	inner := w.factory
	w.factory = func(ctx context.Context) (cloudpb.DataIngestServiceClient, func(), error) {
		passes.Add(1)
		return inner(ctx)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	waitPasses := func(n int64) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for passes.Load() < n {
			if time.Now().After(deadline) {
				t.Fatalf("worker made %d passes, waited for %d", passes.Load(), n)
			}
			time.Sleep(time.Millisecond)
		}
	}
	// Let the startup requeue happen before the episode exists, so only the
	// ticker can explain what follows.
	waitPasses(1)

	writeFixtureEpisode(t, root, "ep-late", "", "failed", map[string][]byte{"blob.bin": []byte("stranded")})
	waitPasses(passes.Load() + 3)
	if got := uploadState(t, mgr, "ep-late").State; got != uploadStateFailed {
		t.Fatalf("state = %q before any requeue tick, want %q", got, uploadStateFailed)
	}

	tick <- time.Now()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if got := uploadState(t, mgr, "ep-late").State; got == uploadStateUploaded {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("state = %q after the requeue tick, want %q", got, uploadStateUploaded)
		}
		time.Sleep(time.Millisecond)
	}
}

// blockingBeginServer holds BeginEpisodeUpload open so a test can act while an
// episode's transfer is in flight.
type blockingBeginServer struct {
	*fakeIngestServer
	entered chan struct{}
	release chan struct{}
}

func (s *blockingBeginServer) BeginEpisodeUpload(ctx context.Context, req *cloudpb.BeginEpisodeUploadRequest) (*cloudpb.BeginEpisodeUploadResponse, error) {
	close(s.entered)
	<-s.release
	return s.fakeIngestServer.BeginEpisodeUpload(ctx, req)
}

// TestUploadInFlightIsPinnedAgainstEviction proves the worker holds the
// manager's read pin for the whole transfer. Quota eviction filters candidates
// on the pin count alone, so before this the "uploading" state bought an
// episode nothing: retention could delete its files from under an open stream,
// and the transfer then failed with an error naming the file rather than the
// eviction.
func TestUploadInFlightIsPinnedAgainstEviction(t *testing.T) {
	mgr, root := newTestManager(t)
	writeFixtureEpisode(t, root, "ep-inflight", "", "pending", map[string][]byte{"blob.bin": []byte("payload under eviction pressure")})
	mgr.SetWarnLogger(func(string) {})

	srv := &blockingBeginServer{
		fakeIngestServer: newFakeIngestServer(),
		entered:          make(chan struct{}),
		release:          make(chan struct{}),
	}
	w := newTestWorker(mgr, startFakeIngest2(t, srv))

	passErr := make(chan error, 1)
	go func() { passErr <- w.runPass(context.Background()) }()

	select {
	case <-srv.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("transfer never started")
	}

	// A quota that cannot hold anything, then a sweep. The in-flight episode
	// must not be a candidate.
	mgr.SetQuota(1, 0)
	if _, err := mgr.Start(data.StartOptions{Sources: []string{"applications"}}); err == nil {
		if _, stopErr := mgr.Stop(data.AdHocEpisodeKey); stopErr != nil {
			t.Fatalf("Stop: %v", stopErr)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "ep-inflight")); err != nil {
		close(srv.release)
		<-passErr
		t.Fatalf("in-flight episode was evicted mid-transfer: %v", err)
	}

	close(srv.release)
	if err := <-passErr; err != nil {
		t.Fatalf("runPass: %v", err)
	}
	if got := uploadState(t, mgr, "ep-inflight").State; got != uploadStateUploaded {
		t.Fatalf("state = %q, want %q", got, uploadStateUploaded)
	}
}

// TestEvictedEpisodeStopsQuietly pins the other side of the retention race. An
// episode that vanishes mid-transfer is not a retry: there is nothing left to
// send and no manifest left to record an attempt on. It used to reach
// handleRetryable, which then failed to write the attempt to a manifest that no
// longer existed and warned about the write instead of the eviction.
func TestEvictedEpisodeStopsQuietly(t *testing.T) {
	mgr, root := newTestManager(t)
	writeFixtureEpisode(t, root, "ep-evicted", "", "pending", map[string][]byte{"blob.bin": []byte("payload")})

	core, logs := observer.New(zap.InfoLevel)
	srv := &blockingBeginServer{
		fakeIngestServer: newFakeIngestServer(),
		entered:          make(chan struct{}),
		release:          make(chan struct{}),
	}
	w := newTestWorker(mgr, startFakeIngest2(t, srv))
	w.logger = zap.New(core)

	passErr := make(chan error, 1)
	go func() { passErr <- w.runPass(context.Background()) }()

	select {
	case <-srv.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("transfer never started")
	}
	// Evict it out from under the open stream, then let the transfer proceed.
	if err := os.RemoveAll(filepath.Join(root, "ep-evicted")); err != nil {
		t.Fatalf("remove episode: %v", err)
	}
	close(srv.release)

	if err := <-passErr; err != nil {
		t.Fatalf("runPass: %v; an evicted episode is not a pass failure", err)
	}
	if _, _, err := mgr.Inspect("ep-evicted", false); err == nil {
		t.Fatal("the evicted episode came back")
	}
	if n := logs.FilterMessageSnippet("evicted mid-transfer").Len(); n != 1 {
		t.Errorf("worker logged the eviction %d times, want 1; entries: %v", n, logs.All())
	}
	if n := logs.FilterMessageSnippet("requeue").Len(); n != 0 {
		t.Errorf("worker tried to requeue an episode that no longer exists: %v", logs.All())
	}
}

// TestMissingFileIsNotAnEviction is the boundary of the rule above: one file
// gone from an episode that is still in the store raises the same
// os.ErrNotExist but is a real, episode-specific fault. Reading it as an
// eviction would drop the episode silently and leave it "uploading" forever,
// retried on every pass and counted by nothing.
func TestMissingFileIsNotAnEviction(t *testing.T) {
	mgr, root := newTestManager(t)
	writeFixtureEpisode(t, root, "ep-holed", "", "pending", map[string][]byte{"blob.bin": []byte("payload")})
	if err := os.Remove(filepath.Join(root, "ep-holed", "blob.bin")); err != nil {
		t.Fatalf("remove file: %v", err)
	}

	w := newTestWorker(mgr, startFakeIngest(t, newFakeIngestServer()))
	if err := w.runPass(context.Background()); err != nil {
		t.Fatalf("runPass: %v", err)
	}
	st := uploadState(t, mgr, "ep-holed")
	if st.State != uploadStatePending {
		t.Fatalf("state = %q, want %q", st.State, uploadStatePending)
	}
	if st.Attempts != 1 {
		t.Errorf("attempts = %d, want 1: a hole in a live episode is the episode's fault", st.Attempts)
	}
}

// commitVerdictServer answers commit with a fixed set of per-file verdicts and
// optionally suppresses the durable offset in its acks, which is how a
// truncated transfer looks on the wire.
type commitVerdictServer struct {
	*fakeIngestServer
	files       []*cloudpb.FileVerification
	withholdAck bool
}

func (s *commitVerdictServer) UploadEpisodeChunk(stream grpc.BidiStreamingServer[cloudpb.EpisodeChunk, cloudpb.EpisodeChunkAck]) error {
	if !s.withholdAck {
		return s.fakeIngestServer.UploadEpisodeChunk(stream)
	}
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		// received_offset only: nothing was made durable.
		if err := stream.Send(&cloudpb.EpisodeChunkAck{
			EpisodeId:      chunk.GetEpisodeId(),
			Path:           chunk.GetPath(),
			ReceivedOffset: chunk.GetOffset() + uint64(len(chunk.GetData())),
		}); err != nil {
			return err
		}
	}
}

func (s *commitVerdictServer) CommitEpisode(context.Context, *cloudpb.CommitEpisodeRequest) (*cloudpb.CommitEpisodeResponse, error) {
	return &cloudpb.CommitEpisodeResponse{State: cloudpb.EpisodeState_EPISODE_STATE_FAILED, Files: s.files}, nil
}

// TestCommitVerdictClassification pins which commit verdicts are terminal.
// Treating every ok == false as local corruption threw away episodes that were
// only ever asking to be sent again: an object the server never stored has no
// hash to disagree with, and a hash over a fragment is a fact about the
// transfer, not about the bytes on disk.
func TestCommitVerdictClassification(t *testing.T) {
	const path = "blob.bin"
	payload := []byte("verify me")

	cases := []struct {
		name        string
		verdict     *cloudpb.FileVerification
		withholdAck bool
		wantState   string
	}{
		{
			name:      "sha mismatch on a stored object is terminal",
			verdict:   &cloudpb.FileVerification{Path: path, Ok: false, ExpectedSha256: "aa", ActualSha256: "bb", Detail: "checksum mismatch"},
			wantState: uploadStateFailed,
		},
		{
			name:      "missing object is retriable",
			verdict:   &cloudpb.FileVerification{Path: path, Ok: false, ExpectedSha256: "aa", ActualSha256: "", Detail: "missing object in storage"},
			wantState: uploadStatePending,
		},
		{
			name:      "an empty actual hash is retriable whatever the detail says",
			verdict:   &cloudpb.FileVerification{Path: path, Ok: false, ExpectedSha256: "aa", ActualSha256: "", Detail: "checksum mismatch"},
			wantState: uploadStatePending,
		},
		{
			name:        "a hash over a never-committed file is retriable",
			verdict:     &cloudpb.FileVerification{Path: path, Ok: false, ExpectedSha256: "aa", ActualSha256: "bb", Detail: "checksum mismatch"},
			withholdAck: true,
			wantState:   uploadStatePending,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mgr, root := newTestManager(t)
			writeFixtureEpisode(t, root, "ep-commit", "", "pending", map[string][]byte{path: payload})
			srv := &commitVerdictServer{
				fakeIngestServer: newFakeIngestServer(),
				files:            []*cloudpb.FileVerification{tc.verdict},
				withholdAck:      tc.withholdAck,
			}
			w := newTestWorker(mgr, startFakeIngest2(t, srv))
			if err := w.runPass(context.Background()); err != nil {
				t.Fatalf("runPass: %v", err)
			}
			st := uploadState(t, mgr, "ep-commit")
			if st.State != tc.wantState {
				t.Fatalf("state = %q, want %q (lastError %q)", st.State, tc.wantState, st.LastError)
			}
			if st.LastError == "" {
				t.Error("failure reason not recorded")
			}
		})
	}
}

// abortingStreamServer accepts the Begin call and then aborts the chunk stream
// with a fixed status, the way a server that revokes a device's access does.
type abortingStreamServer struct {
	*fakeIngestServer
	err error
}

func (s *abortingStreamServer) UploadEpisodeChunk(stream grpc.BidiStreamingServer[cloudpb.EpisodeChunk, cloudpb.EpisodeChunkAck]) error {
	if _, err := stream.Recv(); err != nil {
		return err
	}
	return s.err
}

// TestStreamAbortSurfacesServerStatus proves a server-side abort reaches the
// worker as the server's own status. When the server aborts, the client's Send
// returns io.EOF, which says the stream is closed and nothing about why;
// returning that first masked every abort as an anonymous write failure, so a
// PermissionDenied route was classified as neither blocked nor stalled and the
// episode burned its retry budget against it.
func TestStreamAbortSurfacesServerStatus(t *testing.T) {
	mgr, root := newTestManager(t)
	// Several chunks, so the send loop is still running when the server aborts
	// and Send has an io.EOF to return.
	payload := make([]byte, 4*transferChunkBytes)
	for i := range payload {
		payload[i] = byte(i)
	}
	writeFixtureEpisode(t, root, "ep-abort", "", "pending", map[string][]byte{"blob.bin": payload})

	srv := &abortingStreamServer{fakeIngestServer: newFakeIngestServer(), err: status.Error(codes.PermissionDenied, "device is not allowed to upload")}
	w := newTestWorker(mgr, startFakeIngest2(t, srv))

	err := w.runPass(context.Background())
	if err == nil {
		t.Fatal("runPass returned nil; a PermissionDenied abort is a blocked route")
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.PermissionDenied {
		t.Fatalf("error %v does not carry PermissionDenied; the server's reason was masked", err)
	}
	if ingestBlocked(err) == nil {
		t.Errorf("error %v is not classified as a blocked route", err)
	}
	if state := uploadState(t, mgr, "ep-abort"); state.Attempts != 0 {
		t.Errorf("attempts = %d, want 0: a blocked route costs the episode nothing", state.Attempts)
	}
}

// TestStreamFailurePrefersTheServerReason unit-pins the choice streamFailure
// makes, which the integration test above can only observe indirectly because
// whether Send loses the race and returns io.EOF depends on flow control.
func TestStreamFailurePrefersTheServerReason(t *testing.T) {
	served := status.Error(codes.PermissionDenied, "denied")

	got := streamFailure(io.EOF, nil, served)
	if st, ok := status.FromError(got); !ok || st.Code() != codes.PermissionDenied {
		t.Errorf("streamFailure(io.EOF, nil, PermissionDenied) = %v, want the server status", got)
	}

	// An io.EOF with no ack error still reports the EOF: there is nothing else.
	if got := streamFailure(io.EOF, nil, nil); got == nil || !errors.Is(got, io.EOF) {
		t.Errorf("streamFailure(io.EOF, nil, nil) = %v, want the EOF", got)
	}
	// A real send failure is its own reason and is not replaced.
	sendFail := errors.New("write: broken pipe")
	if got := streamFailure(sendFail, nil, served); !errors.Is(got, sendFail) {
		t.Errorf("streamFailure replaced a genuine send error with the ack error: %v", got)
	}
	if got := streamFailure(nil, nil, served); !errors.Is(got, served) {
		t.Errorf("streamFailure(nil, nil, ack) = %v, want the ack error", got)
	}
	if got := streamFailure(nil, nil, nil); got != nil {
		t.Errorf("streamFailure with no failure = %v, want nil", got)
	}
}
