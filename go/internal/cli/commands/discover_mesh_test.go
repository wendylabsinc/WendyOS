package commands

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/sessionbroker"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

func nearbyMeshFixture() sessionbroker.OperatorDirectory {
	const tenant = "11111111-1111-4111-8111-111111111111"
	const device = "33333333-3333-4333-8333-333333333333"
	return sessionbroker.OperatorDirectory{Version: 1, State: "ready", Captured: time.Now().UnixMilli(), Devices: []sessionbroker.OperatorTargetInfo{{Selector: "mesh://" + tenant + "/" + device, Principal: "spiffe://wendy.sh/tenant/" + tenant + "/device/" + device, Tenant: tenant, Device: device, Name: "C5", Platform: "Lite", Transport: "mtls-v1", Caps: []string{"sensors", "future-optional"}, Expires: time.Now().Add(time.Minute).UnixMilli(), Metric: 8256, NextHop: "next-hop"}}}
}
func TestMeshNearbyJSONOnceContinuousAndWithdrawal(t *testing.T) {
	setTempConfig(t, &config.Config{})
	old := meshDirectoryRead
	t.Cleanup(func() { meshDirectoryRead = old })
	snapshot := nearbyMeshFixture()
	meshDirectoryRead = func(context.Context) (sessionbroker.OperatorDirectory, error) { return snapshot, nil }
	opts := discovery.DiscoveryOptions{Types: []models.InterfaceType{models.InterfaceMesh}, Timeout: time.Millisecond}
	output, err := captureCommandStdout(t, func() error { return discoverJSON(context.Background(), opts) })
	if err != nil || !strings.Contains(output, `"meshDevices"`) || !strings.Contains(output, `"future-optional"`) {
		t.Fatal(output, err)
	}
	collection, _, err := discoverOnceScan(context.Background(), opts, false)
	if err != nil || len(collection.MeshDevices) != 1 || len(discoverTableItems(collection)) != 1 {
		t.Fatal(collection, err)
	}
	item := discoverTableItems(collection)[0]
	if item.defaultDevice != snapshot.Devices[0].Selector || !strings.Contains(item.picker.Type, "Mesh") {
		t.Fatal(item)
	}
	model := newDiscoverModel(context.Background(), opts, false)
	updated, _ := model.Update(meshSnapshotMsg{snapshot: snapshot})
	model = updated.(discoverModel)
	if len(model.collection.MeshDevices) != 1 {
		t.Fatal("continuous feed did not list routed-only device")
	}
	updated, _ = model.Update(meshSnapshotMsg{snapshot: sessionbroker.OperatorDirectory{Version: 1, State: "unavailable"}})
	model = updated.(discoverModel)
	if len(model.collection.MeshDevices) != 0 || model.collection.MeshDiscovery.State != "unavailable" {
		t.Fatal("lost source stayed present")
	}
}
func TestMeshNearbyPickerRetainsIndependentPaths(t *testing.T) {
	snapshot := nearbyMeshFixture()
	rows, _ := meshCollection(snapshot)
	mesh := rows[0]
	lan := models.LANDevice{DisplayName: "physical", Hostname: "physical.local", PrincipalURI: mesh.Principal, TenantUUID: mesh.Tenant, IsMTLS: true}
	picker := tui.NewPicker()
	picker.MergeItem = mergePickerItem
	m := newDevicePickerModel(context.Background(), picker, nil, 0, true, devicePickerLocalTab)
	m, _ = m.updateLocal(tui.PickerAddMsg{Items: []tui.PickerItem{lanPickerItem(lan, false, tui.ProbeOK)}})
	m, _ = reconcileMeshPicker(m, snapshot)
	meshPaths, physicalPaths, total := 0, 0, 0
	count := func(item tui.PickerItem) (tui.PickerItem, bool) {
		total++
		d := item.Value.(*pickerEntry).mergedDevice
		if d.Mesh != nil {
			meshPaths++
		}
		if d.LAN != nil {
			physicalPaths++
		}
		return item, true
	}
	next, _ := m.local.Update(tui.PickerMapMsg{Map: count})
	m.local = next.(tui.PickerModel)
	if total != 1 || meshPaths != 1 || physicalPaths != 1 {
		t.Fatal("same principal not grouped", total, meshPaths, physicalPaths)
	}
	m, _ = reconcileMeshPicker(m, sessionbroker.OperatorDirectory{Version: 1, State: "unavailable"})
	total, meshPaths, physicalPaths = 0, 0, 0
	next, _ = m.local.Update(tui.PickerMapMsg{Map: count})
	m.local = next.(tui.PickerModel)
	if total != 1 || meshPaths != 0 || physicalPaths != 1 {
		t.Fatal("mesh withdrawal removed physical path", total, meshPaths, physicalPaths)
	}
	m, _ = reconcileMeshPicker(m, snapshot)
	msg := lanPickerEventMsg(discovery.LANEvent{Kind: discovery.LANRetracted, Device: lan})
	m, _ = m.updateLocal(msg)
	total, meshPaths, physicalPaths = 0, 0, 0
	next, _ = m.local.Update(tui.PickerMapMsg{Map: count})
	m.local = next.(tui.PickerModel)
	if total != 1 || meshPaths != 1 || physicalPaths != 0 {
		t.Fatal("LAN withdrawal removed mesh", total, meshPaths, physicalPaths)
	}
}
func TestMeshNearbyNameAndDefaultUseCanonicalSelector(t *testing.T) {
	snapshot := nearbyMeshFixture()
	setTempConfig(t, &config.Config{})
	oldRead, oldDescribe := meshDirectoryRead, describeOperatorMeshTarget
	t.Cleanup(func() { meshDirectoryRead = oldRead; describeOperatorMeshTarget = oldDescribe })
	meshDirectoryRead = func(context.Context) (sessionbroker.OperatorDirectory, error) { return snapshot, nil }
	describeOperatorMeshTarget = func(context.Context, string) (sessionbroker.OperatorTargetInfo, error) {
		return snapshot.Devices[0], nil
	}
	picked, matched, err := connectNamedDeviceSelector(context.Background(), "C5", true)
	if err != nil || !matched || picked.External == nil || picked.DefaultSelector != snapshot.Devices[0].Selector {
		t.Fatal(picked, matched, err)
	}
	row := snapshot.Devices[0]
	row.Device = "44444444-4444-4444-8444-444444444444"
	row.Principal = "spiffe://wendy.sh/tenant/" + row.Tenant + "/device/" + row.Device
	row.Selector = "mesh://" + row.Tenant + "/" + row.Device
	snapshot.Devices = append(snapshot.Devices, row)
	if _, matched, err = connectNamedDeviceSelector(context.Background(), "C5", true); !matched || err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatal("normal duplicate name needs explicit selection", matched, err)
	}
}
