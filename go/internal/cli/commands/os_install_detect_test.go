//go:build darwin || linux || windows

package commands

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/wendylabsinc/wendy/go/internal/cli/tegraflash/rcm"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
)

func TestInstallUSBTarget(t *testing.T) {
	for _, tt := range []struct {
		name            string
		vendor, product uint16
		want            string
	}{
		{"Thor", rcm.VendorNVIDIA, rcm.ProductThor, thorDeviceType},
		{"AGX 32", rcm.VendorNVIDIA, rcm.ProductOrinAGX32, orinDeviceType},
		{"AGX 64", rcm.VendorNVIDIA, rcm.ProductOrinAGX64, orinDeviceType},
		{"Nano 4", rcm.VendorNVIDIA, rcm.ProductOrinNano4, orinNanoDeviceType},
		{"Nano 8", rcm.VendorNVIDIA, rcm.ProductOrinNano8, orinNanoDeviceType},
		{"unsupported NX", rcm.VendorNVIDIA, rcm.ProductOrinNX16, ""},
		{"Jetson flashing gadget", rcm.VendorNVIDIA, 0x7100, ""},
		{"wrong vendor", 0x1234, rcm.ProductThor, ""},
		{"Qualcomm EDL", 0x05c6, 0x9008, detectedEDLValue},
		{"Qualcomm runtime", 0x05c6, 0x900e, ""},
		{"S31 ROM", 0x303a, 0x0020, "wlite_target_esp32s31"},
		{"Mosaico runtime", 0x303a, 0x4002, ""},
		{"shared ESP32 JTAG ID", 0x303a, 0x1001, ""},
		{"UART bridge", 0x10c4, 0xea60, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := installUSBTarget(installUSBDevice{tt.vendor, tt.product}); got != tt.want {
				t.Fatalf("target = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDetectedInstallItemsRequireAvailableFirmware(t *testing.T) {
	devices := []installUSBDevice{{rcm.VendorNVIDIA, rcm.ProductThor}, {0x303a, 0x0020}, {0x05c6, 0x9008}}
	// A PR manifest with only Pi images must not offer release firmware or
	// recovery installers absent from that manifest.
	items := installPickerTestItems()[:1]
	if got := detectedInstallItems(items, devices); len(got) != 0 {
		t.Fatalf("offered unavailable firmware: %+v", got)
	}
	items = append(items, tui.PickerItem{Name: "Thor", Description: "(0.20.1)", Value: thorDeviceType})
	got := detectedInstallItems(items, devices)
	if len(got) != 1 || got[0].Value != thorDeviceType {
		t.Fatalf("detected = %+v", got)
	}
	// The detected tab must still say which version will be installed.
	if want := "1 connected in USB recovery mode (0.20.1)"; got[0].Description != want {
		t.Fatalf("description = %q, want %q", got[0].Description, want)
	}
}

func TestDetectedInstallPickerOffersConnectedFamiliesFirst(t *testing.T) {
	items := append(installPickerTestItems(),
		tui.PickerItem{Name: "Jetson AGX Orin", Value: orinDeviceType},
		tui.PickerItem{Name: "ESP32-S31", Value: "wlite_target_esp32s31"},
		tui.PickerItem{Name: "Dragonwing IQ-8275", Value: "dragonwing-iq-8275"},
		tui.PickerItem{Name: "Dragonwing IQ-9075", Value: "dragonwing-iq-9075"},
	)
	detected := detectedInstallItems(items, []installUSBDevice{
		{rcm.VendorNVIDIA, rcm.ProductOrinAGX32},
		{rcm.VendorNVIDIA, rcm.ProductOrinAGX64},
		{0x303a, 0x0020},
		{0x05c6, 0x9008},
	})
	if len(detected) != 3 || !strings.Contains(detected[0].Description, "2 connected") {
		t.Fatalf("expected one recommendation per family: %+v", detected)
	}
	if boards := edlInstallItems(items); len(boards) != 2 {
		t.Fatalf("EDL choices = %+v", boards)
	}
	m := newInstallPickerModel(items, detected...)
	if len(m.tabs) != 4 || m.tabs[m.active].label != "Detected USB" {
		t.Fatalf("detected tab is not first: %+v", m.tabs)
	}
	view := m.View()
	for _, name := range []string{"Jetson AGX Orin", "ESP32-S31", "Qualcomm / Dragonwing"} {
		if !strings.Contains(view, name) {
			t.Errorf("missing %q in detected tab: %s", name, view)
		}
	}
	if strings.Contains(view, "Raspberry Pi 5") || strings.Contains(view, "Dragonwing IQ-8275") {
		t.Fatalf("detected tab includes an unconfirmed board model: %s", view)
	}
	m = updateInstallPicker(m, tea.KeyMsg{Type: tea.KeyTab})
	if m.tabs[m.active].label != "Linux" || !strings.Contains(m.View(), "Raspberry Pi 5") {
		t.Fatal("manual choices unavailable after detected tab")
	}
	// Selecting the S31 recommendation still enters the existing board-model
	// picker, which includes Mosaico, rather than guessing a firmware variant.
	m = newInstallPickerModel(items, detected[1])
	m = updateInstallPicker(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.selected == nil || m.selected.Value != "wlite_target_esp32s31" {
		t.Fatalf("S31 selection = %+v", m.selected)
	}
}

func TestDetectedInstallPickerEmptyScanKeepsManualTabs(t *testing.T) {
	items := installPickerTestItems()
	m := newInstallPickerModel(items, detectedInstallItems(items, nil)...)
	if len(m.tabs) != 3 || m.tabs[m.active].label != "Linux" {
		t.Fatalf("empty scan changed manual picker: %+v", m.tabs)
	}
}
