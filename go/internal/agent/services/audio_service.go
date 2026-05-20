package services

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

// wpctlSetDefault executes "wpctl set-default <id>" and returns the combined
// output and any error. It is a package-level variable so that tests can inject
// a mock implementation without spawning a real wpctl process.
var wpctlSetDefault = func(ctx context.Context, id string) ([]byte, error) {
	return exec.CommandContext(ctx, "wpctl", "set-default", id).CombinedOutput()
}

// pactlSetDefault executes "pactl <set-default-sink|set-default-source> <name>"
// and returns the combined output and any error. It is a package-level variable
// so that tests can inject a mock implementation without spawning a real pactl process.
var pactlSetDefault = func(ctx context.Context, subcmd, name string) ([]byte, error) {
	return exec.CommandContext(ctx, "pactl", subcmd, name).CombinedOutput()
}

// resolvePipeWireNodeIDsFn resolves the ALSA card/device pair to PipeWire node
// IDs. Overriding this variable in tests avoids executing pw-dump.
var resolvePipeWireNodeIDsFn = resolvePipeWireNodeIDs

// resolvePulseAudioSinkOrSourceFn resolves the ALSA card/device pair to
// PulseAudio sink/source matches. Overriding this variable in tests avoids
// executing pactl.
var resolvePulseAudioSinkOrSourceFn = resolvePulseAudioSinkOrSource

// AudioService implements agentpb.WendyAudioServiceServer.
type AudioService struct {
	agentpb.UnimplementedWendyAudioServiceServer
	logger *zap.Logger
}

// NewAudioService creates a new AudioService.
func NewAudioService(logger *zap.Logger) *AudioService {
	return &AudioService{logger: logger}
}

// ListAudioDevices enumerates audio devices via ALSA (arecord/aplay).
// Devices are selected with ALSA card/device arguments in plughw:<card>,<device>
// form. Returned device IDs encode both the ALSA card and device numbers as
// ((card << 8) | device) + 1; alsaDeviceArg decodes them back into the correct
// plughw:<card>,<device> streaming argument. We use plughw rather than hw so
// ALSA's plug layer can handle format conversion when needed. PipeWire/PulseAudio
// node IDs are a different numbering system and would not map correctly to these
// streaming device arguments.
func (s *AudioService) ListAudioDevices(ctx context.Context, _ *agentpb.ListAudioDevicesRequest) (*agentpb.ListAudioDevicesResponse, error) {
	devices, err := s.listALSADevices(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to enumerate audio devices: %v", err)
	}
	return &agentpb.ListAudioDevicesResponse{Devices: devices}, nil
}

// listPipeWireDevices uses pw-cli to list audio nodes.
func (s *AudioService) listPipeWireDevices(ctx context.Context) ([]*agentpb.AudioDevice, error) {
	cmd := exec.CommandContext(ctx, "pw-cli", "list-objects", "Node")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("pw-cli: %w", err)
	}

	var devices []*agentpb.AudioDevice
	var current *agentpb.AudioDevice

	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		if strings.HasPrefix(line, "id ") {
			if current != nil {
				devices = append(devices, current)
			}
			current = &agentpb.AudioDevice{}
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				if id, err := strconv.ParseUint(parts[1], 10, 32); err == nil {
					current.Id = uint32(id)
				}
			}
		}
		if current == nil {
			continue
		}
		if strings.Contains(line, "node.name") {
			current.Name = extractQuotedValue(line)
		}
		if strings.Contains(line, "node.description") {
			current.Description = extractQuotedValue(line)
		}
		if strings.Contains(line, "media.class") {
			cls := extractQuotedValue(line)
			if strings.Contains(cls, "Source") || strings.Contains(cls, "Input") {
				current.Type = agentpb.AudioDeviceType_AUDIO_DEVICE_TYPE_INPUT
			} else if strings.Contains(cls, "Sink") || strings.Contains(cls, "Output") {
				current.Type = agentpb.AudioDeviceType_AUDIO_DEVICE_TYPE_OUTPUT
			}
		}
	}
	if current != nil {
		devices = append(devices, current)
	}

	return devices, nil
}

