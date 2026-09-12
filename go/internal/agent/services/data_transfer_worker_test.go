package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
)

// fakeIngestServer is an in-process DataIngestService used to drive the transfer
// worker. It records received bytes per path, verifies SHA-256 on commit, and
// can be configured to report resume offsets or force verification failures.
type fakeIngestServer struct {
	cloudpb.UnimplementedDataIngestServiceServer

	mu sync.Mutex

	expected  map[string]string // path -> expected sha256 (from Begin manifest)
	sizes     map[string]int64  // path -> declared size
	received  map[string][]byte // path -> bytes streamed this session
	committed map[string]int64  // path -> durable offset

	presetCommitted map[string]int64 // returned by Begin (resume simulation)
	corruptPaths    map[string]bool  // force verification failure for these paths
	beginState      cloudpb.EpisodeState

	beginCalls  int
	commitCalls int

	// Distinct episode ids seen on each UploadEpisodeChunk stream, one entry
	// per stream opened. The client must keep to one episode per stream while
	// it does not match acks on the (episode_id, path) pair.
	streamEpisodes []map[string]bool

	// Incoming request metadata recorded by startFakeIngest, per call kind, so
	// tests can assert on what the client actually put on the wire.
	unaryMD  metadata.MD
	streamMD metadata.MD
}

// recordMD stores the metadata seen on an incoming call.
func (s *fakeIngestServer) recordMD(stream bool, md metadata.MD) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if stream {
		s.streamMD = md
	} else {
		s.unaryMD = md
	}
}

// headerValues returns the values the client sent for key, on the unary and on
// the streaming call respectively.
func (s *fakeIngestServer) headerValues(key string) (unary, stream []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.unaryMD.Get(key), s.streamMD.Get(key)
}

func newFakeIngestServer() *fakeIngestServer {
	return &fakeIngestServer{
		expected:        map[string]string{},
		sizes:           map[string]int64{},
		received:        map[string][]byte{},
		committed:       map[string]int64{},
		presetCommitted: map[string]int64{},
		corruptPaths:    map[string]bool{},
		beginState:      cloudpb.EpisodeState_EPISODE_STATE_UPLOADING,
	}
}

func (s *fakeIngestServer) BeginEpisodeUpload(_ context.Context, req *cloudpb.BeginEpisodeUploadRequest) (*cloudpb.BeginEpisodeUploadResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.beginCalls++
	var files []*cloudpb.FileUploadState
	for _, f := range req.GetManifest().GetFiles() {
		s.expected[f.GetPath()] = f.GetSha256()
		s.sizes[f.GetPath()] = int64(f.GetSizeBytes())
		off := s.presetCommitted[f.GetPath()]
		if off > 0 {
			s.committed[f.GetPath()] = off
		}
		files = append(files, &cloudpb.FileUploadState{Path: f.GetPath(), CommittedOffset: uint64(off)})
	}
	return &cloudpb.BeginEpisodeUploadResponse{State: s.beginState, Files: files}, nil
}

func (s *fakeIngestServer) UploadEpisodeChunk(stream grpc.BidiStreamingServer[cloudpb.EpisodeChunk, cloudpb.EpisodeChunkAck]) error {
	seen := map[string]bool{}
	s.mu.Lock()
	s.streamEpisodes = append(s.streamEpisodes, seen)
	s.mu.Unlock()
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		s.mu.Lock()
		seen[chunk.GetEpisodeId()] = true
		s.received[chunk.GetPath()] = append(s.received[chunk.GetPath()], chunk.GetData()...)
		recv := int64(len(s.received[chunk.GetPath()]))
		var committed int64
		if chunk.GetEof() {
			s.committed[chunk.GetPath()] = recv
			committed = recv
		}
		s.mu.Unlock()
		// A server that carries the field echoes the chunk's episode_id, so the
		// ack names the (episode_id, path) pair the contract matches on.
		if err := stream.Send(&cloudpb.EpisodeChunkAck{
			EpisodeId:       chunk.GetEpisodeId(),
			Path:            chunk.GetPath(),
			ReceivedOffset:  uint64(recv),
			CommittedOffset: uint64(committed),
		}); err != nil {
			return err
		}
	}
}

