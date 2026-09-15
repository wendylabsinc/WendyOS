package commands

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// minBufferMs is the floor for the --buffer-ms latency knob. Below this the
// playback jitter buffer is too shallow to absorb any network jitter.
const minBufferMs uint32 = 10

// defaultAgentChunkMs is the approximate duration of each PCM chunk the agent
// emits for StreamAudio (see StreamAudio in the agent's audio_service.go). The
// playback jitter buffer depth is derived from it.
const defaultAgentChunkMs uint32 = 10

// unusableCapturePatterns are case-insensitive substrings that mark an INPUT
// device that cannot carry real audio: reading it fails or yields nothing. These
// are never auto-selected (only reachable via --all or an explicit --id).
var unusableCapturePatterns = []string{
	"admaif", // Tegra APE DMA routing FIFOs; nothing routed in by default (EIO on read)
	"hdmi",   // display audio, not a microphone
	"dummy",  // snd-dummy driver
	"null",   // null sink/source
}

// loopbackCapturePatterns are case-insensitive substrings that mark an INPUT
// device as a virtual loopback endpoint. Unlike the unusable ones these DO carry
// real audio — a mounted remote-sensor mic exposed via snd-aloop is a legitimate
// microphone — so they are usable, just ranked below physical mics: auto-select
// falls back to them only when no physical mic exists (see resolveListenDeviceID).
var loopbackCapturePatterns = []string{
	"loopback", // ALSA loopback
	"aloop",    // snd-aloop
	"virtual",  // PipeWire/Pulse virtual nodes
}

func matchesAny(d *agentpb.AudioDevice, patterns []string) bool {
	hay := strings.ToLower(d.GetName() + " " + d.GetDescription())
	for _, p := range patterns {
		if strings.Contains(hay, p) {
			return true
		}
	}
	return false
}

// isUnusableCapture reports whether an INPUT device cannot serve as a microphone
// at all (EIO on read, display audio, dummy/null sinks).
func isUnusableCapture(d *agentpb.AudioDevice) bool {
	return matchesAny(d, unusableCapturePatterns)
}

// isLikelyVirtualCapture reports whether an INPUT device is a virtual/dummy
// endpoint rather than a physical microphone. It covers both the unusable
// endpoints and the (usable-but-secondary) loopback endpoints; it is the
// predicate that keeps such devices out of the *default* physical-mic pool.
func isLikelyVirtualCapture(d *agentpb.AudioDevice) bool {
	return isUnusableCapture(d) || matchesAny(d, loopbackCapturePatterns)
}

// inputDevices returns the INPUT (capture) devices, preserving order.
func inputDevices(devs []*agentpb.AudioDevice) []*agentpb.AudioDevice {
	var out []*agentpb.AudioDevice
	for _, d := range devs {
		if d.GetType() == agentpb.AudioDeviceType_AUDIO_DEVICE_TYPE_INPUT {
			out = append(out, d)
		}
	}
	return out
}

// realCaptureDevices returns INPUT devices that are not likely-virtual,
// preserving order.
func realCaptureDevices(devs []*agentpb.AudioDevice) []*agentpb.AudioDevice {
	var out []*agentpb.AudioDevice
	for _, d := range inputDevices(devs) {
		if !isLikelyVirtualCapture(d) {
			out = append(out, d)
		}
	}
	return out
}

// usableCaptureDevices returns INPUT devices that can carry real audio,
// preserving order. This is the physical mics plus loopback endpoints
// (snd-aloop, etc.) — everything except the genuinely-unusable endpoints
// (admaif/hdmi/dummy/null). It is the fallback pool when no physical mic exists.
func usableCaptureDevices(devs []*agentpb.AudioDevice) []*agentpb.AudioDevice {
	var out []*agentpb.AudioDevice
	for _, d := range inputDevices(devs) {
		if !isUnusableCapture(d) {
			out = append(out, d)
		}
	}
	return out
}

func findAudioDeviceByID(devs []*agentpb.AudioDevice, id uint32) *agentpb.AudioDevice {
	for _, d := range devs {
		if d.GetId() == id {
			return d
		}
	}
	return nil
}

// listenDevicePicker runs an interactive picker over the candidate devices and
// returns the chosen device ID, or ErrUserCancelled if the user aborted. It is
// injected into resolveListenDeviceID so the resolver stays unit-testable.
type listenDevicePicker func(candidates []*agentpb.AudioDevice) (uint32, error)

