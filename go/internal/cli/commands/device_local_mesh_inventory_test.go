package commands

import (
	"context"
	"encoding/json"
	"math"
	"net"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/cli/sessionbroker"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

func TestCLI4MeshInventorySelectsLiteReporterWithoutAgentProbe(t *testing.T) {
	const tenant = "11111111-1111-4111-8111-111111111111"
	const c6 = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	const c5 = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	var targets []sessionbroker.OperatorTargetInfo
	for index, device := range []string{c6, c5} {
		selector := "mesh://" + tenant + "/" + device
		targets = append(targets, sessionbroker.OperatorTargetInfo{Selector: selector, Principal: "spiffe://wendy.sh/tenant/" + tenant + "/device/" + device, Tenant: tenant, Device: device, Name: []string{"C6 mesh only", "C5 mesh alias"}[index], Platform: "Lite", Transport: "mtls-v1", Address: net.JoinHostPort(meshid.MeshAddressFor(uuid.MustParse(device)).String(), "5054"), Caps: []string{"sensors"}, Expires: time.Now().Add(5 * time.Minute).UnixMilli(), Revision: math.MaxUint64 - uint64(index), Metric: 128})
	}
	oldDescribe := describeOperatorMeshTarget
	defer func() { describeOperatorMeshTarget = oldDescribe }()
	described := 0
	describeOperatorMeshTarget = func(_ context.Context, selector string) (sessionbroker.OperatorTargetInfo, error) {
		if selector != targets[0].Selector {
			t.Fatal(selector)
		}
		described++
		return targets[0], nil
	}
	selected, matched, err := connectNamedDeviceSelector(context.Background(), targets[0].Selector, true)
	if err != nil || !matched || selected.External == nil || selected.Agent != nil || selected.Provider.Key() != "wendy-lite" || selected.External.ConnectionInfo["type"] != "Mesh" || selected.External.ID != targets[0].Selector || described != 1 {
		t.Fatal(selected, matched, err)
	}
	// Use CLI4's actual production mapper and Go JSON model. No target TLS or
	// discovery probe is needed to project its retained signed metadata.
	rows, state := meshCollection(sessionbroker.OperatorDirectory{State: "ready", Source: "local-cli", Captured: time.Now().UnixMilli(), Devices: targets})
	if len(rows) != 2 || state.State != "ready" {
		t.Fatal(rows, state)
	}
	state.Captured = 1800000000123
	for index := range rows {
		rows[index].Expires = 1800000300123
	}
	collection := models.DevicesCollection{MeshDevices: rows, MeshDiscovery: state, ExternalDevices: []models.ExternalDevice{{ID: "wendy-lite:/dev/cu.usbmodemC5", DisplayName: "C5 physical", ProviderKey: "wendy-lite", IsWendyDevice: true, ConnectionInfo: map[string]string{"type": "USB", "deviceId": "hardware-c5", "serialPort": "/dev/cu.usbmodemC5", "enrolledDeviceId": c5}}}}
	cloud := []struct {
		cloudDiscoverV2Info
		Selector string `json:"selector"`
	}{{cloudDiscoverV2Info: cloudDiscoverV2Info{ID: c5, Name: "C5 Cloud", Type: "ESP32-C5", Address: "cloud"}, Selector: "cloud://cloud.example:443/tenant/" + tenant + "/asset/" + c5}}
	// Bundle the two real command JSON models in one deterministic test file.
	raw, err := json.MarshalIndent(struct {
		Collection models.DevicesCollection `json:"collection"`
		Cloud      any                      `json:"cloud"`
	}{collection, cloud}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if output := os.Getenv("WENDY_GENERATE_MESH_INVENTORY_FIXTURE"); output != "" {
		if err := os.WriteFile(output, append(raw, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	}
}
