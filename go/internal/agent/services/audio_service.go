package services

import (
	"bufio"
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

// ListAudioDevices enumerates audio devices via PipeWire (pw-cli) or ALSA (arecord/aplay).
func (s *AudioService) ListAudioDevices(ctx context.Context, _ *agentpb.ListAudioDevicesRequest) (*agentpb.ListAudioDevicesResponse, error) {
	devices, err := s.listPipeWireDevices(ctx)
	if err != nil {
		s.logger.Warn("PipeWire enumeration failed, falling back to ALSA", zap.Error(err))
		devices, err = s.listALSADevices(ctx)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to enumerate audio devices: %v", err)
		}
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

	// List capture devices.
	cmd := exec.CommandContext(ctx, "arecord", "-l")
	if output, err := cmd.Output(); err == nil {
		devices = append(devices, parseALSAOutput(string(output), agentpb.AudioDeviceType_AUDIO_DEVICE_TYPE_INPUT)...)
	}

	// List playback devices.
	cmd = exec.CommandContext(ctx, "aplay", "-l")
	if output, err := cmd.Output(); err == nil {
		devices = append(devices, parseALSAOutput(string(output), agentpb.AudioDeviceType_AUDIO_DEVICE_TYPE_OUTPUT)...)
	}

	return devices, nil
}

// parseALSAOutput parses the output of arecord -l or aplay -l.
func parseALSAOutput(output string, devType agentpb.AudioDeviceType) []*agentpb.AudioDevice {
	var devices []*agentpb.AudioDevice
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "card ") {
			continue
		}
		// Parse "card N: Name [Description], device N: ..."
		parts := strings.SplitN(line, ":", 2)
		if len(parts) < 2 {
			continue
		}
		cardStr := strings.TrimPrefix(parts[0], "card ")
		cardNum, err := strconv.ParseUint(strings.TrimSpace(cardStr), 10, 32)
		if err != nil {
			continue
		}
		desc := strings.TrimSpace(parts[1])
		devices = append(devices, &agentpb.AudioDevice{
			Id:          uint32(cardNum),
			Name:        fmt.Sprintf("hw:%d", cardNum),
			Description: desc,
			Type:        devType,
		})
	}
	return devices
}

// SetDefaultAudioDevice sets the default audio device using PipeWire.
func (s *AudioService) SetDefaultAudioDevice(ctx context.Context, req *agentpb.SetDefaultAudioDeviceRequest) (*agentpb.SetDefaultAudioDeviceResponse, error) {
	nodeID := fmt.Sprintf("%d", req.GetDeviceId())
	cmd := exec.CommandContext(ctx, "wpctl", "set-default", nodeID)
	if output, err := cmd.CombinedOutput(); err != nil {
		errMsg := fmt.Sprintf("wpctl set-default failed: %s", string(output))
		return &agentpb.SetDefaultAudioDeviceResponse{Success: false, ErrorMessage: &errMsg}, nil
	}

	s.logger.Info("Default audio device set", zap.Uint32("device_id", req.GetDeviceId()))
	return &agentpb.SetDefaultAudioDeviceResponse{Success: true}, nil
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

	// Use arecord to capture a short PCM snippet for level analysis.
	deviceArg := "default"
	if req.GetDeviceId() > 0 {
		deviceArg = fmt.Sprintf("hw:%d", req.GetDeviceId())
	}

	cmd := exec.CommandContext(ctx, "arecord",
		"-D", deviceArg,
		"-f", "S16_LE",
		"-r", "48000",
		"-c", "1",
		"-t", "raw",
		"-",
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return status.Errorf(codes.Internal, "failed to create audio pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		return status.Errorf(codes.Internal, "failed to start audio capture: %v", err)
	}
	defer cmd.Process.Kill()

	buf := make([]byte, 48000*2/int(rateHz)) // samples per interval * 2 bytes per sample

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			n, err := stdout.Read(buf)
			if err != nil {
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

	deviceArg := "default"
	if req.GetDeviceId() > 0 {
		deviceArg = fmt.Sprintf("hw:%d", req.GetDeviceId())
	}

	cmd := exec.CommandContext(ctx, "arecord",
		"-D", deviceArg,
		"-f", "S16_LE",
		"-r", fmt.Sprintf("%d", sampleRate),
		"-c", fmt.Sprintf("%d", channels),
		"-t", "raw",
		"-",
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return status.Errorf(codes.Internal, "failed to create audio pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		return status.Errorf(codes.Internal, "failed to start audio capture: %v", err)
	}
	defer cmd.Process.Kill()

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
