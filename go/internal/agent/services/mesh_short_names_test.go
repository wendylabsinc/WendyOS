package services

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
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

func TestShortResolverUsesOnlyTrustedNameSources(t *testing.T) {
	roster := provisionedRoster(t)
	r := NewMeshResolver(zaptest.NewLogger(t), roster, func(context.Context) ([]models.LANDevice, error) {
		t.Fatal("short DNS must not wait for a LAN browse")
		return nil, nil
	})
	if _, ok := r.ResolveExact("jetson"); ok {
		t.Fatal("short name resolved without a trusted binding")
	}
	roster.applyResponse(roster.generation, &cloudpbv2.GetMeshRosterResponse{Entries: []*cloudpbv2.MeshRosterEntry{
		{Name: "jetson", AssetId: rosterDevice},
	}})
	device, ok := r.ResolveExact("jetson")
	if !ok || device.UUID().String() != rosterDevice {
		t.Fatalf("short Cloud name = %v, %v", device, ok)
	}
}

func TestLegacyResolverKeepsBoundedLANBrowse(t *testing.T) {
	r := NewMeshResolver(zaptest.NewLogger(t), provisionedRoster(t), func(ctx context.Context) ([]models.LANDevice, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > time.Second {
			t.Fatalf("legacy browse deadline = %v, present = %v", deadline, ok)
		}
		return nil, nil
	})
	r.Resolve("jetson")
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

func TestLegacyResolverMeshCannotOverrideCloudBinding(t *testing.T) {
	for _, scenario := range []struct {
		name      string
		entries   []*cloudpbv2.MeshRosterEntry
		wantFound bool
	}{
		{
			name: "existing Cloud binding",
			entries: []*cloudpbv2.MeshRosterEntry{
				{Name: "jetson-a", AssetId: rosterDevice},
			},
			wantFound: true,
		},
		{
			name: "ambiguous Cloud binding",
			entries: []*cloudpbv2.MeshRosterEntry{
				{Name: "jetson-a", AssetId: rosterDevice},
				{Name: "jetson--a", AssetId: rosterDeviceB},
			},
		},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			roster := provisionedRoster(t)
			roster.applyResponse(roster.generation, &cloudpbv2.GetMeshRosterResponse{Entries: scenario.entries})
			r := NewMeshResolver(zaptest.NewLogger(t), roster, nil)
			snapshot := namedMeshSnapshot(t, map[string]string{rosterDeviceB: "jetson--a"})
			r.SetMeshSnapshot(func() localmesh.NodeSnapshot { return snapshot })
			device, found := r.Resolve("jetson-a")
			if found != scenario.wantFound || found && device.UUID().String() != rosterDevice {
				t.Fatalf("legacy lookup = %v, %v; want Cloud binding present=%v", device, found, scenario.wantFound)
			}
		})
	}
}

func TestShortResolverUsesReceiverLease(t *testing.T) {
	for _, scenario := range []struct {
		name             string
		signed, admitted time.Duration
		wantFound        bool
	}{
		{"live receiver lease", -time.Minute, time.Minute, true},
		{"expired receiver lease", time.Minute, -time.Minute, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			now := time.Now()
			r := NewMeshResolver(zaptest.NewLogger(t), provisionedRoster(t), nil)
			snapshot := namedMeshSnapshot(t, map[string]string{rosterDeviceB: "jetson"})
			snapshot.Devices[0].Expires = now.Add(scenario.signed).UnixMilli()
			snapshot.Devices[0].AdmittedUntil = now.Add(scenario.admitted)
			r.SetMeshSnapshot(func() localmesh.NodeSnapshot { return snapshot })
			device, found := r.ResolveExact("jetson")
			if found != scenario.wantFound || found && device.UUID().String() != rosterDeviceB {
				t.Fatalf("short lookup = %v, %v; want signed binding present=%v", device, found, scenario.wantFound)
			}
		})
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
	for _, scenario := range []string{"mesh-roster-conflict", "roster-ambiguous", "same-principal"} {
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
			r := NewMeshResolver(zaptest.NewLogger(t), roster, nil)
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
