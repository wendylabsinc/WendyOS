package commands

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"

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
	wantHash := sha256Hex(content)
	if e.Sha256 != wantHash {
		t.Errorf("SHA256 = %q, want %q", e.Sha256, wantHash)
	}
	if e.Mode&0o755 != 0o755 {
		t.Errorf("Mode = %o, want at least 0755", e.Mode)
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
	}
	for _, want := range []string{"models/v1/weights.bin", "config.json"} {
		if !paths[want] {
			t.Errorf("missing path %q in manifest", want)
		}
	}
}

func TestBuildLocalManifest_SHA256Correctness(t *testing.T) {
	dir := t.TempDir()
	content := []byte("known content for hash check")
	if err := os.WriteFile(filepath.Join(dir, "data.bin"), content, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	entries, err := buildLocalManifest(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if got, want := entries[0].Sha256, sha256Hex(content); got != want {
		t.Errorf("SHA256 = %q, want %q", got, want)
	}
}

// ---- diffManifests tests ----

func TestDiffManifests_Identical(t *testing.T) {
	local := &agentpb.FileSyncManifest{Files: []*agentpb.FileSyncEntry{
		{Path: "app", Sha256: "abc", Size: 10},
		{Path: "config.json", Sha256: "def", Size: 5},
	}}
	remote := &agentpb.FileSyncManifest{Files: []*agentpb.FileSyncEntry{
		{Path: "app", Sha256: "abc", Size: 10},
		{Path: "config.json", Sha256: "def", Size: 5},
	}}
	result := diffManifests(local, remote)
	if len(result) != 0 {
		t.Errorf("expected empty diff for identical manifests, got %v", result)
	}
}

func TestDiffManifests_MissingFromRemote(t *testing.T) {
	local := &agentpb.FileSyncManifest{Files: []*agentpb.FileSyncEntry{
		{Path: "app", Sha256: "abc"},
		{Path: "new.bin", Sha256: "xyz"},
	}}
	remote := &agentpb.FileSyncManifest{Files: []*agentpb.FileSyncEntry{
		{Path: "app", Sha256: "abc"},
	}}
	result := diffManifests(local, remote)
	if len(result) != 1 || result[0] != "new.bin" {
		t.Errorf("expected [new.bin], got %v", result)
	}
}

func TestDiffManifests_SHA256Differs(t *testing.T) {
	local := &agentpb.FileSyncManifest{Files: []*agentpb.FileSyncEntry{
		{Path: "app", Sha256: "newHash"},
	}}
	remote := &agentpb.FileSyncManifest{Files: []*agentpb.FileSyncEntry{
		{Path: "app", Sha256: "oldHash"},
	}}
	result := diffManifests(local, remote)
	if len(result) != 1 || result[0] != "app" {
		t.Errorf("expected [app], got %v", result)
	}
}

func TestDiffManifests_RemoteOnlyFileNotIncluded(t *testing.T) {
	// A file present on the agent but not in the local manifest should
	// not appear in the transfer list (the agent handles deletions).
	local := &agentpb.FileSyncManifest{Files: []*agentpb.FileSyncEntry{
		{Path: "app", Sha256: "abc"},
	}}
	remote := &agentpb.FileSyncManifest{Files: []*agentpb.FileSyncEntry{
		{Path: "app", Sha256: "abc"},
		{Path: "stale.bin", Sha256: "zzz"},
	}}
	result := diffManifests(local, remote)
	if len(result) != 0 {
		t.Errorf("expected empty diff (agent-only file not in transfer list), got %v", result)
	}
}

func TestDiffManifests_EmptyRemote(t *testing.T) {
	local := &agentpb.FileSyncManifest{Files: []*agentpb.FileSyncEntry{
		{Path: "app", Sha256: "abc"},
		{Path: "config.json", Sha256: "def"},
	}}
	result := diffManifests(local, nil)
	if len(result) != 2 {
		t.Errorf("expected 2 entries to transfer against empty remote, got %v", result)
	}
}

// ---- syncFiles integration test via in-process fake server ----

// fakeSyncServer implements WendyFileSyncServiceServer in-memory, recording
// all received messages and returning a scripted response.
type fakeSyncServer struct {
	agentpb.UnimplementedWendyFileSyncServiceServer

	// agentManifest is what the fake agent returns in FileSyncManifest.
	agentManifest []*agentpb.FileSyncEntry

	// received collects all FileSyncRequest messages sent by the CLI.
	received []*agentpb.FileSyncRequest

	// ackedPaths records which paths were committed and acked.
	ackedPaths []string
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
		s.received = append(s.received, req)

		switch r := req.RequestType.(type) {
		case *agentpb.FileSyncRequest_Start:
			// Send the agent's manifest back.
			var resp agentpb.FileSyncResponse
			resp.ResponseType = &agentpb.FileSyncResponse_Manifest{
				Manifest: &agentpb.FileSyncManifest{Files: s.agentManifest},
			}
			if err := stream.Send(&resp); err != nil {
				return err
			}
		case *agentpb.FileSyncRequest_Commit:
			s.ackedPaths = append(s.ackedPaths, r.Commit.Path)
			var resp agentpb.FileSyncResponse
			resp.ResponseType = &agentpb.FileSyncResponse_Ack{
				Ack: &agentpb.FileSyncAck{Path: r.Commit.Path},
			}
			if err := stream.Send(&resp); err != nil {
				return err
			}
		}
	}

	// Send FileSyncComplete.
	var resp agentpb.FileSyncResponse
	resp.ResponseType = &agentpb.FileSyncResponse_Complete{
		Complete: &agentpb.FileSyncComplete{},
	}
	return stream.Send(&resp)
}

// startFakeServer registers srv on a random local port and returns a connected
// AgentConnection plus a cleanup function.
func startFakeServer(t *testing.T, srv *fakeSyncServer) (*grpcclient.AgentConnection, func()) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}

	s := grpc.NewServer()
	agentpb.RegisterWendyFileSyncServiceServer(s, srv)
	go func() { _ = s.Serve(ln) }()

	addr := ln.Addr().String()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
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

