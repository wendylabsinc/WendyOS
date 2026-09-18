package rosmsg

import (
	"fmt"

	"github.com/wendylabsinc/wendy/go/internal/rtps/cdr"
)

// TypeHGLowState is the DDS type name a unitree_hg/msg/LowState writer advertises over
// SEDP. It is the humanoid family — a Unitree G1 publishes this, while the quadrupeds
// publish unitree_go/msg/LowState, a different layout with a different motor count.
const TypeHGLowState = "unitree_hg::msg::dds_::LowState_"

// HGMotorCount is the fixed motor_state array length in unitree_hg. A G1 drives 29 of
// them for the body; the remainder are published and unused, which is why the decoder
// returns all of them and lets the caller decide what is real.
const HGMotorCount = 35

// HGMotor is one motor's state. Nothing here is a claim: every field is what the robot
// reported at the instant the message was published.
type HGMotor struct {
	Mode uint8
	// Position, Velocity, Acceleration and Torque are q, dq, ddq and tau_est on the
	// wire. Radians and radians per second; torque is an estimate, not a measurement
	// from a load cell, which is why the field says est on the wire.
	Position     float32
	Velocity     float32
	Acceleration float32
	Torque       float32
	// TemperatureC holds both sensors the motor reports — the winding and the driver
	// board. Reporting one and dropping the other would hide whichever runs hotter.
	TemperatureC [2]int16
	Voltage      float32
	// State is the motor's error word. Non-zero means the motor is reporting a fault;
	// the meaning of each bit is Unitree's, so it is carried verbatim.
	State uint32
}

// HGIMU is the robot's inertial state.
type HGIMU struct {
	Quaternion    [4]float32
	Gyroscope     [3]float32
	Accelerometer [3]float32
	RPY           [3]float32
	TemperatureC  int16
}

// HGLowState is a unitree_hg/msg/LowState, which is the whole body in one message:
// every motor's position, velocity, torque and temperature, the IMU, and the control
// mode the robot is in.
type HGLowState struct {
	// ModePR and ModeMachine are the robot's own mode words, carried verbatim because
	// their meaning is Unitree's.
	//
	// Neither is the FSM id. Both are uint8 on the wire and so cannot hold the values
	// a G1's execution FSM uses — wendy-g1-can-demo requires 801, reached from 500 —
	// which are set through the API and are not present in LowState at all. Anything
	// reporting "the robot's state" from these would be reporting something else, so
	// they are surfaced under their own names and left uninterpreted.
	ModePR      uint8
	ModeMachine uint8
	// Tick is the robot's own counter, useful for telling a stale message from a fresh
	// one without trusting either clock.
	Tick   uint32
	IMU    HGIMU
	Motors [HGMotorCount]HGMotor
}

// DecodeHGLowState decodes a unitree_hg/msg/LowState payload.
//
// Every field is walked rather than skipped in blocks. CDR aligns per field, not per
// struct, so a MotorState does not begin on a four-byte boundary: its first member is a
// uint8, and the IMU before it ends on an even-but-not-multiple-of-four offset. Treating
// each motor as a fixed 56-byte block inserts phantom padding before the first one and
// shifts every later field — which decodes into plausible wrong numbers instead of
// failing. The agent's unitree_go decoder carries the same warning for the same reason.
//
// The walk finishes the message, including the fields nothing here reads, and asserts
// the payload was consumed exactly. That is what turns a wrong layout assumption into an
// error rather than a report full of confident nonsense.
func DecodeHGLowState(payload []byte) (*HGLowState, error) {
	d, err := cdr.NewDecoder(payload)
	if err != nil {
		return nil, err
	}

	var state HGLowState
	// version: uint32[2]
	for i := 0; i < 2; i++ {
		if _, err := d.Uint32(); err != nil {
			return nil, fmt.Errorf("version[%d]: %w", i, err)
		}
	}
	if state.ModePR, err = d.Uint8(); err != nil {
		return nil, fmt.Errorf("mode_pr: %w", err)
	}
	if state.ModeMachine, err = d.Uint8(); err != nil {
		return nil, fmt.Errorf("mode_machine: %w", err)
	}
	if state.Tick, err = d.Uint32(); err != nil {
		return nil, fmt.Errorf("tick: %w", err)
	}
	if state.IMU, err = decodeHGIMU(d); err != nil {
		return nil, err
	}
	for i := range state.Motors {
		if state.Motors[i], err = decodeHGMotor(d, i); err != nil {
			return nil, err
		}
	}

	// wireless_remote: uint8[40], then reserve: uint32[4], then crc: uint32. Unread,
	// but walked so the exact-consumption check below means something.
	if err := d.SkipBytes(1, 40); err != nil {
		return nil, fmt.Errorf("wireless_remote: %w", err)
	}
	for i := 0; i < 4; i++ {
		if _, err := d.Uint32(); err != nil {
			return nil, fmt.Errorf("reserve[%d]: %w", i, err)
		}
	}
	if _, err := d.Uint32(); err != nil {
		return nil, fmt.Errorf("crc: %w", err)
	}
	if remaining := d.Remaining(); remaining != 0 {
		return nil, fmt.Errorf("rosmsg: unitree_hg LowState left %d bytes unread; the wire layout is not what this decoder expects", remaining)
	}
	return &state, nil
}

