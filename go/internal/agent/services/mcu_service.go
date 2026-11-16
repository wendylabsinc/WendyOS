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
	agentpbv2.UnimplementedWendyArduinoMcuServiceServer

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
	// ContentOK says both images are on the chip, verified. It also holds on
	// the one failure that leaves them there: the core does not run.
	ContentOK bool `json:"content_ok"`
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
	// ContentUnknown marks a flash that was cut short: AppID is the last
	// known owner, but its sketch may be gone.
	ContentUnknown bool `json:"content_unknown,omitempty"`
}

type mcuFlashStream = grpc.BidiStreamingServer[agentpbv2.FlashSketchRequest, agentpbv2.FlashSketchResponse]

func sendMCUProgress(s mcuFlashStream, phase, line string) {
	if s == nil {
		return
	}
	_ = s.Send(&agentpbv2.FlashSketchResponse{ResponseType: &agentpbv2.FlashSketchResponse_Progress_{
		Progress: &agentpbv2.FlashSketchResponse_Progress{Phase: phase, LogLine: line}}})
}

func sendMCUFailure(s mcuFlashStream, msg string) error {
	return s.Send(&agentpbv2.FlashSketchResponse{ResponseType: &agentpbv2.FlashSketchResponse_Failed_{
		Failed: &agentpbv2.FlashSketchResponse_Failed{ErrorMessage: msg}}})
}

// FlashSketch stages the loader and sketch, checks them, then runs the helper.
// Once started, the helper finishes even if the client goes away: a flash cut
// short leaves the microcontroller without a sketch.
func (s *McuService) FlashSketch(stream mcuFlashStream) error {
	first, err := stream.Recv()
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "no flash request received: %v", err)
	}
	spec := first.GetSpec()
	if spec == nil {
		return status.Error(codes.InvalidArgument, "first FlashSketch message must carry an ArduinoSketchSpec")
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

	st, previous, written, err := s.flashLocked(stream, spec, layout.Name, loaderPath, sketchPath)
	if written {
		// Without a record of this flash, the record cannot say who owns the chip.
		s.stopReplaced(context.WithoutCancel(stream.Context()), previous, spec.GetAppId(), errors.Is(err, errOwnerNotRecorded))
	}
	if err != nil {
		s.logger.Warn("Microcontroller flash failed", zap.String("app_id", spec.GetAppId()), zap.Bool("written", written), zap.Error(err))
		return sendMCUFailure(stream, err.Error())
	}
	return stream.Send(&agentpbv2.FlashSketchResponse{ResponseType: &agentpbv2.FlashSketchResponse_Completed_{
		Completed: &agentpbv2.FlashSketchResponse_Completed{
			LoaderWritten: st.LoaderWritten,
			SketchWritten: st.SketchWritten,
			RouterActive:  st.RouterActive,
			PreviousAppId: previous,
		}}})
}

var errOwnerNotRecorded = errors.New("the sketch was written, but the device could not record its owner")

