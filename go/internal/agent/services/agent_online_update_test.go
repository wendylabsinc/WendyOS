package services

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/wendylabsinc/wendy/go/internal/agent/interceptor"
	"github.com/wendylabsinc/wendy/go/internal/supervisor"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func onlineFixture(t *testing.T) ([]byte, []byte) {
	t.Helper()
	binaryData := make([]byte, 64)
	copy(binaryData, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	binary.LittleEndian.PutUint16(binaryData[16:], 2)
	binary.LittleEndian.PutUint16(binaryData[18:], 62)
	binary.LittleEndian.PutUint32(binaryData[20:], 1)
	binary.LittleEndian.PutUint16(binaryData[52:], 64)
	var buffer bytes.Buffer
	gz := gzip.NewWriter(&buffer)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "wendy-agent", Mode: 0755, Size: int64(len(binaryData))}); err != nil {
		t.Fatal(err)
	}
	tw.Write(binaryData)
	tw.Close()
	gz.Close()
	return buffer.Bytes(), binaryData
}
func onlineServiceFixture(t *testing.T, badHash bool) (*AgentUpdateService, *atomic.Int32, string, <-chan struct{}) {
	t.Helper()
	archive, _ := onlineFixture(t)
	sum := sha256.Sum256(archive)
	checksum := hex.EncodeToString(sum[:])
	if badHash {
		checksum = string(bytes.Repeat([]byte("0"), 64))
	}
	calls := &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/agent/manifest.json" {
			fmt.Fprintf(w, `{"latest":"2.0.0","versions":{"2.0.0":{"artifacts":{"amd64":{"path":"agent/test.tar.gz","checksum":%q,"size_bytes":%d}}}}}`, checksum, len(archive))
			return
		}
		calls.Add(1)
		w.Write(archive)
	}))
	t.Cleanup(server.Close)
	svc := NewAgentUpdateService(zap.NewNop(), &AgentInstaller{})
	svc.online.origin = server.URL
	svc.online.client = server.Client()
	svc.online.os = "linux"
	svc.online.arch = "amd64"
	svc.online.installed = "1.0.0"
	executable := filepath.Join(t.TempDir(), "wendy-agent")
	os.WriteFile(executable, []byte("original"), 0755)
	svc.execPathResolver = func() (string, os.FileMode, error) { return executable, 0755, nil }
	restarted := make(chan struct{}, 1)
	svc.restartFn = func() { restarted <- struct{}{} }
	return svc, calls, executable, restarted
}
func TestOnlineUpdateChecksOnlyMetadataThenDownloadsOnAgent(t *testing.T) {
	svc, calls, executable, restarted := onlineServiceFixture(t, false)
	result, err := svc.CheckOnlineUpdate(context.Background(), &pb.CheckOnlineUpdateRequest{})
	if err != nil || !result.GetUpdateAvailable() {
		t.Fatalf("check: %v %v", result, err)
	}
	if calls.Load() != 0 {
		t.Fatal("view-open check downloaded artifact")
	}
	if _, err = svc.StartOnlineUpdate(context.Background(), &pb.StartOnlineUpdateRequest{Version: "1.5.0"}); err == nil {
		t.Fatal("stale displayed release accepted")
	}
	if calls.Load() != 0 {
		t.Fatal("rejected update downloaded artifact")
	}
	if _, err = svc.StartOnlineUpdate(context.Background(), &pb.StartOnlineUpdateRequest{Version: "2.0.0"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-restarted:
	case <-time.After(3 * time.Second):
		t.Fatalf("not restarted: %v", svc.online.snapshot())
	}
	raw, _ := os.ReadFile(executable)
	_, want := onlineFixture(t)
	if !bytes.Equal(raw, want) || calls.Load() != 1 {
		t.Fatal("agent did not install the verified download")
	}
	if svc.installer.TryLock() {
		t.Fatal("install lock released before restart")
	}
}
func TestOnlineUpdateRejectsChecksumAndWrongArchitecture(t *testing.T) {
	svc, _, executable, restarted := onlineServiceFixture(t, true)
	if _, err := svc.StartOnlineUpdate(context.Background(), &pb.StartOnlineUpdateRequest{Version: "2.0.0"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for svc.online.snapshot().Phase != "failed" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if svc.online.snapshot().Phase != "failed" {
		t.Fatal("corrupt update not rejected")
	}
	raw, _ := os.ReadFile(executable)
	if string(raw) != "original" {
		t.Fatal("corrupt download replaced executable")
	}
	select {
	case <-restarted:
		t.Fatal("restarted on failed update")
	default:
	}
	if !svc.installer.TryLock() {
		t.Fatal("failed update retained install lock")
	}
	svc.installer.Unlock()
	archive, _ := onlineFixture(t)
	if _, err := onlineAgentBinary(archive, "arm64"); err == nil {
		t.Fatal("wrong architecture accepted")
	}
}

func TestOnlineUpdateUnderTheSupervisorStagesInTheInbox(t *testing.T) {
	env := superviseForTest(t, nil)
	svc, _, executable, restarted := onlineServiceFixture(t, false)
	if _, err := svc.CheckOnlineUpdate(context.Background(), &pb.CheckOnlineUpdateRequest{}); err != nil {
		t.Fatal(err)
	}
	ctx := peer.NewContext(context.Background(), &peer.Peer{Addr: &net.TCPAddr{IP: net.ParseIP("192.168.1.18"), Port: 50000}})
	if _, err := svc.StartOnlineUpdate(ctx, &pb.StartOnlineUpdateRequest{Version: "2.0.0"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-restarted:
	case <-time.After(3 * time.Second):
		t.Fatalf("not restarted: %v", svc.online.snapshot())
	}
	if raw, _ := os.ReadFile(executable); string(raw) != "original" {
		t.Fatal("the running agent was overwritten instead of being handed to the supervisor")
	}
	_, want := onlineFixture(t)
	sum := sha256.Sum256(want)
	sha := hex.EncodeToString(sum[:])
	data, err := os.ReadFile(filepath.Join(env.inbox, sha+".json"))
	if err != nil {
		t.Fatalf("no inbox entry for the release: %v", err)
	}
	var in supervisor.Incoming
	if err := json.Unmarshal(data, &in); err != nil {
		t.Fatal(err)
	}
	// The device downloads it itself: the app that asked need not come back.
	if !in.Explicit || in.RemoteVia != "" {
		t.Fatalf("want an explicit upload judged by local checks, got %+v", in)
	}
	if len(env.checked) != 1 || !env.held[0] {
		t.Fatalf("the supervisor must check the release, with the update mark held, before the restart: %v %v", env.checked, env.held)
	}
}

func TestOnlineUpdateAsksForTheCloudWhileConnected(t *testing.T) {
	env := superviseForTest(t, nil)
	if err := interceptor.MarkRemoteSeen(supervisorMarkDir, supervisor.ViaCloud); err != nil {
		t.Fatal(err)
	}
	svc, _, _, restarted := onlineServiceFixture(t, false)
	if _, err := svc.CheckOnlineUpdate(context.Background(), &pb.CheckOnlineUpdateRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.StartOnlineUpdate(context.Background(), &pb.StartOnlineUpdateRequest{Version: "2.0.0"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-restarted:
	case <-time.After(3 * time.Second):
		t.Fatalf("not restarted: %v", svc.online.snapshot())
	}
	_, want := onlineFixture(t)
	sum := sha256.Sum256(want)
	data, err := os.ReadFile(filepath.Join(env.inbox, hex.EncodeToString(sum[:])+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var in supervisor.Incoming
	json.Unmarshal(data, &in)
	if in.RemoteVia != supervisor.ViaCloud {
		t.Fatalf("a cloud-connected device must keep its cloud access: want remote_via cloud, got %+v", in)
	}
}

func TestOnlineUpdateIsRefusedDuringATrial(t *testing.T) {
	superviseForTest(t, nil)
	writeSupervisorStatus(t, supervisor.Status{Role: supervisor.RoleTrial, PID: os.Getpid()})
	svc, calls, executable, _ := onlineServiceFixture(t, false)
	if _, err := svc.CheckOnlineUpdate(context.Background(), &pb.CheckOnlineUpdateRequest{}); err != nil {
		t.Fatal(err)
	}
	_, err := svc.StartOnlineUpdate(context.Background(), &pb.StartOnlineUpdateRequest{Version: "2.0.0"})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("want FailedPrecondition, got %v", err)
	}
	if raw, _ := os.ReadFile(executable); string(raw) != "original" || calls.Load() != 0 {
		t.Fatal("a refused update must not download or install anything")
	}
}
