package commands

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"google.golang.org/grpc"

	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

type fakeAudioTUIHandler struct {
	playbackUnavailable bool
	defaultDevice       *agentpbv2.AudioDevice
	volumeDevice        *agentpbv2.AudioDevice
	volume              uint32
	listenDevice        *agentpbv2.AudioDevice
	listenContext       context.Context
}

func (h *fakeAudioTUIHandler) CanListen() bool { return !h.playbackUnavailable }

func (h *fakeAudioTUIHandler) Listen(ctx context.Context, device *agentpbv2.AudioDevice) tea.Cmd {
	h.listenDevice = device
	h.listenContext = ctx
	return func() tea.Msg { return audioListenResultMsg{} }
}

func (h *fakeAudioTUIHandler) SetDefault(device *agentpbv2.AudioDevice) tea.Cmd {
	h.defaultDevice = device
	return func() tea.Msg {
		return audioOpResultMsg{
			action: audioActionSetDefault, deviceID: device.GetDeviceId(), deviceType: device.GetType(),
		}
	}
}

func (h *fakeAudioTUIHandler) SetVolume(device *agentpbv2.AudioDevice, volume uint32) tea.Cmd {
	h.volumeDevice = device
	h.volume = volume
	return func() tea.Msg {
		return audioOpResultMsg{action: audioActionSetVolume, deviceID: device.GetDeviceId(), volume: volume}
	}
}

func audioTUITestDevices() []*agentpbv2.AudioDevice {
	volume := uint32(40)
	return []*agentpbv2.AudioDevice{
		{DeviceId: 513, Name: "hw:2,0", Description: "USB microphone", Type: agentpbv2.AudioDeviceType_AUDIO_DEVICE_TYPE_INPUT},
		{DeviceId: 513, Name: "hw:2,0", Description: "USB speaker", Type: agentpbv2.AudioDeviceType_AUDIO_DEVICE_TYPE_OUTPUT, VolumePercent: &volume},
	}
}

func TestAudioTUIRightArrowRaisesOutputVolume(t *testing.T) {
	handler := &fakeAudioTUIHandler{}
	model := newAudioTUIModel(audioTUITestDevices(), handler)
	model.table.SetCursor(1)

	updated, cmd := model.Update(tea.KeyMsg{Type: tea.KeyRight})
	model = updated.(audioTUIModel)
	if cmd == nil {
		t.Fatal("right arrow did not dispatch a volume command")
	}
	if handler.volumeDevice == nil || handler.volumeDevice.GetDeviceId() != 513 || handler.volume != 45 {
		t.Fatalf("volume request = device %v volume %d; want device 513 volume 45", handler.volumeDevice, handler.volume)
	}

	updated, _ = model.Update(cmd())
	model = updated.(audioTUIModel)
	if got := model.devices[1].GetVolumePercent(); got != 45 {
		t.Fatalf("updated volume = %d, want 45", got)
	}
	if !strings.Contains(model.View(), "45%") {
		t.Fatal("view does not show the updated volume")
	}
}

func TestAudioTUILeftArrowClampsAtZero(t *testing.T) {
	devices := audioTUITestDevices()
	volume := uint32(3)
	devices[1].VolumePercent = &volume
	handler := &fakeAudioTUIHandler{}
	model := newAudioTUIModel(devices, handler)
	model.table.SetCursor(1)

	_, cmd := model.Update(tea.KeyMsg{Type: tea.KeyLeft})
	if cmd == nil {
		t.Fatal("left arrow did not dispatch a volume command")
	}
	if handler.volume != 0 {
		t.Fatalf("volume request = %d, want 0", handler.volume)
	}
}

func TestAudioTUIEnterSetsDefault(t *testing.T) {
	handler := &fakeAudioTUIHandler{}
	model := newAudioTUIModel(audioTUITestDevices(), handler)
	model.table.SetCursor(1)

	updated, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(audioTUIModel)
	if cmd == nil || handler.defaultDevice == nil || handler.defaultDevice.GetDeviceId() != 513 {
		t.Fatal("enter did not dispatch set-default for the selected device")
	}

	updated, _ = model.Update(cmd())
	model = updated.(audioTUIModel)
	if !model.devices[1].GetIsDefault() {
		t.Fatal("selected output was not marked as default after success")
	}
	if model.devices[0].GetIsDefault() {
		t.Fatal("setting the output default changed the input default")
	}
}

func TestAudioTUIRejectsVolumeOnInputOrUnsupportedOutput(t *testing.T) {
	handler := &fakeAudioTUIHandler{}
	model := newAudioTUIModel(audioTUITestDevices(), handler)

	updated, cmd := model.Update(tea.KeyMsg{Type: tea.KeyRight})
	model = updated.(audioTUIModel)
	if cmd != nil || handler.volumeDevice != nil || !model.isError {
		t.Fatal("input volume change should be rejected locally")
	}

	model.table.SetCursor(1)
	model.devices[1].VolumePercent = nil
	model.refreshRows()
	updated, cmd = model.Update(tea.KeyMsg{Type: tea.KeyRight})
	model = updated.(audioTUIModel)
	if cmd != nil || handler.volumeDevice != nil || !strings.Contains(model.flash, "update the device agent") {
		t.Fatalf("unsupported output result: cmd=%v device=%v flash=%q", cmd, handler.volumeDevice, model.flash)
	}
}

