package realsense

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The testdata frames are raw 1024-byte HWM replies captured from a real
// D435i (fw 5.15.1.55, serial 337122071091) on a Unitree G1, 2026-10-12 —
// the hardware run that validated this protocol. Every expected number below
// is what that camera actually said, independently cross-checked at capture
// time against physical geometry (50 mm stereo baseline, ~15 mm RGB offset)
// and against the firmware's second copy of the same values (RECPARAMSGET).
const fixtureDir = "testdata/d435i-fw5-15-1-55"

func fixtureFrame(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixtureDir, name+".bin"))
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != hwmFrameSize {
		t.Fatalf("fixture %s is %d bytes, want %d", name, len(b), hwmFrameSize)
	}
	return b
}

func fixturePayload(t *testing.T, name string, opcode uint32) []byte {
	t.Helper()
	p, err := parseReply(fixtureFrame(t, name), opcode)
	if err != nil {
		t.Fatalf("parseReply(%s): %v", name, err)
	}
	return p
}

func near(t *testing.T, what string, got, want, eps float64) {
	t.Helper()
	if math.Abs(got-want) > eps {
		t.Errorf("%s = %v, want %v (±%v)", what, got, want, eps)
	}
}

func TestGVDIdentity(t *testing.T) {
	fw, serial, hasRGB, err := gvdIdentity(fixturePayload(t, "gvd", opGVD))
	if err != nil {
		t.Fatal(err)
	}
	if fw != "5.15.1.55" {
		t.Errorf("fw = %q, want 5.15.1.55", fw)
	}
	if serial != "337122071091" {
		t.Errorf("serial = %q, want 337122071091", serial)
	}
	if !hasRGB {
		t.Error("hasRGB = false, want true")
	}
}

func TestCoefficientsTable(t *testing.T) {
	payload := fixturePayload(t, "table25", opGETINTCAL)
	if len(payload) != 512 {
		t.Fatalf("payload is %d bytes; this fixture exists to pin the 512-byte firmware variant", len(payload))
	}
	baseline, depth, err := parseCoefficients(payload)
	if err != nil {
		t.Fatal(err)
	}
	// The D435i's stereo baseline is physically 50 mm; the wire value is
	// negative per the right-imager convention.
	near(t, "baseline", baseline, -50.049286, 1e-4)
	byRes := map[string]Intrinsics{}
	for _, in := range depth {
		byRes[fmt.Sprintf("%dx%d", in.Width, in.Height)] = in
	}
	in, ok := byRes["1280x720"]
	if !ok {
		t.Fatalf("no 1280x720 depth intrinsics; have %v", byRes)
	}
	near(t, "fx", in.Fx, 649.503662, 1e-4)
	near(t, "fy", in.Fy, 649.503662, 1e-4)
	near(t, "ppx", in.Ppx, 636.162781, 1e-4)
	near(t, "ppy", in.Ppy, 366.379730, 1e-4)
	if in.Distortion != [5]float64{} {
		t.Error("rectified depth must carry zero distortion")
	}
}

func TestCoefficientsTablePaddedToStructSize(t *testing.T) {
	// Other firmware sends the struct's full 576 bytes. The CRC then covers
	// table_size bytes, not everything received — the content gate must
	// accept that variant too.
	payload := append(append([]byte{}, fixturePayload(t, "table25", opGETINTCAL)...), make([]byte, 64)...)
	if _, _, err := parseCoefficients(payload); err != nil {
		t.Fatalf("576-byte variant refused: %v", err)
	}
}

func TestCoefficientsTableTruncatedIsRefused(t *testing.T) {
	payload := fixturePayload(t, "table25", opGETINTCAL)[:300]
	if _, _, err := parseCoefficients(payload); err == nil {
		t.Fatal("truncated table parsed without error")
	}
}

func TestCorruptTableIsRefused(t *testing.T) {
	payload := append([]byte{}, fixturePayload(t, "table25", opGETINTCAL)...)
	payload[200] ^= 0xff
	if _, _, err := parseCoefficients(payload); err == nil {
		t.Fatal("corrupt table passed its CRC")
	}
}

