package commands

import (
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

func TestSensorSourceItemsFiltersToCapable(t *testing.T) {
	devs := []models.DiscoveredDevice{
		{DisplayName: "hub", Sensorlink: true, AssetID: 5, OrgID: 3},
		{DisplayName: "jetson", Sensorlink: false, AssetID: 6, OrgID: 3},
	}
	items := sensorSourceItems(devs)
	if len(items) != 1 || items[0].Name != "hub" {
		t.Fatalf("expected only the sensorlink device, got %+v", items)
	}
}

func TestTransportForDevice(t *testing.T) {
	agentDev := models.DiscoveredDevice{Sensorlink: true, IsMTLS: true, Caps: []string{"sensors"}, AssetID: 5}
	if transportForDevice(agentDev) != "grpc" {
		t.Fatal("agent source should be grpc")
	}
	mcuDev := models.DiscoveredDevice{Sensorlink: true, IsMTLS: false, AssetID: 6} // legacy tcp sensorlink
	if transportForDevice(mcuDev) != "tcp" {
		t.Fatal("legacy/MCU source should be tcp")
	}
}

func TestOrgAllowedRejectsMismatch(t *testing.T) {
	cli := map[int32]bool{3: true}
	if err := orgAllowed(cli, 3); err != nil {
		t.Fatalf("same org should pass: %v", err)
	}
	if err := orgAllowed(cli, 9); err == nil {
		t.Fatal("cross-org should fail")
	}
}

// With several sessions the user should be able to pair a device in ANY of
// their orgs, not just the default one — the multi-session bug this fixes.
func TestOrgAllowedAcceptsAnyLoggedInOrg(t *testing.T) {
	cli := map[int32]bool{2: true, 77: true}
	if err := orgAllowed(cli, 2); err != nil {
		t.Fatalf("device in org 2 must be allowed when logged in to {2,77}: %v", err)
	}
	if err := orgAllowed(cli, 77); err != nil {
		t.Fatalf("device in org 77 must be allowed when logged in to {2,77}: %v", err)
	}
	err := orgAllowed(cli, 9)
	if err == nil {
		t.Fatal("device in org 9 must be rejected when logged in to {2,77}")
	}
	// The error must name both the device's org and the orgs the CLI holds.
	for _, want := range []string{"9", "2", "77"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %s", want, err.Error())
		}
	}
}
