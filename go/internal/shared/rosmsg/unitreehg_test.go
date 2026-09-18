package rosmsg

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hgBody builds a unitree_hg LowState body with CDR alignment paid per field, which is
// how the wire actually looks. It is written independently of the decoder so the two
// cannot share a misunderstanding — and the size assertion below is computed by hand
// from the IDL rather than from either.
type hgBody struct{ buf []byte }

func (w *hgBody) align(n int) {
	for len(w.buf)%n != 0 {
		w.buf = append(w.buf, 0)
	}
}

func (w *hgBody) u8(v uint8)   { w.buf = append(w.buf, v) }
func (w *hgBody) i16(v int16)  { w.align(2); w.buf = binary.LittleEndian.AppendUint16(w.buf, uint16(v)) }
func (w *hgBody) u32(v uint32) { w.align(4); w.buf = binary.LittleEndian.AppendUint32(w.buf, v) }
func (w *hgBody) f32(v float32) {
	w.align(4)
	w.buf = binary.LittleEndian.AppendUint32(w.buf, math.Float32bits(v))
}

// hgLowStatePayload encodes a full message. motor shapes each motor by index, so a test
// can make every one distinguishable.
func hgLowStatePayload(modeMachine uint8, motor func(i int) HGMotor) []byte {
	var w hgBody
	w.u32(1)
	w.u32(2) // version[2]
	w.u8(3)  // mode_pr
	w.u8(modeMachine)
	w.u32(4242) // tick

	// imu_state
	for i := 0; i < 4; i++ {
		w.f32(float32(i) * 0.25) // quaternion
	}
	for i := 0; i < 3; i++ {
		w.f32(float32(i) + 0.5) // gyroscope
	}
	for i := 0; i < 3; i++ {
		w.f32(float32(i) - 9.81) // accelerometer
	}
	for i := 0; i < 3; i++ {
		w.f32(float32(i) * 0.1) // rpy
	}
	w.i16(41) // imu temperature

	for i := 0; i < HGMotorCount; i++ {
		m := motor(i)
		w.u8(m.Mode)
		w.f32(m.Position)
		w.f32(m.Velocity)
		w.f32(m.Acceleration)
		w.f32(m.Torque)
		w.i16(m.TemperatureC[0])
		w.i16(m.TemperatureC[1])
		w.f32(m.Voltage)
		w.u32(0)
		w.u32(0) // sensor[2]
		w.u32(m.State)
		for r := 0; r < 4; r++ {
			w.u32(0) // reserve[4]
		}
	}

	w.buf = append(w.buf, make([]byte, 40)...) // wireless_remote
	for i := 0; i < 4; i++ {
		w.u32(0) // reserve[4]
	}
	w.u32(0xDEADBEEF) // crc

	return append([]byte{0x00, 0x01, 0x00, 0x00}, w.buf...)
}

// The layout the IDL implies, worked out by hand:
//
//	version 0..8, mode_pr 8..9, mode_machine 9..10, tick 12..16,
//	imu 16..70 (13 float32 then an int16 at an even offset),
//	motor[0] 70..124 — 54 bytes, because its uint8 mode starts at 70 and only the
//	float32 that follows is realigned — then 34 more at 56 bytes each,
//	wireless_remote 2028..2068, reserve 2068..2084, crc 2084..2088.
//
// Treating every motor as 56 bytes would put the tail four bytes late, which is the
// mistake that decodes into plausible wrong numbers instead of failing.
const hgBodyBytes = 2088

func TestHGLowStatePayloadMatchesTheLayoutTheIDLImplies(t *testing.T) {
	payload := hgLowStatePayload(9, func(int) HGMotor { return HGMotor{} })
	if got := len(payload) - 4; got != hgBodyBytes {
		t.Fatalf("body is %d bytes, want %d — the encoder and the hand-computed layout disagree", got, hgBodyBytes)
	}
}

// The whole body in one message: every joint's position, velocity, torque and both
// temperatures, plus the control mode.
func TestDecodeHGLowStateReadsEveryMotorAndTheMode(t *testing.T) {
	shape := func(i int) HGMotor {
		return HGMotor{
			Mode:         uint8(i % 8),
			Position:     float32(i) * 0.01,
			Velocity:     float32(i) * -0.02,
			Acceleration: float32(i) * 0.03,
			Torque:       float32(i) * 1.5,
			TemperatureC: [2]int16{int16(40 + i), int16(50 + i)},
			Voltage:      48 - float32(i)*0.1,
			State:        uint32(i),
		}
	}

	state, err := DecodeHGLowState(hgLowStatePayload(9, shape))
	if err != nil {
		t.Fatal(err)
	}
	if state.ModeMachine != 9 {
		t.Errorf("ModeMachine = %d", state.ModeMachine)
	}
	if state.Tick != 4242 {
		t.Errorf("Tick = %d", state.Tick)
	}
	if state.IMU.TemperatureC != 41 {
		t.Errorf("IMU temperature = %d, want 41", state.IMU.TemperatureC)
	}
	if got := state.IMU.Accelerometer[0]; got != -9.81 {
		t.Errorf("accelerometer[0] = %v, want -9.81", got)
	}

	// Every motor, not just the first — an alignment error usually shows up as drift
	// partway along the array rather than at the start.
	for i := range state.Motors {
		want, got := shape(i), state.Motors[i]
		if got != want {
			t.Fatalf("motor[%d] = %+v, want %+v", i, got, want)
		}
	}
}