func TestRGBTable(t *testing.T) {
	colour, c2d, err := parseRGB(fixturePayload(t, "table32", opGETINTCAL))
	if err != nil {
		t.Fatal(err)
	}
	if colour.Width != 1920 || colour.Height != 1080 {
		t.Fatalf("calibration resolution %dx%d, want 1920x1080", colour.Width, colour.Height)
	}
	near(t, "fx", colour.Fx, 1358.867340, 1e-3)
	near(t, "fy", colour.Fy, 1357.604127, 1e-3)
	near(t, "ppx", colour.Ppx, 980.324593, 1e-3)
	near(t, "ppy", colour.Ppy, 582.584810, 1e-3)
	if colour.Distortion != [5]float64{} {
		t.Errorf("this unit ships zero distortion, got %v", colour.Distortion)
	}
	// ~15 mm between the RGB imager and the depth origin, the D435i's build.
	near(t, "c2d tx", c2d.Translation[0], -0.014853084564208984, 1e-9)
	d2c := c2d.Inverse()
	near(t, "d2c tx", d2c.Translation[0], 0.014856996798284474, 1e-9)
	near(t, "d2c ty", d2c.Translation[1], -8.856679921441773e-05, 1e-12)
	near(t, "d2c tz", d2c.Translation[2], 0.000279437196880718, 1e-12)
	// Rotation is near-identity and must round-trip: R · Rᵀ = I.
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			var s float64
			for k := 0; k < 3; k++ {
				s += c2d.Rotation[i*3+k] * d2c.Rotation[k*3+j]
			}
			want := 0.0
			if i == j {
				want = 1.0
			}
			near(t, fmt.Sprintf("R·Rᵀ[%d,%d]", i, j), s, want, 1e-6)
		}
	}
}

func TestColourIntrinsicsDerivation(t *testing.T) {
	colour, _, err := parseRGB(fixturePayload(t, "table32", opGETINTCAL))
	if err != nil {
		t.Fatal(err)
	}
	cal := &Calibration{Colour: colour}
	// Same aspect ratio: pure scaling.
	in, err := cal.ColourIntrinsicsFor(1280, 720)
	if err != nil {
		t.Fatal(err)
	}
	near(t, "720p fx", in.Fx, 905.911560, 1e-3)
	near(t, "720p ppx", in.Ppx, 653.549728, 1e-3)
	// 4:3 from a 16:9 calibration: horizontal crop.
	in, err = cal.ColourIntrinsicsFor(640, 480)
	if err != nil {
		t.Fatal(err)
	}
	near(t, "VGA fx", in.Fx, 603.941040, 1e-3)
	near(t, "VGA fy", in.Fy, 603.379612, 1e-3)
	near(t, "VGA ppx", in.Ppx, 329.033152, 1e-3)
	near(t, "VGA ppy", in.Ppy, 258.926582, 1e-3)
}

func TestRecParams(t *testing.T) {
	rows := parseRecParams(fixturePayload(t, "recparams", opRECPARAMSGET))
	if len(rows) != 18 {
		t.Fatalf("got %d rows, want 18", len(rows))
	}
	// RECPARAMSGET and table 25 are two independent firmware paths; where
	// they overlap they must agree to float32 precision.
	_, depth, err := parseCoefficients(fixturePayload(t, "table25", opGETINTCAL))
	if err != nil {
		t.Fatal(err)
	}
	table := map[[2]uint32]Intrinsics{}
	for _, in := range depth {
		table[[2]uint32{in.Width, in.Height}] = in
	}
	compared := 0
	for _, rec := range rows {
		in, ok := table[[2]uint32{rec.Width, rec.Height}]
		if !ok {
			continue
		}
		compared++
		near(t, fmt.Sprintf("%dx%d fx", rec.Width, rec.Height), rec.Fx, in.Fx, 1e-3)
		near(t, fmt.Sprintf("%dx%d ppy", rec.Width, rec.Height), rec.Ppy, in.Ppy, 1e-3)
	}
	if compared < 8 {
		t.Errorf("only %d overlapping resolutions compared", compared)
	}
}

// fixtureTransport replays the captured frames, keyed by opcode and first
// parameter — a camera on the desk, minus the desk.
type fixtureTransport struct {
	t      *testing.T
	frames map[[2]uint32][]byte
}

func (f *fixtureTransport) Exchange(tx []byte) ([]byte, error) {
	op := binary.LittleEndian.Uint32(tx[4:])
	p1 := binary.LittleEndian.Uint32(tx[8:])
	frame, ok := f.frames[[2]uint32{op, p1}]
	if !ok {
		return nil, fmt.Errorf("no fixture for op 0x%02x p1=%d", op, p1)
	}
	return frame, nil
}

func newFixtureTransport(t *testing.T) *fixtureTransport {
	return &fixtureTransport{t: t, frames: map[[2]uint32][]byte{
		{opGVD, 0}:                       fixtureFrame(t, "gvd"),
		{opUAMG, 0}:                      fixtureFrame(t, "uamg"),
		{opGETADV, 9}:                    fixtureFrame(t, "getadv9"),
		{opGETINTCAL, tableCoefficients}: fixtureFrame(t, "table25"),
		{opGETINTCAL, tableRGB}:          fixtureFrame(t, "table32"),
		{opRECPARAMSGET, 0}:              fixtureFrame(t, "recparams"),
	}}
}

