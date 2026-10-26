package realsense

import (
	"encoding/binary"
	"fmt"
	"math"
)

// AlignDepthToColour reprojects a Z16 depth plane onto the colour camera's
// pixel grid, so depth[y][x] describes colour[y][x] — the property the
// calibrated-frame contract calls ALIGNED_TO_COLOUR. Ported from Apache-2.0
// librealsense (src/proc/align.cpp, align_images): each depth pixel's two
// half-pixel corners are deprojected at its Z, transformed by the
// depth→colour extrinsics and projected into the colour model; the covered
// rectangle keeps the nearest Z, so a foreground object occludes what is
// behind it instead of averaging with it.
//
// depth is row-major little-endian uint16 in device units; units are
// preserved (alignment moves measurements, it never rescales them). Zero
// stays "no data". The output is a fresh colour-geometry plane; pixels no
// depth ray lands on remain zero.
func AlignDepthToColour(depth []byte, di, ci Intrinsics, depthToColour Extrinsics) ([]byte, error) {
	if want := int(di.Width) * int(di.Height) * 2; len(depth) != want {
		return nil, fmt.Errorf("realsense: depth plane is %d bytes, want %d for %dx%d",
			len(depth), want, di.Width, di.Height)
	}
	if di.Distortion != [5]float64{} {
		// Rectified depth has no distortion by construction; a model that
		// claims some is a parsing or pairing mistake upstream.
		return nil, fmt.Errorf("realsense: depth intrinsics carry distortion; refusing to align")
	}
	cw, ch := int(ci.Width), int(ci.Height)
	out := make([]byte, cw*ch*2)

	for y := 0; y < int(di.Height); y++ {
		for x := 0; x < int(di.Width); x++ {
			z := binary.LittleEndian.Uint16(depth[(y*int(di.Width)+x)*2:])
			if z == 0 {
				continue
			}
			zf := float64(z)

			// Both half-pixel corners, deprojected at this Z (depth is
			// rectified: a pure pinhole), through the extrinsics, into
			// colour pixels.
			x0, y0 := projectToColour(float64(x)-0.5, float64(y)-0.5, zf, di, ci, depthToColour)
			x1, y1 := projectToColour(float64(x)+0.5, float64(y)+0.5, zf, di, ci, depthToColour)

			// Round to the nearest target pixel and clamp to the plane.
			ix0, iy0 := int(x0+0.5), int(y0+0.5)
			ix1, iy1 := int(x1+0.5), int(y1+0.5)
			if ix0 < 0 {
				ix0 = 0
			}
			if iy0 < 0 {
				iy0 = 0
			}
			if ix1 >= cw {
				ix1 = cw - 1
			}
			if iy1 >= ch {
				iy1 = ch - 1
			}
			for ty := iy0; ty <= iy1; ty++ {
				for tx := ix0; tx <= ix1; tx++ {
					off := (ty*cw + tx) * 2
					if cur := binary.LittleEndian.Uint16(out[off:]); cur == 0 || z < cur {
						binary.LittleEndian.PutUint16(out[off:], z)
					}
				}
			}
		}
	}
	return out, nil
}

// projectToColour carries one depth-frame point (pixel px,py at depth z, in
// device units) into a colour pixel coordinate: deproject (pinhole),
// transform (row-major R, T scaled to the same units as z), project
// (Brown-Conrady when the colour model has coefficients). The translation is
// in metres; z is in device units, so T must be expressed in Z units by the
// caller — see the unitsPerMetre handling in AlignDepthToColour's wrapper.
func projectToColour(px, py, z float64, di, ci Intrinsics, e Extrinsics) (float64, float64) {
	// Deproject out of the rectified depth model.
	X := (px - di.Ppx) / di.Fx * z
	Y := (py - di.Ppy) / di.Fy * z
	Z := z

	// Transform into the colour frame.
	tx := e.Rotation[0]*X + e.Rotation[1]*Y + e.Rotation[2]*Z + e.Translation[0]
	ty := e.Rotation[3]*X + e.Rotation[4]*Y + e.Rotation[5]*Z + e.Translation[1]
	tz := e.Rotation[6]*X + e.Rotation[7]*Y + e.Rotation[8]*Z + e.Translation[2]
	if tz <= 0 {
		return math.Inf(-1), math.Inf(-1) // behind the colour camera: lands nowhere
	}

	// Project into the colour model.
	xn, yn := tx/tz, ty/tz
	if d := ci.Distortion; d != [5]float64{} {
		// Brown-Conrady, librealsense coefficient order k1 k2 p1 p2 k3.
		r2 := xn*xn + yn*yn
		f := 1 + d[0]*r2 + d[1]*r2*r2 + d[4]*r2*r2*r2
		xd := xn*f + 2*d[2]*xn*yn + d[3]*(r2+2*xn*xn)
		yd := yn*f + 2*d[3]*xn*yn + d[2]*(r2+2*yn*yn)
		xn, yn = xd, yd
	}
	return xn*ci.Fx + ci.Ppx, yn*ci.Fy + ci.Ppy
}

// AlignFrame is the capture-path entry: it converts the metre-denominated
// extrinsics into Z units once (the translation is factory data in metres;
// the Z plane is device units, depthScaleM metres each), then aligns.
func (c *Calibration) AlignFrame(depth []byte, di, ci Intrinsics) ([]byte, error) {
	if c.DepthScaleM <= 0 {
		return nil, fmt.Errorf("realsense: depth scale unknown; a plane in unknown units must be refused, not aligned")
	}
	e := c.DepthToColour
	for i := range e.Translation {
		e.Translation[i] /= c.DepthScaleM
	}
	return AlignDepthToColour(depth, di, ci, e)
}