// resolveListenDeviceID selects the capture device ID for `audio listen`.
//
// Precedence:
//  1. idFlag != 0 -> use it verbatim (explicit always wins; no filtering).
//  2. candidate pool = physical capture devices; if none exist, fall back to
//     usable devices (adds loopback/aloop mics but never the unusable
//     admaif/hdmi/dummy/null endpoints). --all widens the pool to every INPUT.
//  3. empty pool   -> actionable error.
//  4. one device   -> use it.
//  5. 2+ devices    -> picker when interactive, else the first one.
//
// A physical mic is thus always preferred over a loopback one when both exist,
// but a loopback/virtual mic (e.g. a remote sensor via snd-aloop) is used
// automatically when it is all that is available — no --all required.
//
// The returned *agentpb.AudioDevice is the chosen device when known (for
// logging); it may be nil when idFlag points at a device not present in devs.
func resolveListenDeviceID(
	devs []*agentpb.AudioDevice,
	idFlag uint32,
	all, interactive bool,
	pick listenDevicePicker,
) (uint32, *agentpb.AudioDevice, error) {
	if idFlag != 0 {
		return idFlag, findAudioDeviceByID(devs, idFlag), nil
	}

	pool := realCaptureDevices(devs)
	if len(pool) == 0 {
		pool = usableCaptureDevices(devs)
	}
	if all {
		pool = inputDevices(devs)
	}

	switch len(pool) {
	case 0:
		if all {
			return 0, nil, fmt.Errorf("no capture devices found on the target device")
		}
		return 0, nil, fmt.Errorf(
			"no usable microphone detected; re-run with --all to include unusable " +
				"endpoints (HDMI/dummy/routing FIFOs), or pass --id (see `wendy device audio list`)")
	case 1:
		return pool[0].GetId(), pool[0], nil
	default:
		if !interactive {
			return pool[0].GetId(), pool[0], nil
		}
		id, err := pick(pool)
		if err != nil {
			return 0, nil, err
		}
		return id, findAudioDeviceByID(pool, id), nil
	}
}

// jitterDepthChunks returns the playback jitter-buffer depth (in chunks) for a
// given target latency and per-chunk duration, with a floor of one chunk.
func jitterDepthChunks(bufferMs, chunkMs uint32) int {
	if chunkMs == 0 {
		chunkMs = 1
	}
	if d := int(bufferMs / chunkMs); d >= 1 {
		return d
	}
	return 1
}

// audioPickerItems builds the picker rows for the candidate capture devices.
// PickerItem.Value carries the *agentpb.AudioDevice so the column closures and
// the selection handler can read the device ID without a side table.
func audioPickerItems(candidates []*agentpb.AudioDevice) []tui.PickerItem {
	items := make([]tui.PickerItem, 0, len(candidates))
	for _, d := range candidates {
		items = append(items, tui.PickerItem{
			Name:        d.GetName(),
			Description: d.GetDescription(),
			DedupKey:    fmt.Sprintf("%d", d.GetId()),
			Value:       d,
		})
	}
	return items
}

func audioPickerColumns() []tui.PickerColumn {
	devOf := func(it tui.PickerItem) *agentpb.AudioDevice {
		d, _ := it.Value.(*agentpb.AudioDevice)
		return d
	}
	return []tui.PickerColumn{
		{Title: "ID", MinWidth: 6, Required: true, Value: func(it tui.PickerItem) string {
			if d := devOf(it); d != nil {
				return fmt.Sprintf("%d", d.GetId())
			}
			return ""
		}},
		{Title: "Name", MinWidth: 12, Required: true, Value: func(it tui.PickerItem) string { return it.Name }},
		{Title: "Description", MinWidth: 20, Value: func(it tui.PickerItem) string { return it.Description }},
	}
}

// pickAudioDevice presents the interactive microphone picker and returns the
// chosen device ID. It returns ErrUserCancelled if the user quits.
func pickAudioDevice(candidates []*agentpb.AudioDevice) (uint32, error) {
	picker := tui.NewPickerWithTitleAndColumns("Select a microphone", audioPickerColumns())
	p := tea.NewProgram(picker)

	go func() {
		p.Send(tui.PickerAddMsg{Items: audioPickerItems(candidates)})
		p.Send(tui.PickerDoneMsg{})
	}()

	finalModel, err := p.Run()
	if err != nil {
		return 0, fmt.Errorf("microphone picker: %w", err)
	}
	pm, ok := finalModel.(tui.PickerModel)
	if !ok {
		return 0, fmt.Errorf("microphone picker: unexpected model type")
	}
	if pm.Cancelled() {
		return 0, ErrUserCancelled
	}
	sel := pm.Selected()
	if sel == nil {
		return 0, ErrUserCancelled
	}
	d, ok := sel.Value.(*agentpb.AudioDevice)
	if !ok || d == nil {
		return 0, fmt.Errorf("microphone picker: invalid selection")
	}
	return d.GetId(), nil
}