func (s *fakeIngestServer) CommitEpisode(_ context.Context, _ *cloudpb.CommitEpisodeRequest) (*cloudpb.CommitEpisodeResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commitCalls++
	var files []*cloudpb.FileVerification
	allOK := true
	for path, exp := range s.expected {
		var actual string
		if b, ok := s.received[path]; ok {
			sum := sha256.Sum256(b)
			actual = hex.EncodeToString(sum[:])
		} else if s.committed[path] >= s.sizes[path] {
			// Already durably stored from a prior session; not re-sent.
			actual = exp
		}
		ok := actual == exp
		detail := ""
		if s.corruptPaths[path] {
			ok = false
			actual = "0000000000000000000000000000000000000000000000000000000000000000"
			detail = "forced checksum mismatch"
		}
		if !ok {
			allOK = false
			if detail == "" {
				detail = "checksum mismatch"
			}
		}
		files = append(files, &cloudpb.FileVerification{Path: path, Ok: ok, ExpectedSha256: exp, ActualSha256: actual, Detail: detail})
	}
	state := cloudpb.EpisodeState_EPISODE_STATE_COMPLETE
	if !allOK {
		state = cloudpb.EpisodeState_EPISODE_STATE_FAILED
	}
	return &cloudpb.CommitEpisodeResponse{State: state, Files: files}, nil
}

// startFakeIngest registers the fake on a bufconn gRPC server and returns a
// connected client plus a cleanup function. The server records the incoming
// metadata of every call on srv. Any clientOpts are appended to the client's
// dial options, which lets a test exercise real client interceptors.
func startFakeIngest(t *testing.T, srv *fakeIngestServer, clientOpts ...grpc.DialOption) cloudpb.DataIngestServiceClient {
	t.Helper()
	lis := bufconn.Listen(1024 * 1024)
	g := grpc.NewServer(
		grpc.UnaryInterceptor(func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
			md, _ := metadata.FromIncomingContext(ctx)
			srv.recordMD(false, md)
			return handler(ctx, req)
		}),
		grpc.StreamInterceptor(func(v any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
			md, _ := metadata.FromIncomingContext(ss.Context())
			srv.recordMD(true, md)
			return handler(v, ss)
		}),
	)
	cloudpb.RegisterDataIngestServiceServer(g, srv)
	go func() { _ = g.Serve(lis) }()
	opts := append([]grpc.DialOption{
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}, clientOpts...)
	conn, err := grpc.NewClient("passthrough:///bufnet", opts...)
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

// newTestWorker builds a worker wired to the given manager and fake ingest
// client, with real-time pacing/backoff by default.
func newTestWorker(mgr *data.Manager, client cloudpb.DataIngestServiceClient) *DataTransferWorker {
	w := &DataTransferWorker{
		logger:      zap.NewNop(),
		manager:     mgr,
		maxAttempts: transferMaxAttempts,
		now:         time.Now,
		newSleeper:  contextSleeper,
	}
	w.factory = func(context.Context) (cloudpb.DataIngestServiceClient, func(), error) {
		return client, func() {}, nil
	}
	return w
}

// writeFixtureEpisode creates a finalized episode directory with the given files
// and upload state. Returns the computed total size.
func writeFixtureEpisode(t *testing.T, root, id, campaign, uploadState string, files map[string][]byte) {
	t.Helper()
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir episode: %v", err)
	}
	var manifestFiles []data.File
	for path, content := range files {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatalf("mkdir file parent: %v", err)
		}
		if err := os.WriteFile(full, content, 0o640); err != nil {
			t.Fatalf("write file: %v", err)
		}
		sum := sha256.Sum256(content)
		manifestFiles = append(manifestFiles, data.File{
			Path:      path,
			Size:      int64(len(content)),
			SHA256:    hex.EncodeToString(sum[:]),
			Format:    "bin",
			MediaType: "application/octet-stream",
		})
	}
	mf := data.Manifest{
		Version:          data.ManifestVersion,
		ID:               id,
		State:            "complete",
		StartedUnixNanos: time.Now().UnixNano(),
		Trigger:          data.EpisodeTrigger{Reason: "test", CampaignName: campaign},
		Files:            manifestFiles,
		Upload:           data.WorkflowState{State: uploadState},
	}
	b, err := json.MarshalIndent(mf, "", "  ")
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), b, 0o640); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

