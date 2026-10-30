package commands

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	"github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
)

func TestDiscoverTabsResizeKeepsFrameAndSelectionInsideTerminal(t *testing.T) {
	setTempConfig(t, &config.Config{})
	m := newTestDiscoverTabsModel(0, 0)
	for i := range 40 {
		m.local.upsertLANDevice(models.LANDevice{ID: fmt.Sprintf("device-%02d", i), DisplayName: fmt.Sprintf("device-%02d", i), Hostname: fmt.Sprintf("device-%02d.local", i), IPAddress: fmt.Sprintf("192.168.100.%d", i+1), Port: 50052, IsWendyDevice: true})
	}
	m.local.refreshTable()
	m.local.table.SetCursor(30)
	for _, status := range []bool{false, true} {
		if status {
			m.local.bleWarning = "Bluetooth permission unavailable"
			m.local.err = fmt.Errorf("a discovery source failed")
			m.local.flashMessage = "Copied device"
		}
		for _, size := range []tea.WindowSizeMsg{{Width: 180, Height: 45}, {Width: 80, Height: 24}, {Width: 40, Height: 16}, {Width: 160, Height: 32}, {Width: 60, Height: 18}} {
			updated, _ := m.Update(size)
			m = updated.(discoverTabsModel)
			// Repeated spinner/data redraws after shrinking must obey the same
			// bounds as the first frame, and keep the selected row visible.
			for range 3 {
				m.local.refreshTable()
				view := ansi.Strip(m.View())
				if got := lipgloss.Height(view); got > size.Height {
					t.Fatalf("%dx%d: frame height %d:\n%s", size.Width, size.Height, got, view)
				}
				for _, line := range strings.Split(view, "\n") {
					if got := ansi.StringWidth(line); got > size.Width {
						t.Fatalf("line width %d exceeds %d: %q", got, size.Width, line)
					}
				}
				if !strings.Contains(view, "device-30") {
					t.Fatalf("selected row lost on resize:\n%s", view)
				}
				if !strings.Contains(view, "provisioned") {
					t.Fatalf("legend lost on resize:\n%s", view)
				}
				if status && !strings.Contains(view, "Copied device") {
					t.Fatalf("flash lost on resize:\n%s", view)
				}
			}
		}
	}
}

func TestDiscoverCloudResizeKeepsSelectionAndStatus(t *testing.T) {
	setTempConfig(t, &config.Config{})
	m := newTestDiscoverTabsModel(7, 7)
	m.active = devicePickerCloudTab
	var assets []*cloudpb.Asset
	for i := range 40 {
		assets = append(assets, &cloudpb.Asset{Id: int32(i + 1), Name: fmt.Sprintf("cloud-%02d", i)})
	}
	updated, _ := m.Update(discoverTabsCloudMsg{msg: cloudScanMsg{assets: assets}})
	m = updated.(discoverTabsModel)
	m.cloud.table.SetCursor(30)
	m.cloud.flashMessage = "Copied cloud device"
	m.cloud.err = fmt.Errorf("refresh unavailable")
	for _, size := range []tea.WindowSizeMsg{{Width: 120, Height: 40}, {Width: 60, Height: 18}, {Width: 160, Height: 32}} {
		updated, _ := m.Update(size)
		m = updated.(discoverTabsModel)
		view := ansi.Strip(m.View())
		if lipgloss.Height(view) > size.Height || lipgloss.Width(view) > size.Width {
			t.Fatalf("cloud frame overflows %dx%d:\n%s", size.Width, size.Height, view)
		}
		for _, want := range []string{"cloud-30", "Copied cloud device", "refresh unavailable", "Organization:"} {
			if !strings.Contains(view, want) {
				t.Fatalf("missing %q after resize:\n%s", want, view)
			}
		}
	}
}

func TestDiscoverTabsTinyTerminalBoundsAllTabs(t *testing.T) {
	for _, auth := range []int{0, 7} {
		m := newTestDiscoverTabsModel(auth, 0)
		m.cloudOrg = strings.Repeat("Very long organization name ", 10)
		for _, size := range []tea.WindowSizeMsg{{Width: 8, Height: 3}, {Width: 1, Height: 1}, {Width: 35, Height: 9}} {
			updated, _ := m.Update(size)
			m = updated.(discoverTabsModel)
			for _, tab := range deviceTabOrder() {
				m.active = tab
				view := ansi.Strip(m.View())
				if lipgloss.Height(view) > size.Height || lipgloss.Width(view) > size.Width {
					t.Fatalf("tab %v overflows %dx%d: %q", tab, size.Width, size.Height, view)
				}
			}
		}
	}
}
