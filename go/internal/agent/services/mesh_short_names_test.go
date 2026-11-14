package services

import (
	"net/netip"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"go.uber.org/zap/zaptest"
)

func TestShortRosterKeepsExactLabelsAndLegacyAmbiguity(t *testing.T) {
	r := provisionedRoster(t)
	r.applyResponse(r.generation, &cloudpbv2.GetMeshRosterResponse{Entries: []*cloudpbv2.MeshRosterEntry{
		{Name: "jetson-a", AssetId: rosterDevice}, {Name: "jetson--a", AssetId: rosterDeviceB},
	}})
	for name, id := range map[string]string{"jetson-a": rosterDevice, "jetson--a": rosterDeviceB} {
		entry, ok := r.LookupExact(name)
		if !ok || entry.DeviceUUID != id {
			t.Fatalf("exact %s = %+v,%v", name, entry, ok)
		}
	}
	if _, ok := r.Lookup("jetson-a"); ok {
		t.Fatal("ambiguous legacy normalized alias resolved")
	}
	cert, key := deviceIdentityCertPEM(t, rosterOtherTenant, rosterDevice)
	r.UpdateIdentity("", cert, key, "")
	if _, ok := r.LookupExact("jetson-a"); ok {
		t.Fatal("exact name survived tenant replacement")
	}
}

func TestShortRosterExactCloudLabelPreservesUnambiguousLegacyAlias(t *testing.T) {
	r := provisionedRoster(t)
	r.applyResponse(r.generation, &cloudpbv2.GetMeshRosterResponse{Entries: []*cloudpbv2.MeshRosterEntry{{Name: "camera--a", AssetId: rosterDevice}}})
	for _, lookup := range []struct {
		name string
		fn   func(string) (RosterEntry, bool)
	}{{"camera-a", r.Lookup}, {"camera--a", r.LookupExact}} {
		entry, ok := lookup.fn(lookup.name)
		if !ok || entry.DeviceUUID != rosterDevice {
			t.Fatalf("lookup %s = %+v,%v, want device %s", lookup.name, entry, ok, rosterDevice)
		}
	}
}

func TestShortRosterLegacyCollisionFailsClosed(t *testing.T) {
	r := provisionedRoster(t)
	r.applyResponse(r.generation, &cloudpbv2.GetMeshRosterResponse{Entries: []*cloudpbv2.MeshRosterEntry{
		{Name: "Jetson A", AssetId: rosterDevice}, {Name: "jetson-a", AssetId: rosterDeviceB},
	}})
	if _, ok := r.LookupExact("jetson-a"); ok {
		t.Fatal("legacy alias collided silently with exact name")
	}
}

func TestShortResolverLANWithoutRosterSync(t *testing.T) {
	roster := provisionedRoster(t)
	r := NewMeshResolver(zaptest.NewLogger(t), roster, lanBrowse(
		models.LANDevice{MeshName: "jetson-a", PrincipalURI: certs.DeviceSPIFFEURI(rosterTenant, rosterDevice)},
		models.LANDevice{MeshName: "jetson--a", PrincipalURI: certs.DeviceSPIFFEURI(rosterTenant, rosterDeviceB)},
		models.LANDevice{MeshName: "foreign", PrincipalURI: certs.DeviceSPIFFEURI(rosterOtherTenant, rosterDeviceB)},
	))
	for name, id := range map[string]string{"jetson-a": rosterDevice, "jetson--a": rosterDeviceB} {
		device, ok := r.ResolveExact(name)
		if !ok || device.UUID().String() != id {
			t.Fatalf("LAN %s = %v,%v", name, device, ok)
		}
	}
	if _, ok := r.ResolveExact("foreign"); ok {
		t.Fatal("foreign tenant LAN name resolved")
	}
}

func TestShortResolverAmbiguousLANDoesNotUseRosterFallback(t *testing.T) {
	roster := provisionedRoster(t)
	roster.applyResponse(roster.generation, &cloudpbv2.GetMeshRosterResponse{Entries: []*cloudpbv2.MeshRosterEntry{{Name: "jetson", AssetId: rosterDevice}}})
	r := NewMeshResolver(zaptest.NewLogger(t), roster, lanBrowse(
		models.LANDevice{MeshName: "jetson", PrincipalURI: certs.DeviceSPIFFEURI(rosterTenant, rosterDevice)},
		models.LANDevice{MeshName: "jetson", PrincipalURI: certs.DeviceSPIFFEURI(rosterTenant, rosterDeviceB)},
	))
	if _, ok := r.ResolveExact("jetson"); ok {
		t.Fatal("ambiguous LAN name was hidden by roster fallback")
	}
}