// writeCampaign writes a campaign plan JSON the manager can read.
func writeCampaign(t *testing.T, root, name, when, maxRate string) {
	t.Helper()
	dir := filepath.Join(filepath.Dir(root), "campaigns")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir campaigns: %v", err)
	}
	c := data.Campaign{
		Version: 1,
		Name:    name,
		Upload:  data.CampaignUpload{When: when, MaxRate: maxRate},
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		t.Fatalf("marshal campaign: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".json"), b, 0o640); err != nil {
		t.Fatalf("write campaign: %v", err)
	}
}

func newTestManager(t *testing.T) (*data.Manager, string) {
	t.Helper()
	tmp := t.TempDir()
	root := filepath.Join(tmp, "episodes")
	mgr, err := data.NewManager(root)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return mgr, root
}

func uploadState(t *testing.T, mgr *data.Manager, id string) data.WorkflowState {
	t.Helper()
	mf, _, err := mgr.Inspect(id, false)
	if err != nil {
		t.Fatalf("Inspect %s: %v", id, err)
	}
	return mf.Upload
}

func TestDataTransferWorker_HappyPath(t *testing.T) {
	mgr, root := newTestManager(t)
	fileA := []byte("hello wendy data platform")
	fileB := make([]byte, 3*1024) // multi-KiB, still one chunk
	for i := range fileB {
		fileB[i] = byte(i)
	}
	files := map[string][]byte{"video/000001.jpg": fileA, "meta.json": fileB}
	writeFixtureEpisode(t, root, "ep-happy", "", "pending", files)

	srv := newFakeIngestServer()
	client := startFakeIngest(t, srv)
	w := newTestWorker(mgr, client)

	if err := w.runPass(context.Background()); err != nil {
		t.Fatalf("runPass: %v", err)
	}

	if got := uploadState(t, mgr, "ep-happy").State; got != uploadStateUploaded {
		t.Fatalf("state = %q, want uploaded", got)
	}
	if srv.beginCalls != 1 || srv.commitCalls != 1 {
		t.Fatalf("begin=%d commit=%d, want 1/1", srv.beginCalls, srv.commitCalls)
	}
	for path, want := range files {
		if got := srv.received[path]; string(got) != string(want) {
			t.Errorf("file %s: received %d bytes, want %d", path, len(got), len(want))
		}
	}
}

func TestDataTransferWorker_ResumeSkipsCommittedFiles(t *testing.T) {
	mgr, root := newTestManager(t)
	done := []byte("already uploaded before the crash")
	pending := []byte("still needs uploading")
	files := map[string][]byte{"done.bin": done, "pending.bin": pending}
	// Episode left "uploading" by a crashed worker.
	writeFixtureEpisode(t, root, "ep-resume", "", "uploading", files)

	srv := newFakeIngestServer()
	// Server already has done.bin fully (file-granularity committed offset).
	srv.presetCommitted["done.bin"] = int64(len(done))
	client := startFakeIngest(t, srv)
	w := newTestWorker(mgr, client)

	if err := w.runPass(context.Background()); err != nil {
		t.Fatalf("runPass: %v", err)
	}

	if got := uploadState(t, mgr, "ep-resume").State; got != uploadStateUploaded {
		t.Fatalf("state = %q, want uploaded", got)
	}
	if len(srv.received["done.bin"]) != 0 {
		t.Errorf("done.bin re-uploaded (%d bytes); resume must skip committed files", len(srv.received["done.bin"]))
	}
	if string(srv.received["pending.bin"]) != string(pending) {
		t.Errorf("pending.bin not fully uploaded")
	}
	if srv.beginCalls != 1 {
		t.Errorf("begin called %d times, want 1 (idempotent)", srv.beginCalls)
	}
}

