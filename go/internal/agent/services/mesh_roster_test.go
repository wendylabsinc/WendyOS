package services

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/url"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"go.uber.org/zap/zaptest"
)

const (
	rosterTenant      = "11111111-1111-4111-8111-111111111111"
	rosterOtherTenant = "99999999-9999-4999-8999-999999999999"
	rosterDevice      = "22222222-2222-4222-8222-222222222222"
	rosterAssetA      = rosterDevice
	rosterAssetB      = rosterDeviceB
	rosterDeviceB     = "55555555-5555-4555-8555-555555555555"
)

// deviceIdentityCertPEM mints a self-signed P-256 leaf carrying the tenant
// SPIFFE device principal pki-core stamps on an enrolled device.
func deviceIdentityCertPEM(t *testing.T, tenant, device string) (certPEM, keyPEM string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	uri, err := url.Parse(certs.DeviceSPIFFEURI(tenant, device))
	if err != nil {
		t.Fatalf("parsing principal: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(7),
		Subject:      pkix.Name{CommonName: "roster-test-device"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		URIs:         []*url.URL{uri},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("creating cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("marshaling key: %v", err)
	}
	certPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	keyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	return certPEM, keyPEM
}

// provisionedRoster is a roster authenticated by an enrolled device identity,
// which is the shape a synced roster always has: the cloud scopes the entries
// to the tenant that identity authenticates.
func provisionedRoster(t *testing.T) *MeshRoster {
	t.Helper()
	certPEM, keyPEM := deviceIdentityCertPEM(t, rosterTenant, rosterDevice)
	return NewMeshRoster(zaptest.NewLogger(t), "cloud.example:443", certPEM, keyPEM, "")
}

func TestRosterLookupNormalizes(t *testing.T) {
	r := provisionedRoster(t)
	r.applyResponse(r.generation, &cloudpbv2.GetMeshRosterResponse{
		OrgSlug: "acme",
		Entries: []*cloudpbv2.MeshRosterEntry{
			{Name: "Brave Dolphin", AssetId: rosterAssetA},
			{Name: "calm-otter", AssetId: rosterAssetB},
		},
	})
	if r.OrgSlug() != "acme" {
		t.Fatalf("OrgSlug = %q, want acme", r.OrgSlug())
	}
	entry, ok := r.Lookup("brave-dolphin")
	if !ok {
		t.Fatal("Lookup(brave-dolphin) must resolve")
	}
	if entry.Name != "brave-dolphin" || entry.AssetUUID != rosterAssetA || entry.DeviceUUID != rosterDevice {
		t.Fatalf("Lookup(brave-dolphin) = %+v, want name brave-dolphin asset %s device %s", entry, rosterAssetA, rosterDevice)
	}
	if !entry.Dialable() {
		t.Fatal("an entry with a device UUID must be dialable")
	}
	device, ok := entry.Device()
	if !ok || device.Principal() != certs.DeviceSPIFFEURI(rosterTenant, rosterDevice) {
		t.Fatalf("Device() = %+v,%v want principal %s", device, ok, certs.DeviceSPIFFEURI(rosterTenant, rosterDevice))
	}
	if entry, ok := r.Lookup("calm-otter"); !ok || entry.AssetUUID != rosterAssetB {
		t.Fatalf("Lookup(calm-otter) = %+v,%v want asset %s,true", entry, ok, rosterAssetB)
	}
	if _, ok := r.Lookup("ghost"); ok {
		t.Fatal("Lookup(ghost) should be false")
	}
}

// Empty or noncanonical Cloud IDs must not manufacture device identities.
func TestRosterInvalidDeviceUUIDIsNotListed(t *testing.T) {
	for _, id := range []string{"", "fleet/box-01", "UPPER", "42"} {
		r := provisionedRoster(t)
		r.applyResponse(r.generation, &cloudpbv2.GetMeshRosterResponse{OrgSlug: "acme", Entries: []*cloudpbv2.MeshRosterEntry{{Name: "bad-box", AssetId: id}}})
		if entry, ok := r.Lookup("bad-box"); ok {
			t.Fatalf("invalid UUID %q acquired an entry: %+v", id, entry)
		}
	}
}

func TestRosterDuplicateNameIsAmbiguous(t *testing.T) {
	r := provisionedRoster(t)
	r.applyResponse(r.generation, &cloudpbv2.GetMeshRosterResponse{
		OrgSlug: "acme",
		Entries: []*cloudpbv2.MeshRosterEntry{
			{Name: "Brave Dolphin", AssetId: rosterAssetA},
			{Name: "brave dolphin", AssetId: rosterAssetB}, // normalizes to the same label
		},
	})
	if _, ok := r.Lookup("brave-dolphin"); ok {
		t.Fatal("duplicate normalized name must resolve to ok=false")
	}
	for _, asset := range []string{rosterAssetA, rosterAssetB} {
		if _, ok := r.LookupAsset(asset); !ok {
			t.Fatalf("ambiguous display name hid unambiguous asset %s", asset)
		}
	}
}

func TestRosterDiscardsResponseFromPreviousEnrollment(t *testing.T) {
	r := provisionedRoster(t)
	oldGeneration := r.generation
	certPEM, keyPEM := deviceIdentityCertPEM(t, rosterOtherTenant, rosterDevice)
	r.UpdateIdentity("cloud.example:443", certPEM, keyPEM, "")
	r.applyResponse(oldGeneration, &cloudpbv2.GetMeshRosterResponse{
		OrgSlug: "old-tenant",
		Entries: []*cloudpbv2.MeshRosterEntry{{Name: "camera", AssetId: rosterAssetA}},
	})
	if r.OrgSlug() != "" {
		t.Fatal("stale response restored the old tenant's slug")
	}
	if _, ok := r.LookupAsset(rosterAssetA); ok {
		t.Fatal("stale response relabeled an old tenant asset under the new tenant")
	}
}

func TestRosterSlugNormalized(t *testing.T) {
	r := provisionedRoster(t)
	r.applyResponse(r.generation, &cloudpbv2.GetMeshRosterResponse{OrgSlug: "ACME Corp."})
	if r.OrgSlug() != "acme-corp" {
		t.Fatalf("OrgSlug = %q, want acme-corp", r.OrgSlug())
	}
}

// TestRosterUpdateIdentityClearsCache proves entries scoped to the previous
// identity never survive an identity swap: after moving to a different tenant
// the old names, and the old slug, are gone rather than being served from a
// stale tenant.
func TestRosterUpdateIdentityClearsCache(t *testing.T) {
	r := provisionedRoster(t)
	r.applyResponse(r.generation, &cloudpbv2.GetMeshRosterResponse{
		OrgSlug: "acme",
		Entries: []*cloudpbv2.MeshRosterEntry{{Name: "brave-dolphin", AssetId: rosterAssetA}},
	})
	if _, ok := r.Lookup("brave-dolphin"); !ok {
		t.Fatal("precondition: the entry must resolve before the identity swap")
	}

	certPEM, keyPEM := deviceIdentityCertPEM(t, rosterOtherTenant, rosterDevice)
	r.UpdateIdentity("cloud.example:443", certPEM, keyPEM, "")
	if _, ok := r.Lookup("brave-dolphin"); ok {
		t.Fatal("an entry from the previous tenant must not survive UpdateIdentity")
	}
	if r.OrgSlug() != "" {
		t.Fatalf("OrgSlug = %q after identity change, want empty", r.OrgSlug())
	}

	// The new identity is what the next response is scoped by: its tenant's
	// device UUIDs parse into dialable entries.
	r.applyResponse(r.generation, &cloudpbv2.GetMeshRosterResponse{
		OrgSlug: "other",
		Entries: []*cloudpbv2.MeshRosterEntry{{Name: "brave-dolphin", AssetId: rosterAssetA}},
	})
	entry, ok := r.Lookup("brave-dolphin")
	if !ok || !entry.Dialable() {
		t.Fatalf("Lookup after identity change = %+v,%v want a dialable entry", entry, ok)
	}
	if device, ok := entry.Device(); !ok || device.Principal() != certs.DeviceSPIFFEURI(rosterOtherTenant, rosterDevice) {
		t.Fatalf("entry kept the old tenant: %+v,%v", device, ok)
	}
}

// TestRosterSyncSkipsUnprovisionedIdentity proves Sync fails closed without
// attempting a dial when the device has no provisioned identity (the boot-time
// snapshot before BLE first-boot enrollment completes): a real dial would
// hang/error against the listener below, but Sync must return nil quickly
// because there is no device principal to authenticate an RPC with — and the
// endpoint must never see a connection.
func TestRosterSyncSkipsUnprovisionedIdentity(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	dialed := make(chan struct{}, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			_ = conn.Close()
			dialed <- struct{}{}
		}
	}()

	r := NewMeshRoster(zaptest.NewLogger(t), listener.Addr().String(), "", "", "")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- r.Sync(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Sync() without an identity = %v, want nil (no-dial skip)", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Sync() without an identity should return immediately without dialing")
	}
	select {
	case <-dialed:
		t.Fatal("Sync() dialled the cloud endpoint without a provisioned identity")
	case <-time.After(250 * time.Millisecond):
	}
}

// TestRosterUpdateIdentityIsUsedBySync proves UpdateIdentity's write is what a
// subsequent Sync reads: after moving from no identity (skip branch) to a real
// device principal, Sync no longer takes the skip path — it proceeds to a real
// dial attempt against an unreachable address instead of returning nil.
func TestRosterUpdateIdentityIsUsedBySync(t *testing.T) {
	certPEM, keyPEM := deviceIdentityCertPEM(t, rosterTenant, rosterDevice)
	r := NewMeshRoster(zaptest.NewLogger(t), "127.0.0.1:1", "", "", "")
	r.UpdateIdentity("127.0.0.1:1", certPEM, keyPEM, "")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := r.Sync(ctx); err == nil {
		t.Fatal("Sync() after UpdateIdentity to a real device principal should attempt a dial and fail against an unreachable address, not silently skip")
	}
}

func TestRosterUnsupportedDevicePrincipalCannotEstablishTenant(t *testing.T) {
	certPEM, keyPEM := deviceIdentityCertPEM(t, rosterTenant, "fleet/box")
	r := NewMeshRoster(zaptest.NewLogger(t), "invalid endpoint", certPEM, keyPEM, "")
	if _, ok := r.OwnTenant(); ok {
		t.Fatal("non-UUID device established mesh scope")
	}
	if r.identity != "" {
		t.Fatal("unsupported device enabled roster authentication")
	}
	// No network request should be attempted without a supported mesh identity.
	if err := r.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRosterLookupDeviceKeepsFullTenantBinding(t *testing.T) {
	r := provisionedRoster(t)
	r.applyResponse(r.generation, &cloudpbv2.GetMeshRosterResponse{Entries: []*cloudpbv2.MeshRosterEntry{{Name: "owner", AssetId: rosterDevice}}})
	for _, tc := range []struct {
		name, tenant, device string
		want                 bool
	}{
		{"owner", rosterTenant, rosterDevice, true},
		{"same UUID in another tenant", rosterOtherTenant, rosterDevice, false},
		{"unknown device", rosterTenant, rosterDeviceB, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			device, err := meshid.DeviceFromPrincipal(certs.DeviceSPIFFEURI(tc.tenant, tc.device))
			if err != nil {
				t.Fatal(err)
			}
			entry, ok := r.LookupDevice(device)
			if ok != tc.want || ok && (entry.AssetUUID != rosterDevice || entry.device != device) {
				t.Fatalf("LookupDevice(%s) = %+v,%v", device.Principal(), entry, ok)
			}
		})
	}
	if _, ok := r.LookupDevice(meshid.Device{}); ok {
		t.Fatal("missing identity acquired a roster entry")
	}
}
