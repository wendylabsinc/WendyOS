package commands

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/cli/vm"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

func defaultKey(t *testing.T) string {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	return cfg.DefaultDevice
}

func TestSimulatorPickerSavesStableDefaultWithoutStartingVM(t *testing.T) {
	setTempConfig(t, &config.Config{})
	m, _ := simulatorViewFor(t, stoppedVM("test-sim", "v1"))
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	if got := defaultKey(t); got != "vm:test-sim" {
		t.Fatalf("saved %q", got)
	}
	if m.selected() != nil || m.createRequested() {
		t.Fatal("setting default selected or created a VM")
	}
	// Reopening with a new forwarded address must retain the name-based marker.
	reopened, _ := simulatorViewFor(t, runningVM("test-sim", vm.NetUser, 50199))
	if reopened.picker.DefaultKey != "vm:test-sim" || !strings.Contains(reopened.View(), "✦") {
		t.Fatal("reopened picker lost default marker")
	}
	reopened, _ = reopened.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	if defaultKey(t) != "" {
		t.Fatal("default not cleared")
	}
}

func TestUncreatedSimulatorCannotBeDefault(t *testing.T) {
	setTempConfig(t, &config.Config{DefaultDevice: "existing.local"})
	m, _ := simulatorViewFor(t)
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	if defaultKey(t) != "existing.local" || m.picker.DefaultKey != "existing.local" {
		t.Fatal("placeholder replaced existing default")
	}
	if !strings.Contains(m.View(), "create the simulator") {
		t.Fatal("missing explanation")
	}
}

func TestCloudPickerDefaultPersistsAndSynchronizesTabs(t *testing.T) {
	setTempConfig(t, &config.Config{DefaultDevice: "vm:dev"})
	auth := pickerAuth(7)
	asset := &cloudpb.Asset{Id: 42, Name: "Go2"}
	m := newDevicePickerModel(context.Background(), tui.NewPicker(), auth, 7, false, devicePickerLocalTab)
	m.sim, _ = m.sim.Update(simulatorVMsMsg{vms: []vm.Status{stoppedVM("dev", "")}})
	m.active = devicePickerCloudTab
	updated, _ := m.Update(devicePickerCloudMsg{msg: cloudScanMsg{assets: []*cloudpb.Asset{asset}}})
	m = updated.(devicePickerModel)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	m = updated.(devicePickerModel)
	want := "cloud://cloud.example:443/org/7/asset/42"
	if defaultKey(t) != want || m.cloud.defaultDevice != want || m.cloud.selected != nil {
		t.Fatalf("cloud default did not persist without connecting: %q", defaultKey(t))
	}
	m, _ = tabTo(t, m, devicePickerSimulatorTab)
	if m.sim.picker.DefaultKey != want {
		t.Fatal("simulator tab kept the old default marker")
	}
	// A fresh cloud model recognizes the same ID after a rename.
	reopened := newCloudDiscoverModel(context.Background(), auth, "", false, true,
		[]*cloudpb.Asset{{Id: 42, Name: "renamed"}})
	if reopened.defaultDevice != want || !strings.Contains(reopened.table.View(), "✦") {
		t.Fatal("renamed cloud asset lost default marker")
	}
	updatedCloud, _ := reopened.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	if defaultKey(t) != "" || updatedCloud.(cloudDiscoverModel).defaultDevice != "" {
		t.Fatal("cloud default not cleared")
	}
}