func TestDataTransferWorker_BandwidthCeilingPaces(t *testing.T) {
	mgr, root := newTestManager(t)
	const size = 128 * 1024 // 128 KiB, one chunk
	content := make([]byte, size)
	for i := range content {
		content[i] = byte(i)
	}
	writeFixtureEpisode(t, root, "ep-paced", "paced-campaign", "pending", map[string][]byte{"blob.bin": content})
	// 256 KiB/s ceiling -> 128 KiB should take ~0.5s.
	writeCampaign(t, root, "paced-campaign", "always", "262144")

	campaign, err := mgr.Campaign("paced-campaign")
	if err != nil {
		t.Fatalf("Campaign: %v", err)
	}
	rate := campaign.UploadMaxRateBytes()
	if rate <= 0 {
		t.Fatalf("campaign rate = %d, want > 0", rate)
	}
	expected := time.Duration(float64(size) / float64(rate) * float64(time.Second))

	srv := newFakeIngestServer()
	client := startFakeIngest(t, srv)
	w := newTestWorker(mgr, client)

	start := time.Now()
	if err := w.runPass(context.Background()); err != nil {
		t.Fatalf("runPass: %v", err)
	}
	elapsed := time.Since(start)

	if got := uploadState(t, mgr, "ep-paced").State; got != uploadStateUploaded {
		t.Fatalf("state = %q, want uploaded", got)
	}
	if elapsed < time.Duration(float64(expected)*0.8) {
		t.Errorf("elapsed %v too fast; ceiling not paced (expected ~%v)", elapsed, expected)
	}
	if elapsed > expected*5 {
		t.Errorf("elapsed %v far exceeds expected %v", elapsed, expected)
	}
}

func TestDataTransferWorker_VerificationFailureMarksFailedNoRetry(t *testing.T) {
	mgr, root := newTestManager(t)
	writeFixtureEpisode(t, root, "ep-corrupt", "", "pending", map[string][]byte{"blob.bin": []byte("corrupt me")})

	srv := newFakeIngestServer()
	srv.corruptPaths["blob.bin"] = true
	client := startFakeIngest(t, srv)
	w := newTestWorker(mgr, client)

	if err := w.runPass(context.Background()); err != nil {
		t.Fatalf("runPass: %v", err)
	}
	st := uploadState(t, mgr, "ep-corrupt")
	if st.State != uploadStateFailed {
		t.Fatalf("state = %q, want failed", st.State)
	}
	if st.LastError == "" {
		t.Errorf("failure reason not recorded")
	}

	// Second pass must NOT retry a failed episode.
	if err := w.runPass(context.Background()); err != nil {
		t.Fatalf("second runPass: %v", err)
	}
	if srv.beginCalls != 1 {
		t.Errorf("begin called %d times; failed episode must not be retried", srv.beginCalls)
	}
}

func TestDataTransferWorker_UploadWhenManualSkipped_AlwaysTaken(t *testing.T) {
	mgr, root := newTestManager(t)
	writeFixtureEpisode(t, root, "ep-manual", "manual-campaign", "pending", map[string][]byte{"a.bin": []byte("a")})
	writeFixtureEpisode(t, root, "ep-always", "always-campaign", "pending", map[string][]byte{"b.bin": []byte("b")})
	writeCampaign(t, root, "manual-campaign", "manual", "")
	writeCampaign(t, root, "always-campaign", "always", "")

	srv := newFakeIngestServer()
	client := startFakeIngest(t, srv)
	w := newTestWorker(mgr, client)

	if err := w.runPass(context.Background()); err != nil {
		t.Fatalf("runPass: %v", err)
	}

	if got := uploadState(t, mgr, "ep-manual").State; got != uploadStatePending {
		t.Errorf("manual episode state = %q, want pending (skipped)", got)
	}
	if got := uploadState(t, mgr, "ep-always").State; got != uploadStateUploaded {
		t.Errorf("always episode state = %q, want uploaded", got)
	}
	if _, ok := srv.received["a.bin"]; ok {
		t.Errorf("manual episode file was uploaded")
	}
}

