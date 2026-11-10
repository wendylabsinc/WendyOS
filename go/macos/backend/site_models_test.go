package backend

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/services"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

func siteBackend(t *testing.T, supported bool) *Backend {
	t.Helper()
	if !supported {
		return siteBackendServing(t, nil)
	}
	return siteBackendServing(t, services.NewSiteModelService(t.TempDir()))
}

// A nil service stands for an agent that predates saved site models.
func siteBackendServing(t *testing.T, service pb.SiteModelServiceServer) *Backend {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "site-backend-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "agent.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	if service != nil {
		pb.RegisterSiteModelServiceServer(server, service)
	}
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient("unix://"+socket, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	b := New()
	b.conn = &grpcclient.AgentConnection{Conn: conn}
	b.session = 42
	return b
}
func TestSiteModelsBackendRoundTrip(t *testing.T) {
	b := siteBackend(t, true)
	dir := t.TempDir()
	source := filepath.Join(dir, "source.wendysite")
	target := filepath.Join(dir, "download.wendysite")
	data := append([]byte("WENDYST2"), bytes.Repeat([]byte{19}, 800000)...)
	if err := os.WriteFile(source, data, 0600); err != nil {
		t.Fatal(err)
	}
	saved, err := b.SiteModels(42, "save", source, "Warehouse", "")
	if err != nil {
		t.Fatal(err)
	}
	listed, err := b.SiteModels(42, "list", "", "", "")
	if err != nil || len(listed) != 1 || listed[0].Id != saved[0].Id {
		t.Fatalf("list: %v %v", listed, err)
	}
	if _, err = b.SiteModels(42, "load", target, "", saved[0].Id); err != nil {
		t.Fatal(err)
	}
	loaded, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(data, loaded) {
		t.Fatalf("download: %v", err)
	}
	if _, err = b.SiteModels(41, "save", source, "Wrong session", ""); err == nil {
		t.Fatal("stale device session accepted")
	}
	if _, err = b.SiteModels(42, "load", target, "", saved[0].Id); err == nil {
		t.Fatal("existing local file overwritten")
	}
}
func TestSiteModelsBackendExplainsOldAgent(t *testing.T) {
	_, err := siteBackend(t, false).SiteModels(42, "list", "", "", "")
	if err == nil || !strings.Contains(err.Error(), "agent update") {
		t.Fatalf("error: %v", err)
	}
}

func siteArchive(t *testing.T, size int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "source.wendysite")
	if err := os.WriteFile(path, append([]byte("WENDYST2"), bytes.Repeat([]byte{7}, size)...), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The device ends the stream to refuse an upload, so a later send reports
// only io.EOF. The caller must still learn why, whether or not the archive
// was small enough to be sent before the refusal arrived.
func TestSiteModelsBackendReportsWhyTheDeviceRefusedAnUpload(t *testing.T) {
	root := t.TempDir()
	// The device's quota counts apparent size, so a sparse file fills it.
	full, err := os.Create(filepath.Join(root, strings.Repeat("0", 64)+".wendysite"))
	if err != nil {
		t.Fatal(err)
	}
	if err = full.Truncate(1 << 30); err != nil {
		t.Fatal(err)
	}
	full.Close()
	b := siteBackendServing(t, services.NewSiteModelService(root))
	for _, size := range []int{600 << 10, 16 << 20} {
		_, err := b.SiteModels(42, "save", siteArchive(t, size), "Warehouse", "")
		if status.Code(err) != codes.ResourceExhausted || !strings.Contains(err.Error(), "storage limit reached") {
			t.Fatalf("%d byte archive: got %v, want the device's storage limit refusal", size, err)
		}
	}
}

type stalledSiteUpload struct {
	pb.UnimplementedSiteModelServiceServer
	started chan struct{}
}

func (s *stalledSiteUpload) Upload(stream grpc.ClientStreamingServer[pb.SiteModelUploadRequest, pb.SiteModelInfo]) error {
	if _, err := stream.Recv(); err != nil {
		return err
	}
	close(s.started)
	<-stream.Context().Done()
	return stream.Context().Err()
}

// A transfer can take minutes over a slow link. Other requests to the same
// device must not wait for it, and it must not outlive the connection.
func TestSiteModelsBackendTransferDoesNotHoldTheRequestLock(t *testing.T) {
	device := &stalledSiteUpload{started: make(chan struct{})}
	b := siteBackendServing(t, device)
	saved := make(chan error, 1)
	go func() {
		_, err := b.SiteModels(42, "save", siteArchive(t, 16<<20), "Warehouse", "")
		saved <- err
	}()
	<-device.started
	other := make(chan error, 1)
	go func() { other <- b.StopLogs(1, 42) }()
	select {
	case err := <-other:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("another request waited for the model transfer")
	}
	if err := b.Disconnect(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-saved:
		if err == nil {
			t.Fatal("transfer succeeded after its connection closed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("transfer outlived its connection")
	}
}
