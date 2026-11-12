package services

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

const (
	maxMCULoaderSize = 8 << 20
	mcuStatusPrefix  = "wendy-mcu-status "
	mcuStatePath     = "/data/wendy/mcu/state.json"
)

var mcuSHA256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// McuService writes a microcontroller's firmware through the board's helper.
type McuService struct {
	agentpbv2.UnimplementedWendyMcuServiceServer

	logger        *zap.Logger
	stagingDir    string
	statePath     string
	helperTimeout time.Duration
	layouts       func() []mcuLayout
	runHelper     func(ctx context.Context, args ...string) (io.ReadCloser, func() error, error)
	// holdUpdater keeps the agent auto-updater from restarting the agent, and
	// with it the helper, in the middle of a flash.
	holdUpdater func(*zap.Logger) func()
	// stopApp stops the app whose sketch was just replaced: without its sketch
	// it would run on with stale readings and failing calls.
	stopApp func(ctx context.Context, appID string) error
	mu      sync.Mutex
}

// SetAppStopper wires how the previous sketch owner is stopped.
func (s *McuService) SetAppStopper(stop func(ctx context.Context, appID string) error) {
	s.stopApp = stop
}

func NewMcuService(logger *zap.Logger) *McuService {
	return &McuService{
		logger:        logger,
		stagingDir:    "/run/wendy/mcu",
		statePath:     mcuStatePath,
		helperTimeout: 3 * time.Minute,
		layouts:       loadMCULayouts,
		runHelper:     runMCUHelper,
		holdUpdater:   inhibitAutoUpdater,
	}
}

type mcuHelperStatus struct {
	OK            bool   `json:"ok"`
	LoaderWritten bool   `json:"loader_written"`
	SketchWritten bool   `json:"sketch_written"`
	RouterActive  bool   `json:"router_active"`
	Error         string `json:"error"`
}

func parseMCUStatus(line string) (mcuHelperStatus, bool) {
	var st mcuHelperStatus
	payload, ok := strings.CutPrefix(line, mcuStatusPrefix)
	if !ok || json.Unmarshal([]byte(payload), &st) != nil {
		return mcuHelperStatus{}, false
	}
	return st, true
}

type mcuState struct {
	AppID        string `json:"app_id"`
	Layout       string `json:"layout"`
	LoaderSHA256 string `json:"loader_sha256"`
	SketchSHA256 string `json:"sketch_sha256"`
	FlashedAt    string `json:"flashed_at"`
}

type mcuFlashStream = grpc.BidiStreamingServer[agentpbv2.FlashFirmwareRequest, agentpbv2.FlashFirmwareResponse]

func sendMCUProgress(s mcuFlashStream, phase, line string) {
	if s == nil {
		return
	}
	_ = s.Send(&agentpbv2.FlashFirmwareResponse{ResponseType: &agentpbv2.FlashFirmwareResponse_Progress_{
		Progress: &agentpbv2.FlashFirmwareResponse_Progress{Phase: phase, LogLine: line}}})
}

func sendMCUFailure(s mcuFlashStream, msg string) error {
	return s.Send(&agentpbv2.FlashFirmwareResponse{ResponseType: &agentpbv2.FlashFirmwareResponse_Failed_{
		Failed: &agentpbv2.FlashFirmwareResponse_Failed{ErrorMessage: msg}}})
}