func TestReadCalibrationEndToEnd(t *testing.T) {
	cal, err := ReadCalibration(newFixtureTransport(t), "0b3a")
	if err != nil {
		t.Fatal(err)
	}
	if cal.FirmwareVersion != "5.15.1.55" || cal.Serial != "337122071091" || !cal.HasRGB {
		t.Errorf("identity = %q/%q/%v", cal.FirmwareVersion, cal.Serial, cal.HasRGB)
	}
	near(t, "depth scale", cal.DepthScaleM, 0.001, 1e-12)
	near(t, "baseline", cal.BaselineMM, -50.049286, 1e-4)
	// Table 25 carries 10 modes; RECPARAMSGET adds the 8 the table reserves
	// no slot for (848x100, 1280x960, ...) — the merge must hold all 18.
	if len(cal.Depth) != 18 {
		t.Errorf("merged depth modes = %d, want 18", len(cal.Depth))
	}
	if _, err := cal.DepthIntrinsicsFor(848, 100); err != nil {
		t.Errorf("RECPARAMSGET-only mode missing after merge: %v", err)
	}
	if _, err := cal.DepthIntrinsicsFor(847, 100); err == nil {
		t.Error("an uncalibrated resolution must be refused, not estimated")
	}
}

func TestDeviceErrorsAreNamed(t *testing.T) {
	rx := make([]byte, hwmFrameSize)
	neg := int32(-25)
	binary.LittleEndian.PutUint32(rx[0:], uint32(neg))
	_, err := parseReply(rx, opGETINTCAL)
	if err == nil {
		t.Fatal("negative echo accepted")
	}
	if want := "TABLE_IS_EMPTY"; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not name %s", err, want)
	}
}

func TestClaimedAdvancedModeThatFailsGivesZeroScale(t *testing.T) {
	// A device that says it has advanced mode and then cannot answer the
	// units read must yield scale 0 — a refusal downstream — never 1 mm.
	ft := newFixtureTransport(t)
	delete(ft.frames, [2]uint32{opGETADV, 9})
	cal, err := ReadCalibration(ft, "0b3a")
	if err != nil {
		t.Fatal(err)
	}
	if cal.DepthScaleM != 0 {
		t.Errorf("DepthScaleM = %v, want 0 (refuse, don't guess)", cal.DepthScaleM)
	}
}

// A failed advanced-mode query is a failed read, not "advanced mode off": the
// scale is refused, and the factory tables still arrive.
func TestUnansweredAdvancedModeQueryGivesZeroScale(t *testing.T) {
	ft := newFixtureTransport(t)
	delete(ft.frames, [2]uint32{opUAMG, 0})
	cal, err := ReadCalibration(ft, "0b3a")
	if err != nil {
		t.Fatalf("a units failure cost the factory tables: %v", err)
	}
	if cal.DepthScaleM != 0 {
		t.Errorf("DepthScaleM = %v, want 0 (refuse, don't guess)", cal.DepthScaleM)
	}
	if _, err := ReadDepthScale(ft, "0b3a"); err == nil {
		t.Error("ReadDepthScale reported no error for an unanswered UAMG")
	}
}

// With advanced mode off the unit is the model's fixed one, and a D405's is a
// tenth of everyone else's: 1 mm there would make every distance ten times
// too large.
func TestAdvancedModeOffUsesTheModelsFixedUnit(t *testing.T) {
	for pid, want := range map[string]float64{"0b3a": 0.001, "0b5b": 0.0001, "0bff": 0.001} {
		ft := newFixtureTransport(t)
		ft.frames[[2]uint32{opUAMG, 0}] = replyFrame(opUAMG, []byte{0, 0, 0, 0})
		delete(ft.frames, [2]uint32{opGETADV, 9}) // must not be asked
		got, err := ReadDepthScale(ft, pid)
		if err != nil {
			t.Fatalf("pid %s: %v", pid, err)
		}
		near(t, "depth scale for pid "+pid, got, want, 1e-12)
	}
}

func TestReadDepthScaleFromAdvancedMode(t *testing.T) {
	got, err := ReadDepthScale(newFixtureTransport(t), "0b3a")
	if err != nil {
		t.Fatal(err)
	}
	near(t, "depth scale", got, 0.001, 1e-12)
}

// replyFrame builds a successful HWM reply carrying payload.
func replyFrame(opcode uint32, payload []byte) []byte {
	rx := make([]byte, hwmFrameSize)
	binary.LittleEndian.PutUint32(rx[0:], opcode)
	copy(rx[4:], payload)
	binary.LittleEndian.PutUint32(rx[hwmDataSizeAt:], uint32(len(payload)))
	return rx
}

func TestBuildCommandFrame(t *testing.T) {
	tx := buildCommand(opGETINTCAL, tableCoefficients, 0)
	if len(tx) != hwmFrameSize {
		t.Fatalf("frame is %d bytes", len(tx))
	}
	if got := binary.LittleEndian.Uint16(tx[2:]); got != hwmMagic {
		t.Errorf("magic = %04x", got)
	}
	if got := binary.LittleEndian.Uint16(tx[0:]); got != 20 {
		t.Errorf("size field = %d, want 20", got)
	}
}