func TestDataTransferWorker_AtLeastOnceDuplicateSafe(t *testing.T) {
	mgr, root := newTestManager(t)
	files := map[string][]byte{"x.bin": []byte("payload")}
	writeFixtureEpisode(t, root, "ep-replay", "", "uploading", files)

	srv := newFakeIngestServer()
	// Simulate a prior fully-delivered upload the worker crashed after: Begin
	// reports the episode already COMPLETE. A re-run must not re-stream bytes.
	srv.beginState = cloudpb.EpisodeState_EPISODE_STATE_COMPLETE
	client := startFakeIngest(t, srv)
	w := newTestWorker(mgr, client)

	if err := w.runPass(context.Background()); err != nil {
		t.Fatalf("runPass: %v", err)
	}
	if got := uploadState(t, mgr, "ep-replay").State; got != uploadStateUploaded {
		t.Fatalf("state = %q, want uploaded", got)
	}
	if len(srv.received["x.bin"]) != 0 {
		t.Errorf("bytes re-streamed on idempotent replay: %d", len(srv.received["x.bin"]))
	}
	if srv.commitCalls != 0 {
		t.Errorf("commit called %d times on already-complete replay, want 0", srv.commitCalls)
	}
}

// TestIngestEndpointNormalisation verifies that SetIngestEndpoint reduces
// URL-shaped input to the host[:port] the dialer expects, and that empty means
// disabled.
func TestIngestEndpointNormalisation(t *testing.T) {
	w := &DataTransferWorker{}
	if got := w.IngestEndpoint(); got != "" {
		t.Fatalf("fresh worker: endpoint %q, want empty (uploads disabled)", got)
	}
	cases := []struct {
		in   string
		want string
	}{
		{"https://ingest.data.wendy.sh", "ingest.data.wendy.sh"},
		{"https://ingest.data.wendy.sh/", "ingest.data.wendy.sh"},
		{"http://localhost:9800", "localhost:9800"},
		{"ingest.example.com:50052", "ingest.example.com:50052"},
		{"  ingest.example.com  ", "ingest.example.com"},
	}
	for _, c := range cases {
		w.SetIngestEndpoint(c.in)
		if got := w.IngestEndpoint(); got != c.want {
			t.Errorf("endpoint %q: got %q, want %q", c.in, got, c.want)
		}
	}
	w.SetIngestEndpoint("")
	if got := w.IngestEndpoint(); got != "" {
		t.Fatalf("cleared endpoint: got %q, want empty", got)
	}
}

// runEndpointPass drives one real upload pass over bufconn with the endpoint
// set, dialling with exactly the options dialFactory passes (none), and returns
// the fake server so the caller can inspect the metadata that reached it.
func runEndpointPass(t *testing.T, endpoint string) *fakeIngestServer {
	t.Helper()
	mgr, root := newTestManager(t)
	writeFixtureEpisode(t, root, "ep-identity", "", "pending", map[string][]byte{"blob.bin": []byte("payload")})

	srv := newFakeIngestServer()
	w := &DataTransferWorker{
		logger:      zap.NewNop(),
		manager:     mgr,
		maxAttempts: transferMaxAttempts,
		now:         time.Now,
		newSleeper:  contextSleeper,
	}
	w.SetIngestEndpoint(endpoint)
	client := startFakeIngest(t, srv)
	w.factory = func(context.Context) (cloudpb.DataIngestServiceClient, func(), error) {
		return client, func() {}, nil
	}

	if err := w.runPass(context.Background()); err != nil {
		t.Fatalf("runPass: %v", err)
	}
	if got := uploadState(t, mgr, "ep-identity").State; got != uploadStateUploaded {
		t.Fatalf("state = %q, want uploaded", got)
	}
	return srv
}

// TestIngestCallsCarryNoIdentityHeader is the security-relevant case. Identity
// is the enrolled asset certificate presented in the TLS handshake and read by
// the ingest service from the validated leaf. The x-wendy-client-cert header
// this worker once attached was a self-asserted identity; the service no
// longer reads it and the device must never send it, on any call.
func TestIngestCallsCarryNoIdentityHeader(t *testing.T) {
	srv := runEndpointPass(t, "https://ingest.data.wendy.sh/")
	unary, stream := srv.headerValues("x-wendy-client-cert")
	if len(unary) != 0 {
		t.Errorf("unary call carried x-wendy-client-cert %q; identity is the certificate, never a header", unary)
	}
	if len(stream) != 0 {
		t.Errorf("streaming call carried x-wendy-client-cert %q; identity is the certificate, never a header", stream)
	}
}

