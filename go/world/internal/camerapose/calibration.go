package camerapose

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
	"unicode"

	"github.com/wendylabsinc/wendy/go/world/internal/stationarycamera"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

// CalibrationBinding is an explicit operator declaration of the connected
// camera variant. SensorLink image receipts independently verify dimensions,
// but do not currently attest the board, sensor, lens or hardware identity.
// CaptureMode must distinguish cropping, mirroring, flipping and sensor mode.
type CalibrationBinding struct {
	HardwareID  string `json:"hardware_id"`
	Board       string `json:"board"`
	Sensor      string `json:"sensor"`
	Lens        string `json:"lens"`
	CaptureMode string `json:"capture_mode"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
}

// CalibrationProfileInfo describes the evidence used for a fixed lens. A model
// profile remains a shared calibration, never a measurement of each unit.
type CalibrationProfileInfo struct {
	ID                   string       `json:"id"`
	Name                 string       `json:"name"`
	Scope                string       `json:"scope"`
	Origin               world.Origin `json:"origin"`
	SHA256               string       `json:"sha256"`
	ReferenceDeviceCount int          `json:"reference_device_count"`
	BindingSource        string       `json:"binding_source"`
}

type calibrationProfile struct {
	Schema string       `json:"schema"`
	ID     string       `json:"id"`
	Name   string       `json:"name"`
	Scope  string       `json:"scope"`
	Origin world.Origin `json:"origin"`
	Camera struct {
		Board       string `json:"board"`
		Sensor      string `json:"sensor"`
		Lens        string `json:"lens"`
		CaptureMode string `json:"capture_mode"`
		Width       int    `json:"width"`
		Height      int    `json:"height"`
		HardwareID  string `json:"hardware_id,omitempty"`
	} `json:"camera"`
	Intrinsics struct {
		FX         float64   `json:"fx"`
		FY         float64   `json:"fy"`
		CX         *float64  `json:"cx"`
		CY         *float64  `json:"cy"`
		Distortion []float64 `json:"distortion"`
	} `json:"intrinsics"`
	Evidence struct {
		Method              string    `json:"method"`
		ReferenceDeviceIDs  []string  `json:"reference_device_ids"`
		TrainingViews       int       `json:"training_views"`
		ValidationViews     int       `json:"validation_views"`
		TrainingRMSPixels   *float64  `json:"training_rms_pixels"`
		ValidationRMSPixels *float64  `json:"validation_rms_pixels"`
		DatasetSHA256       string    `json:"dataset_sha256"`
		CalibratedAt        time.Time `json:"calibrated_at"`
	} `json:"evidence"`
}

type calibrationReference struct {
	path    string
	digest  [32]byte
	profile calibrationProfile
	binding CalibrationBinding
	info    CalibrationProfileInfo
	lens    Result
}

func calibrationText(s string, limit int) bool {
	if s == "" || len(s) > limit || strings.TrimSpace(s) != s {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
func validCalibrationDimensions(w, h int) bool {
	return w >= 32 && h >= 32 && w <= 8192 && h <= 8192 && int64(w)*int64(h) <= 32_000_000
}

// Validate checks the declaration's shape. It cannot prove that this is the
// physical hardware connected to the selected transport.
func (b CalibrationBinding) Validate() error {
	if !calibrationText(b.HardwareID, 128) || !calibrationText(b.Board, 128) || !calibrationText(b.Sensor, 128) || !calibrationText(b.Lens, 128) || !calibrationText(b.CaptureMode, 128) || !validCalibrationDimensions(b.Width, b.Height) {
		return errors.New("invalid camera calibration binding")
	}
	return nil
}

func loadCalibration(path string, binding *CalibrationBinding) (*calibrationReference, error) {
	if path == "" {
		if binding != nil {
			return nil, errors.New("camera calibration binding requires a profile")
		}
		return nil, nil
	}
	if binding == nil {
		return nil, errors.New("camera calibration profile requires an explicit camera binding")
	}
	if err := binding.Validate(); err != nil {
		return nil, err
	}
	data, err := readBounded(path, 64<<10)
	if err != nil {
		return nil, errors.New("camera calibration profile is unavailable or exceeds its size limit")
	}
	if err := uniqueJSONFields(data); err != nil {
		return nil, err
	}
	var p calibrationProfile
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&p); err != nil {
		return nil, errors.New("invalid camera calibration profile JSON")
	}
	var extra any
	if err = decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("camera calibration profile must contain one JSON object")
	}
	if p.Schema != "wendy.camera-intrinsics.v1" || !calibrationText(p.ID, 128) || !calibrationText(p.Name, 256) || (p.Scope != "model" && p.Scope != "device") || (p.Origin != world.Measured && p.Origin != world.Simulated) {
		return nil, errors.New("invalid camera calibration profile identity or provenance")
	}
	c := p.Camera
	if !calibrationText(c.Board, 128) || !calibrationText(c.Sensor, 128) || !calibrationText(c.Lens, 128) || !calibrationText(c.CaptureMode, 128) || !validCalibrationDimensions(c.Width, c.Height) {
		return nil, errors.New("invalid camera calibration variant")
	}
	if c.Board != binding.Board || c.Sensor != binding.Sensor || c.Lens != binding.Lens || c.CaptureMode != binding.CaptureMode || c.Width != binding.Width || c.Height != binding.Height {
		return nil, errors.New("camera calibration profile does not match the declared camera variant and capture mode")
	}
	if p.Scope == "device" && (!calibrationText(c.HardwareID, 128) || c.HardwareID != binding.HardwareID) || p.Scope == "model" && c.HardwareID != "" {
		return nil, errors.New("camera calibration profile does not match its hardware scope")
	}
	k := p.Intrinsics
	if !finite(k.FX) || !finite(k.FY) || k.FX < float64(c.Width)*.1 || k.FY < float64(c.Height)*.1 || k.FX > float64(c.Width)*10 || k.FY > float64(c.Height)*10 || k.CX == nil || k.CY == nil || !finite(*k.CX) || !finite(*k.CY) || *k.CX < 0 || *k.CY < 0 || *k.CX > float64(c.Width) || *k.CY > float64(c.Height) || len(k.Distortion) != 5 {
		return nil, errors.New("invalid camera calibration intrinsics")
	}
	for _, v := range k.Distortion {
		if !finite(v) || v < -100 || v > 100 {
			return nil, errors.New("invalid camera calibration distortion")
		}
	}
	e := p.Evidence
	if e.Method != "opencv-checkerboard" || len(e.ReferenceDeviceIDs) < 1 || len(e.ReferenceDeviceIDs) > 64 || e.TrainingViews < 12 || e.TrainingViews > 10000 || e.ValidationViews < 3 || e.ValidationViews > 10000 || e.CalibratedAt.IsZero() || e.TrainingRMSPixels == nil || e.ValidationRMSPixels == nil {
		return nil, errors.New("camera calibration lacks bounded measurement and held-out validation evidence")
	}
	for _, v := range []*float64{e.TrainingRMSPixels, e.ValidationRMSPixels} {
		if !finite(*v) || *v < 0 || *v > 2 {
			return nil, errors.New("camera calibration error exceeds two pixels")
		}
	}
	ids := map[string]bool{}
	for _, id := range e.ReferenceDeviceIDs {
		if !calibrationText(id, 128) || ids[id] {
			return nil, errors.New("invalid calibration reference devices")
		}
		ids[id] = true
	}
	if p.Scope == "device" && (len(ids) != 1 || !ids[c.HardwareID]) {
		return nil, errors.New("device calibration evidence must identify that device")
	}
	digest, err := hex.DecodeString(e.DatasetSHA256)
	if err != nil || len(digest) != 32 || e.DatasetSHA256 != strings.ToLower(e.DatasetSHA256) {
		return nil, errors.New("invalid calibration dataset digest")
	}
	ref := &calibrationReference{path: path, digest: sha256.Sum256(data), profile: p, binding: *binding}
	ref.info = CalibrationProfileInfo{ID: p.ID, Name: p.Name, Scope: p.Scope, Origin: p.Origin, SHA256: hex.EncodeToString(ref.digest[:]), ReferenceDeviceCount: len(ids), BindingSource: "operator"}
	ref.lens = Result{FX: k.FX, FY: k.FY, CX: *k.CX, CY: *k.CY}
	copy(ref.lens.Distortion[:], k.Distortion)
	return ref, nil
}

func cloneCalibrationInfo(p *CalibrationProfileInfo) *CalibrationProfileInfo {
	if p == nil {
		return nil
	}
	out := *p
	return &out
}
func (c *Controller) calibrationUnchanged() bool {
	if c.calibration == nil {
		return true
	}
	b, err := readBounded(c.calibration.path, 64<<10)
	return err == nil && sha256.Sum256(b) == c.calibration.digest
}
func (c *Controller) calibrationMatchesReceipt(r stationarycamera.Receipt) bool {
	if c.calibration == nil {
		return true
	}
	p := c.calibration.profile
	return r.Width == p.Camera.Width && r.Height == p.Camera.Height && (r.Origin != world.Measured || p.Origin == world.Measured)
}
func (c *Controller) calibrationMode() (mode, quality string) {
	if c.calibration.profile.Scope == "model" {
		return "assumed", "model_calibration"
	}
	return "measured", "measured"
}
func sameProfileLens(result, lens Result) bool {
	return result.FX == lens.FX && result.FY == lens.FY && result.CX == lens.CX && result.CY == lens.CY && result.Distortion == lens.Distortion
}

// Duplicate keys can otherwise silently replace calibration provenance or lens
// values. Reject them at every nesting level before decoding the typed schema.
func uniqueJSONFields(data []byte) error {
	d := json.NewDecoder(bytes.NewReader(data))
	var value func(int) error
	value = func(depth int) error {
		if depth > 8 {
			return errors.New("camera calibration JSON exceeds nesting limit")
		}
		token, err := d.Token()
		if err != nil {
			return err
		}
		delimiter, compound := token.(json.Delim)
		if !compound {
			return nil
		}
		switch delimiter {
		case '{':
			names := map[string]bool{}
			for d.More() {
				token, err = d.Token()
				if err != nil {
					return err
				}
				name, ok := token.(string)
				if !ok || names[name] {
					return errors.New("duplicate calibration JSON field")
				}
				names[name] = true
				if err = value(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for d.More() {
				if err = value(depth + 1); err != nil {
					return err
				}
			}
		default:
			return errors.New("invalid camera calibration JSON")
		}
		_, err = d.Token()
		return err
	}
	if err := value(0); err != nil {
		return errors.New("invalid or duplicate camera calibration JSON fields")
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return errors.New("camera calibration profile must contain one JSON object")
	}
	return nil
}
