package services

// Per-camera temperature correction for radiometric thermal cameras.
//
// The TC001 decode is fixed (camera_grid_thermal.go): a count is a sixteenth
// of a kelvin. That is right for the camera wendy-console checked against
// known temperatures (wendy-box-theta: a 21.9 C room, skin at 37.7 C), and
// wrong for others. Some TC001 units read every temperature about twice too
// hot, a room at 45-50 C and a hand at 70-76 C, and the same unit reads double
// on a Jetson and on a Mac, so it is the camera and not the decode. Every
// difference roughly doubles, so neither a different scale (linear in kelvin)
// nor a fixed offset fits it; a line in Celsius does. Until such a camera is
// corrected, any threshold on its temperatures (a 50 C "red", a 150 C flame)
// means nothing.
//
// So each camera may carry its own correction, measured against references
// and never copied from another camera:
//
//	corrected degC = gain * decoded degC + offset
//	decoded degC   = count / 16 - 273.15
//
// This is wendy-console's mac-fire-detector `celsiusCalibration`, with the
// same meaning and the same bounds (gain 0.1..10, offset -100..100 degC), so
// a pair measured for one product works in the other; its THERMAL-ROLLOUT.md
// Phase 1b is the measuring procedure (ice water and boiling water, a
// reference thermometer, the frame's median). The Jetson edge-detector has no
// such term, only FIRE_THERMAL_COUNTS_PER_K=k overriding the scale, which is
// the line gain = 16/k, offset = 273.15 * (16/k - 1).
//
// The correction is kept by the camera's stable identity (video_stable_id.go),
// never by /dev/videoN, which belongs to the boot rather than the camera: a
// correction kept by number would move to whichever camera the kernel numbers
// there next, and a wrong correction is silent. It is stored in
// /var/lib/wendy, beside the camera controls and the network camera registry.
// On WendyOS that directory is on the data partition, so it survives agent
// updates and OS updates; /var/lib/wendy-agent is on the A/B root slot and is
// replaced by an OS update.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/agent/ipcam"
	"github.com/wendylabsinc/wendy/go/internal/agent/ros2camera"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"github.com/wendylabsinc/wendy/go/proto/gen/datapb"
)

const (
	thermalCalibrationPath = "/var/lib/wendy/thermal-calibration.json"

	// The bounds of a correction, mac-fire-detector's: wide enough for any
	// unit measured so far (gain about 0.48 on the unit that reads double),
	// narrow enough that a typo cannot turn a room into a fire or blind the
	// camera.
	thermalCalibrationMinGain   = 0.1
	thermalCalibrationMaxGain   = 10
	thermalCalibrationMaxOffset = 100 // degC, either way
)

// thermalCorrection is one camera's correction as the decoder applies it. The
// zero value is no correction: the camera's own reading.
type thermalCorrection struct {
	calibrated   bool
	gain, offset float64
}

// celsius corrects one decoded temperature.
func (c thermalCorrection) celsius(decoded float64) float64 {
	if !c.calibrated {
		return decoded
	}
	return c.gain*decoded + c.offset
}

// slope is how many corrected degrees one decoded degree is.
func (c thermalCorrection) slope() float64 {
	if !c.calibrated {
		return 1
	}
	return c.gain
}

// proto marks a record's values as corrected, or is nil when they are not.
func (c thermalCorrection) proto() *datapb.Correction {
	if !c.calibrated {
		return nil
	}
	return &datapb.Correction{Gain: c.gain, Offset: c.offset}
}

