package commands

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/wendylabsinc/wendy/go/internal/cli/providers"
	"github.com/wendylabsinc/wendy/go/internal/cli/sessionbroker"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

var meshDirectoryRead = sessionbroker.ReadOperatorDirectoryAutomatic
var meshDirectoryWatch = sessionbroker.WatchOperatorDirectoryAutomatic

func includeMeshDiscovery(opts discovery.DiscoveryOptions) bool {
	if len(opts.Types) == 0 {
		return true
	}
	for _, kind := range opts.Types {
		if kind == models.InterfaceMesh {
			return true
		}
	}
	return false
}
func meshCollection(snapshot sessionbroker.OperatorDirectory) ([]models.MeshDevice, *models.MeshDiscoveryState) {
	state := &models.MeshDiscoveryState{SourceResponseBytes: snapshot.SourceResponseBytes, PollIntervalMillis: snapshot.PollIntervalMillis, HeartbeatMillis: snapshot.HeartbeatMillis, State: snapshot.State, Source: snapshot.Source, Captured: snapshot.Captured, Truncated: snapshot.Truncated}
	if snapshot.State != "ready" || snapshot.Captured > time.Now().Add(time.Second).UnixMilli() || time.Now().UnixMilli()-snapshot.Captured > 10000 {
		if snapshot.State == "ready" {
			state.State = "unavailable"
		}
		return nil, state
	}
	names := make(map[string]string, len(snapshot.Devices))
	for _, d := range snapshot.Devices {
		if d.Expires > time.Now().UnixMilli() {
			names[d.Principal] = discovery.SanitiseDisplayName(d.Name)
		}
	}
	rows := make([]models.MeshDevice, 0, len(snapshot.Devices))
	for _, d := range snapshot.Devices {
		if d.Expires <= time.Now().UnixMilli() {
			continue
		}
		rows = append(rows, models.MeshDevice{Selector: d.Selector, Principal: d.Principal, Tenant: d.Tenant, Device: d.Device, Name: d.Name, Platform: d.Platform, Transport: d.Transport, Address: d.Address, Caps: append([]string(nil), d.Caps...), Expires: d.Expires, Revision: d.Revision, Metric: d.Metric, NextHop: d.NextHop, NextHopName: names[d.NextHop]})
	}
	return rows, state
}
func readMeshDiscovery(parent context.Context) sessionbroker.OperatorDirectory {
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	snapshot, _ := meshDirectoryRead(ctx)
	if snapshot.State == "" {
		snapshot.State = "not-connected"
	}
	return snapshot
}

type meshSnapshotMsg struct {
	snapshot sessionbroker.OperatorDirectory
	ch       <-chan sessionbroker.OperatorDirectory
}

func waitMeshSnapshot(ch <-chan sessionbroker.OperatorDirectory) tea.Cmd {
	return func() tea.Msg {
		snapshot, ok := <-ch
		if !ok {
			return meshSnapshotMsg{snapshot: sessionbroker.OperatorDirectory{Version: 1, State: "unavailable"}}
		}
		return meshSnapshotMsg{snapshot, ch}
	}
}
func (m discoverModel) startMeshStream() tea.Cmd {
	return func() tea.Msg {
		ch, err := meshDirectoryWatch(m.ctx)
		if err != nil {
			return meshSnapshotMsg{snapshot: sessionbroker.OperatorDirectory{Version: 1, State: "not-connected"}}
		}
		return waitMeshSnapshot(ch)()
	}
}

func meshPickerItem(mesh *models.MeshDevice) tui.PickerItem {
	name := mesh.Name
	if name == "" {
		name = mesh.Device
	}
	d := &models.DiscoveredDevice{DisplayName: name, PrincipalURI: mesh.Principal, TenantUUID: mesh.Tenant, Mesh: mesh}
	return tui.PickerItem{Name: discovery.SanitiseDisplayName(name), IdentityKey: mesh.Principal, DedupKey: mesh.Principal, SortKey: deviceSortKey(name, ""), Type: d.ConnectionTypes(), Address: "-", Hint: meshPathHint(mesh), Value: &pickerEntry{mergedDevice: d}}
}

