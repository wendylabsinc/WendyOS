package commands

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/wendylabsinc/wendy/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

// ---- buildLocalManifest tests ----

func TestBuildLocalManifest_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	entries, err := buildLocalManifest(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected empty manifest, got %d entries", len(entries))
	}
}

func TestBuildLocalManifest_SingleFile(t *testing.T) {
	dir := t.TempDir()
	content := []byte("hello world")
	if err := os.WriteFile(filepath.Join(dir, "app.bin"), content, 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	entries, err := buildLocalManifest(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	e := entries[0]
	if e.Path != "app.bin" {
		t.Errorf("Path = %q, want %q", e.Path, "app.bin")
	}
	if e.Size != int64(len(content)) {
		t.Errorf("Size = %d, want %d", e.Size, len(content))
	}
	wantHash := sha256Bytes(content)
	if !bytes.Equal(e.Sha256, wantHash) {
		t.Errorf("SHA256 = %x, want %x", e.Sha256, wantHash)
	}
	if len(e.Sha256) != sha256.Size {
		t.Errorf("SHA256 length = %d, want %d", len(e.Sha256), sha256.Size)
	}
	if e.Mode != 0o755 {
		t.Errorf("Mode = %o, want 0755", e.Mode)
	}
}

func TestBuildLocalManifest_NestedFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "models", "v1"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "models", "v1", "weights.bin"), []byte("w"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	entries, err := buildLocalManifest(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}

	paths := make(map[string]bool)
	for _, e := range entries {
		paths[e.Path] = true
		if len(e.Sha256) != sha256.Size {
			t.Fatalf("SHA256 length for %s = %d, want %d", e.Path, len(e.Sha256), sha256.Size)
		}
	}
	for _, want := range []string{"models/v1/weights.bin", "config.json"} {
		if !paths[want] {
			t.Errorf("missing path %q in manifest", want)
		}
	}
}

// ---- diffManifests tests ----

func TestDiffManifests_Identical(t *testing.T) {
	local := &agentpb.FileSyncManifest{Files: []*agentpb.FileSyncEntry{
		{Path: "app", Sha256: sha256Bytes([]byte("app")), Size: 10, Mode: 0o755},
		{Path: "config.json", Sha256: sha256Bytes([]byte("config")), Size: 5, Mode: 0o644},
	}}
	remote := &agentpb.FileSyncManifest{Files: []*agentpb.FileSyncEntry{
		{Path: "app", Sha256: sha256Bytes([]byte("app")), Size: 10, Mode: 0o755},
		{Path: "config.json", Sha256: sha256Bytes([]byte("config")), Size: 5, Mode: 0o644},
	}}
	result := diffManifests(local, remote)
	if len(result.contentTransfers) != 0 || len(result.modeOnly) != 0 || len(result.staleRemote) != 0 {
		t.Errorf("expected empty diff for identical manifests, got %+v", result)
	}
}

func TestDiffManifests_ModeOnlyChangeSeparated(t *testing.T) {
	sharedHash := sha256Bytes([]byte("same"))
	local := &agentpb.FileSyncManifest{Files: []*agentpb.FileSyncEntry{{
		Path: "app", Sha256: sharedHash, Size: 4, Mode: 0o755,
	}}}
	remote := &agentpb.FileSyncManifest{Files: []*agentpb.FileSyncEntry{{
		Path: "app", Sha256: sharedHash, Size: 4, Mode: 0o644,
	}}}

	result := diffManifests(local, remote)
	if len(result.contentTransfers) != 0 {
		t.Fatalf("expected no content transfer, got %v", result.contentTransfers)
	}
	if len(result.modeOnly) != 1 {
		t.Fatalf("expected 1 mode-only change, got %d", len(result.modeOnly))
	}
	if result.modeOnly[0].path != "app" {
		t.Fatalf("mode-only path = %q, want %q", result.modeOnly[0].path, "app")
	}
	if result.modeOnly[0].oldMode != 0o644 || result.modeOnly[0].newMode != 0o755 {
		t.Fatalf("mode-only modes = %04o -> %04o, want 0644 -> 0755", result.modeOnly[0].oldMode, result.modeOnly[0].newMode)
	}
}

func TestDiffManifests_SortsOperationsDeterministically(t *testing.T) {
	local := &agentpb.FileSyncManifest{Files: []*agentpb.FileSyncEntry{
		{Path: "c.bin", Sha256: sha256Bytes([]byte("new-c")), Size: 5, Mode: 0o644},
		{Path: "a.bin", Sha256: sha256Bytes([]byte("same-a")), Size: 6, Mode: 0o755},
		{Path: "b.bin", Sha256: sha256Bytes([]byte("new-b")), Size: 5, Mode: 0o644},
	}}
	remote := &agentpb.FileSyncManifest{Files: []*agentpb.FileSyncEntry{
		{Path: "stale.bin", Sha256: sha256Bytes([]byte("stale")), Size: 5, Mode: 0o644},
		{Path: "b.bin", Sha256: sha256Bytes([]byte("old-b")), Size: 5, Mode: 0o644},
		{Path: "a.bin", Sha256: sha256Bytes([]byte("same-a")), Size: 6, Mode: 0o644},
	}}

	result := diffManifests(local, remote)
	if got, want := result.contentTransfers, []string{"b.bin", "c.bin"}; !equalStrings(got, want) {
		t.Fatalf("contentTransfers = %v, want %v", got, want)
	}
	if len(result.modeOnly) != 1 || result.modeOnly[0].path != "a.bin" {
		t.Fatalf("modeOnly = %+v, want a.bin", result.modeOnly)
	}
	if got, want := result.staleRemote, []string{"stale.bin"}; !equalStrings(got, want) {
		t.Fatalf("staleRemote = %v, want %v", got, want)
	}
}

// ---- syncFiles integration test via in-process fake server ----

// fakeSyncServer implements WendyFileSyncServiceServer in-memory, recording
// all received messages and returning a scripted response.
type fakeSyncServer struct {
	agentpb.UnimplementedWendyFileSyncServiceServer

	agentManifest []*agentpb.FileSyncEntry

	mu               sync.Mutex
	received         []*agentpb.FileSyncRequest
	ackedPaths       []string
	modeUpdatedPaths []string
	deletedPaths     []string
	onStart          func(*agentpb.FileSyncStart)
	onChunk          func(*agentpb.FileSyncChunk)
}

func (s *fakeSyncServer) SyncFiles(stream agentpb.WendyFileSyncService_SyncFilesServer) error {
	for {
		req, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		s.mu.Lock()
		s.received = append(s.received, req)
		s.mu.Unlock()

		switch r := req.RequestType.(type) {
		case *agentpb.FileSyncRequest_Start:
			if s.onStart != nil {
				s.onStart(r.Start)
			}
			var resp agentpb.FileSyncResponse
			resp.ResponseType = &agentpb.FileSyncResponse_Manifest{
				Manifest: &agentpb.FileSyncManifest{Files: s.agentManifest},
			}
			if err := stream.Send(&resp); err != nil {
				return err
			}
		case *agentpb.FileSyncRequest_Chunk:
			if s.onChunk != nil {
				s.onChunk(r.Chunk)
			}
		case *agentpb.FileSyncRequest_Commit:
			s.mu.Lock()
			s.ackedPaths = append(s.ackedPaths, r.Commit.Path)
			s.mu.Unlock()
			var resp agentpb.FileSyncResponse
			resp.ResponseType = &agentpb.FileSyncResponse_Ack{
				Ack: &agentpb.FileSyncAck{Path: r.Commit.Path},
			}
			if err := stream.Send(&resp); err != nil {
				return err
			}
		case *agentpb.FileSyncRequest_Chmod:
			s.mu.Lock()
			s.modeUpdatedPaths = append(s.modeUpdatedPaths, r.Chmod.Path)
			s.mu.Unlock()
			var resp agentpb.FileSyncResponse
			resp.ResponseType = &agentpb.FileSyncResponse_Ack{
				Ack: &agentpb.FileSyncAck{Path: r.Chmod.Path},
			}
			if err := stream.Send(&resp); err != nil {
				return err
			}
		case *agentpb.FileSyncRequest_Delete:
			s.mu.Lock()
			s.deletedPaths = append(s.deletedPaths, r.Delete.Paths...)
			s.mu.Unlock()
		}
	}

	var resp agentpb.FileSyncResponse
	resp.ResponseType = &agentpb.FileSyncResponse_Complete{
		Complete: &agentpb.FileSyncComplete{},
	}
	return stream.Send(&resp)
}

func (s *fakeSyncServer) snapshotRequests() []*agentpb.FileSyncRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*agentpb.FileSyncRequest(nil), s.received...)
}

