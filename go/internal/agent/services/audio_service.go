package services

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/agent/audio"
	"github.com/wendylabsinc/wendy/go/internal/agent/audioloop"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// AudioService implements agentpb.WendyAudioServiceServer on top of PipeWire.
// Device IDs are PipeWire node IDs, which wpctl accepts directly and which name
// Bluetooth endpoints as well as sound cards.
//
// When no PipeWire user session is reachable (audio.Available() is false),
// every method here falls back to raw ALSA (aplay/arecord/amixer) instead of
// failing outright, so a board with a sound card but no desktop session still
// has basic playback and capture. Every such fallback logs the specific reason
// from audio.UnavailableReason: the backend swap is invisible in the responses,
// so without that line a misconfigured session is indistinguishable from a
// board that never had one. The two paths never mix within one call:
// falling back means enumerating ALSA cards *instead of* the PipeWire graph,
// not alongside it, which is what caused a Bluetooth device to appear on some
// surfaces and not others before this service moved to PipeWire exclusively.
type AudioService struct {
	agentpb.UnimplementedWendyAudioServiceServer
	logger *zap.Logger

	// sensorLinkMounts and pairingName, when set, let the ALSA fallback name a
	// snd-aloop Loopback capture subdevice after the remote sensor-link source
	// it carries. Both nil on a device with no sensor pairings wired.
	sensorLinkMounts func() []audioloop.Mount
	pairingName      func(sourceAssetID int32) (string, bool)
}

// NewAudioService creates a new AudioService.
func NewAudioService(logger *zap.Logger) *AudioService {
	return &AudioService{logger: logger}
}

// SetSensorLinkNaming wires the audioloop mount table and pairing-name lookup so
// ListAudioDevices can rename mounted Loopback capture subdevices after their
// remote source. Called once at startup, after the sensor-pairing supervisor is
// built (both are nil until then). mounts or pairingName may be nil.
func (s *AudioService) SetSensorLinkNaming(mounts func() []audioloop.Mount, pairingName func(int32) (string, bool)) {
	s.sensorLinkMounts = mounts
	s.pairingName = pairingName
}

// pipewireUnavailable reports whether this call must use the ALSA fallback, and
// logs the specific reason when it must.
//
// Falling back silently is what made this hard to diagnose in the field: every
// audio RPC quietly changed backend, the device listing came back full of
// plausible plughw entries, and nothing said the graph had been swapped out or
// which precondition failed. Warn, not Debug: on a device whose image ships
// PipeWire this is a misconfiguration, not a supported steady state.
func (s *AudioService) pipewireUnavailable(op string) (bool, string) {
	if audio.Available() {
		return false, ""
	}
	reason := audio.UnavailableReason()
	if reason == "" {
		// Session became available between checks; avoid falling back with no reason.
		return false, ""
	}
	s.logger.Warn("PipeWire session unavailable; falling back to ALSA",
		zap.String("operation", op),
		zap.String("reason", reason))
	return true, reason
}

// audioDeviceType maps a node's direction onto the proto enum.
func audioDeviceType(n audio.Node) agentpb.AudioDeviceType {
	if n.IsSink {
		return agentpb.AudioDeviceType_AUDIO_DEVICE_TYPE_OUTPUT
	}
	return agentpb.AudioDeviceType_AUDIO_DEVICE_TYPE_INPUT
}

// ListAudioDevices enumerates the sinks and sources in the PipeWire graph, or
// the raw ALSA cards when no PipeWire session is up. The ALSA fallback has no
// notion of a default device, so IsDefault is always false there.
func (s *AudioService) ListAudioDevices(ctx context.Context, req *agentpb.ListAudioDevicesRequest) (*agentpb.ListAudioDevicesResponse, error) {
	var nodes []audio.Node
	var defaults audio.Defaults
	if unavailable, _ := s.pipewireUnavailable("ListAudioDevices"); unavailable {
		var err error
		nodes, err = audio.ListAlsaNodes(ctx)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to enumerate audio devices: %v", err)
		}
		nodes = s.nameSensorLinkMounts(nodes)
	} else {
		var err error
		nodes, defaults, err = audio.ListNodes(ctx)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to enumerate audio devices: %v", err)
		}
	}

	filter := req.GetTypeFilter()
	devices := make([]*agentpb.AudioDevice, 0, len(nodes))
	for _, n := range nodes {
		devType := audioDeviceType(n)
		if filter != agentpb.AudioDeviceType_AUDIO_DEVICE_TYPE_UNSPECIFIED && filter != devType {
			continue
		}
		devices = append(devices, &agentpb.AudioDevice{
			Id:          n.ID,
			Name:        n.Name,
			Description: n.Description,
			Type:        devType,
			IsDefault:   defaults.IsDefault(n),
		})
	}
	return &agentpb.ListAudioDevicesResponse{Devices: devices}, nil
}

