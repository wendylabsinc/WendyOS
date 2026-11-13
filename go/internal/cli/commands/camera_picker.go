package commands

import (
	"errors"
	"fmt"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// cameraPicker runs an interactive picker over the candidates and returns the
// chosen device ID. It mirrors listenDevicePicker in audio_devices.go.
type cameraPicker func(candidates []*agentpb.VideoDevice) (uint32, error)

// errNoCameras is returned when the device reports no cameras at all.
var errNoCameras = errors.New("no cameras found on the device; check `wendy device camera list`")

// resolveCameraID decides which camera to stream.
//
// An explicit --id always wins. A single camera is selected without prompting.
// Several cameras prompt, which is a change for USB and CSI as well: the command
// previously defaulted to ID 0 and gave no sign that other cameras existed.
func resolveCameraID(devices []*agentpb.VideoDevice, explicit uint32, explicitSet bool, pick cameraPicker) (uint32, error) {
	if explicitSet {
		return explicit, nil
	}
	switch len(devices) {
	case 0:
		return 0, errNoCameras
	case 1:
		return devices[0].GetId(), nil
	default:
		if pick == nil {
			return 0, errors.New("multiple cameras found; pass --id to choose one")
		}
		return pick(devices)
	}
}

// cameraPickerItems builds the picker rows. PickerItem.Value carries the device
// so the column closures can read every field without a side table.
func cameraPickerItems(candidates []*agentpb.VideoDevice) []tui.PickerItem {
	items := make([]tui.PickerItem, 0, len(candidates))
	for _, d := range candidates {
		items = append(items, tui.PickerItem{
			Name:        d.GetName(),
			Description: cameraWhere(d),
			DedupKey:    fmt.Sprintf("%d", d.GetId()),
			Value:       d,
		})
	}
	return items
}

// cameraWhere is the location column: an address for network cameras and a
// device node for local ones, so the two are told apart at a glance.
func cameraWhere(d *agentpb.VideoDevice) string {
	if d.GetTransport() == agentpb.VideoTransport_VIDEO_TRANSPORT_IP {
		return d.GetAddress()
	}
	if d.GetTransport() == agentpb.VideoTransport_VIDEO_TRANSPORT_ROS2 {
		return d.GetTopic()
	}
	return d.GetPath()
}

// cameraStatus flags the two states that stop a stream before it starts. Local
// cameras have neither, so they are always ready.
func cameraStatus(d *agentpb.VideoDevice) string {
	if d.GetTransport() != agentpb.VideoTransport_VIDEO_TRANSPORT_IP {
		return "ready"
	}
	switch {
	case !d.GetHasCredentials():
		return "needs login"
	case !d.GetOnline():
		return "offline"
	default:
		return "ready"
	}
}

func cameraPickerColumns() []tui.PickerColumn {
	devOf := func(it tui.PickerItem) *agentpb.VideoDevice {
		d, _ := it.Value.(*agentpb.VideoDevice)
		return d
	}
	return []tui.PickerColumn{
		{Title: "ID", MinWidth: 5, Required: true, Value: func(it tui.PickerItem) string {
			if d := devOf(it); d != nil {
				return fmt.Sprintf("%d", d.GetId())
			}
			return ""
		}},
		{Title: "Type", MinWidth: 5, Required: true, Value: func(it tui.PickerItem) string {
			if d := devOf(it); d != nil {
				return transportLabel(d.GetTransport())
			}
			return ""
		}},
		{Title: "Name", MinWidth: 12, Required: true, Value: func(it tui.PickerItem) string {
			return it.Name
		}},
		{Title: "Where", MinWidth: 14, Required: true, Value: func(it tui.PickerItem) string {
			if d := devOf(it); d != nil {
				return cameraWhere(d)
			}
			return ""
		}},
		{Title: "Status", MinWidth: 11, Value: func(it tui.PickerItem) string {
			if d := devOf(it); d != nil {
				return cameraStatus(d)
			}
			return ""
		}},
	}
}

// pickCamera presents the interactive camera picker and returns the chosen
// device ID. It returns ErrUserCancelled if the user quits.
func pickCamera(candidates []*agentpb.VideoDevice) (uint32, error) {
	picker := tui.NewPickerWithTitleAndColumns("Select a camera", cameraPickerColumns())
	p := tea.NewProgram(picker)

	go func() {
		p.Send(tui.PickerAddMsg{Items: cameraPickerItems(candidates)})
		p.Send(tui.PickerDoneMsg{})
	}()

	finalModel, err := tui.RunProgram(p)
	if err != nil {
		return 0, fmt.Errorf("camera picker: %w", err)
	}
	pm, ok := finalModel.(tui.PickerModel)
	if !ok {
		return 0, fmt.Errorf("camera picker: unexpected model type")
	}
	if pm.Cancelled() {
		return 0, ErrUserCancelled
	}
	sel := pm.Selected()
	if sel == nil {
		return 0, ErrUserCancelled
	}
	d, ok := sel.Value.(*agentpb.VideoDevice)
	if !ok || d == nil {
		return 0, fmt.Errorf("camera picker: invalid selection")
	}
	return d.GetId(), nil
}