// FlashFirmware stages the loader and sketch, checks them, then runs the helper.
// Once started, the helper finishes even if the client goes away: a flash cut
// short leaves the microcontroller without a sketch.
func (s *McuService) FlashFirmware(stream mcuFlashStream) error {
	first, err := stream.Recv()
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "no flash request received: %v", err)
	}
	spec := first.GetSpec()
	if spec == nil {
		return status.Error(codes.InvalidArgument, "first FlashFirmware message must carry a McuFirmwareSpec")
	}
	layout, err := s.validateSpec(spec)
	if err != nil {
		return sendMCUFailure(stream, err.Error())
	}

	loaderPath, sketchPath, cleanup, err := s.stage(stream, spec)
	defer cleanup()
	if err != nil {
		return sendMCUFailure(stream, err.Error())
	}
	if err := checkLoaderELF(loaderPath, layout); err != nil {
		return sendMCUFailure(stream, err.Error())
	}

	// Locked only after staging: an upload is client-paced, and a stalled
	// client must not keep everyone else out.
	if !s.lock(stream.Context(), stream) {
		return sendMCUFailure(stream, "microcontroller is busy with another flash; try again when it finishes")
	}
	defer s.mu.Unlock()
	defer s.holdUpdater(s.logger)()

	ctx, cancel := context.WithTimeout(context.WithoutCancel(stream.Context()), s.helperTimeout)
	defer cancel()
	st, err := s.flash(ctx, stream, layout.Name, loaderPath, sketchPath)
	if err != nil {
		s.logger.Warn("Microcontroller flash failed", zap.String("app_id", spec.GetAppId()), zap.Error(err))
		return sendMCUFailure(stream, err.Error())
	}

	previous := s.recordFlash(spec)
	s.keepFirmware(spec, loaderPath, sketchPath)
	s.stopReplaced(ctx, previous, spec.GetAppId())
	return stream.Send(&agentpbv2.FlashFirmwareResponse{ResponseType: &agentpbv2.FlashFirmwareResponse_Completed_{
		Completed: &agentpbv2.FlashFirmwareResponse_Completed{
			LoaderWritten: st.LoaderWritten,
			SketchWritten: st.SketchWritten,
			RouterActive:  st.RouterActive,
			PreviousAppId: previous,
		}}})
}

// lock waits for a flash in progress, such as one a cancelled run left behind,
// for as long as that flash can take. It gives up when the client goes away.
func (s *McuService) lock(ctx context.Context, stream mcuFlashStream) bool {
	if s.mu.TryLock() {
		return true
	}
	sendMCUProgress(stream, "flashing", "waiting for another flash to finish")
	deadline := time.NewTimer(s.helperTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			if s.mu.TryLock() {
				return true
			}
		case <-deadline.C:
			return false
		case <-ctx.Done():
			return false
		}
	}
}

func (s *McuService) validateSpec(spec *agentpbv2.McuFirmwareSpec) (mcuLayout, error) {
	layout, ok := findMCULayout(s.layouts(), spec.GetLayout())
	if !ok {
		return mcuLayout{}, fmt.Errorf("this device cannot flash microcontroller layout %q", spec.GetLayout())
	}
	// The id names the app's firmware folder, so it must be a real app id.
	if err := appconfig.ValidateAppID(spec.GetAppId()); err != nil {
		return mcuLayout{}, fmt.Errorf("app_id: %w", err)
	}
	if n := spec.GetLoader().GetSize(); n == 0 || n > maxMCULoaderSize {
		return mcuLayout{}, fmt.Errorf("loader size %d is outside 1..%d bytes", n, maxMCULoaderSize)
	}
	if n, max := spec.GetSketch().GetSize(), layout.SketchEnd-layout.SketchStart; n == 0 || n > max {
		return mcuLayout{}, fmt.Errorf("sketch size %d is outside 1..%d bytes for layout %s", n, max, layout.Name)
	}
	for name, img := range map[string]*agentpbv2.McuImage{"loader": spec.GetLoader(), "sketch": spec.GetSketch()} {
		if !mcuSHA256Pattern.MatchString(img.GetSha256()) {
			return mcuLayout{}, fmt.Errorf("%s sha256 must be 64 lowercase hex characters", name)
		}
	}
	return layout, nil
}