// flashLocked runs the helper under the flash lock. written reports that the
// chip now holds this app's images, even if the helper then failed (written,
// but the core does not run): the previous owner's sketch is gone either way.
func (s *McuService) flashLocked(stream mcuFlashStream, spec *agentpbv2.ArduinoSketchSpec, layout, loaderPath, sketchPath string) (st mcuHelperStatus, previous string, written bool, err error) {
	// Locked only after staging: an upload is client-paced, and a stalled
	// client must not keep everyone else out.
	if !s.lock(stream.Context(), stream) {
		return st, "", false, errors.New("microcontroller is busy with another flash; try again when it finishes")
	}
	defer s.mu.Unlock()
	defer s.holdUpdater(s.logger)()

	ctx, cancel := context.WithTimeout(context.WithoutCancel(stream.Context()), s.helperTimeout)
	defer cancel()
	st, err = s.flash(ctx, stream, layout, loaderPath, sketchPath)
	// ContentOK on a failure: the images are on the chip, only the core does not run.
	if err != nil && !st.ContentOK {
		return st, "", false, err
	}
	previous, recErr := s.recordFlash(spec)
	if recErr != nil {
		return st, previous, true, fmt.Errorf("%w: %w", errOwnerNotRecorded, recErr)
	}
	// The previous owner too: one flashed by an older agent has no marker yet.
	s.markSketchApp(spec.GetAppId())
	if previous != "" {
		s.markSketchApp(previous)
	}
	return st, previous, true, err
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

func (s *McuService) validateSpec(spec *agentpbv2.ArduinoSketchSpec) (mcuLayout, error) {
	layout, ok := findMCULayout(s.layouts(), spec.GetLayout())
	if !ok {
		return mcuLayout{}, fmt.Errorf("this device cannot flash microcontroller layout %q", spec.GetLayout())
	}
	// The id names the app's marker file, so it must be a real app id.
	if err := appconfig.ValidateAppID(spec.GetAppId()); err != nil || len(spec.GetAppId()) > 200 {
		return mcuLayout{}, fmt.Errorf("app_id is not a usable app id: %q", spec.GetAppId())
	}
	if n := spec.GetLoader().GetSize(); n == 0 || n > maxMCULoaderSize {
		return mcuLayout{}, fmt.Errorf("loader size %d is outside 1..%d bytes", n, maxMCULoaderSize)
	}
	if n, max := spec.GetSketch().GetSize(), layout.SketchEnd-layout.SketchStart; n == 0 || n > max {
		return mcuLayout{}, fmt.Errorf("sketch size %d is outside 1..%d bytes for layout %s", n, max, layout.Name)
	}
	for name, img := range map[string]*agentpbv2.ArduinoImage{"loader": spec.GetLoader(), "sketch": spec.GetSketch()} {
		if !mcuSHA256Pattern.MatchString(img.GetSha256()) {
			return mcuLayout{}, fmt.Errorf("%s sha256 must be 64 lowercase hex characters", name)
		}
	}
	return layout, nil
}

// stage writes the loader then the sketch from the chunk stream, and checks
// both sizes and hashes before anything reaches the helper.
func (s *McuService) stage(stream mcuFlashStream, spec *agentpbv2.ArduinoSketchSpec) (string, string, func(), error) {
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
		want *agentpbv2.ArduinoImage
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
	// Written ahead: a helper kill, an agent crash or a power cut mid-flash
	// must leave the record saying the owner's sketch may be gone.
	before, owned := s.readState()
	s.markContentUnknown()
	st, touched, err := s.runFlashHelper(ctx, stream, layout, loader, sketch)
	if err != nil && !touched && owned {
		if werr := writeMCUState(s.statePath, before); werr != nil {
			s.logger.Warn("Failed to restore the microcontroller owner record", zap.Error(werr))
		}
	}
	return st, err
}

// runFlashHelper runs the helper. touched is false only when the helper said
// it wrote nothing; a helper that ends without saying so may have.
func (s *McuService) runFlashHelper(ctx context.Context, stream mcuFlashStream, layout, loader, sketch string) (st mcuHelperStatus, touched bool, err error) {
	out, wait, err := s.runHelper(ctx, layout, loader, sketch)
	if err != nil {
		return st, false, fmt.Errorf("starting the flash helper: %w", err)
	}
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
	touched = !seen || st.LoaderWritten || st.SketchWritten || st.ContentOK
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		s.resume(layout)
		return st, true, fmt.Errorf("microcontroller flash timed out after %s; run again", s.helperTimeout)
	}
	switch {
	case !seen && waitErr != nil:
		return st, true, fmt.Errorf("flash helper: %w", waitErr)
	case !seen:
		return st, true, errors.New("flash helper reported no status")
	case !st.OK:
		return st, touched, errors.New(st.Error)
	case waitErr != nil:
		return st, touched, fmt.Errorf("flash helper: %w", waitErr)
	}
	return st, touched, nil
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
func (s *McuService) recordFlash(spec *agentpbv2.ArduinoSketchSpec) (string, error) {
	return s.recordOwner(mcuState{
		AppID:        spec.GetAppId(),
		Layout:       spec.GetLayout(),
		LoaderSHA256: spec.GetLoader().GetSha256(),
		SketchSHA256: spec.GetSketch().GetSha256(),
	})
}

// markContentUnknown records that the chip may no longer hold the owner's
// sketch, so that app's next start is refused. The owner stays on
// record: the next flash by another app still has to stop it.
func (s *McuService) markContentUnknown() {
	st, owned := s.readState()
	if !owned {
		return
	}
	st.ContentUnknown = true
	if err := writeMCUState(s.statePath, st); err != nil {
		s.logger.Warn("Failed to record an interrupted microcontroller flash", zap.Error(err))
	}
}

func (s *McuService) recordOwner(next mcuState) (string, error) {
	prev, _ := s.readState()
	next.FlashedAt = time.Now().UTC().Format(time.RFC3339)
	return prev.AppID, writeMCUState(s.statePath, next)
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
