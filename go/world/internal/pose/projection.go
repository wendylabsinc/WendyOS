// Package pose projects image-derived ankles into an explicitly calibrated
// World floor. It does not identify people or infer a floor from image bounds.
package pose

import (
	"errors"
	"fmt"
	"math"

	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
)

type Keypoint struct {
	X          float64 `json:"x"`
	Y          float64 `json:"y"`
	Confidence float64 `json:"confidence"`
}
type Person struct {
	Score      float64  `json:"score"`
	LeftAnkle  Keypoint `json:"left_ankle"`
	RightAnkle Keypoint `json:"right_ankle"`
}
type ImageResult struct {
	ModelID      string   `json:"model_id"`
	ModelVersion string   `json:"model_version"`
	ModelSHA256  string   `json:"model_sha256"`
	ImageSHA256  string   `json:"image_sha256"`
	ImageWidth   int      `json:"image_width"`
	ImageHeight  int      `json:"image_height"`
	Detections   []Person `json:"detections"`
}
type Config struct {
	FloorY             float64
	AnkleHeightM       float64
	FloorUncertaintyM  float64
	CameraUncertaintyM float64
	PixelUncertainty   float64
	MinConfidence      float64
	MaxRangeM          float64
}
type Detection struct {
	MemberIndex uint32     `json:"member_index"`
	PositionXZ  [2]float64 `json:"position_xz"`
	Covariance  [4]float64 `json:"covariance"`
}
type Rejection struct {
	MemberIndex uint32 `json:"member_index"`
	Reason      string `json:"reason"`
}
type Result struct {
	Detections []Detection `json:"detections"`
	Rejected   []Rejection `json:"rejected"`
}

func finite(v float64) bool      { return !math.IsNaN(v) && !math.IsInf(v, 0) }
func probability(v float64) bool { return finite(v) && v >= 0 && v <= 1 }

func validateCamera(c sensing.Camera) error {
	if c.Width < 1 || c.Height < 1 || c.Width > 4096 || c.Height > 4096 || !finite(c.FX) || !finite(c.FY) || c.FX <= 0 || c.FY <= 0 || !finite(c.CX) || !finite(c.CY) || c.CX < 0 || c.CX > float64(c.Width) || c.CY < 0 || c.CY > float64(c.Height) || len(c.CameraToSite) != 16 {
		return errors.New("invalid camera intrinsics or dimensions")
	}
	m := c.CameraToSite
	for _, x := range m {
		if !finite(x) {
			return errors.New("nonfinite camera transform")
		}
	}
	for i, want := range []float64{0, 0, 0, 1} {
		if math.Abs(m[12+i]-want) > 1e-9 {
			return errors.New("invalid homogeneous transform")
		}
	}
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			dot, want := 0.0, 0.0
			for k := 0; k < 3; k++ {
				dot += m[i*4+k] * m[j*4+k]
			}
			if i == j {
				want = 1
			}
			if math.Abs(dot-want) > 1e-6 {
				return errors.New("camera rotation must be orthonormal")
			}
		}
	}
	det := m[0]*(m[5]*m[10]-m[6]*m[9]) - m[1]*(m[4]*m[10]-m[6]*m[8]) + m[2]*(m[4]*m[9]-m[5]*m[8])
	if math.Abs(det-1) > 1e-6 {
		return errors.New("camera transform must preserve handedness")
	}
	return nil
}

func floorPoint(c sensing.Camera, x, y, floor, maxRange float64) ([2]float64, error) {
	m := c.CameraToSite
	v := [3]float64{(x - c.CX) / c.FX, (y - c.CY) / c.FY, 1}
	var d [3]float64
	for i := 0; i < 3; i++ {
		d[i] = m[i*4]*v[0] + m[i*4+1]*v[1] + m[i*4+2]*v[2]
	}
	norm := math.Sqrt(d[0]*d[0] + d[1]*d[1] + d[2]*d[2])
	if d[1]/norm >= -.05 || m[7] <= floor {
		return [2]float64{}, errors.New("ankle ray does not constrain the floor")
	}
	t := (floor - m[7]) / d[1]
	if t <= 0 || t*norm > maxRange {
		return [2]float64{}, errors.New("ankle floor intersection outside range")
	}
	return [2]float64{m[3] + t*d[0], m[11] + t*d[2]}, nil
}