func TestSyncFiles_AllDiffedFilesTransferred(t *testing.T) {
	dir := t.TempDir()
	content := []byte("binary data")
	if err := os.WriteFile(filepath.Join(dir, "MyApp"), content, 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	srv := &fakeSyncServer{agentManifest: nil} // empty agent dir
	conn, cleanup := startFakeServer(t, srv)
	defer cleanup()

	entries := []fileSyncEntry{
		{localPath: filepath.Join(dir, "MyApp"), remotePath: "MyApp"},
	}

	if err := syncFiles(context.Background(), conn, "sh.wendy.MyApp", entries); err != nil {
		t.Fatalf("syncFiles: %v", err)
	}

	if len(srv.ackedPaths) != 1 || srv.ackedPaths[0] != "MyApp" {
		t.Errorf("ackedPaths = %v, want [MyApp]", srv.ackedPaths)
	}

	// Verify a commit was sent with the correct hash.
	var commitMsg *agentpb.FileSyncCommit
	for _, r := range srv.received {
		if c, ok := r.RequestType.(*agentpb.FileSyncRequest_Commit); ok {
			commitMsg = c.Commit
		}
	}
	if commitMsg == nil {
		t.Fatal("no FileSyncCommit sent")
	}
	if commitMsg.Path != "MyApp" {
		t.Errorf("commit path = %q, want %q", commitMsg.Path, "MyApp")
	}
	if commitMsg.Sha256 != sha256Hex(content) {
		t.Errorf("commit sha256 = %q, want %q", commitMsg.Sha256, sha256Hex(content))
	}
}

func TestSyncFiles_UnchangedFileNotReSent(t *testing.T) {
	dir := t.TempDir()
	content := []byte("unchanged binary")
	if err := os.WriteFile(filepath.Join(dir, "MyApp"), content, 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Agent already has the file with matching hash.
	srv := &fakeSyncServer{
		agentManifest: []*agentpb.FileSyncEntry{
			{Path: "MyApp", Sha256: sha256Hex(content), Size: int64(len(content))},
		},
	}
	conn, cleanup := startFakeServer(t, srv)
	defer cleanup()

	entries := []fileSyncEntry{
		{localPath: filepath.Join(dir, "MyApp"), remotePath: "MyApp"},
	}

	if err := syncFiles(context.Background(), conn, "sh.wendy.MyApp", entries); err != nil {
		t.Fatalf("syncFiles: %v", err)
	}

	// No commits should have been sent.
	for _, r := range srv.received {
		if _, ok := r.RequestType.(*agentpb.FileSyncRequest_Commit); ok {
			t.Error("FileSyncCommit sent for unchanged file")
		}
	}
}

func TestSyncFiles_DirectoryEntry_AllFilesTransferredWithPrefix(t *testing.T) {
	// A fileSyncEntry whose localPath is a directory: every file under it
	// should be transferred with the remotePath prefix.
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

	entries := []fileSyncEntry{
		{localPath: dir, remotePath: "data"},
	}

	if err := syncFiles(context.Background(), conn, "sh.wendy.App", entries); err != nil {
		t.Fatalf("syncFiles: %v", err)
	}

	// Both files should have been committed with the "data/" prefix.
	ackedSet := make(map[string]bool)
	for _, p := range srv.ackedPaths {
		ackedSet[p] = true
	}
	if !ackedSet["data/top.bin"] {
		t.Errorf("missing ack for data/top.bin; got %v", srv.ackedPaths)
	}
	if !ackedSet["data/sub/deep.bin"] {
		t.Errorf("missing ack for data/sub/deep.bin; got %v", srv.ackedPaths)
	}
}

// ---- progress test ----

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

	entries := []fileSyncEntry{
		{localPath: dir, remotePath: ""},
	}

	// syncFiles should succeed and produce acks for both files.
	if err := syncFiles(context.Background(), conn, "sh.wendy.App", entries); err != nil {
		t.Fatalf("syncFiles: %v", err)
	}

	if len(srv.ackedPaths) != 2 {
		t.Errorf("ackedPaths count = %d, want 2", len(srv.ackedPaths))
	}
}

func TestSyncFiles_EmptyFileTransferred(t *testing.T) {
	dir := t.TempDir()
	// Create an empty file (e.g. a .gitkeep placeholder).
	if err := os.MkdirAll(filepath.Join(dir, "Models"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Models", ".gitkeep"), []byte{}, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	srv := &fakeSyncServer{agentManifest: nil}
	conn, cleanup := startFakeServer(t, srv)
	defer cleanup()

	entries := []fileSyncEntry{
		{localPath: dir, remotePath: ""},
	}

	if err := syncFiles(context.Background(), conn, "sh.wendy.App", entries); err != nil {
		t.Fatalf("syncFiles: %v", err)
	}

	// The empty file must be committed with no chunks.
	if len(srv.ackedPaths) != 1 || srv.ackedPaths[0] != "Models/.gitkeep" {
		t.Errorf("ackedPaths = %v, want [Models/.gitkeep]", srv.ackedPaths)
	}
	for _, r := range srv.received {
		if c, ok := r.RequestType.(*agentpb.FileSyncRequest_Chunk); ok {
			t.Errorf("unexpected chunk for empty file: path=%q", c.Chunk.Path)
		}
	}

	// Commit must carry size=0 and the SHA256 of empty content.
	var commitMsg *agentpb.FileSyncCommit
	for _, r := range srv.received {
		if c, ok := r.RequestType.(*agentpb.FileSyncRequest_Commit); ok {
			commitMsg = c.Commit
		}
	}
	if commitMsg == nil {
		t.Fatal("no FileSyncCommit sent")
	}
	if commitMsg.Size != 0 {
		t.Errorf("commit size = %d, want 0", commitMsg.Size)
	}
	if commitMsg.Sha256 != sha256Hex(nil) {
		t.Errorf("commit sha256 = %q, want %q", commitMsg.Sha256, sha256Hex(nil))
	}
}

func TestSyncFiles_NothingToSyncPrintsUpToDate(t *testing.T) {
	dir := t.TempDir()
	content := []byte("data")
	if err := os.WriteFile(filepath.Join(dir, "app"), content, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	srv := &fakeSyncServer{
		agentManifest: []*agentpb.FileSyncEntry{
			{Path: "app", Sha256: sha256Hex(content), Size: int64(len(content))},
		},
	}
	conn, cleanup := startFakeServer(t, srv)
	defer cleanup()

	entries := []fileSyncEntry{{localPath: filepath.Join(dir, "app"), remotePath: "app"}}

	// Should complete without error.
	if err := syncFiles(context.Background(), conn, "sh.wendy.App", entries); err != nil {
		t.Fatalf("syncFiles: %v", err)
	}

	// No commits, no chunks sent.
	for _, r := range srv.received {
		switch r.RequestType.(type) {
		case *agentpb.FileSyncRequest_Commit, *agentpb.FileSyncRequest_Chunk:
			t.Error("unexpected chunk or commit when nothing to sync")
		}
	}
}

// ---- helpers ----

func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}
