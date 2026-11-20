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

func (p devicePickerPurpose) banner() (title, description string, color lipgloss.Color, ok bool) {
	switch p {
	case mainDevicePicker:
		return "MAIN DEVICE  --device", "Choose the device that runs your app or simulation.", tui.ColorPrimary, true
	case hilInferencePicker:
		return "HIL INFERENCE  --hil", "Choose the device that runs inference for the simulator.", tui.ColorInfo, true
	case buildHostPicker:
		return "BUILD HOST  --build-host", "Choose the device that builds images before they are deployed.", tui.ColorNotice, true
	}
	return "", "", "", false
}

func (p devicePickerPurpose) header(width int) string {
	title, description, color, ok := p.banner()
	if !ok {
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

// devicePickerMinBodyRows is what a picker needs under the banner for its
// title, key hints, column header and a few device rows.
const devicePickerMinBodyRows = 8

// fittedHeader is header collapsed to its one-line title when the full panel
// would leave fewer than devicePickerMinBodyRows of a height-row terminal for
// the picker. belowRows are rows the caller adds under the banner, such as
// tabs. A height of 0 (size not known yet) keeps the full panel.
func (p devicePickerPurpose) fittedHeader(width, height, belowRows int) string {
	full := p.header(width)
	if full == "" || height <= 0 || height-strings.Count(full, "\n")-belowRows >= devicePickerMinBodyRows {
		return full
	}
	title, _, color, _ := p.banner()
	return tui.CropANSIView(lipgloss.NewStyle().Bold(true).Foreground(color).Render(title), 0, width) + "\n"
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
