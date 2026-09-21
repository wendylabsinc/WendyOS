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
	m := newDevicePickerModel(context.Background(), tui.NewPicker(), auth, 7, false)
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
	selector := cloudDeviceSelector{auth7.CloudGRPC, 7, 42}
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
