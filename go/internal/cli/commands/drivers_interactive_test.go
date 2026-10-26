package commands

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

type driverScreenClient struct {
	fakeDriverClient
	removals []*agentpbv2.RemoveDriverRequest
}

func (f *driverScreenClient) RemoveDriver(_ context.Context, req *agentpbv2.RemoveDriverRequest, _ ...grpc.CallOption) (agentpbv2.WendyDriverService_RemoveDriverClient, error) {
	f.removals = append(f.removals, req)
	return &fakeInstallStream{}, nil
}

func preserveDriverScreenHooks(t *testing.T) {
	t.Helper()
	pick, action, confirm := pickDriverFn, pickDriverActionFn, confirmDriverRemovalFn
	t.Cleanup(func() {
		pickDriverFn, pickDriverActionFn, confirmDriverRemovalFn = pick, action, confirm
	})
}

func TestDriverPickerItemsIncludeInstalledAndCompatiblePublished(t *testing.T) {
	dl := &agentpbv2.ListDriversResponse{
		KernelVersion: "6.19.0",
		Installed: []*agentpbv2.InstalledDriver{
			{Name: "loaded", KernelVersion: "6.19.0", Loaded: true},
			{Name: "stale", KernelVersion: "6.18.0"},
			{Name: "unreadable", Unreadable: true},
			{Name: "local-only", KernelVersion: "6.19.0", ModulesLoad: []string{"local_module"}},
			{Name: "firmware"},
			{Name: "staged-copy", KernelVersion: "6.20.0"},
			{Name: "staged-copy", KernelVersion: "6.19.0", Loaded: true},
			{Name: "loaded", KernelVersion: "6.20.0"},
		},
	}
	exts := []extensionEntry{
		{Name: "available", Version: "0.20.0-nightly", KernelVersion: "6.19.0", ModulesLoad: []string{"one", "two"}},
		{Name: "stale", Version: "0.20.0", KernelVersion: "6.19.0"},
		{Name: "wrong-kernel", KernelVersion: "6.20.0"},
		{Name: "missing-kernel"},
	}
	items := driverPickerItems(dl, exts)
	wantStatus := map[string]string{
		"available": "available", "loaded": "loaded", "stale": "stale", "unreadable": "stale",
		"local-only": "installed", "firmware": "installed", "staged-copy": "loaded",
	}
	if len(items) != len(wantStatus) {
		t.Fatalf("items = %+v, want %d installed or compatible drivers", items, len(wantStatus))
	}
	var names []string
	for _, item := range items {
		names = append(names, item.Name)
		if got, ok := wantStatus[item.Name]; !ok || item.Type != got {
			t.Errorf("driver %q status = %q, want %q", item.Name, item.Type, got)
		}
		if item.Value != item.Name {
			t.Errorf("driver %q selection value = %v", item.Name, item.Value)
		}
		switch item.Name {
		case "available":
			if item.AgentVersion != "0.20.0-nightly" || item.Parameters != "one, two" {
				t.Errorf("published driver metadata = %+v", item)
			}
		case "local-only":
			if item.Parameters != "local_module" {
				t.Errorf("installed-only modules = %q", item.Parameters)
			}
		}
	}
	wantNames := []string{"available", "firmware", "loaded", "local-only", "staged-copy", "stale", "unreadable"}
	if !reflect.DeepEqual(names, wantNames) {
		t.Errorf("driver ordering = %v, want %v", names, wantNames)
	}
}