func startFakeServer(t *testing.T, srv *fakeSyncServer) (*grpcclient.AgentConnection, func()) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}

	s := grpc.NewServer(
		grpc.MaxRecvMsgSize(16*1024*1024),
		grpc.MaxSendMsgSize(16*1024*1024),
	)
	agentpb.RegisterWendyFileSyncServiceServer(s, srv)
	go func() { _ = s.Serve(ln) }()

	addr := ln.Addr().String()
	conn, err := grpc.NewClient(
		addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(16*1024*1024),
			grpc.MaxCallSendMsgSize(16*1024*1024),
		),
	)
	if err != nil {
		s.Stop()
		ln.Close()
		t.Fatalf("grpc.NewClient: %v", err)
	}

	ac := &grpcclient.AgentConnection{
		Conn:            conn,
		FileSyncService: agentpb.NewWendyFileSyncServiceClient(conn),
	}

	cleanup := func() {
		conn.Close()
		s.Stop()
	}
	return ac, cleanup
}

func TestSyncFiles_ContentChunksCarryCumulativeState(t *testing.T) {
	dir := t.TempDir()
	content := bytes.Repeat([]byte("abc12345"), fileSyncChunkSize/8+1) // slightly over one chunk => multiple chunks
	if err := os.WriteFile(filepath.Join(dir, "MyApp"), content, 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	srv := &fakeSyncServer{agentManifest: nil}
	conn, cleanup := startFakeServer(t, srv)
	defer cleanup()

	entries := []fileSyncEntry{{localPath: filepath.Join(dir, "MyApp"), remotePath: "MyApp"}}
	if err := syncFiles(context.Background(), conn, "sh.wendy.MyApp", entries); err != nil {
		t.Fatalf("syncFiles: %v", err)
	}

	requests := srv.snapshotRequests()
	var chunks []*agentpb.FileSyncChunk
	var commitMsg *agentpb.FileSyncCommit
	for _, r := range requests {
		switch msg := r.RequestType.(type) {
		case *agentpb.FileSyncRequest_Chunk:
			chunks = append(chunks, msg.Chunk)
		case *agentpb.FileSyncRequest_Commit:
			commitMsg = msg.Commit
		}
	}
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	if commitMsg == nil {
		t.Fatal("no FileSyncCommit sent")
	}
	if !bytes.Equal(commitMsg.Sha256, sha256Bytes(content)) {
		t.Fatalf("commit sha256 = %x, want %x", commitMsg.Sha256, sha256Bytes(content))
	}
	if commitMsg.Size != int64(len(content)) {
		t.Fatalf("commit size = %d, want %d", commitMsg.Size, len(content))
	}

	h := sha256.New()
	var cumulativeSize int64
	for i, chunk := range chunks {
		if chunk.Path != "MyApp" {
			t.Fatalf("chunk path = %q, want %q", chunk.Path, "MyApp")
		}
		if chunk.Sequence != uint64(i) {
			t.Fatalf("chunk sequence[%d] = %d, want %d", i, chunk.Sequence, i)
		}
		cumulativeSize += int64(len(chunk.Data))
		if _, err := h.Write(chunk.Data); err != nil {
			t.Fatalf("hash write: %v", err)
		}
		if chunk.CumulativeSize != cumulativeSize {
			t.Fatalf("chunk cumulative size[%d] = %d, want %d", i, chunk.CumulativeSize, cumulativeSize)
		}
		if !bytes.Equal(chunk.Sha256, h.Sum(nil)) {
			t.Fatalf("chunk sha256[%d] = %x, want %x", i, chunk.Sha256, h.Sum(nil))
		}
	}
}

func TestSyncFiles_DirectoryEntry_AllFilesTransferredWithPrefix(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "top.bin"), []byte("t"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "deep.bin"), []byte("d"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	srv := &fakeSyncServer{agentManifest: nil}
	conn, cleanup := startFakeServer(t, srv)
	defer cleanup()

	entries := []fileSyncEntry{{localPath: dir, remotePath: "data"}}
	if err := syncFiles(context.Background(), conn, "sh.wendy.App", entries); err != nil {
		t.Fatalf("syncFiles: %v", err)
	}

	ackedSet := make(map[string]bool)
	for _, path := range srv.ackedPaths {
		ackedSet[path] = true
	}
	if !ackedSet["data/top.bin"] {
		t.Errorf("missing ack for data/top.bin; got %v", srv.ackedPaths)
	}
	if !ackedSet["data/sub/deep.bin"] {
		t.Errorf("missing ack for data/sub/deep.bin; got %v", srv.ackedPaths)
	}
}

func TestSyncFiles_EmptyFileSendsOneEmptyChunk(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "empty.txt"), nil, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	srv := &fakeSyncServer{agentManifest: nil}
	conn, cleanup := startFakeServer(t, srv)
	defer cleanup()

	entries := []fileSyncEntry{{localPath: filepath.Join(dir, "empty.txt"), remotePath: "empty.txt"}}
	if err := syncFiles(context.Background(), conn, "sh.wendy.App", entries); err != nil {
		t.Fatalf("syncFiles: %v", err)
	}

	requests := srv.snapshotRequests()
	var chunks []*agentpb.FileSyncChunk
	var commitMsg *agentpb.FileSyncCommit
	for _, r := range requests {
		switch msg := r.RequestType.(type) {
		case *agentpb.FileSyncRequest_Chunk:
			chunks = append(chunks, msg.Chunk)
		case *agentpb.FileSyncRequest_Commit:
			commitMsg = msg.Commit
		}
	}
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	chunk := chunks[0]
	if chunk.Sequence != 0 {
		t.Fatalf("empty chunk sequence = %d, want 0", chunk.Sequence)
	}
	if len(chunk.Data) != 0 {
		t.Fatalf("empty chunk data length = %d, want 0", len(chunk.Data))
	}
	if chunk.CumulativeSize != 0 {
		t.Fatalf("empty chunk cumulative size = %d, want 0", chunk.CumulativeSize)
	}
	if !bytes.Equal(chunk.Sha256, sha256Bytes(nil)) {
		t.Fatalf("empty chunk sha256 = %x, want %x", chunk.Sha256, sha256Bytes(nil))
	}
	if commitMsg == nil {
		t.Fatal("no commit sent")
	}
	if commitMsg.Size != 0 {
		t.Fatalf("commit size = %d, want 0", commitMsg.Size)
	}
	if !bytes.Equal(commitMsg.Sha256, sha256Bytes(nil)) {
		t.Fatalf("commit sha256 = %x, want %x", commitMsg.Sha256, sha256Bytes(nil))
	}
}

