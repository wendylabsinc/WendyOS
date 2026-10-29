package services

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/localsocket"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshsharing"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const (
	adminTenantID = "0b7c1d9e-2a3f-4c5d-8e90-1234567890ab"
	adminDeviceID = "11111111-2222-4333-8444-555555555555"
	adminPeerID   = "22222222-3333-4444-8555-666666666666"
	otherTenantID = "f4a1b2c3-d4e5-4f60-9a7b-8c9d0e1f2a3b"
)

func adminTenant(t *testing.T) meshid.Tenant {
	t.Helper()
	tenant, err := meshid.ParseTenant(adminTenantID)
	if err != nil {
		t.Fatalf("tenant UUID: %v", err)
	}
	return tenant
}

func adminDevice(t *testing.T) meshid.Device {
	t.Helper()
	device, err := meshid.ParseDevice(adminTenant(t), adminDeviceID)
	if err != nil {
		t.Fatalf("device UUID: %v", err)
	}
	return device
}

// userPrincipal names a tenant-scoped user caller, the only principal the
// admin service accepts for local mesh administration.
func userPrincipal(tenantID, name string) string {
	return certs.UserSPIFFEURI(tenantID, name)
}

// localAdminSocketCtx is a caller on the agent's local control socket as grpc
// reports it on Linux: a unix peer with no TLS.
func localAdminSocketCtx() context.Context {
	return peer.NewContext(context.Background(), &peer.Peer{Addr: &net.UnixAddr{Name: "@", Net: "unix"}})
}

// plaintextTCPCtx is a caller on the pre-provisioning plaintext listener. It
// has no TLS either, and anyone on the LAN can be it.
func plaintextTCPCtx() context.Context {
	return peer.NewContext(context.Background(), &peer.Peer{Addr: &net.TCPAddr{IP: net.IPv4(10, 0, 0, 9), Port: 50051}})
}

// Only the read-only RPCs widen, and only for the local admin socket. Writes
// still demand a user in this device's tenant, and the plaintext listener is
// refused everything even though it also carries no certificate.
func TestLocalMeshAdminCallerMatrix(t *testing.T) {
	_, snapshot, _ := topologyFixture(t)
	tenant, device := adminTenant(t), adminDevice(t)
	for _, tc := range []struct {
		name        string
		ctx         context.Context
		read, write codes.Code
	}{
		{"local admin socket", localAdminSocketCtx(), codes.OK, codes.PermissionDenied},
		{"plaintext TCP", plaintextTCPCtx(), codes.PermissionDenied, codes.PermissionDenied},
		{"no peer", context.Background(), codes.PermissionDenied, codes.PermissionDenied},
		{"device certificate", ctxWithIdentity(t, device.Principal()), codes.PermissionDenied, codes.PermissionDenied},
		{"user in another tenant", ctxWithIdentity(t, userPrincipal(otherTenantID, "bob")), codes.PermissionDenied, codes.PermissionDenied},
		{"user in this tenant", ctxWithIdentity(t, userPrincipal(adminTenantID, "alice")), codes.OK, codes.OK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			svc := NewLocalMeshAdminService(nil, dir, func() (meshid.Tenant, meshid.Device) { return tenant, device })
			reads := 0
			svc.SetTopologySource(func() localmesh.NodeSnapshot { reads++; return snapshot })
			if _, err := svc.GetLocalMeshStatus(tc.ctx, &pb.GetLocalMeshStatusRequest{}); status.Code(err) != tc.read {
				t.Fatalf("status: got %v, want %v", err, tc.read)
			}
			topology, err := svc.GetLocalMeshTopology(tc.ctx, &pb.GetLocalMeshTopologyRequest{})
			if status.Code(err) != tc.read {
				t.Fatalf("topology: got %v, want %v", err, tc.read)
			}
			if tc.read == codes.OK && (!topology.GetRuntimeAvailable() || topology.GetSelfDeviceId() != device.UUID().String() || len(topology.GetPeers()) != 2) {
				t.Fatalf("topology: %v", topology)
			}
			if tc.read != codes.OK && reads != 0 {
				t.Fatal("refused caller read topology")
			}
			_, err = svc.ConfigureLocalMesh(tc.ctx, &pb.ConfigureLocalMeshRequest{Participate: proto.Bool(true), Nan: proto.Bool(true)})
			if status.Code(err) != tc.write {
				t.Fatalf("configure: got %v, want %v", err, tc.write)
			}
			if tc.write != codes.OK {
				for _, name := range []string{"nan-mesh.json", "local-mesh.json"} {
					if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
						t.Fatalf("refused configure saved %s: %v", name, err)
					}
				}
			}
		})
	}
}

