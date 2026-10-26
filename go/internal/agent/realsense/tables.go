package realsense

import (
	"encoding/binary"
	"fmt"
	"math"
)

func f32at(b []byte, off int) float64 {
	return float64(math.Float32frombits(binary.LittleEndian.Uint32(b[off:])))
}

// gvdIdentity extracts the device identity from a GVD payload: the firmware
// version is four bytes at offset 12 in reversed order, the serial six raw
// bytes at 48 (printed as hex digits — that *is* the serial number), and RGB
// presence one byte at 174 on payloads long enough to carry it.
func gvdIdentity(payload []byte) (fw, serial string, hasRGB bool, err error) {
	if len(payload) < 54 {
		return "", "", false, fmt.Errorf("realsense: GVD payload is %d bytes, need 54", len(payload))
	}
	fw = fmt.Sprintf("%d.%d.%d.%d", payload[15], payload[14], payload[13], payload[12])
	serial = fmt.Sprintf("%x", payload[48:54])
	if len(payload) > 174 {
		hasRGB = payload[174] != 0
	}
	return fw, serial, hasRGB, nil
}

// rectResolutions is the fixed index→resolution order of the coefficients
// table's rect_params array (librealsense ds_rect_resolutions). Zero entries
// are reserved slots.
var rectResolutions = [16][2]uint32{
	{1920, 1080}, {1280, 720}, {640, 480}, {848, 480},
	{640, 360}, {424, 240}, {320, 240}, {480, 270},
	{1280, 800}, {960, 540}, {0, 0}, {0, 0},
	{640, 400}, {576, 576}, {720, 720}, {1152, 1152},
}

// parseCoefficients reads table 25: stereo baseline (mm, f32 at 160, negative
// per the right-imager convention) and rectified depth intrinsics per
// resolution (float4 entries at 256). Depth is rectified, so distortion is
// zero by construction. minLen 512 is the last byte read here — fw 5.15.1.55
// sends exactly 512, trimming the struct's trailing reserved bytes.
func parseCoefficients(payload []byte) (baselineMM float64, depth []Intrinsics, err error) {
	if _, err := checkTable(payload, 512); err != nil {
		return 0, nil, fmt.Errorf("coefficients table: %w", err)
	}
	baselineMM = f32at(payload, 160)
	for i, wh := range rectResolutions {
		if wh[0] == 0 {
			continue
		}
		off := 256 + i*16
		fx, fy := f32at(payload, off), f32at(payload, off+4)
		if fx == 0 && fy == 0 {
			continue
		}
		depth = append(depth, Intrinsics{
			Width: wh[0], Height: wh[1],
			Fx: fx, Fy: fy,
			Ppx: f32at(payload, off+8), Ppy: f32at(payload, off+12),
		})
	}
	if len(depth) == 0 {
		return 0, nil, fmt.Errorf("coefficients table carries no usable rect params")
	}
	return baselineMM, depth, nil
}

// parseRecParams reads the RECPARAMSGET reply: 20-byte rows of
// width/height/fx/fy/ppx/ppy, the firmware's own per-resolution rectified
// params (fw >= 5.11.9.5). It covers modes the coefficients table reserves no
// slot for.
func parseRecParams(payload []byte) []Intrinsics {
	var out []Intrinsics
	for off := 0; off+20 <= len(payload); off += 20 {
		w := uint32(binary.LittleEndian.Uint16(payload[off:]))
		h := uint32(binary.LittleEndian.Uint16(payload[off+2:]))
		if w == 0 {
			continue
		}
		out = append(out, Intrinsics{
			Width: w, Height: h,
			Fx: f32at(payload, off+4), Fy: f32at(payload, off+8),
			Ppx: f32at(payload, off+12), Ppy: f32at(payload, off+16),
		})
	}
	return out
}

// parseRGB reads table 32: the colour camera's normalised 3x3 intrinsic model
// (column-major floats at 16), Brown-Conrady distortion at 52, the resolution
// the model was calibrated at (u16 pair at 144), and the colour→depth
// extrinsics (column-major rotation at 184, translation at 220 in
// millimetres with the wire's -0.001 sign convention to metres).
func parseRGB(payload []byte) (colour Intrinsics, colourToDepth Extrinsics, err error) {
	if _, err := checkTable(payload, 232); err != nil {
		return Intrinsics{}, Extrinsics{}, fmt.Errorf("rgb table: %w", err)
	}
	cw := uint32(binary.LittleEndian.Uint16(payload[144:]))
	ch := uint32(binary.LittleEndian.Uint16(payload[146:]))
	if cw == 0 || ch == 0 {
		return Intrinsics{}, Extrinsics{}, fmt.Errorf("rgb table names no calibration resolution")
	}
	// Normalised column-major 3x3: fx at 0, ppx at 2, fy at 4, ppy at 5,
	// denormalised against the calibration resolution.
	w, h := float64(cw), float64(ch)
	colour = Intrinsics{
		Width: cw, Height: ch,
		Fx:  f32at(payload, 16+0*4) * w / 2,
		Fy:  f32at(payload, 16+4*4) * h / 2,
		Ppx: (1 + f32at(payload, 16+2*4)) * w / 2,
		Ppy: (1 + f32at(payload, 16+5*4)) * h / 2,
	}
	for i := range colour.Distortion {
		colour.Distortion[i] = f32at(payload, 52+i*4)
	}
	colour.BrownConrady = true

	// Wire rotation is column-major; Extrinsics stores row-major.
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			colourToDepth.Rotation[i*3+j] = f32at(payload, 184+(j*3+i)*4)
		}
	}
	for i := range colourToDepth.Translation {
		colourToDepth.Translation[i] = f32at(payload, 220+i*4) * -0.001
	}
	return colour, colourToDepth, nil
}