func TestSyncFiles_FinalStreamedHashMustMatchManifestBeforeCommit(t *testing.T) {
	dir := t.TempDir()
	originalPath := filepath.Join(dir, "original.bin")
	updatedPath := filepath.Join(dir, "updated.bin")
	symlinkPath := filepath.Join(dir, "current.bin")
	original := bytes.Repeat([]byte("a"), 300*1024)
	updated := bytes.Repeat([]byte("b"), 300*1024)
	if err := os.WriteFile(originalPath, original, 0o755); err != nil {
		t.Fatalf("WriteFile original: %v", err)
	}
	if err := os.WriteFile(updatedPath, updated, 0o755); err != nil {
		t.Fatalf("WriteFile updated: %v", err)
	}
	if err := os.Symlink(originalPath, symlinkPath); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	startSeen := make(chan struct{})
	allowManifest := make(chan struct{})
	var once sync.Once
	srv := &fakeSyncServer{
		agentManifest: nil,
		onStart: func(start *agentpb.FileSyncStart) {
			once.Do(func() {
				close(startSeen)
				<-allowManifest
			})
		},
	}
	conn, cleanup := startFakeServer(t, srv)
	defer cleanup()

	errCh := make(chan error, 1)
	go func() {
		errCh <- syncFiles(context.Background(), conn, "sh.wendy.MyApp", []fileSyncEntry{{
			localPath:  symlinkPath,
			remotePath: "MyApp",
		}})
	}()

	select {
	case <-startSeen:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for FileSyncStart")
	}

	if err := os.Remove(symlinkPath); err != nil {
		t.Fatalf("Remove symlink: %v", err)
	}
	if err := os.Symlink(updatedPath, symlinkPath); err != nil {
		t.Fatalf("Replace symlink: %v", err)
	}
	close(allowManifest)

	if err := <-errCh; err == nil {
		t.Fatal("expected syncFiles to fail after streamed file diverged from manifest")
	}

	for _, r := range srv.snapshotRequests() {
		if _, ok := r.RequestType.(*agentpb.FileSyncRequest_Commit); ok {
			t.Fatal("commit should not be sent when streamed file diverges from manifest")
		}
	}
}