// Project requires both detected ankles. It rejects an occluded ankle instead
// of substituting the bottom of a person's bounding box. The covariance is a
// geometric uncertainty approximation, not a calibrated detection confidence.
func Project(camera sensing.Camera, image ImageResult, cfg Config) (Result, error) {
	out := Result{Detections: []Detection{}, Rejected: []Rejection{}}
	if err := validateCamera(camera); err != nil {
		return out, err
	}
	if image.ModelID == "" || image.ModelVersion == "" || !sensing.DigestValid(image.ModelSHA256) || !sensing.DigestValid(image.ImageSHA256) || image.ImageWidth != camera.Width || image.ImageHeight != camera.Height || image.Detections == nil || len(image.Detections) > 64 {
		return out, errors.New("invalid pose image metadata")
	}
	if cfg.PixelUncertainty == 0 {
		cfg.PixelUncertainty = 2
	}
	if cfg.MinConfidence == 0 {
		cfg.MinConfidence = .35
	}
	if cfg.MaxRangeM == 0 {
		cfg.MaxRangeM = 30
	}
	if !finite(cfg.FloorY) || !finite(cfg.AnkleHeightM) || cfg.AnkleHeightM < 0 || cfg.AnkleHeightM > .3 || !finite(cfg.FloorUncertaintyM) || cfg.FloorUncertaintyM < 0 || !finite(cfg.CameraUncertaintyM) || cfg.CameraUncertaintyM < 0 || !finite(cfg.PixelUncertainty) || cfg.PixelUncertainty <= 0 || !probability(cfg.MinConfidence) || !finite(cfg.MaxRangeM) || cfg.MaxRangeM <= 0 {
		return out, errors.New("invalid floor projection configuration")
	}
	for i, p := range image.Detections {
		for _, k := range []Keypoint{p.LeftAnkle, p.RightAnkle} {
			if !finite(k.X) || !finite(k.Y) || !probability(k.Confidence) || !probability(p.Score) {
				return out, fmt.Errorf("person %d has invalid keypoints", i)
			}
		}
		d, reason := projectPerson(camera, p, cfg)
		if reason != "" {
			out.Rejected = append(out.Rejected, Rejection{uint32(i), reason})
			continue
		}
		d.MemberIndex = uint32(i)
		out.Detections = append(out.Detections, d)
	}
	return out, nil
}

func projectPerson(camera sensing.Camera, p Person, cfg Config) (Detection, string) {
	var out Detection
	// Ankles lie above the floor. This explicit anatomical approximation and
	// its uncertainty belong in calibration, never in a simulator truth lookup.
	cfg.FloorY += cfg.AnkleHeightM
	if p.Score < cfg.MinConfidence || p.LeftAnkle.Confidence < cfg.MinConfidence || p.RightAnkle.Confidence < cfg.MinConfidence {
		return out, "both ankles require sufficient confidence"
	}
	points := [2][2]float64{}
	cov := [4]float64{}
	for i, k := range []Keypoint{p.LeftAnkle, p.RightAnkle} {
		if k.X < 0 || k.Y < 0 || k.X >= float64(camera.Width) || k.Y >= float64(camera.Height) {
			return out, "ankle lies outside the captured image"
		}
		point, err := floorPoint(camera, k.X, k.Y, cfg.FloorY, cfg.MaxRangeM)
		if err != nil {
			return out, err.Error()
		}
		points[i] = point
		// Central finite differences retain oblique-view X/Z covariance.
		var jac [3][2]float64
		for axis := 0; axis < 3; axis++ {
			x, y, f, step := k.X, k.Y, cfg.FloorY, 1.0
			if axis == 0 {
				x++
			} else if axis == 1 {
				y++
			} else {
				f += .001
				step = .001
			}
			plus, e1 := floorPoint(camera, x, y, f, cfg.MaxRangeM)
			x, y, f = k.X, k.Y, cfg.FloorY
			if axis == 0 {
				x--
			} else if axis == 1 {
				y--
			} else {
				f -= .001
			}
			minus, e2 := floorPoint(camera, x, y, f, cfg.MaxRangeM)
			if e1 != nil || e2 != nil {
				return out, "ankle projection uncertainty exceeds usable geometry"
			}
			jac[axis] = [2]float64{(plus[0] - minus[0]) / (2 * step), (plus[1] - minus[1]) / (2 * step)}
		}
		sigmas := [3]float64{cfg.PixelUncertainty / k.Confidence, cfg.PixelUncertainty / k.Confidence, cfg.FloorUncertaintyM}
		for axis, v := range jac {
			variance := sigmas[axis] * sigmas[axis]
			cov[0] += .5 * v[0] * v[0] * variance
			cov[1] += .5 * v[0] * v[1] * variance
			cov[3] += .5 * v[1] * v[1] * variance
		}
	}
	if math.Hypot(points[0][0]-points[1][0], points[0][1]-points[1][1]) > 1.2 {
		return out, "projected ankles are inconsistent with a standing person"
	}
	out.PositionXZ = [2]float64{(points[0][0] + points[1][0]) / 2, (points[0][1] + points[1][1]) / 2}
	// Shared calibration error is not reduced by combining two feet.
	variance := cfg.CameraUncertaintyM*cfg.CameraUncertaintyM + .0001
	cov[0] += variance
	cov[3] += variance
	cov[2] = cov[1]
	out.Covariance = cov
	return out, ""
}