// nameSensorLinkMounts replaces the one generic snd-aloop "Loopback" capture row
// (plughw:<card>,1) with one row per active sensor-link mount, each named after
// the remote source it carries — turning an unrecognizable "plughw:2,1 /
// Loopback PCM" into "plughw:2,1,0 / sensor-link: parakeet-demo · mic0". Every
// other device, and the Loopback card when nothing is mounted, is returned
// unchanged. ALSA-path only: node IDs here are ALSA-encoded, so this must not
// run over PipeWire node IDs.
func (s *AudioService) nameSensorLinkMounts(nodes []audio.Node) []audio.Node {
	if s.sensorLinkMounts == nil {
		return nodes
	}
	mounts := s.sensorLinkMounts()
	if len(mounts) == 0 {
		return nodes
	}
	out := make([]audio.Node, 0, len(nodes)+len(mounts))
	for _, n := range nodes {
		card, device, isSink := audio.DecodeAlsaID(n.ID)
		// The consumer captures from the snd-aloop card's device 1; that is the
		// row whose subdevices carry the mounted mics.
		if isSink || device != loopbackCaptureDevice || !strings.Contains(n.Description, "Loopback") {
			out = append(out, n)
			continue
		}
		for _, m := range mounts {
			label := fmt.Sprintf("asset-%d", m.SourceAssetID)
			if s.pairingName != nil {
				if name, ok := s.pairingName(m.SourceAssetID); ok {
					label = name
				}
			}
			out = append(out, audio.Node{
				ID:          audio.EncodeAlsaSubdeviceID(card, uint64(m.Sub)),
				Name:        fmt.Sprintf("plughw:%d,%d,%d", card, device, m.Sub),
				Description: fmt.Sprintf("sensor-link: %s · %s", label, m.SensorName),
				IsSink:      false,
			})
		}
	}
	return out
}

// loopbackCaptureDevice mirrors audio.loopbackCaptureDevice (the snd-aloop
// device the consumer captures from); duplicated as an untyped constant to keep
// it unexported in the audio package.
const loopbackCaptureDevice = 1

// volumeQueryConcurrency bounds the wpctl processes nodeVolumes has in flight.
const volumeQueryConcurrency = 8

// nodeVolumes reports each device's volume as a percentage; devices whose
// volume cannot be read are absent from the map. Each read is a wpctl process,
// so an unresponsive node cannot serialise the whole listing behind it.
func (s *AudioService) nodeVolumes(ctx context.Context, devices []*agentpb.AudioDevice) map[uint32]*uint32 {
	if !audio.Available() {
		return s.alsaVolumes(ctx, devices)
	}

	volumes := make(map[uint32]*uint32, len(devices))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, volumeQueryConcurrency)
	for _, device := range devices {
		id := device.GetId()
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			volume, ok := audio.NodeVolume(ctx, id)
			if !ok {
				s.logger.Debug("Volume unavailable", zap.Uint32("node_id", id))
				return
			}
			mu.Lock()
			volumes[id] = &volume
			mu.Unlock()
		}()
	}
	wg.Wait()
	return volumes
}

// alsaVolumeResult caches one card's mixerControl lookup, so cards with
// several devices (e.g. multiple HDMI outputs) are not re-queried per device.
type alsaVolumeResult struct {
	volume uint32
	ok     bool
}

// alsaVolumes is nodeVolumes' fallback for when no PipeWire session is up.
// ALSA has no per-node volume concept — the mixer control is card-scoped —
// so only outputs are reported, matching what a card's mixer can honestly
// answer.
func (s *AudioService) alsaVolumes(ctx context.Context, devices []*agentpb.AudioDevice) map[uint32]*uint32 {
	volumes := make(map[uint32]*uint32, len(devices))
	cache := make(map[uint64]alsaVolumeResult)
	for _, device := range devices {
		if device.GetType() != agentpb.AudioDeviceType_AUDIO_DEVICE_TYPE_OUTPUT {
			continue
		}
		card, _, _ := audio.DecodeAlsaID(device.GetId())
		result, cached := cache[card]
		if !cached {
			volume, ok := audio.AlsaVolume(ctx, card)
			result = alsaVolumeResult{volume: volume, ok: ok}
			cache[card] = result
		}
		if !result.ok {
			s.logger.Debug("ALSA playback volume unavailable", zap.Uint64("alsa_card", card))
			continue
		}
		volume := result.volume
		volumes[device.GetId()] = &volume
	}
	return volumes
}