func TestSyncFiles_DeterministicOperationOrder(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "c.bin"), []byte("new-c"), 0o644); err != nil {
		t.Fatalf("WriteFile c.bin: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.bin"), []byte("new-b"), 0o644); err != nil {
		t.Fatalf("WriteFile b.bin: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.bin"), []byte("same-a"), 0o755); err != nil {
		t.Fatalf("WriteFile a.bin: %v", err)
	}

	srv := &fakeSyncServer{
		agentManifest: []*agentpb.FileSyncEntry{
			{Path: "stale.bin", Sha256: sha256Bytes([]byte("stale")), Size: 5, Mode: 0o644},
			{Path: "b.bin", Sha256: sha256Bytes([]byte("old-b")), Size: 5, Mode: 0o644},
			{Path: "a.bin", Sha256: sha256Bytes([]byte("same-a")), Size: 6, Mode: 0o644},
		},
	}
	conn, cleanup := startFakeServer(t, srv)
	defer cleanup()

	output := captureStdout(t, func() {
		if err := syncFiles(context.Background(), conn, "sh.wendy.App", []fileSyncEntry{{
			localPath:  dir,
			remotePath: "",
		}}); err != nil {
			t.Fatalf("syncFiles: %v", err)
		}
	})

	requests := srv.snapshotRequests()
	var gotOrder []string
	for _, r := range requests {
		switch msg := r.RequestType.(type) {
		case *agentpb.FileSyncRequest_Commit:
			gotOrder = append(gotOrder, "commit:"+msg.Commit.Path)
		case *agentpb.FileSyncRequest_Chmod:
			gotOrder = append(gotOrder, "mode:"+msg.Chmod.Path)
		case *agentpb.FileSyncRequest_Delete:
			for _, path := range msg.Delete.Paths {
				gotOrder = append(gotOrder, "delete:"+path)
			}
		}
	}
	wantOrder := []string{"commit:b.bin", "commit:c.bin", "mode:a.bin", "delete:stale.bin"}
	if !equalStrings(gotOrder, wantOrder) {
		t.Fatalf("operation order = %v, want %v", gotOrder, wantOrder)
	}
	if !strings.Contains(output, "mode changed: a.bin 0644 -> 0755") {
		t.Fatalf("stdout missing mode change line: %q", output)
	}
	if !strings.Contains(output, "deleted: stale.bin") {
		t.Fatalf("stdout missing deletion line: %q", output)
	}
}