// validateThermalCorrection refuses a pair outside the bounds rather than
// clamping it: a clamped typo is a different wrong number, applied silently.
func validateThermalCorrection(gain, offset float64) error {
	switch {
	case math.IsNaN(gain) || math.IsInf(gain, 0):
		return fmt.Errorf("gain %v is not a number", gain)
	case gain <= 0:
		return fmt.Errorf("gain %g must be greater than 0: a correction multiplies every temperature by it", gain)
	case gain < thermalCalibrationMinGain || gain > thermalCalibrationMaxGain:
		return fmt.Errorf("gain %g is outside %g..%g; a camera that reads about twice too hot needs about 0.5, one that reads true needs 1. Re-measure rather than widen it",
			gain, float64(thermalCalibrationMinGain), float64(thermalCalibrationMaxGain))
	case math.IsNaN(offset) || math.IsInf(offset, 0):
		return fmt.Errorf("offset %v is not a number", offset)
	case math.Abs(offset) > thermalCalibrationMaxOffset:
		return fmt.Errorf("offset %g °C is outside -%g..%g °C", offset, float64(thermalCalibrationMaxOffset), float64(thermalCalibrationMaxOffset))
	}
	return nil
}

// storedThermalCalibration is one camera's entry in the store's file.
type storedThermalCalibration struct {
	Gain    float64   `json:"gain"`
	Offset  float64   `json:"offset"`
	Updated time.Time `json:"updated"`
}

type thermalCalibrationFile struct {
	// Cameras by stable id, scheme tag included ("by-id:usb-...").
	Cameras map[string]storedThermalCalibration `json:"cameras"`
}

// thermalCalibrationStore keeps the corrections by camera stable id. It reads
// its file on first use, and again on each use after a failed read, so an
// agent that started before the file could be read picks it up once it can.
//
// An unreadable or invalid file fails closed: no thermal camera publishes
// temperatures, because the store cannot tell which of them needs a
// correction, and Set and Clear refuse rather than overwrite the other
// cameras' entries.
type thermalCalibrationStore struct {
	path string
	// generation counts changes, so a decoder notices a new correction with
	// one atomic load per frame.
	generation atomic.Uint64

	mu      sync.Mutex
	loaded  bool
	loadErr error
	byID    map[string]storedThermalCalibration
}

func newThermalCalibrationStore(path string) *thermalCalibrationStore {
	return &thermalCalibrationStore{path: path}
}

// load reads the file unless it has been read. The caller holds s.mu.
func (s *thermalCalibrationStore) load() error {
	if s.loaded {
		return nil
	}
	b, err := os.ReadFile(s.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		s.byID = map[string]storedThermalCalibration{}
	case err != nil:
		s.loadErr = fmt.Errorf("reading thermal calibrations from %s: %w", s.path, err)
		return s.loadErr
	default:
		var f thermalCalibrationFile
		if err := json.Unmarshal(b, &f); err != nil {
			s.loadErr = fmt.Errorf("thermal calibrations in %s are not valid JSON: %w", s.path, err)
			return s.loadErr
		}
		for id, c := range f.Cameras {
			if err := validateThermalCorrection(c.Gain, c.Offset); err != nil {
				s.loadErr = fmt.Errorf("thermal calibration for %s in %s: %w", id, s.path, err)
				return s.loadErr
			}
		}
		s.byID = f.Cameras
		if s.byID == nil {
			s.byID = map[string]storedThermalCalibration{}
		}
	}
	s.loaded, s.loadErr = true, nil
	s.generation.Add(1)
	return nil
}

// get returns a camera's stored correction.
func (s *thermalCalibrationStore) get(stableID string) (storedThermalCalibration, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return storedThermalCalibration{}, false, err
	}
	c, ok := s.byID[stableID]
	return c, ok, nil
}

// set stores a camera's correction, replacing any it had.
func (s *thermalCalibrationStore) set(stableID string, c storedThermalCalibration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return err
	}
	next := make(map[string]storedThermalCalibration, len(s.byID)+1)
	for id, v := range s.byID {
		next[id] = v
	}
	next[stableID] = c
	return s.save(next)
}

// clear removes a camera's correction, reporting whether it had one.
func (s *thermalCalibrationStore) clear(stableID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return false, err
	}
	if _, ok := s.byID[stableID]; !ok {
		return false, nil
	}
	next := make(map[string]storedThermalCalibration, len(s.byID))
	for id, v := range s.byID {
		if id != stableID {
			next[id] = v
		}
	}
	return true, s.save(next)
}

