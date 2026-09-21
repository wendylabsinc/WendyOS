package commands

import (
	"context"
	"fmt"
	"strings"

	bubbleTable "github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

const audioVolumeStep uint32 = 5

type audioAction int

const (
	audioActionNone audioAction = iota
	audioActionSetDefault
	audioActionSetVolume
)

type audioOpResultMsg struct {
	action     audioAction
	deviceID   uint32
	deviceType agentpbv2.AudioDeviceType
	volume     uint32
	err        error
}

type audioTUIHandler interface {
	CanListen() bool
	SetDefault(*agentpbv2.AudioDevice) tea.Cmd
	SetVolume(*agentpbv2.AudioDevice, uint32) tea.Cmd
	Listen(context.Context, *agentpbv2.AudioDevice) tea.Cmd
}

type audioListenResultMsg struct{ err error }

type audioTUIModel struct {
	devices      []*agentpbv2.AudioDevice
	table        tui.BubbleTable
	handler      audioTUIHandler
	busy         bool
	done         bool
	flash        string
	isError      bool
	width        int
	ctx          context.Context
	listening    *agentpbv2.AudioDevice
	listenCancel context.CancelFunc
}

func newAudioTUIModel(devices []*agentpbv2.AudioDevice, handler audioTUIHandler) audioTUIModel {
	m := audioTUIModel{
		devices: devices,
		table:   tui.NewBubbleTable(true, audioTUIColumns()),
		handler: handler,
		ctx:     context.Background(),
	}
	m.refreshRows()
	return m
}

func audioTUIColumns() []bubbleTable.Column {
	return []bubbleTable.Column{
		{Title: "ID", Width: 6},
		{Title: "Name", Width: 18},
		{Title: "Type", Width: 8},
		{Title: "Default", Width: 8},
		{Title: "Volume", Width: 8},
		{Title: "Description", Width: 48},
	}
}

func audioDeviceTypeLabelV2(deviceType agentpbv2.AudioDeviceType) string {
	switch deviceType {
	case agentpbv2.AudioDeviceType_AUDIO_DEVICE_TYPE_INPUT:
		return "Input"
	case agentpbv2.AudioDeviceType_AUDIO_DEVICE_TYPE_OUTPUT:
		return "Output"
	default:
		return "Unknown"
	}
}

func (m *audioTUIModel) refreshRows() {
	rows := make([]bubbleTable.Row, 0, len(m.devices))
	for _, device := range m.devices {
		isDefault := ""
		if device.GetIsDefault() {
			isDefault = "●"
		}
		volume := "—"
		if device.VolumePercent != nil {
			volume = fmt.Sprintf("%d%%", device.GetVolumePercent())
		}
		rows = append(rows, bubbleTable.Row{
			fmt.Sprintf("%d", device.GetDeviceId()),
			tui.StripControl(device.GetName()),
			audioDeviceTypeLabelV2(device.GetType()),
			isDefault,
			volume,
			tui.StripControl(device.GetDescription()),
		})
	}
	m.table.SetRows(rows)
	m.table.SetHeight(max(4, min(len(rows)+1, 14)))
}

func (m audioTUIModel) Init() tea.Cmd { return nil }

func (m audioTUIModel) selectedDevice() *agentpbv2.AudioDevice {
	index := m.table.Cursor()
	if index < 0 || index >= len(m.devices) {
		return nil
	}
	return m.devices[index]
}

func (m audioTUIModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.table.SetViewportWidth(msg.Width)
		return m, nil
	case audioOpResultMsg:
		m.busy = false
		if msg.err != nil {
			m.flash = msg.err.Error()
			m.isError = true
			return m, nil
		}
		switch msg.action {
		case audioActionSetDefault:
			for _, device := range m.devices {
				if device.GetType() == msg.deviceType {
					device.IsDefault = device.GetDeviceId() == msg.deviceID
				}
			}
			m.flash = "Default " + strings.ToLower(audioDeviceTypeLabelV2(msg.deviceType)) + " set."
		case audioActionSetVolume:
			for _, device := range m.devices {
				if device.GetDeviceId() == msg.deviceID && device.GetType() == agentpbv2.AudioDeviceType_AUDIO_DEVICE_TYPE_OUTPUT {
					volume := msg.volume
					device.VolumePercent = &volume
				}
			}
			m.flash = fmt.Sprintf("Volume set to %d%%.", msg.volume)
		}
		m.isError = false
		m.refreshRows()
		return m, nil
	case audioListenResultMsg:
		if m.listenCancel != nil {
			m.listenCancel()
		}
		m.listenCancel = nil
		m.listening = nil
		m.busy = false
		m.flash = "Listening stopped."
		m.isError = msg.err != nil
		if msg.err != nil {
			m.flash = msg.err.Error()
		}
		return m, nil
	case tea.KeyMsg:
		if m.listening != nil {
			switch msg.String() {
			case "esc", "l":
				m.listenCancel()
				m.listening = nil
				m.flash = "Stopping audio…"
			case "q", "ctrl+c":
				m.listenCancel()
				m.done = true
				return m, tea.Quit
			}
			return m, nil
		}
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			if m.listenCancel != nil {
				m.listenCancel()
			}
			m.done = true
			return m, tea.Quit
		case "l":
			if m.busy || m.handler == nil {
				return m, nil
			}
			if !m.handler.CanListen() {
				m.flash = "Audio playback is not available in this CLI build."
				m.isError = true
				return m, nil
			}
			device := m.selectedDevice()
			if device == nil || device.GetType() != agentpbv2.AudioDeviceType_AUDIO_DEVICE_TYPE_INPUT {
				m.flash = "Select an input device to listen."
				m.isError = true
				return m, nil
			}
			ctx, cancel := context.WithCancel(m.ctx)
			m.listenCancel = cancel
			m.listening = device
			m.busy = true
			m.flash = ""
			m.isError = false
			return m, m.handler.Listen(ctx, device)
		case "enter", " ":
			if m.busy || m.handler == nil {
				return m, nil
			}
			device := m.selectedDevice()
			if device == nil {
				return m, nil
			}
			m.busy = true
			m.flash = "Setting default…"
			m.isError = false
			return m, m.handler.SetDefault(device)
		case "left", "right":
			if m.busy || m.handler == nil {
				return m, nil
			}
			device := m.selectedDevice()
			if device == nil || device.GetType() != agentpbv2.AudioDeviceType_AUDIO_DEVICE_TYPE_OUTPUT {
				m.flash = "Select an output device to change volume."
				m.isError = true
				return m, nil
			}
			if device.VolumePercent == nil {
				m.flash = "Volume control unavailable; update the device agent if this output has a mixer."
				m.isError = true
				return m, nil
			}
			volume := device.GetVolumePercent()
			if msg.String() == "left" {
				if volume <= audioVolumeStep {
					volume = 0
				} else {
					volume -= audioVolumeStep
				}
			} else {
				volume = min(uint32(100), volume+audioVolumeStep)
			}
			if volume == device.GetVolumePercent() {
				return m, nil
			}
			m.busy = true
			m.flash = fmt.Sprintf("Setting volume to %d%%…", volume)
			m.isError = false
			return m, m.handler.SetVolume(device, volume)
		}
	}

	var cmd tea.Cmd
	m.table, cmd = m.table.Update(msg)
	return m, cmd
}

