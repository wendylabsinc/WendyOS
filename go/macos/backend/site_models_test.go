package backend

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/services"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func siteBackend(t *testing.T, supported bool) *Backend {
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
	if supported {
		pb.RegisterSiteModelServiceServer(server, services.NewSiteModelService(t.TempDir()))
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