// Strip only the old mesh path; unrelated physical/provider rows remain owned
// by their existing sources. The next authoritative mesh snapshot replaces it.
func withoutMeshPickerItem(item tui.PickerItem) (tui.PickerItem, bool) {
	entry, ok := item.Value.(*pickerEntry)
	if !ok || entry.mergedDevice == nil || entry.mergedDevice.Mesh == nil {
		return item, true
	}
	copy := *entry.mergedDevice
	copy.Mesh = nil
	if copy.LAN == nil && copy.Bluetooth == nil && len(copy.Externals) == 0 {
		return item, false
	}
	e := *entry
	e.mergedDevice = &copy
	item.Value = &e
	item.Type = copy.ConnectionTypes()
	item.Address = copy.Address()
	item.Hint = lanProbeHint(copy.LAN)
	return item, true
}
func reconcileMeshPicker(m devicePickerModel, snapshot sessionbroker.OperatorDirectory) (devicePickerModel, tea.Cmd) {
	model, _ := m.local.Update(tui.PickerMapMsg{Map: withoutMeshPickerItem})
	m.local = model.(tui.PickerModel)
	rows, _ := meshCollection(snapshot)
	items := make([]tui.PickerItem, 0, len(rows))
	for i := range rows {
		items = append(items, meshPickerItem(&rows[i]))
	}
	return m.updateLocal(tui.PickerAddMsg{Items: items})
}

func connectMeshName(ctx context.Context, name string, suppress bool) (*SelectedDevice, bool, error) {
	if name == "" || providers.ProviderForKey(name) != nil || strings.ContainsAny(name, "./:[") {
		return nil, false, nil
	}
	snapshot := readMeshDiscovery(ctx)
	var matches []sessionbroker.OperatorTargetInfo
	for _, d := range snapshot.Devices {
		if snapshot.State != "ready" || d.Expires <= time.Now().UnixMilli() {
			continue
		}
		if d.Device == name || strings.EqualFold(d.Name, name) {
			matches = append(matches, d)
		}
	}
	if len(matches) == 0 {
		return nil, false, nil
	}
	if len(matches) > 1 {
		return nil, true, fmt.Errorf("mesh name %q is ambiguous; select the canonical mesh://tenant/device identity", name)
	}
	picked, _, err := connectNamedDeviceSelector(ctx, matches[0].Selector, suppress)
	return picked, true, err
}

func withoutLANPickerItem(item tui.PickerItem, dev models.LANDevice) (tui.PickerItem, bool) {
	entry, ok := item.Value.(*pickerEntry)
	if !ok || entry.mergedDevice == nil || entry.mergedDevice.LAN == nil || models.CanonicalDevicePrincipal(entry.mergedDevice.PrincipalURI) != models.CanonicalDevicePrincipal(dev.PrincipalURI) {
		return item, true
	}
	copy := *entry.mergedDevice
	copy.LAN = nil
	if copy.Mesh == nil && copy.Bluetooth == nil && len(copy.Externals) == 0 {
		return item, false
	}
	if copy.Mesh != nil {
		updated := meshPickerItem(copy.Mesh)
		updated.Value = &pickerEntry{mergedDevice: &copy}
		updated.Type = copy.ConnectionTypes()
		return updated, true
	}
	e := *entry
	e.mergedDevice = &copy
	item.Value = &e
	item.Type = copy.ConnectionTypes()
	item.Address = copy.Address()
	return item, true
}

func meshPathHint(mesh *models.MeshDevice) string {
	if mesh.NextHop == "" {
		return "Attached Wendy Net router"
	}
	if mesh.NextHopName == "" {
		return fmt.Sprintf("Wendy Net; route metric %d", mesh.Metric)
	}
	return fmt.Sprintf("Wendy Net via %s; route metric %d", mesh.NextHopName, mesh.Metric)
}