// listALSADevices falls back to ALSA for audio device enumeration.
func (s *AudioService) listALSADevices(ctx context.Context) ([]*agentpb.AudioDevice, error) {
	var devices []*agentpb.AudioDevice
	var firstErr error

	for _, info := range []struct {
		bin     string
		devType agentpb.AudioDeviceType
	}{
		{"arecord", agentpb.AudioDeviceType_AUDIO_DEVICE_TYPE_INPUT},
		{"aplay", agentpb.AudioDeviceType_AUDIO_DEVICE_TYPE_OUTPUT},
	} {
		var stdout, stderr bytes.Buffer
		cmd := exec.CommandContext(ctx, info.bin, "-l")
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			if firstErr == nil {
				se := strings.TrimSpace(stderr.String())
				if se != "" {
					firstErr = fmt.Errorf("%s -l: %w: %s", info.bin, err, se)
				} else {
					firstErr = fmt.Errorf("%s -l: %w", info.bin, err)
				}
			}
			continue
		}
		devices = append(devices, parseALSAOutput(stdout.String(), info.devType)...)
	}

	if len(devices) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return devices, nil
}

// parseALSAOutput parses the output of arecord -l or aplay -l.
// IDs are encoded as ((card << 8) | device) + 1 so that 0 remains the
// "unspecified" sentinel used by alsaDeviceArg.
func parseALSAOutput(output string, devType agentpb.AudioDeviceType) []*agentpb.AudioDevice {
	var devices []*agentpb.AudioDevice
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "card ") {
			continue
		}
		// Parse "card N: CardName [Desc], device M: DeviceName [Desc]"
		parts := strings.SplitN(line, ":", 2)
		if len(parts) < 2 {
			continue
		}
		cardNum, err := strconv.ParseUint(strings.TrimSpace(strings.TrimPrefix(parts[0], "card ")), 10, 32)
		if err != nil {
			continue
		}
		var deviceNum uint64
		rest := parts[1]
		if idx := strings.Index(rest, ", device "); idx >= 0 {
			after := rest[idx+len(", device "):]
			if ci := strings.Index(after, ":"); ci >= 0 {
				if d, err := strconv.ParseUint(strings.TrimSpace(after[:ci]), 10, 32); err == nil {
					deviceNum = d
				}
			}
		}
		id := ((cardNum << 8) | deviceNum) + 1
		devices = append(devices, &agentpb.AudioDevice{
			Id:          uint32(id),
			Name:        fmt.Sprintf("hw:%d,%d", cardNum, deviceNum),
			Description: strings.TrimSpace(rest),
			Type:        devType,
		})
	}
	return devices
}

// decodeALSAID decodes an ALSA-encoded device ID (as produced by ListAudioDevices)
// into its constituent ALSA card and device numbers.
// The encoding is: id = ((card << 8) | device) + 1.
// Card occupies bits 15–8 and device occupies bits 7–0; both are in [0, 255].
// The caller is responsible for validating that the decoded card is ≤ 255.
func decodeALSAID(id uint32) (card, device uint64) {
	encoded := uint64(id) - 1
	return encoded >> 8, encoded & 0xFF
}

// pwDumpNode models the relevant fields of a single object emitted by pw-dump.
// pw-dump outputs a JSON array of such objects.
type pwDumpNode struct {
	ID   uint32 `json:"id"`
	Type string `json:"type"`
	Info struct {
		Props map[string]json.RawMessage `json:"props"`
	} `json:"info"`
}