// TestRunWithoutEndpointDoesNotDial pins the no-fallback rule: with no ingest
// endpoint configured the worker logs once and stops. It never asks the factory
// for a client, so it cannot dial the enrolled cloud host, which does not serve
// DataIngestService and used to answer every pass with Unimplemented.
func TestRunWithoutEndpointDoesNotDial(t *testing.T) {
	mgr, root := newTestManager(t)
	writeFixtureEpisode(t, root, "ep-queued", "", "pending", map[string][]byte{"blob.bin": []byte("payload")})

	var dials int32
	w := &DataTransferWorker{
		logger:      zap.NewNop(),
		manager:     mgr,
		maxAttempts: transferMaxAttempts,
		now:         time.Now,
		newSleeper:  contextSleeper,
	}
	w.factory = func(context.Context) (cloudpb.DataIngestServiceClient, func(), error) {
		atomic.AddInt32(&dials, 1)
		return nil, nil, errors.New("must not be called")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("Run did not return without an endpoint; it must stop rather than retry against nothing")
	}
	if n := atomic.LoadInt32(&dials); n != 0 {
		t.Fatalf("factory called %d times without an endpoint, want 0", n)
	}
	if got := uploadState(t, mgr, "ep-queued").State; got != uploadStatePending {
		t.Fatalf("queued episode state = %q, want %q (nothing may be marked failed)", got, uploadStatePending)
	}
}

// TestBuildEpisodeManifestCarriesFileRole pins the derived-artifact contract on
// the upload wire. The device marks the seal-time playable.mp4 remux as derived
// and everything else as capture payload; before the manifest carried a role,
// the catalog saw the remux as a second capture on the camera's source id and
// double-counted its bytes.
func TestBuildEpisodeManifestCarriesFileRole(t *testing.T) {
	mf := data.Manifest{
		ID: "ep-role",
		Files: []data.File{
			{Path: "cameras/front/index.jsonl", SourceID: "front", MediaType: "application/jsonl"},
			{Path: "cameras/front/000001.h264", SourceID: "front", MediaType: "video/h264"},
			{Path: "cameras/front/playable.mp4", SourceID: "front", MediaType: "video/mp4", Role: data.FileRoleDerived},
		},
	}
	got, err := buildEpisodeManifest(mf)
	if err != nil {
		t.Fatalf("buildEpisodeManifest: %v", err)
	}
	want := map[string]cloudpb.EpisodeFileRole{
		"cameras/front/index.jsonl":  cloudpb.EpisodeFileRole_EPISODE_FILE_ROLE_CAPTURED,
		"cameras/front/000001.h264":  cloudpb.EpisodeFileRole_EPISODE_FILE_ROLE_CAPTURED,
		"cameras/front/playable.mp4": cloudpb.EpisodeFileRole_EPISODE_FILE_ROLE_DERIVED,
	}
	if len(got.Files) != len(want) {
		t.Fatalf("sent %d files, want %d", len(got.Files), len(want))
	}
	for _, f := range got.Files {
		expected, ok := want[f.Path]
		if !ok {
			t.Fatalf("unexpected file %q on the wire", f.Path)
		}
		if f.Role != expected {
			t.Errorf("file %q role = %v, want %v", f.Path, f.Role, expected)
		}
	}
}

// TestEpisodeFileRoleUnmappableFails pins the wire contract's rule that a
// sender holding a role it cannot express MUST NOT send UNSPECIFIED, because
// UNSPECIFIED means only "the sender predates this field" and is accounted as
// capture payload. A role string this build does not know is a programming
// error in this repository, so the mapper refuses it instead of quietly
// booking the file as capture.
//
// The error assertions are what stop the old degrade-to-UNSPECIFIED default
// from coming back: restoring it makes episodeFileRole return a nil error, and
// both the mapper case and the buildEpisodeManifest case below fail.
func TestEpisodeFileRoleUnmappableFails(t *testing.T) {
	got, err := episodeFileRole("some-future-role")
	if err == nil {
		t.Errorf("unknown role returned %v and no error, want an error refusing to map it", got)
	}
	if err != nil && !strings.Contains(err.Error(), "some-future-role") {
		t.Errorf("error %q does not name the offending role", err)
	}
	if got != cloudpb.EpisodeFileRole_EPISODE_FILE_ROLE_UNSPECIFIED {
		// The zero value is the only thing a failed mapping may return; what
		// matters is that it never reaches the wire.
		t.Errorf("unknown role value = %v, want the zero value alongside the error", got)
	}

	if got, err := episodeFileRole(""); err != nil || got != cloudpb.EpisodeFileRole_EPISODE_FILE_ROLE_CAPTURED {
		t.Errorf("empty role = %v (err %v), want CAPTURED and no error", got, err)
	}
	if got, err := episodeFileRole(data.FileRoleDerived); err != nil || got != cloudpb.EpisodeFileRole_EPISODE_FILE_ROLE_DERIVED {
		t.Errorf("derived role = %v (err %v), want DERIVED and no error", got, err)
	}
}