// An installed row describes what the device runs; showing the published build's
// version there would read as already installed and hide that Reinstall upgrades.
func TestDriverPickerItemsShowInstalledVersion(t *testing.T) {
	dl := &agentpbv2.ListDriversResponse{
		KernelVersion: "6.19.0",
		Installed: []*agentpbv2.InstalledDriver{
			{Name: "acme-nic", Version: "0.17.0", KernelVersion: "6.19.0", ModulesLoad: []string{"nic_v17"}, Loaded: true},
			{Name: "local-only", Version: "1.2.3", KernelVersion: "6.19.0"},
		},
	}
	exts := []extensionEntry{{Name: "acme-nic", Version: "0.18.0-nightly.2", KernelVersion: "6.19.0", ModulesLoad: []string{"nic_nightly"}}}
	items := driverPickerItems(dl, exts)
	if len(items) != 2 {
		t.Fatalf("items = %+v, want acme-nic and local-only", items)
	}
	upgradable, local := items[0], items[1]
	if upgradable.Type != "loaded" || upgradable.AgentVersion != "0.17.0" || upgradable.Parameters != "nic_v17" {
		t.Errorf("installed row = status %q, version %q, modules %q; want loaded, 0.17.0, nic_v17", upgradable.Type, upgradable.AgentVersion, upgradable.Parameters)
	}
	if !strings.Contains(upgradable.Hint, "0.18.0-nightly.2") {
		t.Errorf("installed row hint = %q, want the published version Reinstall would apply", upgradable.Hint)
	}
	if local.AgentVersion != "1.2.3" {
		t.Errorf("installed-only row version = %q, want 1.2.3", local.AgentVersion)
	}
}

func TestDriversScreenInstallPreservesPublishedSpec(t *testing.T) {
	for _, tc := range []struct {
		name      string
		installed []*agentpbv2.InstalledDriver
		label     string
	}{
		{name: "install", label: "Install"},
		{name: "reinstall", installed: []*agentpbv2.InstalledDriver{{Name: "acme-nic", KernelVersion: "6.19.0"}}, label: "Reinstall"},
		{name: "replace stale", installed: []*agentpbv2.InstalledDriver{{Name: "acme-nic", KernelVersion: "6.18.0"}}, label: "Reinstall"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			preserveDriverScreenHooks(t)
			pickDriverFn = func(title string, items []tui.PickerItem) (string, error) {
				if !strings.Contains(title, "0.20.0") || !strings.Contains(title, "6.19.0") {
					t.Errorf("picker title = %q, want OS and kernel", title)
				}
				if len(items) != 1 || items[0].Name != "acme-nic" {
					t.Fatalf("picker items = %+v", items)
				}
				return "acme-nic", nil
			}
			pickDriverActionFn = func(_ string, items []tui.PickerItem) (string, error) {
				want := []string{tc.label, "Cancel"}
				if len(tc.installed) > 0 {
					want = []string{tc.label, "Remove", "Cancel"}
				}
				var got []string
				for _, item := range items {
					got = append(got, item.Name)
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("actions = %v, want %v", got, want)
				}
				return "install", nil
			}
			confirmDriverRemovalFn = func(string, ...tea.ProgramOption) (bool, error) {
				t.Fatal("install asked to confirm removal")
				return false, nil
			}
			var installs []*fakeInstallStream
			client := &driverScreenClient{fakeDriverClient: fakeDriverClient{installs: &installs}}
			dl := &agentpbv2.ListDriversResponse{BaseVersion: "0.20.0", KernelVersion: "6.19.0", Installed: tc.installed}
			ext := extensionEntry{Name: "acme-nic", Version: "0.20.0-nightly", KernelVersion: "6.19.0", Path: "images/nightly/acme-nic.raw", SHA256: "published-digest", Signature: "AQID", ModulesLoad: []string{"nic", "helper"}}
			if err := runDriversScreen(context.Background(), client, dl, []extensionEntry{ext}, driverManifestOptions{}); err != nil {
				t.Fatal(err)
			}
			if len(installs) != 1 || len(installs[0].specs) != 1 || len(client.removals) != 0 {
				t.Fatalf("installs = %+v, removals = %+v; want one install", installs, client.removals)
			}
			want := &agentpbv2.DriverSpec{Name: "acme-nic", Version: "0.20.0-nightly", KernelVersion: "6.19.0", Sha256: "published-digest", Signature: []byte{1, 2, 3}, ArtifactUrl: gcsBaseURL + "/images/nightly/acme-nic.raw", ModulesLoad: []string{"nic", "helper"}}
			if !proto.Equal(installs[0].specs[0], want) {
				t.Errorf("install spec = %v, want %v", installs[0].specs[0], want)
			}
		})
	}
}

