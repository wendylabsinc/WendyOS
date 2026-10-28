package commands

import (
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/sessionbroker"
)

func TestDesktopMeshPreservesBrokerIdentity(t *testing.T) {
	now := time.Now()
	selector := "mesh://11111111-1111-4111-8111-111111111111/22222222-2222-4222-8222-222222222222"
	principal := "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/device/22222222-2222-4222-8222-222222222222"
	snapshot := sessionbroker.OperatorDirectory{State: "ready", Captured: now.UnixMilli(), Devices: []sessionbroker.OperatorTargetInfo{{Selector: selector, Principal: principal, Device: "22222222-2222-4222-8222-222222222222", Name: "robot", Platform: "WendyOS", Address: "[fd00::1234]:50052", Expires: now.Add(time.Minute).UnixMilli()}}}
	devices := desktopMeshStatus(snapshot).Devices
	if len(devices) != 1 {
		t.Fatalf("devices: %+v", devices)
	}
	row := meshDeviceRow(devices[0])
	if row.Hostname != selector || row.PrincipalURI != principal || !row.Mesh || row.Port != 50052 {
		t.Fatalf("lost broker target identity: %+v", row)
	}
	snapshot.Captured = now.Add(-time.Minute).UnixMilli()
	if len(desktopMeshStatus(snapshot).Devices) != 0 {
		t.Fatal("stale broker snapshot became a desktop target")
	}
}