// Both temperatures are kept. Reporting one and dropping the other hides whichever runs
// hotter, which on a hand is the one that matters.
func TestDecodeHGLowStateKeepsBothMotorTemperatures(t *testing.T) {
	state, err := DecodeHGLowState(hgLowStatePayload(1, func(int) HGMotor {
		return HGMotor{TemperatureC: [2]int16{77, 91}}
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got := state.Motors[0].TemperatureC; got != [2]int16{77, 91} {
		t.Errorf("temperatures = %v, want both kept", got)
	}
}

// A payload that is not this layout must fail rather than decode into something
// plausible. This is the whole reason the decoder walks the tail it never reads.
func TestDecodeHGLowStateRejectsAWrongLayout(t *testing.T) {
	full := hgLowStatePayload(1, func(int) HGMotor { return HGMotor{} })

	if _, err := DecodeHGLowState(full[:len(full)-8]); err == nil {
		t.Error("a truncated payload decoded without error")
	}
	longer := append(append([]byte{}, full...), 0, 0, 0, 0)
	_, err := DecodeHGLowState(longer)
	if err == nil {
		t.Fatal("a payload with trailing bytes decoded without error")
	}
	if !strings.Contains(err.Error(), "unread") {
		t.Errorf("error should name the unread tail: %v", err)
	}
}

// A quadruped publishes unitree_go, a different layout. Handing one to this decoder must
// not half-work.
func TestDecodeHGLowStateRejectsAShortMessageFromAnotherFamily(t *testing.T) {
	if _, err := DecodeHGLowState([]byte{0x00, 0x01, 0x00, 0x00, 1, 2, 3, 4}); err == nil {
		t.Error("a payload far too short decoded without error")
	}
}

// mode_machine is a uint8, so it cannot be the execution FSM id a G1 runs in — the
// demo's bridge requires 801, reached from 500. Those are set through the API and are
// absent from LowState. Recording that here because reading mode_machine as "the robot's
// state" would report a different thing under a familiar name, which is the class of
// error this whole effort exists to prevent.
func TestHGModeWordsAreNotTheFSMId(t *testing.T) {
	state, err := DecodeHGLowState(hgLowStatePayload(255, func(int) HGMotor { return HGMotor{} }))
	if err != nil {
		t.Fatal(err)
	}
	if state.ModeMachine != 255 {
		t.Fatalf("ModeMachine = %d, want the full byte range to round-trip", state.ModeMachine)
	}
	// 801 does not fit in the field at all, which is the point.
	if int(state.ModeMachine) == 801 {
		t.Error("a uint8 held 801")
	}
}

// A decoder tested only against a message its own author reconstructed from a spec
// proves the two agree, not that either matches the robot. This pins it to bytes
// unitree-g1-nx-2 actually published, captured on 2026-09-18 with the robot idle.
//
// The strongest assertion is simply that it decodes: the walk consumes every field
// including the ones nothing reads and requires the payload to end exactly, so a layout
// that is off by a single byte anywhere fails here rather than returning plausible
// numbers. The captured payload is 2092 bytes — four of encapsulation plus the 2088 the
// IDL implies — which is itself independent confirmation.
func TestDecodeHGLowStateAgainstBytesTheRobotSent(t *testing.T) {
	payload, err := os.ReadFile(filepath.Join("testdata", "unitree_hg_lowstate.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) != 4+hgBodyBytes {
		t.Fatalf("fixture is %d bytes, want %d", len(payload), 4+hgBodyBytes)
	}

	state, err := DecodeHGLowState(payload)
	if err != nil {
		t.Fatalf("the robot's own bytes did not decode: %v", err)
	}

	// The robot was idle when this was captured, which is what makes the next two
	// assertions worth making: they are the signature that distinguishes a position
	// vector from a velocity one, and so would catch the adjacent float fields having
	// been read in the wrong order.
	var live, distinct int
	seen := map[float32]bool{}
	var maxAbsPosition, maxAbsTorque float64
	for _, motor := range state.Motors {
		if motor.Voltage == 0 && motor.TemperatureC == [2]int16{} {
			continue
		}
		live++
		if !seen[motor.Position] {
			seen[motor.Position] = true
			distinct++
		}
		if abs := math.Abs(float64(motor.Position)); abs > maxAbsPosition {
			maxAbsPosition = abs
		}
		if abs := math.Abs(float64(motor.Torque)); abs > maxAbsTorque {
			maxAbsTorque = abs
		}
		// A motor on a healthy bus, not a float read from the wrong offset.
		if motor.Voltage < 20 || motor.Voltage > 60 {
			t.Errorf("motor voltage %v is outside anything a battery produces", motor.Voltage)
		}
		for sensor, celsius := range motor.TemperatureC {
			if celsius < 0 || celsius > 120 {
				t.Errorf("motor temperature[%d] = %d C is not a temperature", sensor, celsius)
			}
		}
	}

	if live < 20 {
		t.Errorf("only %d live motors; a G1 drives 27", live)
	}
	// A pose: many different angles, spread over a radian or so.
	if distinct < live/2 {
		t.Errorf("%d distinct positions across %d motors; a position vector is varied", distinct, live)
	}
	if maxAbsPosition < 0.1 || maxAbsPosition > math.Pi {
		t.Errorf("largest position %v rad does not look like a joint angle", maxAbsPosition)
	}
	// Holding still: torques near zero. If position and torque had been read in each
	// other's place, these two bounds would be the wrong way round.
	if maxAbsTorque > 20 {
		t.Errorf("largest torque %v Nm is too large for an idle robot", maxAbsTorque)
	}
}