// resolvePipeWireNodeIDs uses pw-dump to find all PipeWire node IDs whose
// ALSA card/device properties match the given card/device numbers. Matching
// requires a paired-family approach: BOTH alsa.card AND alsa.device must match
// (legacy family), OR BOTH api.alsa.card AND api.alsa.pcm.device must match
// (native PipeWire family). Cross-family mixing — e.g. alsa.card from the
// legacy family paired with api.alsa.pcm.device from the native family — is
// rejected. A single ALSA card/device pair may appear as multiple PipeWire
// nodes (e.g. one Audio/Sink and one Audio/Source), so we return all matches.
// Returns nil if no matching nodes are found or pw-dump is unavailable.
func resolvePipeWireNodeIDs(ctx context.Context, card, device uint64) []string {
	cmdCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, "pw-dump")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}

	var nodes []pwDumpNode
	if err := json.Unmarshal(out, &nodes); err != nil {
		return nil
	}

	return filterPipeWireNodeIDs(nodes, card, device)
}

// filterPipeWireNodeIDs returns the string node IDs from nodes whose type is
// PipeWire:Interface:Node, whose media.class is exactly "Audio/Sink" or
// "Audio/Source" (excluding virtual/monitor nodes such as "Audio/Source/Virtual"),
// and whose ALSA card/device properties match the given card and device numbers
// using a paired-family approach: BOTH alsa.card AND alsa.device must match
// (legacy family), OR BOTH api.alsa.card AND api.alsa.pcm.device must match
// (native PipeWire family). Cross-family mixing — e.g. alsa.card matched with
// api.alsa.pcm.device — is explicitly rejected to avoid false positives.
// Extracted from resolvePipeWireNodeIDs to allow unit testing without executing
// pw-dump.
func filterPipeWireNodeIDs(nodes []pwDumpNode, card, device uint64) []string {
	cardStr := fmt.Sprintf("%d", card)
	deviceStr := fmt.Sprintf("%d", device)

	var ids []string
	for _, node := range nodes {
		// Reject ID 0; valid PipeWire node IDs are positive integers.
		if node.ID == 0 {
			continue
		}
		// Only consider PipeWire Node objects; other types (Device, Port, etc.)
		// can share ALSA properties but are not valid wpctl targets.
		if node.Type != "PipeWire:Interface:Node" {
			continue
		}

		props := node.Info.Props
		if props == nil {
			continue
		}

		// Only include real audio sink/source nodes. PipeWire uses exactly
		// "Audio/Sink" for playback and "Audio/Source" for capture. Virtual or
		// monitor nodes appear as "Audio/Source/Virtual" and must be excluded to
		// avoid accidentally changing the default input to a monitor source.
		var mediaClass string
		if raw, ok := props["media.class"]; ok {
			_ = json.Unmarshal(raw, &mediaClass)
		}
		if mediaClass != "Audio/Sink" && mediaClass != "Audio/Source" {
			continue
		}

		// Match card+device as a paired family to avoid cross-family mixing
		// (e.g. alsa.card matching with api.alsa.pcm.device).
		legacyMatch := jsonPropMatches(props, "alsa.card", cardStr) &&
			jsonPropMatches(props, "alsa.device", deviceStr)
		apiMatch := jsonPropMatches(props, "api.alsa.card", cardStr) &&
			jsonPropMatches(props, "api.alsa.pcm.device", deviceStr)
		if !legacyMatch && !apiMatch {
			continue
		}

		ids = append(ids, fmt.Sprintf("%d", node.ID))
	}

	return ids
}

// jsonPropMatches reports whether the named property in a pw-dump props map
// equals wantStr. The property value may be a JSON string ("N") or number (N).
func jsonPropMatches(props map[string]json.RawMessage, key, wantStr string) bool {
	raw, ok := props[key]
	if !ok {
		return false
	}
	// Try as a JSON string first.
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s == wantStr
	}
	// Try as a number.
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		return n.String() == wantStr
	}
	return false
}

// pulseAudioMatch holds a resolved PulseAudio sink or source name and the
// category it was found in ("sinks" or "sources").
type pulseAudioMatch struct {
	name     string
	category string
}