func TestCloudDefaultResolvesExactCloudOrgAndAsset(t *testing.T) {
	auth7, auth9 := pickerAuth(7), pickerAuth(9)
	otherCloud := *auth7
	otherCloud.CloudGRPC = "other.example:443"
	setTempConfig(t, &config.Config{Auth: []config.AuthConfig{*auth9, otherCloud, *auth7}, DefaultOrgID: 9})
	oldFetch, oldConnect := fetchDefaultCloudAssetsFn, connectDefaultCloudAssetFn
	t.Cleanup(func() { fetchDefaultCloudAssetsFn, connectDefaultCloudAssetFn = oldFetch, oldConnect })
	fetchDefaultCloudAssetsFn = func(_ context.Context, auth *config.AuthConfig) ([]*cloudpb.Asset, error) {
		if auth.CloudGRPC != auth7.CloudGRPC || cloudAuthOrgID(auth) != 7 {
			t.Fatal("used the default org instead of the saved org")
		}
		return []*cloudpb.Asset{{Id: 99, Name: "42"}, {Id: 42, Name: "renamed"}}, nil
	}
	connectDefaultCloudAssetFn = func(_ context.Context, auth *config.AuthConfig, asset *cloudpb.Asset, _ string) (*grpcclient.AgentConnection, error) {
		if asset.Id != 42 || cloudAuthOrgID(auth) != 7 {
			t.Fatal("connected to wrong asset or organization")
		}
		return &grpcclient.AgentConnection{Host: asset.Name}, nil
	}
	selector := cloudDeviceSelector{Endpoint: auth7.CloudGRPC, OrgID: 7, AssetID: 42}
	selected, err := connectCloudDeviceSelector(context.Background(), selector)
	if err != nil {
		t.Fatal(err)
	}
	if key, err := defaultDeviceNameFor(selected); err != nil || key != selector.String() {
		t.Fatalf("saved tunnel/display address: %q, %v", key, err)
	}
	selector.AssetID = 100
	if _, err := connectCloudDeviceSelector(context.Background(), selector); err == nil {
		t.Fatal("missing asset silently selected another")
	}
	selector.OrgID = 100
	if _, err := connectCloudDeviceSelector(context.Background(), selector); err == nil {
		t.Fatal("missing organization silently selected another")
	}
}

func TestCloudDefaultsReachBothConnectionPathsAndMCP(t *testing.T) {
	key := "cloud://cloud.example:443/org/7/asset/42"
	setTempConfig(t, &config.Config{DefaultDevice: key})
	t.Setenv("WENDY_AGENT_SOCKET", "")
	oldFlag, oldJSON, oldConnect := deviceFlag, jsonOutput, connectCloudDeviceSelectorFn
	t.Cleanup(func() { deviceFlag, jsonOutput, connectCloudDeviceSelectorFn = oldFlag, oldJSON, oldConnect })
	deviceFlag, jsonOutput = "", true
	calls := 0
	connectCloudDeviceSelectorFn = func(_ context.Context, selector cloudDeviceSelector) (*SelectedDevice, error) {
		calls++
		if selector.AssetID != 42 {
			t.Fatal("wrong default asset")
		}
		return &SelectedDevice{Agent: &grpcclient.AgentConnection{Host: "Go2"}}, nil
	}
	if _, err := connectToAgent(context.Background(), NonInteractive()); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveTargetInner(context.Background(), NonInteractive()); err != nil {
		t.Fatal(err)
	}
	if _, err := connectMCPDevice(context.Background(), mcpStartupAddress(key)); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("routed %d calls", calls)
	}
	// Explicit target overrides the saved default; a failed named target never
	// falls through to LAN discovery or another organization.
	failure := errors.New("selected asset offline")
	connectCloudDeviceSelectorFn = func(_ context.Context, selector cloudDeviceSelector) (*SelectedDevice, error) {
		if selector.AssetID != 43 {
			t.Fatal("explicit device did not override the default")
		}
		return nil, failure
	}
	deviceFlag = "cloud://cloud.example:443/org/7/asset/43"
	if _, err := connectToAgent(context.Background(), NonInteractive()); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if _, err := resolveTargetInner(context.Background(), NonInteractive()); !errors.Is(err, failure) {
		t.Fatal(err)
	}
}

func TestCloudDeviceSelectorValidationAndMCPAddress(t *testing.T) {
	for _, value := range []string{"cloud://host/org/0/asset/1", "cloud://host/org/7/asset/-1", "cloud://host/org/7/asset/2147483648",
		"cloud://user:pass@host/org/1/asset/2", "cloud://host/org/1/asset/2?org=3", "cloud:42", "cloud://host/org/1/asset/2#extra"} {
		if _, matched, err := parseCloudDeviceSelector(value); !matched || err == nil {
			t.Fatalf("accepted %q", value)
		}
	}
	for _, value := range []string{"cloud://host:443/org/1/asset/2", "vm:dev", "sim"} {
		if mcpStartupAddress(value) != value {
			t.Fatalf("MCP corrupted %q", value)
		}
	}
	if mcpStartupAddress("robot.local") != "robot.local:50051" {
		t.Fatal("LAN default changed")
	}
}

