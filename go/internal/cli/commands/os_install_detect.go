//go:build darwin || linux || windows

package commands

import (
	"fmt"

	"github.com/wendylabsinc/wendy/go/internal/cli/qdl"
	"github.com/wendylabsinc/wendy/go/internal/cli/tegraflash/rcm"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
)

const (
	detectedEDLValue = "detected_qualcomm_edl"

	espressifVendor     = 0x303a
	esp32S31ROMProduct  = 0x0020 // ROM download mode over USB OTG
	esp32S31TargetValue = "wlite_target_esp32s31"
)

// installUSBDevice contains descriptors only. The initial scan must not claim
// interfaces, reset running boards, or require permission to open a device.
type installUSBDevice struct {
	vendor, product uint16
}

// installUSBTarget identifies only USB IDs specific to a supported recovery
// mode. ESP32 Serial/JTAG (303a:1001), TinyUSB CDC (303a:4002), and UART bridges
// also appear during normal operation, so they cannot prove download mode.
func installUSBTarget(d installUSBDevice) string {
	switch d.vendor {
	case rcm.VendorNVIDIA:
		r := rcm.RecoveryDevice{Product: d.product}
		switch {
		case r.IsThor():
			return thorDeviceType
		case r.IsOrinAGX():
			return orinDeviceType
		case r.IsOrinNano():
			return orinNanoDeviceType
		}
	case uint16(qdl.VendorQualcomm):
		if d.product == uint16(qdl.ProductEDL) {
			return detectedEDLValue
		}
	case espressifVendor:
		if d.product == esp32S31ROMProduct {
			return esp32S31TargetValue
		}
	}
	return ""
}

func edlInstallItems(items []tui.PickerItem) []tui.PickerItem {
	var boards []tui.PickerItem
	for _, item := range items {
		key, _ := item.Value.(string)
		if _, ok := dragonwingBoardFor(key); ok {
			boards = append(boards, item)
		}
	}
	return boards
}

// Recommend install types, not physical targets: the existing flash flow
// rescans and selects the actual device immediately before flashing. Shared
// ROM IDs identify a chip family, never a board model or carrier.
func detectedInstallItems(items []tui.PickerItem, devices []installUSBDevice) []tui.PickerItem {
	counts := make(map[string]int)
	for _, d := range devices {
		if key := installUSBTarget(d); key != "" {
			counts[key]++
		}
	}
	var detected []tui.PickerItem
	for _, item := range items {
		key, _ := item.Value.(string)
		if count := counts[key]; count > 0 {
			item.Section = "USB flashing devices"
			description := fmt.Sprintf("%d connected in USB recovery mode", count)
			if key == esp32S31TargetValue {
				description = fmt.Sprintf("%d connected in ROM download mode; select board model next", count)
			}
			// Keep the version the manual tab shows, such as "(0.20.1)".
			if item.Description != "" {
				description += " " + item.Description
			}
			item.Description = description
			detected = append(detected, item)
		}
	}
	if count := counts[detectedEDLValue]; count > 0 && len(edlInstallItems(items)) > 0 {
		detected = append(detected, tui.PickerItem{
			Name:        "Qualcomm / Dragonwing",
			Description: fmt.Sprintf("%d connected in EDL mode; select board model next", count),
			Section:     "USB flashing devices",
			SortKey:     "0_wendyos_qualcomm",
			Value:       detectedEDLValue,
		})
	}
	return detected
}
