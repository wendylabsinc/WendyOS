package commands

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
)

func pickerAuth(orgID int) *config.AuthConfig {
	return &config.AuthConfig{
		CloudGRPC:    "cloud.example:443",
		Certificates: []config.CertificateInfo{{OrganizationID: orgID}},
	}
}

func TestDevicePickerShowsLocalAndCloudTabs(t *testing.T) {
	m := newDevicePickerModel(context.Background(), tui.NewPicker(), nil, 0)
	updated, _ := m.Update(devicePickerLocalMsg{msg: tui.PickerAddMsg{Items: []tui.PickerItem{{Name: "local-pi"}}}})
	m = updated.(devicePickerModel)

	view := m.View()
	for _, want := range []string{"Local", "Cloud", "tab switch", "local-pi"} {
		if !strings.Contains(view, want) {
			t.Fatalf("local picker view does not contain %q: %q", want, view)
		}
	}
}

// tabTo presses Tab until the model lands on want, so a test that cares about
// one tab does not encode how many tabs precede it. Returns the cmd from the
// last press, which is what the lazy-start assertions read.
func tabTo(t *testing.T, m devicePickerModel, want devicePickerTab) (devicePickerModel, tea.Cmd) {
	t.Helper()
	var cmd tea.Cmd
	for range len(deviceTabOrder()) {
		if m.active == want {
			return m, cmd
		}
		var updated tea.Model
		updated, cmd = m.Update(tea.KeyMsg{Type: tea.KeyTab})
		m = updated.(devicePickerModel)
	}
	if m.active != want {
		t.Fatalf("never reached tab %v", want)
	}
	return m, cmd
}

func TestDevicePickerLoggedOutCloudTabOffersLoginRow(t *testing.T) {
	m := newDevicePickerModel(context.Background(), tui.NewPicker(), nil, 0)
	m, _ = tabTo(t, m, devicePickerCloudTab)

	view := m.View()
	for _, want := range []string{"Wendy Cloud login", "Not logged in", "enter log in"} {
		if !strings.Contains(view, want) {
			t.Fatalf("logged-out cloud view does not contain %q: %q", want, view)
		}
	}

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(devicePickerModel)
	if cmd == nil {
		t.Fatal("login row did not quit the picker")
	}
	if m.action != devicePickerLogin {
		t.Fatalf("action = %v, want login", m.action)
	}
}

func TestDevicePickerStartsCloudDiscoveryOnFirstCloudTabVisit(t *testing.T) {
	m := newDevicePickerModel(context.Background(), tui.NewPicker(), pickerAuth(7), 7)
	if m.cloudStarted {
		t.Fatal("cloud discovery started before the Cloud tab was visited")
	}

	m, cmd := tabTo(t, m, devicePickerCloudTab)
	if !m.cloudStarted {
		t.Fatal("cloud discovery did not start on the first Cloud tab visit")
	}
	if cmd == nil {
		t.Fatal("first Cloud tab visit did not schedule discovery")
	}

	m, cmd = tabTo(t, m, devicePickerLocalTab)
	if cmd != nil {
		t.Fatal("returning to Local unexpectedly restarted cloud discovery")
	}
	_, cmd = tabTo(t, m, devicePickerCloudTab)
	if cmd != nil {
		t.Fatal("second Cloud tab visit restarted cloud discovery")
	}
}

func TestDevicePickerCloudTabShowsDefaultOrgAndSwitchHotkey(t *testing.T) {
	auth := pickerAuth(7)
	m := newDevicePickerModel(context.Background(), tui.NewPicker(), auth, 7)
	m.active = devicePickerCloudTab
	updated, _ := m.Update(devicePickerOrgMsg{name: "Robotics"})
	m = updated.(devicePickerModel)

	view := m.View()
	for _, want := range []string{"Organization: Robotics (org 7)", "default", "o switch"} {
		if !strings.Contains(view, want) {
			t.Fatalf("authenticated cloud view does not contain %q: %q", want, view)
		}
	}

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'o'}})
	m = updated.(devicePickerModel)
	if cmd == nil {
		t.Fatal("org switch hotkey did not quit the picker")
	}
	if m.action != devicePickerSwitchOrg {
		t.Fatalf("action = %v, want switch org", m.action)
	}
}

func TestDevicePickerSelectsCloudAsset(t *testing.T) {
	auth := pickerAuth(7)
	m := newDevicePickerModel(context.Background(), tui.NewPicker(), auth, 7)
	m.active = devicePickerCloudTab
	asset := &cloudpb.Asset{Id: 42, Name: "cloud-pi"}

	updated, _ := m.Update(devicePickerCloudMsg{msg: cloudScanMsg{assets: []*cloudpb.Asset{asset}}})
	m = updated.(devicePickerModel)
	if !strings.Contains(m.View(), "cloud-pi") {
		t.Fatalf("cloud asset missing from view: %q", m.View())
	}

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(devicePickerModel)
	if cmd == nil {
		t.Fatal("selecting a cloud asset did not quit the picker")
	}
	if got := m.selectedCloud(); got == nil || got.GetId() != 42 {
		t.Fatalf("selected cloud asset = %v, want id 42", got)
	}
}

func TestTagDevicePickerCmdPreservesBatchChildren(t *testing.T) {
	cmd := tagDevicePickerCmd(tea.Batch(
		func() tea.Msg { return "first" },
		func() tea.Msg { return "second" },
	), devicePickerCloudTab)
	batch, ok := cmd().(tea.BatchMsg)
	if !ok {
		t.Fatal("tagged batch returned unexpected message type")
	}
	if len(batch) != 2 {
		t.Fatalf("tagged batch children = %d, want 2", len(batch))
	}
	for i, child := range batch {
		msg, ok := child().(devicePickerCloudMsg)
		if !ok {
			t.Fatalf("child %d returned unexpected message type", i)
		}
		if msg.msg == nil {
			t.Fatalf("child %d lost its payload", i)
		}
	}
}

func TestDevicePickerInitialAuthUsesDefaultOrg(t *testing.T) {
	cfg := &config.Config{
		DefaultOrgID: 9,
		Auth: []config.AuthConfig{
			*pickerAuth(7),
			*pickerAuth(9),
		},
	}
	if got := devicePickerInitialAuth(cfg); cloudAuthOrgID(got) != 9 {
		t.Fatalf("initial org = %d, want default org 9", cloudAuthOrgID(got))
	}
}