func namedMeshSnapshot(t *testing.T, names map[string]string) localmesh.NodeSnapshot {
	t.Helper()
	self := mustDevice(t, rosterTenant, rosterDevice)
	snapshot := localmesh.NodeSnapshot{Self: self, RouteAuthorizations: map[localmesh.RouteOrigin]bool{}}
	for id, name := range names {
		device := mustDevice(t, rosterTenant, id)
		origin := localmesh.RouteOrigin{Prefix: netip.PrefixFrom(device.MeshAddress(), 128), RouterID: babel.RouterID(len(snapshot.Devices) + 1), Seqno: 1}
		snapshot.Devices = append(snapshot.Devices, localmesh.Manifest{Device: device.UUID(), Name: name, Expires: time.Now().Add(time.Minute).UnixMilli(), Route: &origin})
		snapshot.RouteAuthorizations[origin] = true
	}
	return snapshot
}

func TestShortResolverSignedMeshNamesWorkOffline(t *testing.T) {
	roster := provisionedRoster(t)
	r := NewMeshResolver(zaptest.NewLogger(t), roster, nil)
	snapshot := namedMeshSnapshot(t, map[string]string{rosterDeviceB: "jetson--a"})
	r.SetMeshSnapshot(func() localmesh.NodeSnapshot { return snapshot })
	device, ok := r.ResolveExact("jetson--a")
	if !ok || device.UUID().String() != rosterDeviceB {
		t.Fatalf("offline mesh lookup = %v,%v", device, ok)
	}
	// A compatibility alias can still use the old normalization semantics.
	if _, ok := r.Resolve("jetson-a"); !ok {
		t.Fatal("legacy normalized mesh alias stopped resolving")
	}
}

func TestShortResolverMeshRequiresCurrentTenantAndSignedLease(t *testing.T) {
	for _, scenario := range []string{"expired", "withdrawn", "unauthorized", "wrong-prefix", "foreign-tenant"} {
		t.Run(scenario, func(t *testing.T) {
			r := NewMeshResolver(zaptest.NewLogger(t), provisionedRoster(t), nil)
			snapshot := namedMeshSnapshot(t, map[string]string{rosterDeviceB: "jetson"})
			switch scenario {
			case "expired":
				snapshot.Devices[0].Expires = 1
			case "withdrawn":
				snapshot.Devices[0].Withdraw = true
			case "unauthorized":
				snapshot.RouteAuthorizations = map[localmesh.RouteOrigin]bool{}
			case "wrong-prefix":
				snapshot.Devices[0].Device = mustDevice(t, rosterTenant, rosterDevice).UUID()
			case "foreign-tenant":
				snapshot.Self = meshid.NewDevice(mustTenant(t, rosterOtherTenant), snapshot.Self.UUID())
			}
			r.SetMeshSnapshot(func() localmesh.NodeSnapshot { return snapshot })
			if _, ok := r.ResolveExact("jetson"); ok {
				t.Fatal("inadmissible mesh name resolved")
			}
		})
	}
}

func TestShortResolverSignedDuplicateNameDoesNotUseRosterFallback(t *testing.T) {
	roster := provisionedRoster(t)
	roster.applyResponse(roster.generation, &cloudpbv2.GetMeshRosterResponse{Entries: []*cloudpbv2.MeshRosterEntry{{Name: "jetson", AssetId: rosterDevice}}})
	r := NewMeshResolver(zaptest.NewLogger(t), roster, nil)
	snapshot := namedMeshSnapshot(t, map[string]string{rosterDevice: "jetson", rosterDeviceB: "jetson"})
	r.SetMeshSnapshot(func() localmesh.NodeSnapshot { return snapshot })
	if _, ok := r.ResolveExact("jetson"); ok {
		t.Fatal("ambiguous signed name was hidden by Cloud roster")
	}
}

func TestShortResolverRequiresCrossSourceAgreement(t *testing.T) {
	for _, scenario := range []string{"mesh-roster-conflict", "mesh-lan-conflict", "roster-ambiguous", "same-principal"} {
		t.Run(scenario, func(t *testing.T) {
			roster := provisionedRoster(t)
			id := rosterDeviceB
			if scenario == "mesh-roster-conflict" {
				id = rosterDevice
			}
			entries := []*cloudpbv2.MeshRosterEntry{{Name: "camera", AssetId: id}}
			if scenario == "roster-ambiguous" {
				entries = append(entries, &cloudpbv2.MeshRosterEntry{Name: "camera", AssetId: rosterDevice})
			}
			roster.applyResponse(roster.generation, &cloudpbv2.GetMeshRosterResponse{Entries: entries})
			lanID := rosterDeviceB
			if scenario == "mesh-lan-conflict" {
				lanID = rosterDevice
			}
			r := NewMeshResolver(zaptest.NewLogger(t), roster, lanBrowse(models.LANDevice{MeshName: "camera", PrincipalURI: certs.DeviceSPIFFEURI(rosterTenant, lanID)}))
			snapshot := namedMeshSnapshot(t, map[string]string{rosterDeviceB: "camera"})
			r.SetMeshSnapshot(func() localmesh.NodeSnapshot { return snapshot })
			device, ok := r.ResolveExact("camera")
			if scenario == "same-principal" {
				if !ok || device.UUID().String() != rosterDeviceB {
					t.Fatalf("agreeing sources = %v,%v", device, ok)
				}
			} else if ok {
				t.Fatalf("conflicting sources resolved to %v", device)
			}
		})
	}
}