// parsePulseAudioOutput scans a raw "pactl list sinks" or "pactl list sources"
// output string and returns the names of all entries whose alsa.card and
// alsa.device properties match the given card and device numbers. category is
// only used to populate the returned pulseAudioMatch entries; it does not affect
// filtering logic. Extracted from resolvePulseAudioSinkOrSource to allow unit
// testing without executing pactl.
func parsePulseAudioOutput(output string, card, device uint64, category string) []pulseAudioMatch {
	cardStr := fmt.Sprintf("%d", card)
	deviceStr := fmt.Sprintf("%d", device)

	var matches []pulseAudioMatch
	var currentName string
	var currentCard, currentDevice string

	flush := func() {
		if currentName != "" && currentCard == cardStr && currentDevice == deviceStr {
			// Exclude monitor sources: PulseAudio exposes a monitor source for every
			// sink (e.g. "alsa_output.*.monitor"). These share the same alsa.card /
			// alsa.device as the sink, so without this filter we would erroneously
			// set the default input to the monitor when the user selected an output.
			if strings.HasSuffix(currentName, ".monitor") {
				return
			}
			matches = append(matches, pulseAudioMatch{name: currentName, category: category})
		}
	}

	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		if strings.HasPrefix(line, "Name:") {
			// Flush previous entry before starting a new one.
			flush()
			currentName = strings.TrimSpace(strings.TrimPrefix(line, "Name:"))
			currentCard = ""
			currentDevice = ""
			continue
		}

		// ALSA properties appear in the Properties section as:
		//   alsa.card = "N"
		//   alsa.device = "M"
		// We split on " = " and compare the trimmed key exactly to avoid matching
		// related properties such as alsa.card_name when looking for alsa.card.
		if parts := strings.SplitN(line, " = ", 2); len(parts) == 2 {
			key := strings.TrimSpace(parts[0])
			switch key {
			case "alsa.card":
				val := extractPactlPropertyValue(line)
				if val == cardStr {
					currentCard = val
				}
			case "alsa.device":
				val := extractPactlPropertyValue(line)
				if val == deviceStr {
					currentDevice = val
				}
			}
		}
	}
	// Flush last entry.
	flush()

	return matches
}

// resolvePulseAudioSinkOrSource finds all PulseAudio sinks and sources whose
// ALSA card/device properties match the given values. It inspects
// "pactl list sinks" and "pactl list sources" for alsa.card and alsa.device
// properties. Returning all matches (instead of the first) prevents the
// ambiguity where an input and output on the same ALSA card/device share the
// same encoded ID: callers can then set the correct default for each category.
// Returns nil if no match is found.
func resolvePulseAudioSinkOrSource(ctx context.Context, card, device uint64) []pulseAudioMatch {
	var matches []pulseAudioMatch

	for _, cat := range []string{"sinks", "sources"} {
		cmdCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		cmd := exec.CommandContext(cmdCtx, "pactl", "list", cat)
		out, err := cmd.Output()
		cancel()
		if err != nil {
			continue
		}

		matches = append(matches, parsePulseAudioOutput(string(out), card, device, cat)...)
	}
	return matches
}

// extractPactlPropertyValue extracts the value from a pactl property line like:
//
//	alsa.card = "0"
func extractPactlPropertyValue(line string) string {
	eqIdx := strings.Index(line, "=")
	if eqIdx < 0 {
		return ""
	}
	val := strings.TrimSpace(line[eqIdx+1:])
	if len(val) >= 2 && val[0] == '"' && val[len(val)-1] == '"' {
		return val[1 : len(val)-1]
	}
	return val
}