func TestFormatTransferRate(t *testing.T) {
	if got := formatTransferRate(0, time.Second); got != "0 B/s" {
		t.Fatalf("formatTransferRate(0, 1s) = %q, want %q", got, "0 B/s")
	}
	if got := formatTransferRate(1536, time.Second); got != "1.5 kB/s" {
		t.Fatalf("formatTransferRate(1536, 1s) = %q, want %q", got, "1.5 kB/s")
	}
}

func TestPrintFileSyncProgress_IncludesTransferRate(t *testing.T) {
	output := captureStdout(t, func() {
		printFileSyncProgress(false, "a.bin", 1024, 2048, 1536, time.Second, 1, 2)
	})

	if !strings.Contains(output, "1.5 kB/s") {
		t.Fatalf("stdout missing transfer rate: %q", output)
	}
	if !strings.Contains(output, "a.bin") {
		t.Fatalf("stdout missing file name: %q", output)
	}
}

func TestSyncFiles_ProgressReportedPerFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.bin"), []byte("aaaa"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.bin"), []byte("bbbb"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	srv := &fakeSyncServer{agentManifest: nil}
	conn, cleanup := startFakeServer(t, srv)
	defer cleanup()

	output := captureStdout(t, func() {
		if err := syncFiles(context.Background(), conn, "sh.wendy.App", []fileSyncEntry{{
			localPath:  dir,
			remotePath: "",
		}}); err != nil {
			t.Fatalf("syncFiles: %v", err)
		}
	})

	if len(srv.ackedPaths) != 2 {
		t.Fatalf("ackedPaths count = %d, want 2", len(srv.ackedPaths))
	}
	if !strings.Contains(output, "Syncing files...") {
		t.Fatalf("stdout missing sync header: %q", output)
	}
	if !strings.Contains(output, "a.bin") {
		t.Fatalf("stdout missing a.bin progress line: %q", output)
	}
	if !strings.Contains(output, "b.bin") {
		t.Fatalf("stdout missing b.bin progress line: %q", output)
	}
	if !strings.Contains(output, "/s") {
		t.Fatalf("stdout missing transfer rate: %q", output)
	}
	if !strings.Contains(output, "Total: 8 B in 2 file(s)") {
		t.Fatalf("stdout missing total line: %q", output)
	}
}

