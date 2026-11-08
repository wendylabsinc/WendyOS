//go:build darwin || linux || windows

package commands

import (
	"fmt"
	"log"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
)

func newInstallPickerModel(items []tui.PickerItem, detected ...tui.PickerItem) tabbedPickerModel {
	groups := []pickerGroup{
		{label: "Detected USB", items: detected},
		{label: "Linux"},
		{label: "Mac"},
		{label: "Microcontrollers"},
	}
	for _, item := range items {
		value, _ := item.Value.(string)
		group := 1
		switch {
		case value == headlessMacValue:
			group = 2
		case strings.HasPrefix(value, "wlite_"):
			group = 3
		}
		groups[group].items = append(groups[group].items, item)
	}

	return newTabbedPickerModel("Select a device", groups)
}

func pickInstallDevice(items []tui.PickerItem) (string, error) {
	if len(items) == 0 {
		return "", fmt.Errorf("no devices available")
	}
	devices, err := scanInstallUSBDevices()
	if err != nil {
		log.Printf("WARNING: could not scan USB flashing devices: %v", err)
	}
	selected, err := runTabbedPicker(newInstallPickerModel(items, detectedInstallItems(items, devices)...))
	if err != nil {
		return "", err
	}
	if selected == detectedEDLValue {
		return pickFromItems("Select the board connected in EDL mode", edlInstallItems(items))
	}
	return selected, nil
}