// SetDefaultAudioDevice sets the default audio device using PipeWire or PulseAudio.
// The device ID in the request is an ALSA-encoded ID as returned by ListAudioDevices
// (encoded as ((card << 8) | device) + 1). This function decodes the ID and resolves
// it to the appropriate PipeWire node ID or PulseAudio sink/source name before
// invoking wpctl/pactl.
func (s *AudioService) SetDefaultAudioDevice(ctx context.Context, req *agentpb.SetDefaultAudioDeviceRequest) (*agentpb.SetDefaultAudioDeviceResponse, error) {
	if req.GetDeviceId() == 0 {
		return nil, status.Errorf(codes.InvalidArgument, "device ID 0 is not a valid audio device")
	}

	alsaCard, alsaDevice := decodeALSAID(req.GetDeviceId())
	if alsaCard > 255 {
		return nil, status.Errorf(codes.InvalidArgument,
			"device ID %d encodes out-of-range ALSA card %d (max 255)", req.GetDeviceId(), alsaCard)
	}

	// Try PipeWire first: resolve the ALSA card/device to all matching PipeWire
	// node IDs (a device may appear as both a sink and a source in PipeWire).
	nodeIDs := resolvePipeWireNodeIDsFn(ctx, alsaCard, alsaDevice)
	var wpctlErr error
	if len(nodeIDs) > 0 {
		var failedIDs []string
		var lastWpctlErr error
		var lastWpctlOutput string
		for _, nodeID := range nodeIDs {
			if output, err := wpctlSetDefault(ctx, nodeID); err != nil {
				s.logger.Warn("wpctl set-default failed for node",
					zap.String("node_id", nodeID), zap.Error(err),
					zap.String("output", strings.TrimSpace(string(output))))
				failedIDs = append(failedIDs, nodeID)
				lastWpctlErr = err
				lastWpctlOutput = strings.TrimSpace(string(output))
			} else {
				s.logger.Info("Default audio device set via PipeWire",
					zap.Uint32("device_id", req.GetDeviceId()),
					zap.Uint64("alsa_card", alsaCard),
					zap.Uint64("alsa_device", alsaDevice),
					zap.String("pw_node_id", nodeID))
			}
		}
		if len(failedIDs) == 0 {
			// All wpctl calls succeeded.
			return &agentpb.SetDefaultAudioDeviceResponse{Success: true}, nil
		}
		// One or more wpctl calls failed; fall through to PulseAudio so a partial
		// failure (e.g. sink set but source failed) does not silently report success.
		wpctlErr = fmt.Errorf("wpctl set-default %v: %w; output: %s", failedIDs, lastWpctlErr, lastWpctlOutput)
	}

	s.logger.Debug("PipeWire node not found or wpctl failed for ALSA device, trying PulseAudio",
		zap.Uint64("alsa_card", alsaCard), zap.Uint64("alsa_device", alsaDevice))

	// Fall back to PulseAudio: resolve the ALSA card/device to a sink/source name.
	resp, paErr := s.setPulseAudioDefaultByALSA(ctx, req.GetDeviceId(), alsaCard, alsaDevice)
	if paErr != nil {
		var errMsg string
		if len(nodeIDs) > 0 && wpctlErr != nil {
			errMsg = fmt.Sprintf("PipeWire node(s) found (%v) but wpctl failed; PulseAudio also failed: %v; wpctl error: %v", nodeIDs, paErr, wpctlErr)
		} else {
			errMsg = fmt.Sprintf("no PipeWire node or PulseAudio sink/source found for ALSA card %d device %d: %v", alsaCard, alsaDevice, paErr)
		}
		return &agentpb.SetDefaultAudioDeviceResponse{Success: false, ErrorMessage: &errMsg}, nil
	}
	// If PA returned a failure response (not error) and PW had also failed,
	// augment the error message so callers see both failure contexts.
	if !resp.GetSuccess() && len(nodeIDs) > 0 && wpctlErr != nil {
		combined := fmt.Sprintf("PipeWire node(s) found (%v) but wpctl failed (%v); PulseAudio also reported failure: %s",
			nodeIDs, wpctlErr, resp.GetErrorMessage())
		return &agentpb.SetDefaultAudioDeviceResponse{Success: false, ErrorMessage: &combined}, nil
	}
	return resp, nil
}