// setAudioVolume sets a node's volume and reports the value that took effect.
func (s *AudioService) setAudioVolume(ctx context.Context, deviceID, volumePercent uint32) (uint32, error) {
	if deviceID == 0 {
		return 0, status.Error(codes.InvalidArgument, "device ID 0 is not a valid audio device")
	}
	if volumePercent > 100 {
		return 0, status.Errorf(codes.InvalidArgument, "volume must be between 0 and 100, got %d", volumePercent)
	}

	if unavailable, _ := s.pipewireUnavailable("SetAudioVolume"); unavailable {
		return s.setAlsaVolume(ctx, deviceID, volumePercent)
	}

	nodes, _, err := audio.ListNodes(ctx)
	if err != nil {
		return 0, status.Errorf(codes.Internal, "failed to enumerate audio devices: %v", err)
	}
	node, ok := audio.FindNode(nodes, deviceID)
	if !ok {
		return 0, status.Errorf(codes.NotFound, "no audio device with ID %d", deviceID)
	}

	if err := audio.SetNodeVolume(ctx, deviceID, volumePercent); err != nil {
		return 0, err
	}

	// PipeWire clamps and quantises, and a hardware mixer may land on a nearby
	// step, so report what the graph holds.
	actual := volumePercent
	if v, ok := audio.NodeVolume(ctx, deviceID); ok {
		actual = v
	}
	s.logger.Info("Audio volume set",
		zap.Uint32("device_id", deviceID),
		zap.String("node", node.Name),
		zap.Uint32("volume_percent", actual))
	return actual, nil
}

// setAlsaVolume is setAudioVolume's fallback for when no PipeWire session is
// up. The mixer control is card-scoped, so an input device is rejected rather
// than silently changing a playback control it does not own.
func (s *AudioService) setAlsaVolume(ctx context.Context, deviceID, volumePercent uint32) (uint32, error) {
	nodes, err := audio.ListAlsaNodes(ctx)
	if err != nil {
		return 0, status.Errorf(codes.Internal, "failed to enumerate audio devices: %v", err)
	}
	node, ok := audio.FindNode(nodes, deviceID)
	if !ok {
		return 0, status.Errorf(codes.NotFound, "no audio device with ID %d", deviceID)
	}
	if !node.IsSink {
		return 0, status.Errorf(codes.InvalidArgument, "device %d (%s) is an input; it has no playback volume", deviceID, node.Name)
	}

	card, _, _ := audio.DecodeAlsaID(deviceID)
	actual, err := audio.SetAlsaVolume(ctx, card, volumePercent)
	if err != nil {
		return 0, err
	}
	s.logger.Info("ALSA playback volume set",
		zap.Uint32("device_id", deviceID),
		zap.Uint64("alsa_card", card),
		zap.String("node", node.Name),
		zap.Uint32("volume_percent", actual))
	return actual, nil
}

// SetDefaultAudioDevice makes a node the default for its direction. Setting a
// sink leaves the default source alone, and vice versa.
func (s *AudioService) SetDefaultAudioDevice(ctx context.Context, req *agentpb.SetDefaultAudioDeviceRequest) (*agentpb.SetDefaultAudioDeviceResponse, error) {
	if req.GetDeviceId() == 0 {
		return nil, status.Errorf(codes.InvalidArgument, "device ID 0 is not a valid audio device")
	}

	if unavailable, reason := s.pipewireUnavailable("SetDefaultAudioDevice"); unavailable {
		// "Default sink/source" is PipeWire/WirePlumber metadata; raw ALSA has
		// no equivalent to set. Saying so beats reporting success for a setting
		// that does not exist.
		//
		// The reason is included rather than a flat "no PipeWire session is
		// running", which claims more than the agent knows: all it established
		// is that it found no session it would trust at the path it checked.
		errMsg := fmt.Sprintf("cannot set a default audio device: %s (device %d)", reason, req.GetDeviceId())
		return &agentpb.SetDefaultAudioDeviceResponse{Success: false, ErrorMessage: &errMsg}, nil
	}

	nodes, _, err := audio.ListNodes(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to enumerate audio devices: %v", err)
	}
	node, ok := audio.FindNode(nodes, req.GetDeviceId())
	if !ok {
		errMsg := fmt.Sprintf("no audio device with ID %d", req.GetDeviceId())
		return &agentpb.SetDefaultAudioDeviceResponse{Success: false, ErrorMessage: &errMsg}, nil
	}

	if err := audio.SetDefaultNode(ctx, req.GetDeviceId()); err != nil {
		errMsg := err.Error()
		return &agentpb.SetDefaultAudioDeviceResponse{Success: false, ErrorMessage: &errMsg}, nil
	}

	s.logger.Info("Default audio device set",
		zap.Uint32("device_id", req.GetDeviceId()),
		zap.String("node", node.Name),
		zap.Bool("sink", node.IsSink))
	return &agentpb.SetDefaultAudioDeviceResponse{Success: true}, nil
}