var (
	audioTitleStyle = lipgloss.NewStyle().Bold(true).Foreground(tui.ColorPrimary)
	audioHintStyle  = lipgloss.NewStyle().Foreground(tui.ColorDim)
	audioErrorStyle = lipgloss.NewStyle().Foreground(tui.ColorError)
	audioFlashStyle = lipgloss.NewStyle().Foreground(tui.ColorNotice)
)

func (m audioTUIModel) View() string {
	if m.done {
		return ""
	}
	var view strings.Builder
	if m.listening != nil {
		name := m.listening.GetDescription()
		if name == "" {
			name = m.listening.GetName()
		}
		view.WriteString(audioTitleStyle.Render("Listening to " + tui.StripControl(name)))
		view.WriteString("\n\nAudio plays through your computer's speakers.\n\n")
		view.WriteString(audioHintStyle.Render("esc / l stop and back · q quit"))
		view.WriteString("\n")
		return view.String()
	}
	view.WriteString(audioTitleStyle.Render("Audio devices"))
	view.WriteString("\n\n")
	view.WriteString(m.table.View())
	view.WriteString("\n")
	if m.flash != "" {
		style := audioFlashStyle
		if m.isError {
			style = audioErrorStyle
		}
		view.WriteString(style.Render(tui.StripControl(m.flash)))
		view.WriteString("\n")
	}
	hint := "↑/↓ select · enter set default"
	if device := m.selectedDevice(); device != nil {
		switch device.GetType() {
		case agentpbv2.AudioDeviceType_AUDIO_DEVICE_TYPE_INPUT:
			if m.handler != nil && m.handler.CanListen() {
				hint += " · l listen"
			}
		case agentpbv2.AudioDeviceType_AUDIO_DEVICE_TYPE_OUTPUT:
			hint += " · ←/→ volume"
		}
	}
	view.WriteString(audioHintStyle.Render(hint + " · q quit"))
	view.WriteString("\n")
	return view.String()
}

