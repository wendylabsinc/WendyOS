package realsense

import "fmt"

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

// defaultDepthUnitsUM is the documented D4xx default when advanced mode is
// unavailable. It is NOT a fallback for a failed read: a device that claims
// advanced mode and then fails to answer gets a zero scale, which callers
// must refuse (depth in unknown units is not depth).
const defaultDepthUnitsUM = 1000

// ReadCalibration performs the full bounded calibration read: identity (GVD),
// depth units, the coefficients table, the RGB table, and the per-resolution
// rectified params where the firmware has them. One call makes at most six
// exchanges; it never retries a failed one.
func ReadCalibration(t Transport) (*Calibration, error) {
	cal := &Calibration{}

	gvd, err := command(t, opGVD, 0, 0)
	if err != nil {
		return nil, err
	}
	if cal.FirmwareVersion, cal.Serial, cal.HasRGB, err = gvdIdentity(gvd); err != nil {
		return nil, err
	}

	// Depth units: writable at runtime (advanced-mode presets change them),
	// so this value is only as fresh as this read — capture starts re-read it.
	cal.DepthScaleM = float64(defaultDepthUnitsUM) * 1e-6
	if am, err := command(t, opUAMG, 0, 0); err == nil && len(am) > 0 && am[0] != 0 {
		units, err := command(t, opGETADV, 9, 0)
		if err != nil || len(units) < 4 {
			cal.DepthScaleM = 0 // claimed advanced mode, failed to answer: refuse, don't guess
		} else {
			cal.DepthScaleM = float64(uint32(units[0])|uint32(units[1])<<8|uint32(units[2])<<16|uint32(units[3])<<24) * 1e-6
		}
	}

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