// listPulseAudioDevices uses pactl to enumerate sinks and sources.
func (s *AudioService) listPulseAudioDevices(ctx context.Context) ([]*agentpb.AudioDevice, error) {
	var devices []*agentpb.AudioDevice

	// Collect sinks (output devices).
	sinkDevices, err := s.parsePulseAudioDevices(ctx, "sink", agentpb.AudioDeviceType_AUDIO_DEVICE_TYPE_OUTPUT)
	if err != nil {
		return nil, fmt.Errorf("pactl list sinks: %w", err)
	}
	devices = append(devices, sinkDevices...)

	// Collect sources (input devices).
	sourceDevices, err := s.parsePulseAudioDevices(ctx, "source", agentpb.AudioDeviceType_AUDIO_DEVICE_TYPE_INPUT)
	if err != nil {
		return nil, fmt.Errorf("pactl list sources: %w", err)
	}
	devices = append(devices, sourceDevices...)

	return devices, nil
}

// parsePulseAudioDevices parses "pactl list sinks short" and "pactl list sinks" for a device category.
func (s *AudioService) parsePulseAudioDevices(ctx context.Context, category string, devType agentpb.AudioDeviceType) ([]*agentpb.AudioDevice, error) {
	plural := category + "s"

	// Get short listing for ID and name.
	shortCmd := exec.CommandContext(ctx, "pactl", "list", plural, "short")
	shortOutput, err := shortCmd.Output()
	if err != nil {
		return nil, fmt.Errorf("pactl list %s short: %w", plural, err)
	}

	type paDevice struct {
		id   uint32
		name string
	}
	var parsed []paDevice

	scanner := bufio.NewScanner(strings.NewReader(string(shortOutput)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		id, err := strconv.ParseUint(fields[0], 10, 32)
		if err != nil {
			continue
		}
		parsed = append(parsed, paDevice{id: uint32(id), name: fields[1]})
	}

	// Get long listing for descriptions.
	longCmd := exec.CommandContext(ctx, "pactl", "list", plural)
	longOutput, err := longCmd.Output()
	if err != nil {
		// Fall back to short-form only (no descriptions).
		var devices []*agentpb.AudioDevice
		for _, p := range parsed {
			devices = append(devices, &agentpb.AudioDevice{
				Id:   p.id,
				Name: p.name,
				Type: devType,
			})
		}
		return devices, nil
	}

	// Parse descriptions from long output, indexed by order of appearance.
	var descriptions []string
	longScanner := bufio.NewScanner(strings.NewReader(string(longOutput)))
	for longScanner.Scan() {
		line := strings.TrimSpace(longScanner.Text())
		if strings.HasPrefix(line, "Description:") {
			descriptions = append(descriptions, strings.TrimSpace(strings.TrimPrefix(line, "Description:")))
		}
	}

	var devices []*agentpb.AudioDevice
	for i, p := range parsed {
		dev := &agentpb.AudioDevice{
			Id:   p.id,
			Name: p.name,
			Type: devType,
		}
		if i < len(descriptions) {
			dev.Description = descriptions[i]
		}
		devices = append(devices, dev)
	}
	return devices, nil
}

// setPulseAudioDefaultByALSA resolves the given ALSA card/device to all
// matching PulseAudio sinks and sources by matching ALSA properties, then sets
// each as the system default. Both sink and source defaults are updated when a
// device appears in both categories (which is possible when input and output
// share the same ALSA card/device numbers).
func (s *AudioService) setPulseAudioDefaultByALSA(ctx context.Context, deviceID uint32, card, device uint64) (*agentpb.SetDefaultAudioDeviceResponse, error) {
	matches := resolvePulseAudioSinkOrSourceFn(ctx, card, device)
	if len(matches) == 0 {
		return nil, fmt.Errorf("no PulseAudio sink or source found for ALSA card %d device %d", card, device)
	}

	var setErrors []string
	for _, m := range matches {
		var pactlCmd string
		if m.category == "sinks" {
			pactlCmd = "set-default-sink"
		} else {
			pactlCmd = "set-default-source"
		}
		if out, err := pactlSetDefault(ctx, pactlCmd, m.name); err != nil {
			setErrors = append(setErrors, fmt.Sprintf("pactl %s %s: %v: %s", pactlCmd, m.name, err, strings.TrimSpace(string(out))))
			continue
		}
		s.logger.Info("Default audio device set via PulseAudio",
			zap.Uint32("device_id", deviceID),
			zap.Uint64("alsa_card", card),
			zap.Uint64("alsa_device", device),
			zap.String("pa_name", m.name),
			zap.String("category", m.category))
	}

	if len(setErrors) > 0 {
		// All matches must succeed; any failure means the device was not fully
		// set as default (e.g. sink succeeded but source failed).
		errMsg := fmt.Sprintf("pactl set-default failed: %s", strings.Join(setErrors, "; "))
		return &agentpb.SetDefaultAudioDeviceResponse{Success: false, ErrorMessage: &errMsg}, nil
	}
	return &agentpb.SetDefaultAudioDeviceResponse{Success: true}, nil
}

// firstALSACaptureDeviceID returns the encoded device ID of the first ALSA capture
// device from arecord -l. The ID is the encoded form ((card << 8) | device) + 1,
// matching the IDs returned by ListAudioDevices. Returns 0 if no device is found.
func firstALSACaptureDeviceID(ctx context.Context) uint32 {
	cmd := exec.CommandContext(ctx, "arecord", "-l")
	out, err := cmd.Output()
	if err != nil {
		return 0
	}
	devices := parseALSAOutput(string(out), agentpb.AudioDeviceType_AUDIO_DEVICE_TYPE_INPUT)
	if len(devices) > 0 {
		return devices[0].GetId()
	}
	return 0
}

// alsaDeviceArg returns the arecord -D argument for the given device ID.
// IDs from ListAudioDevices are encoded as ((card << 8) | device) + 1; 0 means
// "unspecified" and triggers auto-selection of the first capture card.
// plughw is used instead of hw so ALSA's plug layer handles format/rate conversion.
func alsaDeviceArg(ctx context.Context, id uint32) string {
	if id == 0 {
		id = firstALSACaptureDeviceID(ctx)
		if id == 0 {
			return "plughw:0,0"
		}
	}
	encoded := uint64(id) - 1
	card := encoded >> 8
	device := encoded & 0xFF
	return fmt.Sprintf("plughw:%d,%d", card, device)
}

// StreamAudioLevels streams peak/RMS dB levels for a device.
func (s *AudioService) StreamAudioLevels(req *agentpb.StreamAudioLevelsRequest, stream grpc.ServerStreamingServer[agentpb.AudioLevelUpdate]) error {
	ctx := stream.Context()

	rateHz := req.GetUpdateRateHz()
	if rateHz == 0 {
		rateHz = 20
	}
	if rateHz > 60 {
		rateHz = 60
	}

	interval := time.Second / time.Duration(rateHz)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Device IDs from audio list are encoded ALSA card+device pairs (see ListAudioDevices).
	// ID 0 means "unspecified" — auto-select the first capture device.
	deviceArg := alsaDeviceArg(ctx, req.GetDeviceId())

	cmd := exec.CommandContext(ctx, "arecord",
		"-D", deviceArg,
		"-f", "S16_LE",
		"-r", "48000",
		"-c", "1",
		"-t", "raw",
		"-",
	)
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return status.Errorf(codes.Internal, "failed to create audio pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		return status.Errorf(codes.Internal, "failed to start audio capture: %v", err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }() //nolint:errcheck

	buf := make([]byte, 48000*2/int(rateHz)) // samples per interval * 2 bytes per sample

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			n, err := stdout.Read(buf)
			if err != nil {
				if msg := strings.TrimSpace(stderrBuf.String()); msg != "" {
					return status.Errorf(codes.Internal, "audio capture failed: %s", msg)
				}
				return nil
			}

			peak, rms := computeAudioLevels(buf[:n])

			if err := stream.Send(&agentpb.AudioLevelUpdate{
				PeakDb:      peak,
				RmsDb:       rms,
				TimestampNs: uint64(time.Now().UnixNano()),
			}); err != nil {
				return err
			}
		}
	}
}

