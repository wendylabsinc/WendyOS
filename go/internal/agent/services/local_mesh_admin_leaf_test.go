package services

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshsharing"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// leaf_listen is a partial update like the carrier booleans: setting it saves
// it, omitting it preserves it, and an empty string turns the listener off.
// Status, topology and the configure response all report the saved address.
func TestLocalMeshAdminLeafListenSetPreserveAndClear(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "local-mesh.json")
	tenant, device := adminTenant(t), adminDevice(t)
	svc := NewLocalMeshAdminService(nil, dir, func() (meshid.Tenant, meshid.Device) { return tenant, device })
	ctx := ctxWithIdentity(t, userPrincipal(adminTenantID, "alice"))
	savedLeafListen := func() string {
		t.Helper()
		saved, err := localmesh.LoadTCPConfig(path, device)
		if err != nil {
			t.Fatal(err)
		}
		if saved == nil {
			return ""
		}
		return saved.LeafListen
	}

	// A leaf listener alone is a valid carrier document: it must be written,
	// not treated as "everything off" and removed.
	got, err := svc.ConfigureLocalMesh(ctx, &pb.ConfigureLocalMeshRequest{Participate: proto.Bool(true), LeafListen: proto.String("0.0.0.0:43024")})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetConfigured().GetLeafListen() != "0.0.0.0:43024" || savedLeafListen() != "0.0.0.0:43024" {
		t.Fatalf("leaf listener not saved: %+v, file %q", got.GetConfigured(), savedLeafListen())
	}
	current, err := svc.GetLocalMeshStatus(ctx, &pb.GetLocalMeshStatusRequest{})
	if err != nil || current.GetConfigured().GetLeafListen() != "0.0.0.0:43024" {
		t.Fatalf("status does not report the leaf listener: %+v %v", current, err)
	}
	topology, err := svc.GetLocalMeshTopology(ctx, &pb.GetLocalMeshTopologyRequest{})
	if err != nil || topology.GetStatus().GetConfigured().GetLeafListen() != "0.0.0.0:43024" {
		t.Fatalf("topology does not report the leaf listener: %+v %v", topology.GetStatus(), err)
	}

	// Other settings, including turning the last other carrier off, keep it.
	for _, update := range []*pb.ConfigureLocalMeshRequest{
		{InfrastructureWifi: proto.Bool(true)},
		{InfrastructureWifi: proto.Bool(false)},
		{Participate: proto.Bool(false)},
	} {
		got, err := svc.ConfigureLocalMesh(ctx, update)
		if err != nil {
			t.Fatal(err)
		}
		if got.GetConfigured().GetLeafListen() != "0.0.0.0:43024" || savedLeafListen() != "0.0.0.0:43024" {
			t.Fatalf("%v dropped the leaf listener: %+v, file %q", update, got.GetConfigured(), savedLeafListen())
		}
	}

	// A different address replaces it; an empty string turns it off, and with
	// nothing else enabled the carrier document goes away.
	got, err = svc.ConfigureLocalMesh(ctx, &pb.ConfigureLocalMeshRequest{LeafListen: proto.String("192.168.1.20:43100")})
	if err != nil || got.GetConfigured().GetLeafListen() != "192.168.1.20:43100" || savedLeafListen() != "192.168.1.20:43100" {
		t.Fatalf("leaf listener not replaced: %+v %v", got.GetConfigured(), err)
	}
	got, err = svc.ConfigureLocalMesh(ctx, &pb.ConfigureLocalMeshRequest{LeafListen: proto.String("")})
	if err != nil || got.GetConfigured().GetLeafListen() != "" {
		t.Fatalf("leaf listener not cleared: %+v %v", got.GetConfigured(), err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("clearing the only carrier should remove local-mesh.json: %v", err)
	}
}

// A bad address is the caller's mistake, not an agent fault, and must leave
// both saved documents exactly as they were.
func TestLocalMeshAdminLeafListenRejectsInvalidAddress(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "local-mesh.json")
	tenant, device := adminTenant(t), adminDevice(t)
	if err := meshsharing.SaveCarrierConfig(path, device, localmesh.TCPConfig{Listen: "0.0.0.0:43024", Ethernet: true}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	svc := NewLocalMeshAdminService(nil, dir, func() (meshid.Tenant, meshid.Device) { return tenant, device })
	ctx := ctxWithIdentity(t, userPrincipal(adminTenantID, "alice"))
	for _, address := range []string{"43024", "0.0.0.0", "0.0.0.0:0", "0.0.0.0:65536", "0.0.0.0:leaf", "0.0.0.0:43024"} {
		_, err := svc.ConfigureLocalMesh(ctx, &pb.ConfigureLocalMeshRequest{Participate: proto.Bool(true), LeafListen: proto.String(address)})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("leaf_listen %q: got %v, want InvalidArgument", address, err)
		}
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(after, before) {
			t.Fatalf("leaf_listen %q changed local-mesh.json: %s %v", address, after, err)
		}
		if _, err := os.Stat(filepath.Join(dir, "nan-mesh.json")); !os.IsNotExist(err) {
			t.Fatalf("leaf_listen %q saved the sharing policy: %v", address, err)
		}
	}
}

// Two Jetsons had leafListen added to local-mesh.json by hand. Toggling the
// other carriers through ConfigureLocalMesh must not drop it, nor the BLE leaf
// settings that only the file can set, even when no other carrier stays on.
func TestLocalMeshAdminCarrierTogglesPreserveHandWrittenLeafSettings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "local-mesh.json")
	tenant, device := adminTenant(t), adminDevice(t)
	handWritten := `{"listen":"","peers":null,"leafListen":"0.0.0.0:43024",` +
		`"leafBLEPeers":[{"device":"` + adminPeerID + `","address":"80:45:6b:00:00:01"}],` +
		`"leafTimeURL":"https://time.example/timestamp"}`
	if err := os.WriteFile(path, []byte(handWritten), 0600); err != nil {
		t.Fatal(err)
	}
	svc := NewLocalMeshAdminService(nil, dir, func() (meshid.Tenant, meshid.Device) { return tenant, device })
	ctx := ctxWithIdentity(t, userPrincipal(adminTenantID, "alice"))
	for _, update := range []*pb.ConfigureLocalMeshRequest{
		{Ethernet: proto.Bool(true), Nan: proto.Bool(true)},
		{Ethernet: proto.Bool(false), Nan: proto.Bool(false), Ble: proto.Bool(false), InfrastructureWifi: proto.Bool(false)},
	} {
		if _, err := svc.ConfigureLocalMesh(ctx, update); err != nil {
			t.Fatal(err)
		}
		saved, err := localmesh.LoadTCPConfig(path, device)
		if err != nil || saved == nil {
			t.Fatalf("after %v the leaf-only document was removed or unreadable: %+v %v", update, saved, err)
		}
		if saved.LeafListen != "0.0.0.0:43024" || len(saved.LeafBLEPeers) != 1 || saved.LeafBLEPeers[0].Device != adminPeerID ||
			saved.LeafTimeURL != "https://time.example/timestamp" {
			t.Fatalf("after %v leaf settings lost: %+v", update, saved)
		}
	}
}
