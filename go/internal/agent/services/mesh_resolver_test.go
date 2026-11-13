package services

import (
	"context"
	"errors"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"go.uber.org/zap/zaptest"
)

func lanBrowse(devices ...models.LANDevice) func(context.Context) ([]models.LANDevice, error) {
	return func(context.Context) ([]models.LANDevice, error) { return devices, nil }
}

func mustDevice(t *testing.T, tenant, device string) meshid.Device {
	t.Helper()
	parsed, err := meshid.ParseDevice(mustTenant(t, tenant), device)
	if err != nil {
		t.Fatalf("parsing fixture device: %v", err)
	}
	return parsed
}

func mustTenant(t *testing.T, tenant string) meshid.Tenant {
	t.Helper()
	parsed, err := meshid.ParseTenant(tenant)
	if err != nil {
		t.Fatalf("parsing fixture tenant: %v", err)
	}
	return parsed
}

func TestResolverMDNSHitShortCircuits(t *testing.T) {
	roster := provisionedRoster(t)
	roster.applyResponse(roster.generation, &cloudpbv2.GetMeshRosterResponse{
		OrgSlug: "acme",
		Entries: []*cloudpbv2.MeshRosterEntry{{Name: "brave-dolphin", AssetId: rosterAssetA}},
	})
	r := NewMeshResolver(zaptest.NewLogger(t), roster, lanBrowse(models.LANDevice{
		MeshName:     "Brave Dolphin",
		PrincipalURI: certs.DeviceSPIFFEURI(rosterTenant, rosterDeviceB),
	}))
	got, ok := r.Resolve("brave-dolphin")
	if !ok || got != mustDevice(t, rosterTenant, rosterDeviceB) {
		t.Fatalf("mDNS hit = %v,%v want the LAN device principal", got, ok)
	}
}

func TestResolverIgnoresForeignTenantOnLAN(t *testing.T) {
	roster := provisionedRoster(t)
	r := NewMeshResolver(zaptest.NewLogger(t), roster, lanBrowse(models.LANDevice{
		MeshName:     "brave-dolphin",
		PrincipalURI: certs.DeviceSPIFFEURI(rosterOtherTenant, rosterDeviceB),
	}))
	if _, ok := r.Resolve("brave-dolphin"); ok {
		t.Fatal("must not resolve a same-named device from another tenant on the LAN")
	}
}

// TestResolverFallsBackToRosterWithDeviceUUIDs proves the off-LAN half hands
// through the Cloud v2 device UUID unchanged. A roster row with no device
// UUID carries no mesh identity and resolves to nothing rather than an
// invented identity.
func TestResolverFallsBackToRosterWithDeviceUUIDs(t *testing.T) {
	roster := provisionedRoster(t)
	roster.applyResponse(roster.generation, &cloudpbv2.GetMeshRosterResponse{
		OrgSlug: "acme",
		Entries: []*cloudpbv2.MeshRosterEntry{
			{Name: "calm-otter", AssetId: rosterAssetB},
			{Name: "legacy-box", AssetId: "42"},
		},
	})
	r := NewMeshResolver(zaptest.NewLogger(t), roster, lanBrowse())
	got, ok := r.Resolve("calm-otter")
	if !ok || got != mustDevice(t, rosterTenant, rosterDeviceB) {
		t.Fatalf("roster fallback = %v,%v want device %s", got, ok, rosterDeviceB)
	}
	if got.MeshAddress() != mustDevice(t, rosterTenant, rosterDeviceB).MeshAddress() {
		t.Fatal("resolved device must carry the deterministic mesh address")
	}
	if _, ok := r.Resolve("legacy-box"); ok {
		t.Fatal("roster row without a device UUID must not resolve")
	}
}

func TestResolverAmbiguousDifferentDevicesOnLAN(t *testing.T) {
	roster := provisionedRoster(t)
	r := NewMeshResolver(zaptest.NewLogger(t), roster, lanBrowse(
		models.LANDevice{MeshName: "brave-dolphin", PrincipalURI: certs.DeviceSPIFFEURI(rosterTenant, rosterDevice)},
		models.LANDevice{MeshName: "brave dolphin", PrincipalURI: certs.DeviceSPIFFEURI(rosterTenant, rosterDeviceB)},
	))
	if _, ok := r.Resolve("brave-dolphin"); ok {
		t.Fatal("two different LAN devices sharing a name must not resolve")
	}
}

func TestResolverSameDeviceRepeatedNotAmbiguous(t *testing.T) {
	roster := provisionedRoster(t)
	device := certs.DeviceSPIFFEURI(rosterTenant, rosterDeviceB)
	r := NewMeshResolver(zaptest.NewLogger(t), roster, lanBrowse(
		models.LANDevice{MeshName: "brave-dolphin", PrincipalURI: device},
		models.LANDevice{MeshName: "brave-dolphin", PrincipalURI: device},
	))
	got, ok := r.Resolve("brave-dolphin")
	if !ok || got != mustDevice(t, rosterTenant, rosterDeviceB) {
		t.Fatalf("repeated sighting = %v,%v want one device", got, ok)
	}
}

// TestResolverUpdateIdentityReachesFilter proves the tenant filter follows the
// roster's provisioned identity: an unenrolled device has no tenant to filter
// LAN sightings by, and provisioning mid-flight re-scopes resolution without
// rebuilding the resolver.
func TestResolverUpdateIdentityReachesFilter(t *testing.T) {
	unenrolled := NewMeshRoster(zaptest.NewLogger(t), "", "", "", "")
	r := NewMeshResolver(zaptest.NewLogger(t), unenrolled, lanBrowse(models.LANDevice{
		MeshName:     "brave-dolphin",
		PrincipalURI: certs.DeviceSPIFFEURI(rosterTenant, rosterDeviceB),
	}))
	if _, ok := r.Resolve("brave-dolphin"); ok {
		t.Fatal("unenrolled device must not trust an unfiltered LAN sighting")
	}
	certPEM, keyPEM := deviceIdentityCertPEM(t, rosterTenant, rosterDevice)
	unenrolled.UpdateIdentity("", certPEM, keyPEM, "")
	got, ok := r.Resolve("brave-dolphin")
	if !ok || got != mustDevice(t, rosterTenant, rosterDeviceB) {
		t.Fatalf("after provisioning = %v,%v want the tenant device", got, ok)
	}
}

func TestResolverBrowseErrorFallsBackToRoster(t *testing.T) {
	roster := provisionedRoster(t)
	roster.applyResponse(roster.generation, &cloudpbv2.GetMeshRosterResponse{
		OrgSlug: "acme",
		Entries: []*cloudpbv2.MeshRosterEntry{{Name: "calm-otter", AssetId: rosterAssetB}},
	})
	broken := func(context.Context) ([]models.LANDevice, error) { return nil, errors.New("mdns down") }
	r := NewMeshResolver(zaptest.NewLogger(t), roster, broken)
	got, ok := r.Resolve("calm-otter")
	if !ok || got != mustDevice(t, rosterTenant, rosterDeviceB) {
		t.Fatalf("browse failure fallback = %v,%v want roster entry", got, ok)
	}
}