// The local socket widens who may read, not when: an unenrolled device has no
// tenant to scope its view to, so every reader gets the same refusal.
func TestLocalMeshAdminReadRequiresEnrollment(t *testing.T) {
	_, snapshot, _ := topologyFixture(t)
	for name, identity := range map[string]func() (meshid.Tenant, meshid.Device){
		"no identity source": nil,
		"not enrolled":       func() (meshid.Tenant, meshid.Device) { return meshid.Tenant{}, meshid.Device{} },
	} {
		t.Run(name, func(t *testing.T) {
			svc := NewLocalMeshAdminService(nil, t.TempDir(), identity)
			reads := 0
			svc.SetTopologySource(func() localmesh.NodeSnapshot { reads++; return snapshot })
			for caller, ctx := range map[string]context.Context{
				"local admin socket":  localAdminSocketCtx(),
				"user in this tenant": ctxWithIdentity(t, userPrincipal(adminTenantID, "alice")),
			} {
				if _, err := svc.GetLocalMeshStatus(ctx, &pb.GetLocalMeshStatusRequest{}); status.Code(err) != codes.FailedPrecondition {
					t.Fatalf("%s status: got %v, want FailedPrecondition", caller, err)
				}
				if _, err := svc.GetLocalMeshTopology(ctx, &pb.GetLocalMeshTopologyRequest{}); status.Code(err) != codes.FailedPrecondition {
					t.Fatalf("%s topology: got %v, want FailedPrecondition", caller, err)
				}
			}
			if reads != 0 {
				t.Fatal("unenrolled device read topology")
			}
		})
	}
}

// The tests above fake the peer. This one serves the service on a real
// localsocket listener, so it fails if grpc stops reporting such a caller as a
// unix peer: that network is all that separates it from the plaintext listener.
func TestLocalMeshAdminOverRealLocalSocket(t *testing.T) {
	// Not t.TempDir: unix socket paths are limited to about 100 bytes.
	dir, err := os.MkdirTemp("", "wnet")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "agent.sock")
	lis, err := localsocket.Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	_, snapshot, _ := topologyFixture(t)
	tenant, device := adminTenant(t), adminDevice(t)
	svc := NewLocalMeshAdminService(nil, t.TempDir(), func() (meshid.Tenant, meshid.Device) { return tenant, device })
	svc.SetTopologySource(func() localmesh.NodeSnapshot { return snapshot })
	srv := grpc.NewServer()
	pb.RegisterWendyLocalMeshAdminServiceServer(srv, svc)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("unix://"+path, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client := pb.NewWendyLocalMeshAdminServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	topology, err := client.GetLocalMeshTopology(ctx, &pb.GetLocalMeshTopologyRequest{})
	if err != nil || topology.GetSelfDeviceId() != device.UUID().String() {
		t.Fatalf("topology over the local socket: %v %v", topology, err)
	}
	if _, err := client.ConfigureLocalMesh(ctx, &pb.ConfigureLocalMeshRequest{Nan: proto.Bool(true)}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("configure over the local socket: got %v, want PermissionDenied", err)
	}
}