func TestDriversScreenRemoveAndCancellation(t *testing.T) {
	for _, tc := range []struct {
		name       string
		pickErr    error
		action     string
		actionErr  error
		confirmed  bool
		confirmErr error
		wantErr    error
		wantRemove bool
		wantPrompt bool
	}{
		{name: "remove installed-only", action: "remove", confirmed: true, wantRemove: true, wantPrompt: true},
		{name: "decline removal", action: "remove", wantPrompt: true},
		{name: "cancel confirmation", action: "remove", confirmErr: tui.ErrCancelled, wantErr: ErrUserCancelled, wantPrompt: true},
		{name: "cancel driver picker", pickErr: ErrUserCancelled, wantErr: ErrUserCancelled},
		{name: "cancel action picker", actionErr: ErrUserCancelled, wantErr: ErrUserCancelled},
		{name: "choose cancel", action: "cancel"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			preserveDriverScreenHooks(t)
			pickDriverFn = func(string, []tui.PickerItem) (string, error) { return "local-only", tc.pickErr }
			pickDriverActionFn = func(_ string, items []tui.PickerItem) (string, error) {
				if tc.pickErr != nil {
					t.Fatal("action picker opened after driver picker cancelled")
				}
				var values []string
				for _, item := range items {
					values = append(values, item.Value.(string))
				}
				if !reflect.DeepEqual(values, []string{"remove", "cancel"}) {
					t.Errorf("installed-only actions = %v, want remove and cancel", values)
				}
				return tc.action, tc.actionErr
			}
			prompted := false
			confirmDriverRemovalFn = func(prompt string, _ ...tea.ProgramOption) (bool, error) {
				prompted = true
				if !strings.Contains(prompt, "local-only") {
					t.Errorf("confirmation does not name selected driver: %q", prompt)
				}
				return tc.confirmed, tc.confirmErr
			}
			var installs []*fakeInstallStream
			client := &driverScreenClient{fakeDriverClient: fakeDriverClient{installs: &installs}}
			dl := &agentpbv2.ListDriversResponse{KernelVersion: "6.19.0", Installed: []*agentpbv2.InstalledDriver{{Name: "local-only", Unreadable: true}}}
			err := runDriversScreen(context.Background(), client, dl, nil, driverManifestOptions{})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("runDriversScreen error = %v, want %v", err, tc.wantErr)
			}
			if prompted != tc.wantPrompt {
				t.Errorf("removal prompted = %v, want %v", prompted, tc.wantPrompt)
			}
			if len(installs) != 0 || (len(client.removals) == 1) != tc.wantRemove || len(client.removals) > 1 {
				t.Fatalf("installs = %+v, removals = %+v, want removal = %v", installs, client.removals, tc.wantRemove)
			}
			if tc.wantRemove && client.removals[0].GetName() != "local-only" {
				t.Errorf("removed driver = %q, want local-only", client.removals[0].GetName())
			}
		})
	}
}

