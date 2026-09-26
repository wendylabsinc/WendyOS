package commands

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
)

func TestDevicePickerPurposePersistsAcrossTabsAndResize(t *testing.T) {
	for _, purpose := range []devicePickerPurpose{mainDevicePicker, buildHostPicker} {
		ctx := withDevicePickerPurpose(context.Background(), purpose)
		m := newDevicePickerModel(ctx, tui.NewPicker(), pickerAuth(7), 7, true, devicePickerLocalTab)
		for _, width := range []int{80, 40} {
			updated, _ := m.Update(tea.WindowSizeMsg{Width: width, Height: 24})
			m = updated.(devicePickerModel)
			for _, tab := range deviceTabOrder() {
				m.active = tab
				header := purpose.header(width)
				view := m.View()
				if !strings.HasPrefix(view, header) || strings.Count(view, "Choose the device") != 1 {
					t.Fatalf("purpose=%d tab=%d width=%d lost or duplicated context: %s", purpose, tab, width, view)
				}
				for _, line := range strings.Split(header, "\n") {
					if lipgloss.Width(line) > width {
						t.Fatalf("header overflows width %d: %q", width, line)
					}
				}
			}
		}
	}
}

func TestBuildHostDisplayNamePreservesCloudIdentity(t *testing.T) {
	selector := "cloud://cloud.example:443/org/2/asset/211"
	builder := &grpcclient.AgentConnection{Host: "Spark 48fd (Spark 3)"}
	if got := buildHostDisplayName(selector, builder); got != "Spark 48fd (Spark 3) [asset 211]" {
		t.Fatalf("got %q", got)
	}
	for _, host := range []string{"spark.local:50052", "spark-office"} {
		if got := buildHostDisplayName(host, builder); got != host {
			t.Fatalf("LAN identity changed: %q", got)
		}
	}
	if got := buildHostDisplayName(selector, &grpcclient.AgentConnection{}); got != selector {
		t.Fatalf("lost unnamed cloud identity: %q", got)
	}
}