// TestBuildEpisodeManifestRejectsUnmappableRole proves the refusal is not
// confined to the mapper: a manifest carrying an unknown role produces no
// EpisodeManifest at all, so nothing is uploaded under a wrong role. The
// caller must propagate the error rather than drop it.
func TestBuildEpisodeManifestRejectsUnmappableRole(t *testing.T) {
	mf := data.Manifest{
		ID: "ep-unmappable",
		Files: []data.File{
			{Path: "cameras/front/000001.h264", Size: 1, SHA256: "a"},
			{Path: "cameras/front/thumbnail.jpg", Size: 1, SHA256: "b", Role: "some-future-role"},
		},
	}
	got, err := buildEpisodeManifest(mf)
	if err == nil {
		t.Fatalf("buildEpisodeManifest accepted an unmappable role and returned %v, want an error", got)
	}
	if got != nil {
		t.Errorf("buildEpisodeManifest returned a manifest alongside the error: %v", got)
	}
	if !strings.Contains(err.Error(), "some-future-role") {
		t.Errorf("error %q does not name the offending role", err)
	}
	if !strings.Contains(err.Error(), "cameras/front/thumbnail.jpg") {
		t.Errorf("error %q does not name the offending file", err)
	}
}

// TestUploadStreamCarriesOneEpisode pins the invariant that lets the upload
// loop read acks without matching them.
//
// The wire contract requires an ack to be matched on the (episode_id, path)
// pair, because one stream may carry chunks for several episodes and a path is
// unique only within an episode. This client matches on nothing: it drains acks
// for flow control and error surfacing, and takes resume offsets from
// BeginEpisodeUpload instead. The contract's fallback for a client that cannot
// match is to keep to one stream per episode, which is what uploadEpisode does
// by opening its stream per manifest.
//
// So this test guards the assumption rather than the ack handling. Two pending
// episodes in one pass must produce two streams, each carrying exactly one
// episode id. If someone batches episodes onto a shared stream, this fails, and
// the ack matching has to be built before it can pass again.
func TestUploadStreamCarriesOneEpisode(t *testing.T) {
	mgr, root := newTestManager(t)
	writeFixtureEpisode(t, root, "ep-one", "", "pending", map[string][]byte{
		"video/000001.jpg": []byte("first episode payload"),
	})
	writeFixtureEpisode(t, root, "ep-two", "", "pending", map[string][]byte{
		"video/000002.jpg": []byte("second episode payload"),
	})

	srv := newFakeIngestServer()
	client := startFakeIngest(t, srv)
	w := newTestWorker(mgr, client)

	if err := w.runPass(context.Background()); err != nil {
		t.Fatalf("runPass: %v", err)
	}

	for _, id := range []string{"ep-one", "ep-two"} {
		if got := uploadState(t, mgr, id).State; got != uploadStateUploaded {
			t.Fatalf("episode %s state = %q, want uploaded", id, got)
		}
	}

	srv.mu.Lock()
	defer srv.mu.Unlock()

	if len(srv.streamEpisodes) != 2 {
		t.Fatalf("opened %d upload streams for 2 episodes, want 2", len(srv.streamEpisodes))
	}
	all := map[string]bool{}
	for i, seen := range srv.streamEpisodes {
		if len(seen) != 1 {
			ids := make([]string, 0, len(seen))
			for id := range seen {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			t.Errorf("stream %d carried %d episodes (%v), want exactly 1; "+
				"a multi-episode stream requires matching acks on (episode_id, path)", i, len(seen), ids)
		}
		for id := range seen {
			all[id] = true
		}
	}
	if !all["ep-one"] || !all["ep-two"] {
		t.Errorf("streams carried %v, want both ep-one and ep-two", all)
	}
}

// startBareIngest starts a server that implements nothing, so every call
// returns Unimplemented. This is not a contrived error: it is exactly what a
// device gets today when no ingest endpoint is configured and the worker falls
// back to dialling the enrolled cloud host, which serves the broker and not
// DataIngestService.
func startBareIngest(t *testing.T) cloudpb.DataIngestServiceClient {
	t.Helper()
	lis := bufconn.Listen(1024 * 1024)
	g := grpc.NewServer()
	cloudpb.RegisterDataIngestServiceServer(g, &cloudpb.UnimplementedDataIngestServiceServer{})
	go func() { _ = g.Serve(lis) }()
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { conn.Close(); g.Stop(); lis.Close() })
	return cloudpb.NewDataIngestServiceClient(conn)
}