func TestDriversScreenEmptySkipsPicker(t *testing.T) {
	preserveDriverScreenHooks(t)
	pickDriverFn = func(string, []tui.PickerItem) (string, error) {
		t.Fatal("opened picker with no installed or compatible published drivers")
		return "", nil
	}
	var installs []*fakeInstallStream
	client := &driverScreenClient{fakeDriverClient: fakeDriverClient{installs: &installs}}
	dl := &agentpbv2.ListDriversResponse{KernelVersion: "6.19.0"}
	if err := runDriversScreen(context.Background(), client, dl, []extensionEntry{{Name: "wrong-kernel", KernelVersion: "6.20.0"}}, driverManifestOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(installs) != 0 || len(client.removals) != 0 {
		t.Fatal("empty driver list mutated device")
	}
}

func TestDriversCommandStaticWithoutTerminalOrWithJSON(t *testing.T) {
	for _, tc := range []struct {
		name        string
		interactive bool
		json        bool
	}{
		{name: "no terminal"},
		{name: "JSON terminal", interactive: true, json: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			preserveDriverScreenHooks(t)
			oldResolve, oldTerminal, oldJSON := resolveDriversTargetFn, isInteractiveTerminalFn, jsonOutput
			t.Cleanup(func() {
				resolveDriversTargetFn, isInteractiveTerminalFn, jsonOutput = oldResolve, oldTerminal, oldJSON
			})
			jsonOutput = tc.json
			isInteractiveTerminalFn = func() bool { return tc.interactive }
			pickDriverFn = func(string, []tui.PickerItem) (string, error) {
				t.Fatal("static command opened a driver picker")
				return "", nil
			}
			resolved := 0
			resolveDriversTargetFn = func(context.Context, ...resolveOption) (*SelectedDevice, error) {
				resolved++
				return &SelectedDevice{Agent: &grpcclient.AgentConnection{DriverService: fakeDriverClient{
					list: &agentpbv2.ListDriversResponse{BaseVersion: "0.20.0", KernelVersion: "6.19.0", Installed: []*agentpbv2.InstalledDriver{{Name: "local-only", KernelVersion: "6.19.0"}}},
				}}}, nil
			}
			var runErr error
			output := captureStdout(t, func() {
				cmd := newDriversCmd()
				cmd.SetArgs([]string{})
				runErr = cmd.Execute()
			})
			if runErr != nil {
				t.Fatal(runErr)
			}
			if resolved != 1 || !strings.Contains(output, "local-only") {
				t.Fatalf("resolved = %d, output = %q; want installed driver list", resolved, output)
			}
			if tc.json {
				var doc map[string]any
				if err := json.Unmarshal([]byte(output), &doc); err != nil || doc["baseVersion"] != "0.20.0" {
					t.Errorf("JSON output = %q, parse error = %v", output, err)
				}
			}
		})
	}
}