// captureSource names where startCapture should read audio from: a PipeWire
// object (pwTarget, pw-record's --target) or a raw ALSA device (alsaDevice,
// arecord's -D). Exactly one of the two is set.
type captureSource struct {
	pwTarget   string
	alsaDevice string
}

// captureTarget resolves a request's device ID to a capture source, using
// PipeWire when a session is up and raw ALSA otherwise. Device ID 0 means
// "unspecified": prefer the default source when one exists (PipeWire only),
// otherwise take the first input available.
func captureTarget(ctx context.Context, deviceID uint32) (captureSource, error) {
	if !audio.Available() {
		return alsaCaptureTarget(ctx, deviceID)
	}

	nodes, defaults, err := audio.ListNodes(ctx)
	if err != nil {
		return captureSource{}, status.Errorf(codes.Internal, "failed to enumerate audio devices: %v", err)
	}

	if deviceID != 0 {
		node, ok := audio.FindNode(nodes, deviceID)
		if !ok {
			return captureSource{}, status.Errorf(codes.NotFound, "no audio device with ID %d", deviceID)
		}
		if node.IsSink {
			return captureSource{}, status.Errorf(codes.InvalidArgument,
				"device %d (%s) is an output; capture needs an input", deviceID, node.Name)
		}
		return captureSource{pwTarget: targetArg(node)}, nil
	}

	var first *audio.Node
	for i, n := range nodes {
		if n.IsSink {
			continue
		}
		if n.Name == defaults.SourceName {
			return captureSource{pwTarget: targetArg(n)}, nil
		}
		if first == nil {
			first = &nodes[i]
		}
	}
	if first == nil {
		return captureSource{}, status.Error(codes.FailedPrecondition, "no audio input devices available")
	}
	return captureSource{pwTarget: targetArg(*first)}, nil
}

// alsaCaptureTarget is captureTarget's fallback for when no PipeWire session
// is up. ALSA has no default-source concept, so device ID 0 takes the first
// input card found.
func alsaCaptureTarget(ctx context.Context, deviceID uint32) (captureSource, error) {
	nodes, err := audio.ListAlsaNodes(ctx)
	if err != nil {
		return captureSource{}, status.Errorf(codes.Internal, "failed to enumerate audio devices: %v", err)
	}

	if deviceID != 0 {
		// A sensor-link mount row's ID encodes a snd-aloop subdevice that
		// ListAlsaNodes does not enumerate, so resolve it directly to its
		// plughw:<card>,1,<sub> capture handle rather than failing FindNode.
		if sub, ok := audio.AlsaSubdevice(deviceID); ok {
			card, device, _ := audio.DecodeAlsaID(deviceID)
			return captureSource{alsaDevice: fmt.Sprintf("plughw:%d,%d,%d", card, device, sub)}, nil
		}
		node, ok := audio.FindNode(nodes, deviceID)
		if !ok {
			return captureSource{}, status.Errorf(codes.NotFound, "no audio device with ID %d", deviceID)
		}
		if node.IsSink {
			return captureSource{}, status.Errorf(codes.InvalidArgument,
				"device %d (%s) is an output; capture needs an input", deviceID, node.Name)
		}
		return captureSource{alsaDevice: node.Name}, nil
	}

	for _, n := range nodes {
		if !n.IsSink {
			return captureSource{alsaDevice: n.Name}, nil
		}
	}
	return captureSource{}, status.Error(codes.FailedPrecondition, "no audio input devices available")
}

