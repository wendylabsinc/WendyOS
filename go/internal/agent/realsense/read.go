package realsense

import (
	"encoding/binary"
	"fmt"
)

// Transport carries one HWM exchange: write a 1024-byte command frame, read
// the 1024-byte reply frame. The linux implementation speaks
// UVCIOC_CTRL_QUERY against a depth node's file descriptor; tests replay
// frames recorded from real hardware. Implementations own the retry budget —
// callers of this package never loop around an exchange.
type Transport interface {
	Exchange(tx []byte) (rx []byte, err error)
}

func command(t Transport, opcode, p1, p2 uint32) ([]byte, error) {
	rx, err := t.Exchange(buildCommand(opcode, p1, p2))
	if err != nil {
		return nil, fmt.Errorf("realsense: op 0x%02x: %w", opcode, err)
	}
	return parseReply(rx, opcode)
}

// defaultDepthUnitsUM is the fixed depth unit of a camera whose advanced mode
// is off: librealsense registers RS2_OPTION_DEPTH_UNITS as a constant then,
// 1000 µm for every D4xx except the short-range D405, whose unit is 100 µm.
// It is NOT a fallback for a failed read: a device whose advanced-mode state
// or units cannot be read gets a zero scale, which callers must refuse (depth
// in unknown units is not depth).
func defaultDepthUnitsUM(pid string) uint32 {
	if knownModels[pid] == "D405" {
		return 100
	}
	return 1000
}

// ReadDepthScale reads the one writable calibration value, metres per Z16
// unit, in at most two exchanges: whether advanced mode is on (UAMG) and, when
// it is, the units it set (GETADV 9). pid is the camera's USB product id,
// which decides the fixed unit when advanced mode is off. Any failure returns
// an error and scale 0, never a default standing in for the read.
func ReadDepthScale(t Transport, pid string) (float64, error) {
	am, err := command(t, opUAMG, 0, 0)
	if err != nil {
		return 0, err
	}
	if len(am) == 0 {
		return 0, fmt.Errorf("realsense: op 0x%02x: empty reply", opUAMG)
	}
	if am[0] == 0 {
		return float64(defaultDepthUnitsUM(pid)) * 1e-6, nil
	}
	units, err := command(t, opGETADV, 9, 0)
	if err != nil {
		return 0, err
	}
	if len(units) < 4 {
		return 0, fmt.Errorf("realsense: depth units reply is %d bytes, want 4", len(units))
	}
	um := binary.LittleEndian.Uint32(units)
	if um == 0 {
		return 0, fmt.Errorf("realsense: advanced mode reports a depth unit of 0 µm")
	}
	return float64(um) * 1e-6, nil
}

// ReadCalibration performs the full bounded calibration read: identity (GVD),
// depth units, the coefficients table, the RGB table, and the per-resolution
// rectified params where the firmware has them. One call makes at most six
// exchanges; it never retries a failed one. pid is as for ReadDepthScale.
func ReadCalibration(t Transport, pid string) (*Calibration, error) {
	cal := &Calibration{}

	gvd, err := command(t, opGVD, 0, 0)
	if err != nil {
		return nil, err
	}
	if cal.FirmwareVersion, cal.Serial, cal.HasRGB, err = gvdIdentity(gvd); err != nil {
		return nil, err
	}

	// Depth units: writable at runtime (advanced-mode presets change them),
	// so this value is only as fresh as this read — capture starts re-read it
	// with ReadDepthScale. A failed read leaves the scale 0, which disables
	// depth by name downstream, without costing the factory tables.
	cal.DepthScaleM, _ = ReadDepthScale(t, pid)

	coef, err := command(t, opGETINTCAL, tableCoefficients, 0)
	if err != nil {
		return nil, err
	}
	if cal.BaselineMM, cal.Depth, err = parseCoefficients(coef); err != nil {
		return nil, err
	}

	rgb, err := command(t, opGETINTCAL, tableRGB, 0)
	if err != nil {
		return nil, err
	}
	colour, c2d, err := parseRGB(rgb)
	if err != nil {
		return nil, err
	}
	cal.Colour = colour
	cal.ColourToDepth = c2d
	cal.DepthToColour = c2d.Inverse()

	// RECPARAMSGET is the firmware's own statement of the same depth numbers;
	// older firmware lacks it, so a refusal here is not an error. Rows for
	// modes the coefficients table has no slot for are added; overlapping
	// rows are kept from the table (the two agree to float32 precision on
	// validated hardware).
	if rec, err := command(t, opRECPARAMSGET, 0, 0); err == nil {
		have := make(map[uint64]bool, len(cal.Depth))
		for _, in := range cal.Depth {
			have[uint64(in.Width)<<32|uint64(in.Height)] = true
		}
		for _, in := range parseRecParams(rec) {
			if !have[uint64(in.Width)<<32|uint64(in.Height)] {
				cal.Depth = append(cal.Depth, in)
			}
		}
	}
	return cal, nil
}