func TestDriversInteractiveCatalogFailure(t *testing.T) {
	for _, installed := range []bool{true, false} {
		name := "installed driver remains removable"
		if !installed {
			name = "empty device returns catalog failure"
		}
		t.Run(name, func(t *testing.T) {
			preserveDriverScreenHooks(t)
			oldResolve, oldCatalog := resolveDriversTargetFn, driverExtensionsForChannelFn
			t.Cleanup(func() {
				resolveDriversTargetFn, driverExtensionsForChannelFn = oldResolve, oldCatalog
			})
			dl := &agentpbv2.ListDriversResponse{BaseVersion: "0.20.0", KernelVersion: "6.19.0"}
			if installed {
				dl.Installed = []*agentpbv2.InstalledDriver{{Name: "local-only", KernelVersion: "6.18.0"}}
			}
			var installs []*fakeInstallStream
			client := &driverScreenClient{fakeDriverClient: fakeDriverClient{list: dl, installs: &installs}}
			deviceType := "raspberry-pi-5"
			resolveDriversTargetFn = func(context.Context, ...resolveOption) (*SelectedDevice, error) {
				return &SelectedDevice{Agent: &grpcclient.AgentConnection{
					AgentService:  &fakeAgentVersionClient{resp: &agentpb.GetAgentVersionResponse{DeviceType: &deviceType}},
					DriverService: client,
				}}, nil
			}
			catalogErr := errors.New("registry unavailable")
			driverExtensionsForChannelFn = func(_ context.Context, device, version string, nightly bool, pr int) ([]extensionEntry, error) {
				if device != deviceType || version != "0.20.0" || !nightly || pr != 0 {
					t.Errorf("catalog coordinates = %q, %q, nightly=%v, pr=%d", device, version, nightly, pr)
				}
				return nil, catalogErr
			}
			picked := false
			pickDriverFn = func(_ string, items []tui.PickerItem) (string, error) {
				picked = true
				if !installed || len(items) != 1 || items[0].Name != "local-only" || items[0].Type != "stale" {
					t.Fatalf("catalog failure picker items = %+v, installed = %v", items, installed)
				}
				return "local-only", nil
			}
			pickDriverActionFn = func(_ string, items []tui.PickerItem) (string, error) {
				if len(items) != 2 || items[0].Value != "remove" || items[1].Value != "cancel" {
					t.Fatalf("catalog failure actions = %+v, want remove and cancel", items)
				}
				return "remove", nil
			}
			confirmDriverRemovalFn = func(string, ...tea.ProgramOption) (bool, error) { return true, nil }
			err := runDriversInteractive(context.Background(), driverManifestOptions{nightly: true})
			if installed {
				if err != nil || !picked || len(client.removals) != 1 || client.removals[0].GetName() != "local-only" {
					t.Fatalf("fallback err = %v, picked = %v, removals = %+v", err, picked, client.removals)
				}
			} else if !errors.Is(err, catalogErr) || picked || len(client.removals) != 0 {
				t.Fatalf("empty-device err = %v, picked = %v, removals = %+v; want original catalog failure", err, picked, client.removals)
			}
			if len(installs) != 0 {
				t.Fatal("catalog failure installed a driver")
			}
		})
	}
}

// A nightly or PR catalog offers builds the device's OS never shipped, so the
// picker must say where its Install versions come from.
func TestDriversInteractiveTitleNamesCatalog(t *testing.T) {
	for _, tc := range []struct {
		source driverManifestOptions
		want   string
	}{
		{source: driverManifestOptions{}, want: "OS 0.20.0"},
		{source: driverManifestOptions{nightly: true}, want: "latest nightly"},
		{source: driverManifestOptions{pr: 123}, want: "PR 123"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			preserveDriverScreenHooks(t)
			oldResolve, oldCatalog := resolveDriversTargetFn, driverExtensionsForChannelFn
			t.Cleanup(func() {
				resolveDriversTargetFn, driverExtensionsForChannelFn = oldResolve, oldCatalog
			})
			deviceType := "raspberry-pi-5"
			resolveDriversTargetFn = func(context.Context, ...resolveOption) (*SelectedDevice, error) {
				return &SelectedDevice{Agent: &grpcclient.AgentConnection{
					AgentService:  &fakeAgentVersionClient{resp: &agentpb.GetAgentVersionResponse{DeviceType: &deviceType}},
					DriverService: fakeDriverClient{list: &agentpbv2.ListDriversResponse{BaseVersion: "0.20.0", KernelVersion: "6.19.0"}},
				}}, nil
			}
			driverExtensionsForChannelFn = func(context.Context, string, string, bool, int) ([]extensionEntry, error) {
				return []extensionEntry{{Name: "acme-nic", Version: "0.21.0-nightly.4", KernelVersion: "6.19.0"}}, nil
			}
			var title string
			pickDriverFn = func(got string, _ []tui.PickerItem) (string, error) {
				title = got
				return "", ErrUserCancelled
			}
			if err := runDriversInteractive(context.Background(), tc.source); !errors.Is(err, ErrUserCancelled) {
				t.Fatalf("runDriversInteractive error = %v, want the picker's cancellation", err)
			}
			if !strings.Contains(title, tc.want) || !strings.Contains(title, "6.19.0") {
				t.Errorf("picker title = %q, want catalog %q and kernel 6.19.0", title, tc.want)
			}
		})
	}
}