// targetArg renders a node as a --target value. pw-record takes an object
// serial, not the object ID used everywhere else, and falls back to the name.
func targetArg(n audio.Node) string {
	if n.Serial != 0 {
		return fmt.Sprintf("%d", n.Serial)
	}
	return n.Name
}

// capture is a running pw-record or arecord process emitting raw s16le PCM on
// stdout.
type capture struct {
	cmd    *exec.Cmd
	stdout io.ReadCloser
	stderr bytes.Buffer
	reaped sync.Once
}

// startCapture records from src. latency may be empty to leave PipeWire's
// default quantum alone; it has no ALSA equivalent and is ignored there.
// Sample rate and channel bounds are enforced here, not by callers, so every
// present and future caller is protected rather than only the ones that
// happen to validate first.
func startCapture(ctx context.Context, src captureSource, sampleRate, channels uint32, latency string) (*capture, error) {
	if sampleRate < minSampleRate || sampleRate > maxSampleRate {
		return nil, status.Errorf(codes.InvalidArgument,
			"sample rate must be between %d and %d, got %d", minSampleRate, maxSampleRate, sampleRate)
	}
	if channels == 0 || channels > maxChannels {
		return nil, status.Errorf(codes.InvalidArgument,
			"channels must be between 1 and %d, got %d", maxChannels, channels)
	}

	if src.alsaDevice != "" {
		cmd := audio.ArecordCommand(ctx, src.alsaDevice, sampleRate, channels)
		return runCapture(cmd)
	}

	args := []string{
		"--target", src.pwTarget,
		"--rate", strconv.FormatUint(uint64(sampleRate), 10),
		"--channels", strconv.FormatUint(uint64(channels), 10),
		"--format", "s16",
		"--raw",
	}
	if latency != "" {
		args = append(args, "--latency", latency)
	}
	args = append(args, "-")

	cmd, err := audio.Command(ctx, "pw-record", args...)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "%v", err)
	}
	return runCapture(cmd)
}

// runCapture starts cmd and wires up its stdout/stderr for capture, common to
// both the pw-record and arecord code paths.
func runCapture(cmd *exec.Cmd) (*capture, error) {
	c := &capture{cmd: cmd}
	cmd.Stderr = &c.stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to create audio pipe: %v", err)
	}
	c.stdout = stdout
	if err := cmd.Start(); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to start audio capture: %v", err)
	}
	return c, nil
}

// Err reaps the process and returns anything it printed. Reading the buffer
// before the reap would race with the child's writes, so stderr is only
// reachable through here.
func (c *capture) Err() string {
	c.reaped.Do(func() { _ = c.cmd.Wait() })
	return strings.TrimSpace(c.stderr.String())
}

// Close stops the capture.
func (c *capture) Close() {
	_ = c.cmd.Process.Kill()
	c.reaped.Do(func() { _ = c.cmd.Wait() })
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

	s.pipewireUnavailable("StreamAudioLevels")

	target, err := captureTarget(ctx, req.GetDeviceId())
	if err != nil {
		return err
	}

	rec, err := startCapture(ctx, target, 48000, 1, "")
	if err != nil {
		return err
	}
	defer rec.Close()

	interval := time.Second / time.Duration(rateHz)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	buf := make([]byte, 48000*2/int(rateHz)) // samples per interval * 2 bytes per sample

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			n, err := rec.stdout.Read(buf)
			if err != nil {
				if msg := rec.Err(); msg != "" {
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

// Bounds on the capture format. The lower rate bound also keeps the 10ms chunk
// below from rounding to zero samples.
const (
	minSampleRate = 8000
	maxSampleRate = 192000
	maxChannels   = 8
)

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
	// Bounds on sampleRate/channels are enforced by startCapture, not here, so
	// every caller of startCapture is protected uniformly.

	s.pipewireUnavailable("StreamAudio")

	target, err := captureTarget(ctx, req.GetDeviceId())
	if err != nil {
		return err
	}

	// A 10ms graph latency matches the chunk size below.
	rec, err := startCapture(ctx, target, sampleRate, channels, "10ms")
	if err != nil {
		return err
	}
	defer rec.Close()

	// Send ~10ms chunks of PCM data to keep per-chunk latency low.
	chunkSamples := sampleRate / 100 // 10ms worth of samples
	chunkBytes := chunkSamples * channels * 2
	buf := make([]byte, chunkBytes)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		n, err := rec.stdout.Read(buf)
		if err != nil {
			if msg := rec.Err(); msg != "" {
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