func TestLocalMeshAdminAuthorizationAndPartialUpdates(t *testing.T) {
	dir := t.TempDir()
	tenant, device := adminTenant(t), adminDevice(t)
	svc := NewLocalMeshAdminService(nil, dir, func() (meshid.Tenant, meshid.Device) { return tenant, device })
	request := &pb.ConfigureLocalMeshRequest{Participate: proto.Bool(true), Nan: proto.Bool(true)}
	for _, ctx := range []context.Context{
		context.Background(),
		ctxWithIdentity(t, device.Principal()),
		ctxWithIdentity(t, userPrincipal(otherTenantID, "bob")),
	} {
		if _, err := svc.ConfigureLocalMesh(ctx, request); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("configure authorization: %v", err)
		}
		if _, err := svc.GetLocalMeshStatus(ctx, &pb.GetLocalMeshStatusRequest{}); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("status authorization: %v", err)
		}
	}
	ctx := ctxWithIdentity(t, userPrincipal(adminTenantID, "alice"))
	if _, err := svc.ConfigureLocalMesh(ctx, &pb.ConfigureLocalMeshRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty update accepted: %v", err)
	}
	if _, err := svc.ConfigureLocalMesh(ctx, &pb.ConfigureLocalMeshRequest{Roam: proto.Bool(true)}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("roam without participate accepted: %v", err)
	}
	statusBefore, err := svc.GetLocalMeshStatus(ctx, &pb.GetLocalMeshStatusRequest{})
	if err != nil || statusBefore.GetRuntimeAvailable() || statusBefore.GetConfigured().GetNan() {
		t.Fatalf("incorrect disabled default: %+v %v", statusBefore, err)
	}
	if _, err := svc.ConfigureLocalMesh(ctx, request); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for _, update := range []*pb.ConfigureLocalMeshRequest{{Roam: proto.Bool(true)}, {ShareUplink: proto.Bool(true)}, {Ble: proto.Bool(true)}, {Ethernet: proto.Bool(true)}, {InfrastructureWifi: proto.Bool(true)}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := svc.ConfigureLocalMesh(ctx, update); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	got, err := svc.GetLocalMeshStatus(ctx, &pb.GetLocalMeshStatusRequest{})
	if err != nil || !got.GetConfigured().GetParticipate() || !got.GetConfigured().GetRoam() ||
		!got.GetConfigured().GetShareUplink() || !got.GetConfigured().GetNan() || !got.GetConfigured().GetBle() ||
		!got.GetConfigured().GetEthernet() || !got.GetConfigured().GetInfrastructureWifi() {
		t.Fatalf("partial settings lost: %+v %v", got, err)
	}
	if _, err = svc.ConfigureLocalMesh(ctx, &pb.ConfigureLocalMeshRequest{Participate: proto.Bool(false)}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("non-atomic disable accepted: %v", err)
	}
	if _, err = svc.ConfigureLocalMesh(ctx, &pb.ConfigureLocalMeshRequest{Nan: proto.Bool(false)}); err != nil {
		t.Fatal(err)
	}
	got, err = svc.GetLocalMeshStatus(ctx, &pb.GetLocalMeshStatusRequest{})
	if err != nil || got.GetConfigured().GetNan() || !got.GetConfigured().GetBle() ||
		!got.GetConfigured().GetEthernet() || !got.GetConfigured().GetInfrastructureWifi() {
		t.Fatalf("BLE inferred from NAN toggle: %+v %v", got, err)
	}
	svc.SetRuntimeStatusSource(func() LocalMeshRuntimeStatus {
		return LocalMeshRuntimeStatus{Available: true, State: "roaming", Detail: "using verified donor", AuthenticatedPeers: 3, GatewayPrincipal: device.Principal()}
	})
	got, err = svc.GetLocalMeshStatus(ctx, &pb.GetLocalMeshStatusRequest{})
	if err != nil || !got.GetRuntimeAvailable() || got.GetGatewayPrincipal() != device.Principal() || got.GetAuthenticatedPeers() != 3 {
		t.Fatalf("runtime status not attached: %+v %v", got, err)
	}
	tenant, err = meshid.ParseTenant(otherTenantID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.GetLocalMeshStatus(ctx, &pb.GetLocalMeshStatusRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("stale user tenant accepted: %v", err)
	}
}

func TestLocalMeshAdminCarrierUpdatePreservesConfiguredTCP(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "local-mesh.json")
	tenant, device := adminTenant(t), adminDevice(t)
	before := localmesh.TCPConfig{Listen: "127.0.0.1:17700", Peers: []localmesh.TCPPeer{{Device: adminPeerID, Address: "127.0.0.1:17701"}}}
	if err := meshsharing.SaveCarrierConfig(path, device, before); err != nil {
		t.Fatal(err)
	}
	svc := NewLocalMeshAdminService(nil, dir, func() (meshid.Tenant, meshid.Device) { return tenant, device })
	ctx := ctxWithIdentity(t, userPrincipal(adminTenantID, "alice"))
	if _, err := svc.ConfigureLocalMesh(ctx, &pb.ConfigureLocalMeshRequest{Nan: proto.Bool(true), Ble: proto.Bool(true)}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ConfigureLocalMesh(ctx, &pb.ConfigureLocalMeshRequest{Nan: proto.Bool(false), Ble: proto.Bool(false), Ethernet: proto.Bool(false), InfrastructureWifi: proto.Bool(false)}); err != nil {
		t.Fatal(err)
	}
	after, err := localmesh.LoadTCPConfig(path, device)
	if err != nil || after == nil || after.Listen != before.Listen || len(after.Peers) != 1 || after.Peers[0] != before.Peers[0] || after.NAN || after.BLE {
		t.Fatalf("TCP configuration lost: %+v %v", after, err)
	}
}

func TestLocalMeshAdminCanDisableLastCarrierWithoutTCPConfig(t *testing.T) {
	dir := t.TempDir()
	tenant, device := adminTenant(t), adminDevice(t)
	svc := NewLocalMeshAdminService(nil, dir, func() (meshid.Tenant, meshid.Device) { return tenant, device })
	ctx := ctxWithIdentity(t, userPrincipal(adminTenantID, "alice"))
	if _, err := svc.ConfigureLocalMesh(ctx, &pb.ConfigureLocalMeshRequest{Nan: proto.Bool(true)}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ConfigureLocalMesh(ctx, &pb.ConfigureLocalMeshRequest{Ethernet: proto.Bool(true), InfrastructureWifi: proto.Bool(true)}); err != nil {
		t.Fatal(err)
	}
	disableOtherCarriers := &pb.ConfigureLocalMeshRequest{Nan: proto.Bool(false), Ethernet: proto.Bool(false)}
	if _, err := svc.ConfigureLocalMesh(ctx, disableOtherCarriers); err != nil {
		t.Fatal(err)
	}
	carriers, err := localmesh.LoadTCPConfig(filepath.Join(dir, "local-mesh.json"), device)
	if err != nil || carriers == nil || !carriers.InfrastructureWiFi || carriers.NAN || carriers.BLE || carriers.Ethernet {
		t.Fatalf("partial update must preserve the remaining Wi-Fi carrier: %+v %v", carriers, err)
	}
	if _, err := svc.ConfigureLocalMesh(ctx, &pb.ConfigureLocalMeshRequest{InfrastructureWifi: proto.Bool(false)}); err != nil {
		t.Fatal(err)
	}
	carriers, err = localmesh.LoadTCPConfig(filepath.Join(dir, "local-mesh.json"), device)
	if err != nil || carriers != nil {
		t.Fatalf("carrier-only disabled state should remove opt-in file: %+v %v", carriers, err)
	}
}

func TestLocalMeshParticipationDisableRetainsCarrierChoices(t *testing.T) {
	dir := t.TempDir()
	tenant, device := adminTenant(t), adminDevice(t)
	svc := NewLocalMeshAdminService(nil, dir, func() (meshid.Tenant, meshid.Device) { return tenant, device })
	ctx := ctxWithIdentity(t, userPrincipal(adminTenantID, "alice"))
	_, err := svc.ConfigureLocalMesh(ctx, &pb.ConfigureLocalMeshRequest{Participate: proto.Bool(true), Nan: proto.Bool(true), Ble: proto.Bool(true), Ethernet: proto.Bool(true), InfrastructureWifi: proto.Bool(true)})
	if err != nil {
		t.Fatal(err)
	}
	for _, enabled := range []bool{false, true} {
		got, err := svc.ConfigureLocalMesh(ctx, &pb.ConfigureLocalMeshRequest{Participate: proto.Bool(enabled)})
		if err != nil {
			t.Fatal(err)
		}
		c := got.GetConfigured()
		if c.GetParticipate() != enabled || !c.GetNan() || !c.GetBle() || !c.GetEthernet() || !c.GetInfrastructureWifi() {
			t.Fatalf("lost carrier preferences: %+v", c)
		}
		if !enabled && got.GetState() != "disabled" {
			t.Fatalf("saved disabled state: %+v", got)
		}
	}
}

// The runtime polls the saved documents, so when ConfigureLocalMesh returns it
// can still report the previous configuration, including on retries. Configure
// must not present that state as the result: enabling a disabled device used to answer
// "disabled", and disabling an active one answered "mesh-active".
func TestLocalMeshAdminConfigureDoesNotReportPreChangeRuntimeState(t *testing.T) {
	dir := t.TempDir()
	tenant, device := adminTenant(t), adminDevice(t)
	svc := NewLocalMeshAdminService(nil, dir, func() (meshid.Tenant, meshid.Device) { return tenant, device })
	runtime := LocalMeshRuntimeStatus{Available: true, State: "disabled", Detail: "Wendy Net participation is disabled."}
	svc.SetRuntimeStatusSource(func() LocalMeshRuntimeStatus { return runtime })
	ctx := ctxWithIdentity(t, userPrincipal(adminTenantID, "alice"))
	enable := &pb.ConfigureLocalMeshRequest{Participate: proto.Bool(true), Ble: proto.Bool(true), Ethernet: proto.Bool(true), InfrastructureWifi: proto.Bool(true)}

	got, err := svc.ConfigureLocalMesh(ctx, enable)
	if err != nil {
		t.Fatal(err)
	}
	if c := got.GetConfigured(); !c.GetParticipate() || !c.GetBle() || !c.GetEthernet() || !c.GetInfrastructureWifi() {
		t.Fatalf("enable not saved: %+v", c)
	}
	if got.GetState() != "pending-runtime" || got.GetRuntimeAvailable() || got.GetAuthenticatedPeers() != 0 {
		t.Fatalf("enable reported the pre-change runtime state: %+v", got)
	}

	// Retrying before the runtime polls must return the same saved intent.
	retry, err := svc.ConfigureLocalMesh(ctx, enable)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(got, retry) {
		t.Fatalf("retry attached stale runtime: first=%+v retry=%+v", got, retry)
	}

	// Configure always acknowledges saved intent; status reports observations
	// once the runtime has applied the configuration.
	runtime = LocalMeshRuntimeStatus{Available: true, State: "mesh-active", Detail: "Carriers: BLE: ready; LAN: ready.", AuthenticatedPeers: 2, GatewayPrincipal: device.Principal()}
	got, err = svc.ConfigureLocalMesh(ctx, enable)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(got, retry) {
		t.Fatalf("unchanged configuration attached runtime observations: %+v", got)
	}
	got, err = svc.GetLocalMeshStatus(ctx, &pb.GetLocalMeshStatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetState() != runtime.State || got.GetDetail() != runtime.Detail || !got.GetRuntimeAvailable() ||
		got.GetAuthenticatedPeers() != 2 || got.GetGatewayPrincipal() != runtime.GatewayPrincipal {
		t.Fatalf("status lost runtime observations: %+v", got)
	}

	for attempt := 0; attempt < 2; attempt++ {
		got, err = svc.ConfigureLocalMesh(ctx, &pb.ConfigureLocalMeshRequest{Nan: proto.Bool(true)})
		if err != nil {
			t.Fatal(err)
		}
		if !got.GetConfigured().GetNan() || got.GetState() != "pending-runtime" || got.GetDetail() == runtime.Detail ||
			got.GetRuntimeAvailable() || got.GetAuthenticatedPeers() != 0 || got.GetGatewayPrincipal() != "" {
			t.Fatalf("carrier configure attempt %d reported the previous carriers' health: %+v", attempt+1, got)
		}
	}

	for attempt := 0; attempt < 2; attempt++ {
		got, err = svc.ConfigureLocalMesh(ctx, &pb.ConfigureLocalMeshRequest{Participate: proto.Bool(false)})
		if err != nil {
			t.Fatal(err)
		}
		if got.GetConfigured().GetParticipate() || got.GetState() != "disabled" || got.GetDetail() == runtime.Detail ||
			got.GetRuntimeAvailable() || got.GetAuthenticatedPeers() != 0 || got.GetGatewayPrincipal() != "" {
			t.Fatalf("disable attempt %d reported the pre-change runtime state: %+v", attempt+1, got)
		}
	}
}

func TestLocalMeshAdminReportsPendingLANCarriers(t *testing.T) {
	for name, carriers := range map[string]localmesh.TCPConfig{
		"Ethernet": {Ethernet: true},
		"Wi-Fi":    {InfrastructureWiFi: true},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := meshsharing.SaveConfig(filepath.Join(dir, "nan-mesh.json"), meshsharing.Config{Participate: true}); err != nil {
				t.Fatal(err)
			}
			tenant, device := adminTenant(t), adminDevice(t)
			if err := meshsharing.SaveCarrierConfig(filepath.Join(dir, "local-mesh.json"), device, carriers); err != nil {
				t.Fatal(err)
			}
			svc := NewLocalMeshAdminService(nil, dir, func() (meshid.Tenant, meshid.Device) { return tenant, device })
			ctx := ctxWithIdentity(t, userPrincipal(adminTenantID, "alice"))
			got, err := svc.GetLocalMeshStatus(ctx, &pb.GetLocalMeshStatusRequest{})
			if err != nil || got.GetState() != "pending-runtime" || got.GetRuntimeAvailable() {
				t.Fatalf("saved %s opt-in reported as disabled or active: %+v %v", name, got, err)
			}
		})
	}
}