// stage writes the loader then the sketch from the chunk stream, and checks
// both sizes and hashes before anything reaches the helper.
func (s *McuService) stage(stream mcuFlashStream, spec *agentpbv2.McuFirmwareSpec) (string, string, func(), error) {
	dir := s.stagingDir
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", func() {}, fmt.Errorf("staging: %w", err)
	}
	var paths []string
	cleanup := func() {
		for _, p := range paths {
			_ = os.Remove(p)
		}
	}
	create := func(pattern string) (*os.File, error) {
		f, err := os.CreateTemp(dir, pattern)
		if err == nil {
			paths = append(paths, f.Name())
		}
		return f, err
	}
	loader, err := create("loader-*.elf")
	if err != nil {
		return "", "", cleanup, fmt.Errorf("staging: %w", err)
	}
	defer loader.Close()
	sketch, err := create("sketch-*.bin")
	if err != nil {
		return "", "", cleanup, fmt.Errorf("staging: %w", err)
	}
	defer sketch.Close()

	images := []struct {
		name string
		f    *os.File
		want *agentpbv2.McuImage
		left uint64
		h    hash.Hash
	}{
		{"loader", loader, spec.GetLoader(), spec.GetLoader().GetSize(), sha256.New()},
		{"sketch", sketch, spec.GetSketch(), spec.GetSketch().GetSize(), sha256.New()},
	}
	cur := 0
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", "", cleanup, fmt.Errorf("receiving firmware: %w", err)
		}
		data := msg.GetChunk().GetData()
		for len(data) > 0 {
			if cur == len(images) {
				return "", "", cleanup, errors.New("received more bytes than the spec declares")
			}
			img := &images[cur]
			n := min(uint64(len(data)), img.left)
			if _, err := img.f.Write(data[:n]); err != nil {
				return "", "", cleanup, fmt.Errorf("staging %s: %w", img.name, err)
			}
			img.h.Write(data[:n])
			img.left -= n
			data = data[n:]
			if img.left == 0 {
				cur++
			}
		}
	}
	for _, img := range images {
		if img.left != 0 {
			return "", "", cleanup, fmt.Errorf("%s ended %d bytes short", img.name, img.left)
		}
		if got := hex.EncodeToString(img.h.Sum(nil)); got != img.want.GetSha256() {
			return "", "", cleanup, fmt.Errorf("%s sha256 mismatch: got %s", img.name, got)
		}
	}
	return loader.Name(), sketch.Name(), cleanup, nil
}

func (s *McuService) flash(ctx context.Context, stream mcuFlashStream, layout, loader, sketch string) (mcuHelperStatus, error) {
	out, wait, err := s.runHelper(ctx, layout, loader, sketch)
	if err != nil {
		return mcuHelperStatus{}, fmt.Errorf("starting the flash helper: %w", err)
	}
	var st mcuHelperStatus
	seen := false
	scanner := bufio.NewScanner(out)
	for scanner.Scan() {
		line := scanner.Text()
		if parsed, ok := parseMCUStatus(line); ok {
			st, seen = parsed, true
			continue
		}
		sendMCUProgress(stream, "flashing", line)
	}
	_ = out.Close()
	waitErr := wait()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		s.resume(layout)
		return st, fmt.Errorf("microcontroller flash timed out after %s; run again", s.helperTimeout)
	}
	switch {
	case seen && !st.OK:
		return st, errors.New(st.Error)
	case waitErr != nil:
		return st, fmt.Errorf("flash helper: %w", waitErr)
	case !seen:
		return st, errors.New("flash helper reported no status")
	}
	return st, nil
}

// resume restarts the MCU after the helper was killed mid-flash: a killed helper
// cannot do it itself, and a halted core runs no sketch until the next flash.
func (s *McuService) resume(layout string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, wait, err := s.runHelper(ctx, "--resume", layout)
	if err == nil {
		_, _ = io.Copy(io.Discard, out)
		_ = out.Close()
		err = wait()
	}
	if err != nil {
		s.logger.Warn("Could not restart the microcontroller after a timed-out flash", zap.Error(err))
	}
}

// recordFlash notes which app owns the sketch now and returns the previous owner.
func (s *McuService) recordFlash(spec *agentpbv2.McuFirmwareSpec) string {
	return s.recordOwner(mcuState{
		AppID:        spec.GetAppId(),
		Layout:       spec.GetLayout(),
		LoaderSHA256: spec.GetLoader().GetSha256(),
		SketchSHA256: spec.GetSketch().GetSha256(),
	})
}

func (s *McuService) recordOwner(next mcuState) string {
	prev, _ := s.readState()
	next.FlashedAt = time.Now().UTC().Format(time.RFC3339)
	if err := writeMCUState(s.statePath, next); err != nil {
		s.logger.Warn("Failed to record microcontroller flash", zap.Error(err))
	}
	return prev.AppID
}

func writeMCUState(path string, st mcuState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