func TestCloudV2DefaultPreservesTenantAndAssetAcrossRename(t *testing.T) {
	const tenant = "8a53be77-2a69-464f-8f73-83643fe0beaa"
	const assetID = "7791d76e-a942-4a8e-b582-063d063b5623"
	auth := &config.AuthConfig{CloudGRPC: "cloud.example:443", Certificates: []config.CertificateInfo{{
		PrincipalURI: "spiffe://wendy.sh/tenant/" + tenant + "/operator/test",
	}}}
	otherTenant := *auth
	otherTenant.Certificates = []config.CertificateInfo{{PrincipalURI: "spiffe://wendy.sh/tenant/ed6e09f3-d287-450b-a053-e4554f3c70ed/operator/test"}}
	otherCloud := *auth
	otherCloud.CloudGRPC = "other.example:443"
	setTempConfig(t, &config.Config{Auth: []config.AuthConfig{otherTenant, otherCloud, *auth}})
	asset := &cloudpbv2.Asset{Id: assetID, Name: "original"}
	m := newCloudDiscoverModel(context.Background(), auth, "", false, true, nil)
	updated, _ := m.Update(cloudScanMsg{devices: []cloudDiscoveryDevice{{cloudAssetMetadata: asset, v2: asset, key: assetID}}})
	updated, _ = updated.(cloudDiscoverModel).Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	want := "cloud://cloud.example:443/tenant/" + tenant + "/asset/" + assetID
	if defaultKey(t) != want || !strings.Contains(updated.(cloudDiscoverModel).table.View(), "✦") {
		t.Fatalf("UUID default was not persisted and marked: %q", defaultKey(t))
	}
	selector, matched, err := parseCloudDeviceSelector(want)
	if err != nil || !matched || selector.String() != want {
		t.Fatalf("selector round trip: %+v, %v", selector, err)
	}
	oldFetch, oldConnect := fetchDefaultCloudAssetsV2Fn, connectDefaultCloudAssetV2Fn
	t.Cleanup(func() { fetchDefaultCloudAssetsV2Fn, connectDefaultCloudAssetV2Fn = oldFetch, oldConnect })
	fetchDefaultCloudAssetsV2Fn = func(_ context.Context, selected *config.AuthConfig, onlineOnly bool) ([]*cloudpbv2.Asset, error) {
		if selected.CloudGRPC != auth.CloudGRPC || selected.Certificates[0].TenantUUID() != tenant || !onlineOnly {
			t.Fatal("default resolved the wrong cloud or tenant")
		}
		return []*cloudpbv2.Asset{{Id: "a20403ae-9251-4937-aa9b-bb1d773df071", Name: "original"}, {Id: assetID, Name: "renamed"}}, nil
	}
	connectDefaultCloudAssetV2Fn = func(_ context.Context, _ *config.AuthConfig, selected *cloudpbv2.Asset, _ string) (*grpcclient.AgentConnection, error) {
		if selected.Id != assetID || selected.Name != "renamed" {
			t.Fatal("default resolved by display name instead of UUID")
		}
		return &grpcclient.AgentConnection{Host: selected.Name}, nil
	}
	selected, err := connectCloudDeviceSelector(context.Background(), selector)
	if err != nil || selected.DefaultSelector != want {
		t.Fatalf("connect default: %v, %v", selected, err)
	}
	selector.AssetUUID = "ede01a43-7ef8-49d9-bfaf-88192b5124af"
	if _, err := connectCloudDeviceSelector(context.Background(), selector); err == nil {
		t.Fatal("missing asset selected another device")
	}
	selector.TenantUUID = "4d344658-f8f6-4c1e-aa14-9d96c2d56963"
	if _, err := selector.auth(&config.Config{Auth: []config.AuthConfig{*auth}}); err == nil {
		t.Fatal("missing tenant selected another login")
	}
	for _, key := range []string{
		"cloud://cloud.example:443/tenant/not-a-uuid/asset/" + assetID,
		"cloud://cloud.example:443/tenant/" + tenant + "/asset/00000000-0000-0000-0000-000000000000",
	} {
		if _, matched, err := parseCloudDeviceSelector(key); !matched || err == nil {
			t.Fatalf("invalid UUID selector accepted: %s", key)
		}
	}
}
