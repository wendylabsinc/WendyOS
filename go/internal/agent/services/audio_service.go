package services

import (
	"bufio"
	"bytes"
	"context"
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

// SetDefaultAudioDevice sets the default audio device using PipeWire or PulseAudio.
func (s *AudioService) SetDefaultAudioDevice(ctx context.Context, req *agentpb.SetDefaultAudioDeviceRequest) (*agentpb.SetDefaultAudioDeviceResponse, error) {
	// Try PipeWire first.
	nodeID := fmt.Sprintf("%d", req.GetDeviceId())
	cmd := exec.CommandContext(ctx, "wpctl", "set-default", nodeID)
	if output, err := cmd.CombinedOutput(); err != nil {
		s.logger.Warn("wpctl set-default failed, trying PulseAudio", zap.Error(err))
		resp, paErr := s.setPulseAudioDefaultDevice(ctx, req)
		if paErr != nil {
			errMsg := fmt.Sprintf("wpctl set-default failed: %s; pactl also failed: %v", string(output), paErr)
			return &agentpb.SetDefaultAudioDeviceResponse{Success: false, ErrorMessage: &errMsg}, nil
		}
		return resp, nil
	}

	s.logger.Info("Default audio device set", zap.Uint32("device_id", req.GetDeviceId()))
	return &agentpb.SetDefaultAudioDeviceResponse{Success: true}, nil
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

// setPulseAudioDefaultDevice resolves a device ID to a PulseAudio name and sets it as default.
func (s *AudioService) setPulseAudioDefaultDevice(ctx context.Context, req *agentpb.SetDefaultAudioDeviceRequest) (*agentpb.SetDefaultAudioDeviceResponse, error) {
	targetID := req.GetDeviceId()

	// Try sinks first, then sources.
	for _, category := range []string{"sinks", "sources"} {
		cmd := exec.CommandContext(ctx, "pactl", "list", category, "short")
		output, err := cmd.Output()
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(strings.NewReader(string(output)))
		for scanner.Scan() {
			fields := strings.Fields(scanner.Text())
			if len(fields) < 2 {
				continue
			}
			id, err := strconv.ParseUint(fields[0], 10, 32)
			if err != nil {
				continue
			}
			if uint32(id) == targetID {
				var setCmd *exec.Cmd
				if category == "sinks" {
					setCmd = exec.CommandContext(ctx, "pactl", "set-default-sink", fields[1])
				} else {
					setCmd = exec.CommandContext(ctx, "pactl", "set-default-source", fields[1])
				}
				if out, err := setCmd.CombinedOutput(); err != nil {
					errMsg := fmt.Sprintf("pactl set-default failed: %s", string(out))
					return &agentpb.SetDefaultAudioDeviceResponse{Success: false, ErrorMessage: &errMsg}, nil
				}
				s.logger.Info("Default audio device set via PulseAudio", zap.Uint32("device_id", targetID))
				return &agentpb.SetDefaultAudioDeviceResponse{Success: true}, nil
			}
		}
	}

	return nil, fmt.Errorf("device ID %d not found in PulseAudio sinks or sources", targetID)
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