func TestAudioTUIListenStopsAndCanRestart(t *testing.T) {
	handler := &fakeAudioTUIHandler{}
	model := newAudioTUIModel(audioTUITestDevices(), handler)
	for _, stopKey := range []tea.KeyMsg{{Type: tea.KeyEsc}, {Type: tea.KeyRunes, Runes: []rune{'l'}}} {
		updated, cmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
		model = updated.(audioTUIModel)
		if cmd == nil || handler.listenDevice != model.devices[0] || !model.busy || model.listening == nil {
			t.Fatal("l did not start listening to the selected input")
		}
		if !strings.Contains(model.View(), "USB microphone") || !strings.Contains(model.View(), "esc / l stop and back") {
			t.Fatalf("listening view = %q", model.View())
		}
		updated, stopCmd := model.Update(stopKey)
		model = updated.(audioTUIModel)
		if handler.listenContext.Err() != context.Canceled || model.done || model.listening != nil || stopCmd != nil {
			t.Fatal("stopping should cancel playback and return to the audio table")
		}
		// Cancellation retains busy until the old command actually completes.
		pendingContext := handler.listenContext
		updated, restart := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
		model = updated.(audioTUIModel)
		if restart != nil || !model.busy || handler.listenContext != pendingContext {
			t.Fatal("a replacement listener started before cancellation completed")
		}
		updated, _ = model.Update(cmd())
		model = updated.(audioTUIModel)
		if model.busy || model.isError || !strings.Contains(model.View(), "l listen") {
			t.Fatal("completed playback did not restore the audio picker")
		}
	}
}

func TestAudioTUIListenRejectsOutput(t *testing.T) {
	handler := &fakeAudioTUIHandler{}
	model := newAudioTUIModel(audioTUITestDevices(), handler)
	model.table.SetCursor(1)
	if strings.Contains(model.View(), "l listen") {
		t.Fatal("output footer should not offer listening")
	}
	updated, cmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	model = updated.(audioTUIModel)
	if cmd != nil || handler.listenDevice != nil || !model.isError {
		t.Fatal("listening on an output should be rejected locally")
	}
}

func TestAudioTUIQuitCancelsListen(t *testing.T) {
	for _, key := range []tea.KeyMsg{{Type: tea.KeyRunes, Runes: []rune{'q'}}, {Type: tea.KeyCtrlC}} {
		handler := &fakeAudioTUIHandler{}
		model := newAudioTUIModel(audioTUITestDevices(), handler)
		updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
		updated, cmd := updated.(audioTUIModel).Update(key)
		if !updated.(audioTUIModel).done || cmd == nil || handler.listenContext.Err() != context.Canceled {
			t.Fatal("quit did not cancel listening")
		}
	}
}

func TestAudioTUIListenFailureReturnsToPicker(t *testing.T) {
	handler := &fakeAudioTUIHandler{}
	model := newAudioTUIModel(audioTUITestDevices(), handler)
	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	updated, _ = updated.(audioTUIModel).Update(audioListenResultMsg{err: errors.New("microphone unavailable")})
	model = updated.(audioTUIModel)
	if model.listening != nil || model.busy || !model.isError || !strings.Contains(model.View(), "microphone unavailable") {
		t.Fatal("playback failure should return to the picker with an error")
	}
	if handler.listenContext.Err() != context.Canceled {
		t.Fatal("failed playback should release the stream context")
	}
}

type fakeListenAudioClient struct {
	agentpb.WendyAudioServiceClient
	request *agentpb.StreamAudioRequest
	ctx     context.Context
	err     error
}

func (c *fakeListenAudioClient) StreamAudio(ctx context.Context, req *agentpb.StreamAudioRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[agentpb.AudioChunk], error) {
	c.request = req
	c.ctx = ctx
	return nil, c.err
}

func TestListenToAudioInputUsesSelectedDeviceAndCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &fakeListenAudioClient{}
	play := func(playCtx context.Context, _ interface {
		Recv() (*agentpb.AudioChunk, error)
	}, rate, channels, buffer uint32) error {
		if playCtx != ctx || rate != 16000 || channels != 1 || buffer != 150 {
			t.Fatal("unexpected playback configuration")
		}
		cancel()
		return context.Canceled
	}
	if err := listenToAudioInput(ctx, client, 513, play); err != nil {
		t.Fatalf("user cancellation reported as error: %v", err)
	}
	if client.ctx != ctx || client.request.GetDeviceId() != 513 || client.request.GetSampleRate() != 16000 || client.request.GetChannels() != 1 {
		t.Fatalf("unexpected stream request: %v", client.request)
	}
}

func TestListenToAudioInputReportsStreamFailure(t *testing.T) {
	client := &fakeListenAudioClient{err: errors.New("microphone unavailable")}
	err := listenToAudioInput(context.Background(), client, 513, nil)
	if err == nil || !strings.Contains(err.Error(), "microphone unavailable") {
		t.Fatalf("stream failure = %v", err)
	}
}

func TestAudioTUIUnavailablePlayback(t *testing.T) {
	handler := &fakeAudioTUIHandler{playbackUnavailable: true}
	model := newAudioTUIModel(audioTUITestDevices(), handler)
	if strings.Contains(model.View(), "l listen") {
		t.Fatal("unsupported playback advertised")
	}
	updated, cmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	model = updated.(audioTUIModel)
	if cmd != nil || handler.listenDevice != nil || model.listening != nil || !model.isError {
		t.Fatal("unsupported playback must not open an audio stream")
	}
	rpc := &audioRPCHandler{}
	if rpc.CanListen() != realtimeAudioAvailable {
		t.Fatal("RPC playback capability differs from build")
	}
}

func TestAudioViewStripsRemoteDeviceControls(t *testing.T) {
	devices := audioTUITestDevices()
	devices[0].Description = "Microphone\x1b[2J\r\u202e"
	model := newAudioTUIModel(devices, &fakeAudioTUIHandler{})
	model.listening = devices[0]
	view := model.View()
	if strings.Contains(view, "\x1b[2J") || strings.ContainsAny(view, "\r\u202e") {
		t.Fatalf("remote controls reached listening title: %q", view)
	}
}