func TestSyncFiles_NothingToSyncPrintsUpToDate(t *testing.T) {
	dir := t.TempDir()
	content := []byte("data")
	if err := os.WriteFile(filepath.Join(dir, "app"), content, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	srv := &fakeSyncServer{
		agentManifest: []*agentpb.FileSyncEntry{{
			Path: "app", Sha256: sha256Bytes(content), Size: int64(len(content)), Mode: 0o644,
		}},
	}
	conn, cleanup := startFakeServer(t, srv)
	defer cleanup()

	output := captureStdout(t, func() {
		if err := syncFiles(context.Background(), conn, "sh.wendy.App", []fileSyncEntry{{
			localPath:  filepath.Join(dir, "app"),
			remotePath: "app",
		}}); err != nil {
			t.Fatalf("syncFiles: %v", err)
		}
	})

	if !strings.Contains(output, "Files up to date.") {
		t.Fatalf("stdout missing up-to-date line: %q", output)
	}
	for _, r := range srv.snapshotRequests() {
		switch r.RequestType.(type) {
		case *agentpb.FileSyncRequest_Commit, *agentpb.FileSyncRequest_Chunk, *agentpb.FileSyncRequest_Chmod, *agentpb.FileSyncRequest_Delete:
			t.Fatal("unexpected chunk, commit, mode update, or delete when nothing to sync")
		}
	}
}

func TestSyncFiles_StaleOnlySendsExplicitDelete(t *testing.T) {
	srv := &fakeSyncServer{
		agentManifest: []*agentpb.FileSyncEntry{{
			Path: "stale.bin", Sha256: sha256Bytes([]byte("stale")), Size: 5, Mode: 0o644,
		}},
	}
	conn, cleanup := startFakeServer(t, srv)
	defer cleanup()

	output := captureStdout(t, func() {
		if err := syncFiles(context.Background(), conn, "sh.wendy.App", []fileSyncEntry{}); err != nil {
			t.Fatalf("syncFiles: %v", err)
		}
	})

	if !equalStrings(srv.deletedPaths, []string{"stale.bin"}) {
		t.Fatalf("deletedPaths = %v, want [stale.bin]", srv.deletedPaths)
	}
	if !strings.Contains(output, "deleted: stale.bin") {
		t.Fatalf("stdout missing deletion line: %q", output)
	}
	if strings.Contains(output, "Syncing files...") {
		t.Fatalf("stdout should not contain sync header for delete-only sync: %q", output)
	}
}

// ---- helpers ----

func sha256Bytes(data []byte) []byte {
	h := sha256.Sum256(data)
	return h[:]
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = old }()

	outputCh := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		outputCh <- buf.String()
	}()

	fn()
	_ = w.Close()
	return <-outputCh
}
