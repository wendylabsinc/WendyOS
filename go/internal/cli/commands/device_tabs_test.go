package commands

import (
	"strings"
	"testing"
)

func TestCycleTabWrapsInBothDirections(t *testing.T) {
	// Explicit rather than deviceTabOrder(): this is the cycling
	// mechanism, not any particular strip.
	tabs := []devicePickerTab{devicePickerLocalTab, devicePickerSimulatorTab, devicePickerCloudTab}
	cases := []struct {
		name   string
		active devicePickerTab
		delta  int
		want   devicePickerTab
	}{
		{"forward from first", devicePickerLocalTab, 1, devicePickerSimulatorTab},
		{"forward from middle", devicePickerSimulatorTab, 1, devicePickerCloudTab},
		{"forward wraps", devicePickerCloudTab, 1, devicePickerLocalTab},
		{"backward wraps", devicePickerLocalTab, -1, devicePickerCloudTab},
		{"backward from middle", devicePickerSimulatorTab, -1, devicePickerLocalTab},
	}
	for _, tc := range cases {
		if got := cycleTab(tabs, tc.active, tc.delta); got != tc.want {
			t.Errorf("%s: cycleTab() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestCycleTabRecoversFromATabNotInTheStrip(t *testing.T) {
	// An active tab absent from the given strip must not leave it rendering
	// with nothing selected.
	twoTab := []devicePickerTab{devicePickerLocalTab, devicePickerCloudTab}
	if got := cycleTab(twoTab, devicePickerSimulatorTab, 1); got != devicePickerLocalTab {
		t.Errorf("cycleTab() = %v, want the first tab", got)
	}
}

func TestTabCycleDeltaReversesOnShiftTab(t *testing.T) {
	if got := tabCycleDelta("tab"); got != 1 {
		t.Errorf("tabCycleDelta(tab) = %d, want 1", got)
	}
	if got := tabCycleDelta("shift+tab"); got != -1 {
		t.Errorf("tabCycleDelta(shift+tab) = %d, want -1", got)
	}
}

func TestDeviceTabsHeaderRendersOnlyTheGivenStrip(t *testing.T) {
	// The guard that keeps `wendy discover` from silently growing a tab.
	run := deviceTabsHeader(devicePickerLocalTab,
		[]devicePickerTab{devicePickerLocalTab, devicePickerSimulatorTab, devicePickerCloudTab}, 0)
	for _, want := range []string{"Local", "Simulator", "Cloud", "tab switch"} {
		if !strings.Contains(run, want) {
			t.Errorf("run picker header = %q, want it to contain %q", run, want)
		}
	}

	// A strip that was not given the tab must not render it.
	twoTab := deviceTabsHeader(devicePickerLocalTab,
		[]devicePickerTab{devicePickerLocalTab, devicePickerCloudTab}, 0)
	if strings.Contains(twoTab, "Simulator") {
		t.Errorf("two-tab header = %q, want no Simulator tab", twoTab)
	}
	discover := deviceTabsHeader(devicePickerLocalTab, deviceTabOrder(), 0)
	for _, want := range []string{"Local", "Simulator", "Cloud"} {
		if !strings.Contains(discover, want) {
			t.Errorf("discover header = %q, want it to contain %q", discover, want)
		}
	}
}
