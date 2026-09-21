package commands

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
)

type devicePickerPurpose int

const (
	unspecifiedDevicePicker devicePickerPurpose = iota
	mainDevicePicker
	hilInferencePicker
	buildHostPicker
)

type devicePickerPurposeKey struct{}

func withDevicePickerPurpose(ctx context.Context, purpose devicePickerPurpose) context.Context {
	return context.WithValue(ctx, devicePickerPurposeKey{}, purpose)
}

func devicePickerPurposeFromContext(ctx context.Context) devicePickerPurpose {
	purpose, _ := ctx.Value(devicePickerPurposeKey{}).(devicePickerPurpose)
	return purpose
}

func (p devicePickerPurpose) header(width int) string {
	var title, description string
	var color lipgloss.Color
	switch p {
	case mainDevicePicker:
		title, color = "MAIN DEVICE  --device", tui.ColorPrimary
		description = "Choose the device that runs your app or simulation."
	case hilInferencePicker:
		title, color = "HIL INFERENCE  --hil", tui.ColorInfo
		description = "Choose the device that runs inference for the simulator."
	case buildHostPicker:
		title, color = "BUILD HOST  --build-host", tui.ColorNotice
		description = "Choose the device that builds images before they are deployed."
	default:
		return ""
	}
	if width <= 0 {
		width = 80
	}
	panel := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(color).
		Padding(0, 1).Width(max(1, width-2)).MaxWidth(width)
	heading := lipgloss.NewStyle().Bold(true).Foreground(color).Render(title)
	return panel.Render(heading+"\n"+description) + "\n\n"
}

// Keep the connection selector for dialing, and use the cloud roster's name
// for progress and errors once connected. Asset IDs distinguish duplicate names.
func buildHostDisplayName(selector string, builder *grpcclient.AgentConnection) string {
	cloud, matched, err := parseCloudDeviceSelector(selector)
	if !matched || err != nil || builder == nil {
		return selector
	}
	name := strings.TrimSpace(tui.StripControl(builder.Host))
	if name == "" {
		return selector
	}
	return fmt.Sprintf("%s [asset %d]", name, cloud.AssetID)
}