func decodeHGIMU(d *cdr.Decoder) (HGIMU, error) {
	var imu HGIMU
	read := func(into []float32, name string) error {
		for i := range into {
			v, err := d.Float32()
			if err != nil {
				return fmt.Errorf("imu_state.%s[%d]: %w", name, i, err)
			}
			into[i] = v
		}
		return nil
	}
	if err := read(imu.Quaternion[:], "quaternion"); err != nil {
		return imu, err
	}
	if err := read(imu.Gyroscope[:], "gyroscope"); err != nil {
		return imu, err
	}
	if err := read(imu.Accelerometer[:], "accelerometer"); err != nil {
		return imu, err
	}
	if err := read(imu.RPY[:], "rpy"); err != nil {
		return imu, err
	}
	temperature, err := d.Int16()
	if err != nil {
		return imu, fmt.Errorf("imu_state.temperature: %w", err)
	}
	imu.TemperatureC = temperature
	return imu, nil
}

func decodeHGMotor(d *cdr.Decoder, index int) (HGMotor, error) {
	var motor HGMotor
	field := func(name string, read func() error) error {
		if err := read(); err != nil {
			return fmt.Errorf("motor_state[%d].%s: %w", index, name, err)
		}
		return nil
	}

	if err := field("mode", func() (err error) { motor.Mode, err = d.Uint8(); return }); err != nil {
		return motor, err
	}
	for _, f := range []struct {
		name string
		into *float32
	}{
		{"q", &motor.Position},
		{"dq", &motor.Velocity},
		{"ddq", &motor.Acceleration},
		{"tau_est", &motor.Torque},
	} {
		if err := field(f.name, func() (err error) { *f.into, err = d.Float32(); return }); err != nil {
			return motor, err
		}
	}
	for i := range motor.TemperatureC {
		if err := field(fmt.Sprintf("temperature[%d]", i), func() (err error) {
			motor.TemperatureC[i], err = d.Int16()
			return
		}); err != nil {
			return motor, err
		}
	}
	if err := field("vol", func() (err error) { motor.Voltage, err = d.Float32(); return }); err != nil {
		return motor, err
	}
	// sensor: uint32[2] — unread, but walked.
	for i := 0; i < 2; i++ {
		if err := field(fmt.Sprintf("sensor[%d]", i), func() error { _, err := d.Uint32(); return err }); err != nil {
			return motor, err
		}
	}
	if err := field("motorstate", func() (err error) { motor.State, err = d.Uint32(); return }); err != nil {
		return motor, err
	}
	for i := 0; i < 4; i++ {
		if err := field(fmt.Sprintf("reserve[%d]", i), func() error { _, err := d.Uint32(); return err }); err != nil {
			return motor, err
		}
	}
	return motor, nil
}