// StreamAudio streams raw PCM audio data from a microphone.
func (s *AudioService) StreamAudio(req *agentpb.StreamAudioRequest, stream grpc.ServerStreamingServer[agentpb.AudioChunk]) error {
	ctx := stream.Context()

	sampleRate := req.GetSampleRate()
	if sampleRate == 0 {
		sampleRate = 48000
	}
	channels := req.GetChannels()
	if channels == 0 {
		channels = 1
	}

	// Device IDs from audio list are encoded ALSA card+device pairs (see ListAudioDevices).
	// ID 0 means "unspecified" — auto-select the first capture device.
	deviceArg := alsaDeviceArg(ctx, req.GetDeviceId())

	cmd := exec.CommandContext(ctx, "arecord",
		"-D", deviceArg,
		"-f", "S16_LE",
		"-r", fmt.Sprintf("%d", sampleRate),
		"-c", fmt.Sprintf("%d", channels),
		"-t", "raw",
		"--buffer-time=50000", // 50ms ALSA buffer to minimise capture latency
		"-",
	)
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return status.Errorf(codes.Internal, "failed to create audio pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		return status.Errorf(codes.Internal, "failed to start audio capture: %v", err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }() //nolint:errcheck

	// Send ~20ms chunks of PCM data.
	chunkSamples := sampleRate / 50 // 20ms worth of samples
	chunkBytes := chunkSamples * channels * 2
	buf := make([]byte, chunkBytes)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		n, err := stdout.Read(buf)
		if err != nil {
			if msg := strings.TrimSpace(stderrBuf.String()); msg != "" {
				return status.Errorf(codes.Internal, "audio capture failed: %s", msg)
			}
			return nil
		}

		if err := stream.Send(&agentpb.AudioChunk{
			PcmData:     buf[:n],
			TimestampNs: uint64(time.Now().UnixNano()),
			SampleRate:  sampleRate,
			Channels:    channels,
		}); err != nil {
			return err
		}
	}
}