// save writes the corrections atomically and only then makes them current,
// so what the decoders apply is always what the file says. The caller holds
// s.mu. 0o700 and 0o600, as the camera controls: a local user who could
// rewrite a correction could hide a fire from every rule on the grid.
func (s *thermalCalibrationStore) save(next map[string]storedThermalCalibration) error {
	b, err := json.MarshalIndent(thermalCalibrationFile{Cameras: next}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	s.byID = next
	s.generation.Add(1)
	return nil
}

// thermalCorrectionFor is the correction the decoder applies to the camera at
// path, found by the camera's stable id, with the store's generation it was
// read at. A camera with no stable id has no correction: none can have been
// stored for it.
func (s *VideoService) thermalCorrectionFor(path string) (thermalCorrection, uint64, error) {
	gen := s.thermalCalibration.generation.Load()
	stableID := s.stableIDOf(path)
	if stableID == "" {
		return thermalCorrection{}, gen, nil
	}
	c, ok, err := s.thermalCalibration.get(stableID)
	if err != nil || !ok {
		return thermalCorrection{}, gen, err
	}
	return thermalCorrection{calibrated: true, gain: c.Gain, offset: c.Offset}, gen, nil
}

// stableIDOf is the stable id ListVideoDevices reports for the node at path,
// or empty.
func (s *VideoService) stableIDOf(path string) string {
	if s.readStableNames == nil {
		return ""
	}
	return s.readStableNames()[path].stableID()
}

// thermalCamera is the camera a calibration request names.
type thermalCamera struct {
	stableID string
	// path is empty when the camera is not attached, which only a clear by
	// stable id accepts.
	path  string
	devID uint32
}

// resolveThermalCamera finds the camera a request names, as StreamVideo does:
// stable_id wins over device_id. It must be a local camera that offers the
// TC001's stacked mode, the only radiometric mode the agent decodes, and have
// a stable id. With absentOK, a stable_id that names no attached camera is
// returned without a path, so its stored correction can still be cleared.
func (s *VideoService) resolveThermalCamera(deviceID uint32, stableID string, absentOK bool) (thermalCamera, error) {
	if stableID != "" {
		var names map[string]stableNames
		if s.readStableNames != nil {
			names = s.readStableNames()
		}
		devID, ok := resolveStableID(names, stableID)
		if !ok {
			if absentOK {
				return thermalCamera{stableID: stableID}, nil
			}
			return thermalCamera{}, status.Errorf(codes.NotFound,
				"no camera with stable id %q is attached; ListVideoDevices reports %v", stableID, knownStableIDs(names))
		}
		deviceID = devID
	}
	switch {
	case deviceID >= ipcam.IDBandStart && deviceID <= ipcam.IDBandEnd:
		return thermalCamera{}, status.Errorf(codes.InvalidArgument,
			"camera %d is a network camera; only a local radiometric thermal camera has a thermal calibration", deviceID)
	case deviceID >= ros2camera.IDBandStart && deviceID <= ros2camera.IDBandEnd:
		return thermalCamera{}, status.Errorf(codes.InvalidArgument,
			"camera %d is a ROS 2 topic; only a local radiometric thermal camera has a thermal calibration", deviceID)
	case deviceID > maxVideoDeviceID:
		return thermalCamera{}, status.Errorf(codes.InvalidArgument, "device ID out of range")
	}
	path := fmt.Sprintf("/dev/video%d", deviceID)
	if _, ok := gridNodeIdentity(path); !ok {
		return thermalCamera{}, status.Errorf(codes.NotFound, "no camera at %s", path)
	}
	thermal, answered := advertisesTC001StackedMode(path)
	switch {
	case !answered:
		return thermalCamera{}, status.Errorf(codes.Unavailable,
			"camera %d did not answer when asked for its modes; try again", deviceID)
	case !thermal:
		return thermalCamera{}, status.Errorf(codes.FailedPrecondition,
			"camera %d is not a radiometric thermal camera: it does not offer the TOPDON TC001's stacked 512x484 YUYV mode, so the agent publishes no temperatures for it", deviceID)
	}
	id := s.stableIDOf(path)
	if id == "" {
		return thermalCamera{}, status.Errorf(codes.FailedPrecondition,
			"camera %d has no stable identity (no /dev/v4l/by-id or by-path name), and a correction kept by %s would move to another camera after a reboot", deviceID, path)
	}
	return thermalCamera{stableID: id, path: path, devID: deviceID}, nil
}

// calibrationStoreError is the status for a store that cannot be read or
// written.
func calibrationStoreError(err error) error {
	return status.Errorf(codes.FailedPrecondition,
		"%v; the agent publishes no thermal temperatures until it can read the file. Repair or remove it, then set each camera's correction again", err)
}

// thermalCalibrationReply describes cam's stored correction.
func thermalCalibrationReply(cam thermalCamera, c storedThermalCalibration, calibrated bool) *agentpb.ThermalCalibration {
	reply := &agentpb.ThermalCalibration{StableId: cam.stableID, Path: cam.path, DeviceId: cam.devID, Gain: 1}
	if calibrated {
		reply.Calibrated, reply.Gain, reply.Offset = true, c.Gain, c.Offset
		reply.UpdatedUnixNanos = c.Updated.UnixNano()
	}
	return reply
}

// GetThermalCalibration reports a thermal camera's correction.
func (s *VideoService) GetThermalCalibration(_ context.Context, req *agentpb.GetThermalCalibrationRequest) (*agentpb.ThermalCalibration, error) {
	cam, err := s.resolveThermalCamera(req.GetDeviceId(), req.GetStableId(), false)
	if err != nil {
		return nil, err
	}
	c, ok, err := s.thermalCalibration.get(cam.stableID)
	if err != nil {
		return nil, calibrationStoreError(err)
	}
	return thermalCalibrationReply(cam, c, ok), nil
}

// SetThermalCalibration stores a thermal camera's correction. A decoder that
// is running applies it from its next frame.
func (s *VideoService) SetThermalCalibration(_ context.Context, req *agentpb.SetThermalCalibrationRequest) (*agentpb.ThermalCalibration, error) {
	if err := validateThermalCorrection(req.GetGain(), req.GetOffset()); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	cam, err := s.resolveThermalCamera(req.GetDeviceId(), req.GetStableId(), false)
	if err != nil {
		return nil, err
	}
	c := storedThermalCalibration{Gain: req.GetGain(), Offset: req.GetOffset(), Updated: time.Now().UTC()}
	if err := s.thermalCalibration.set(cam.stableID, c); err != nil {
		return nil, calibrationStoreError(err)
	}
	s.logger.Info("thermal calibration set",
		zap.String("camera", cam.stableID), zap.String("device", cam.path),
		zap.Float64("gain", c.Gain), zap.Float64("offset", c.Offset))
	return thermalCalibrationReply(cam, c, true), nil
}

// ClearThermalCalibration removes a thermal camera's correction.
func (s *VideoService) ClearThermalCalibration(_ context.Context, req *agentpb.ClearThermalCalibrationRequest) (*agentpb.ThermalCalibration, error) {
	cam, err := s.resolveThermalCamera(req.GetDeviceId(), req.GetStableId(), true)
	if err != nil {
		return nil, err
	}
	cleared, err := s.thermalCalibration.clear(cam.stableID)
	if err != nil {
		return nil, calibrationStoreError(err)
	}
	if cleared {
		s.logger.Info("thermal calibration cleared", zap.String("camera", cam.stableID), zap.String("device", cam.path))
	}
	return thermalCalibrationReply(cam, storedThermalCalibration{}, false), nil
}
