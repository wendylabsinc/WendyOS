package realsense

import "fmt"

// Intrinsics is a pinhole model for one stream at one resolution. Distortion
// follows librealsense's Brown-Conrady ordering: k1, k2, p1, p2, k3. A zero
// array means the stream is rectified (depth always is; D435i colour ships
// zeros too, but nothing here may assume that).
type Intrinsics struct {
	Width, Height uint32
	Fx, Fy        float64
	Ppx, Ppy      float64
	Distortion    [5]float64
	BrownConrady  bool // distortion model confirmed by the table
}

// Extrinsics maps points between two sensor frames: P_dst = R·P_src + T.
// R is row-major here (the wire format is column-major; parsing converts),
// T is metres.
type Extrinsics struct {
	Rotation    [9]float64
	Translation [3]float64
}

// Inverse returns the opposite direction: Rᵀ, −Rᵀt.
func (e Extrinsics) Inverse() Extrinsics {
	var inv Extrinsics
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			inv.Rotation[i*3+j] = e.Rotation[j*3+i]
			inv.Translation[i] -= e.Rotation[j*3+i] * e.Translation[j]
		}
	}
	return inv
}

// Calibration is everything the factory burned in, plus the one runtime
// value (depth units) that is writable and therefore read per capture start,
// never cached across captures.
type Calibration struct {
	// Identity, from GVD.
	FirmwareVersion string
	Serial          string
	HasRGB          bool

	// DepthScaleM is metres per Z16 unit (0.001 for a stock D435i). It comes
	// from the advanced-mode depth-units control when advanced mode is on, and
	// the model's fixed unit otherwise (1000 µm; 100 µm on a D405). Zero means
	// unreadable, and a zero-scale calibration must be refused, never defaulted.
	DepthScaleM float64

	// BaselineMM is the stereo baseline from the coefficients table, in
	// millimetres, negative per the wire convention (right-imager reference).
	BaselineMM float64

	// Depth intrinsics per advertised rectified resolution, from the
	// coefficients table (cross-checked against RECPARAMSGET where the
	// firmware provides it).
	Depth []Intrinsics

	// Colour intrinsics at the calibration resolution. Intrinsics for other
	// colour modes are derived with ColourIntrinsicsFor, which applies the
	// SDK's scale/crop rule.
	Colour Intrinsics

	// DepthToColour maps depth-frame points into the colour frame;
	// ColourToDepth is its inverse, both from table 32.
	DepthToColour Extrinsics
	ColourToDepth Extrinsics
}

// DepthIntrinsicsFor returns the depth intrinsics the camera calibrated for
// exactly this resolution. There is no interpolation: a resolution the table
// does not name is a refusal, not an estimate.
func (c *Calibration) DepthIntrinsicsFor(width, height uint32) (Intrinsics, error) {
	for _, in := range c.Depth {
		if in.Width == width && in.Height == height {
			return in, nil
		}
	}
	return Intrinsics{}, fmt.Errorf("realsense: no factory depth intrinsics for %dx%d", width, height)
}

// ColourIntrinsicsFor derives colour intrinsics for a requested mode from the
// calibration-resolution model, per librealsense's normalised scale/crop rule
// (get_intrinsic_by_resolution, RECT_RESOLUTIONS path for RGB): the stored
// model is normalised to [-1,1]; a mode with a different aspect ratio crops
// the longer axis.
func (c *Calibration) ColourIntrinsicsFor(width, height uint32) (Intrinsics, error) {
	if width == 0 || height == 0 {
		return Intrinsics{}, fmt.Errorf("realsense: colour intrinsics for %dx%d", width, height)
	}
	cw, ch := float64(c.Colour.Width), float64(c.Colour.Height)
	if cw == 0 || ch == 0 {
		return Intrinsics{}, fmt.Errorf("realsense: calibration carries no colour resolution")
	}
	// Reconstruct the normalised model from the calibration-resolution one.
	nfx := c.Colour.Fx * 2 / cw
	nfy := c.Colour.Fy * 2 / ch
	nppx := c.Colour.Ppx*2/cw - 1
	nppy := c.Colour.Ppy*2/ch - 1

	w, h := float64(width), float64(height)
	calibAR := ch / cw
	actualAR := h / w
	if actualAR < calibAR {
		nfy *= calibAR / actualAR
		nppy *= calibAR / actualAR
	} else if actualAR > calibAR {
		nfx *= actualAR / calibAR
		nppx *= actualAR / calibAR
	}
	return Intrinsics{
		Width: width, Height: height,
		Fx: nfx * w / 2, Fy: nfy * h / 2,
		Ppx: (1 + nppx) * w / 2, Ppy: (1 + nppy) * h / 2,
		Distortion:   c.Colour.Distortion,
		BrownConrady: c.Colour.BrownConrady,
	}, nil
}
