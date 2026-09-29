//go:build darwin || linux || windows

package commands

import (
	"fmt"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
)

func newInstallPickerModel(items []tui.PickerItem) tabbedPickerModel {
	groups := []pickerGroup{
		{label: "Linux"},
		{label: "Mac"},
		{label: "Microcontrollers"},
	}
	for _, item := range items {
		value, _ := item.Value.(string)
		group := 0
		switch {
		case value == headlessMacValue:
			group = 1
		case strings.HasPrefix(value, "wlite_"):
			group = 2
		}
		groups[group].items = append(groups[group].items, item)
	}

	return newTabbedPickerModel("Select a device", groups)
}

func pickInstallDevice(items []tui.PickerItem) (string, error) {
	if len(items) == 0 {
		return "", fmt.Errorf("no devices available")
	}
	return runTabbedPicker(newInstallPickerModel(items))
}