type audioRPCHandler struct {
	ctx          context.Context
	client       agentpbv2.WendyAudioServiceClient
	streamClient agentpb.WendyAudioServiceClient
}

func (h *audioRPCHandler) CanListen() bool { return realtimeAudioAvailable }

func (h *audioRPCHandler) Listen(ctx context.Context, device *agentpbv2.AudioDevice) tea.Cmd {
	return func() tea.Msg {
		return audioListenResultMsg{err: listenToAudioInput(ctx, h.streamClient, device.GetDeviceId(), playRealtimeAudio)}
	}
}

type audioPlaybackFunc func(context.Context, interface {
	Recv() (*agentpb.AudioChunk, error)
}, uint32, uint32, uint32) error

func listenToAudioInput(ctx context.Context, client agentpb.WendyAudioServiceClient, deviceID uint32, play audioPlaybackFunc) error {
	const sampleRate, channels, bufferMs = 16000, 1, 150
	stream, err := client.StreamAudio(ctx, &agentpb.StreamAudioRequest{
		DeviceId: deviceID, SampleRate: sampleRate, Channels: channels,
	})
	if err == nil {
		err = play(ctx, stream, sampleRate, channels, bufferMs)
	} else {
		err = fmt.Errorf("starting audio stream: %w", err)
	}
	if ctx.Err() != nil {
		return nil
	}
	return err
}

func (h *audioRPCHandler) SetDefault(device *agentpbv2.AudioDevice) tea.Cmd {
	return func() tea.Msg {
		resp, err := h.client.SetDefaultAudioDevice(h.ctx, &agentpbv2.SetDefaultAudioDeviceRequest{DeviceId: device.GetDeviceId()})
		if err == nil && !resp.GetSuccess() {
			err = fmt.Errorf("setting default audio device: %s", resp.GetErrorMessage())
		}
		return audioOpResultMsg{
			action: audioActionSetDefault, deviceID: device.GetDeviceId(), deviceType: device.GetType(), err: err,
		}
	}
}

func (h *audioRPCHandler) SetVolume(device *agentpbv2.AudioDevice, volume uint32) tea.Cmd {
	return func() tea.Msg {
		resp, err := h.client.SetAudioVolume(h.ctx, &agentpbv2.SetAudioVolumeRequest{
			DeviceId: device.GetDeviceId(), VolumePercent: volume,
		})
		if err == nil && !resp.GetSuccess() {
			err = fmt.Errorf("setting audio volume: %s", resp.GetErrorMessage())
		}
		actual := volume
		if err == nil && resp.VolumePercent != nil {
			actual = resp.GetVolumePercent()
		}
		return audioOpResultMsg{action: audioActionSetVolume, deviceID: device.GetDeviceId(), volume: actual, err: err}
	}
}
