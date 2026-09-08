package commands

import (
	"slices"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
)

type devicePickerTab int

const (
	devicePickerLocalTab devicePickerTab = iota
	devicePickerSimulatorTab
	devicePickerCloudTab
)

type devicePickerAction int

const (
	devicePickerNoAction devicePickerAction = iota
	devicePickerLogin
	devicePickerSwitchOrg

	// devicePickerCreateVM quits the view so the image download can own the
	// terminal; the caller creates the VM and re-enters.
	devicePickerCreateVM
)

// deviceTabOrder is the strip both device views show, left to right. One
// source for the cycle and the header, so a tab can never be reachable by Tab
// yet missing from the strip, or the reverse.
func deviceTabOrder() []devicePickerTab {
	return []devicePickerTab{devicePickerLocalTab, devicePickerSimulatorTab, devicePickerCloudTab}
}

func deviceTabLabel(t devicePickerTab) string {
	switch t {
	case devicePickerSimulatorTab:
		return "Simulator"
	case devicePickerCloudTab:
		return "Cloud"
	default:
		return "Local"
	}
}

// cycleTab moves active by delta positions through tabs, wrapping at both ends.
// delta is -1 for shift+tab. An active tab that is not in tabs yields the first,
// so the strip can never render with nothing selected.
func cycleTab(tabs []devicePickerTab, active devicePickerTab, delta int) devicePickerTab {
	if len(tabs) == 0 {
		return active
	}
	idx := slices.Index(tabs, active)
	if idx < 0 {
		return tabs[0]
	}
	n := len(tabs)
	return tabs[((idx+delta)%n+n)%n]
}

// tabCycleDelta maps a key to a direction. shift+tab used to be a silent alias
// for tab, which was indistinguishable while there were only two tabs.
func tabCycleDelta(key string) int {
	if key == "shift+tab" {
		return -1
	}
	return 1
}

func deviceTabsHeader(active devicePickerTab, tabs []devicePickerTab, width int) string {
	header := ""
	for i, t := range tabs {
		if i > 0 {
			header += devicePickerTabInactive.Render(" | ")
		}
		if t == active {
			header += devicePickerTabActive.Render(deviceTabLabel(t))
		} else {
			header += devicePickerTabInactive.Render(deviceTabLabel(t))
		}
	}
	header += devicePickerTabInactive.Render("  (tab switch)")
	if width > 0 {
		header = tui.CropANSIView(header, 0, width)
	}
	return header
}