// TestBlockedRouteSpendsNoRetryBudget is the regression that matters most here.
// Before this, an endpoint that does not serve the service was classified as a
// transient transport failure, so every sealed episode on the device burned its
// five attempts and was then marked permanently failed. The route being wrong
// is not the episode's fault and must cost it nothing.
func TestBlockedRouteSpendsNoRetryBudget(t *testing.T) {
	mgr, root := newTestManager(t)
	writeFixtureEpisode(t, root, "ep-blocked", "", "pending", map[string][]byte{"a.bin": []byte("x")})

	w := newTestWorker(mgr, startBareIngest(t))
	err := w.runPass(context.Background())
	if err == nil {
		t.Fatal("runPass returned nil; a blocked route must surface as a pass failure")
	}
	if ingestBlocked(err) == nil {
		t.Fatalf("runPass error %v is not classified as blocked", err)
	}

	st := uploadState(t, mgr, "ep-blocked")
	if st.State == uploadStateFailed {
		t.Error("episode was marked failed because the endpoint was wrong")
	}
	if st.Attempts != 0 {
		t.Errorf("attempts = %d, want 0: a blocked route must not consume the retry budget", st.Attempts)
	}
}

// TestBlockedRouteStopsTheWholePass: every episode in the backlog would fail
// identically, so the worker must stop rather than walk the queue.
func TestBlockedRouteStopsTheWholePass(t *testing.T) {
	mgr, root := newTestManager(t)
	for _, id := range []string{"ep-1", "ep-2", "ep-3"} {
		writeFixtureEpisode(t, root, id, "", "pending", map[string][]byte{"a.bin": []byte("x")})
	}

	w := newTestWorker(mgr, startBareIngest(t))
	if err := w.runPass(context.Background()); err == nil {
		t.Fatal("runPass returned nil on a blocked route")
	}
	for _, id := range []string{"ep-1", "ep-2", "ep-3"} {
		if st := uploadState(t, mgr, id); st.Attempts != 0 {
			t.Errorf("%s: attempts = %d, want 0", id, st.Attempts)
		}
	}
}

// TestBlockedClassification pins which codes are permanent. Getting this set
// wrong in either direction is costly: too wide and a transient outage stops
// uploads entirely, too narrow and a misconfiguration silently kills episodes.
func TestBlockedClassification(t *testing.T) {
	for code, want := range map[codes.Code]bool{
		codes.Unimplemented:     true,
		codes.Unauthenticated:   true,
		codes.PermissionDenied:  true,
		codes.Unavailable:       false,
		codes.DeadlineExceeded:  false,
		codes.ResourceExhausted: false,
		codes.Internal:          false,
		codes.Aborted:           false,
	} {
		if got := ingestBlocked(status.Error(code, "x")) != nil; got != want {
			t.Errorf("ingestBlocked(%s) = %v, want %v", code, got, want)
		}
	}
	if ingestBlocked(nil) != nil {
		t.Error("ingestBlocked(nil) must be nil")
	}
	if ingestBlocked(errors.New("plain")) != nil {
		t.Error("a non-status error is not a blocked route")
	}
}