// computeAudioLevels computes peak and RMS levels in dB from s16le PCM data.
func computeAudioLevels(data []byte) (peakDb, rmsDb float32) {
	if len(data) < 2 {
		return -96.0, -96.0
	}

	var peak int16
	var sumSquares float64
	samples := len(data) / 2

	for i := 0; i < len(data)-1; i += 2 {
		sample := int16(data[i]) | int16(data[i+1])<<8
		if sample < 0 {
			sample = -sample
		}
		if sample > peak {
			peak = sample
		}
		sumSquares += float64(sample) * float64(sample)
	}

	if peak == 0 {
		return -96.0, -96.0
	}

	peakDb = float32(20.0 * math.Log10(float64(peak)/32768.0))
	rmsVal := math.Sqrt(sumSquares / float64(samples))
	rmsDb = float32(20.0 * math.Log10(rmsVal/32768.0))

	return peakDb, rmsDb
}

// extractQuotedValue extracts a value between quotes from a PipeWire property line.
func extractQuotedValue(line string) string {
	start := strings.Index(line, "\"")
	if start < 0 {
		// Try without quotes (some pw-cli formats use = value).
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			return strings.TrimSpace(parts[1])
		}
		return ""
	}
	end := strings.Index(line[start+1:], "\"")
	if end < 0 {
		return line[start+1:]
	}
	return line[start+1 : start+1+end]
}
