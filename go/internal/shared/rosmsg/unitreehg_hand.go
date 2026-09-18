package rosmsg

import (
	"fmt"

	"github.com/wendylabsinc/wendy/go/internal/rtps/cdr"
)

// TypeHGHandState is the DDS type name a unitree_hg/msg/HandState writer advertises. A
// G1's Dex3 hands publish one each, on /dex3/left/state and /dex3/right/state.
const TypeHGHandState = "unitree_hg::msg::dds_::HandState_"

// hgSequenceCap bounds a length-prefixed sequence. Unlike LowState, HandState uses
// sequences rather than fixed arrays, so a corrupt length field would otherwise ask for
// an enormous allocation before any other check could fire.
const hgSequenceCap = 256

// HGPressSensor is one of a hand's pressure sensors: twelve pads, each with its own
// temperature, plus a count of readings the sensor itself knows it dropped.
type HGPressSensor struct {
	Pressure     [12]float32
	TemperatureC [12]float32
	Lost         uint32
}

// HGHandState is one hand. The finger motors are the same MotorState the body uses, so
// position, torque and both temperatures come through per finger.
//
// The power rails are the interesting part beyond the motors: a hand reports its own
// supply and draw, which is where a hand that is browning out under load shows up.
type HGHandState struct {
	Motors       []HGMotor
	PressSensors []HGPressSensor
	IMU          HGIMU
	PowerVolts   float32
	PowerAmps    float32
	SystemVolts  float32
	DeviceVolts  float32
	// Error is the hand's own fault words, carried verbatim.
	Error [2]uint32
}

// DecodeHGHandState decodes a unitree_hg/msg/HandState payload.
func DecodeHGHandState(payload []byte) (*HGHandState, error) {
	d, err := cdr.NewDecoder(payload)
	if err != nil {
		return nil, err
	}

	var hand HGHandState
	motors, err := d.Uint32()
	if err != nil {
		return nil, fmt.Errorf("motor_state length: %w", err)
	}
	if motors > hgSequenceCap {
		return nil, fmt.Errorf("rosmsg: hand reports %d motors, beyond anything plausible", motors)
	}
	for i := 0; i < int(motors); i++ {
		motor, err := decodeHGMotor(d, i)
		if err != nil {
			return nil, err
		}
		hand.Motors = append(hand.Motors, motor)
	}

	sensors, err := d.Uint32()
	if err != nil {
		return nil, fmt.Errorf("press_sensor_state length: %w", err)
	}
	if sensors > hgSequenceCap {
		return nil, fmt.Errorf("rosmsg: hand reports %d pressure sensors, beyond anything plausible", sensors)
	}
	for i := 0; i < int(sensors); i++ {
		sensor, err := decodeHGPressSensor(d, i)
		if err != nil {
			return nil, err
		}
		hand.PressSensors = append(hand.PressSensors, sensor)
	}

	if hand.IMU, err = decodeHGIMU(d); err != nil {
		return nil, err
	}
	for _, f := range []struct {
		name string
		into *float32
	}{
		{"power_v", &hand.PowerVolts},
		{"power_a", &hand.PowerAmps},
		{"system_v", &hand.SystemVolts},
		{"device_v", &hand.DeviceVolts},
	} {
		if *f.into, err = d.Float32(); err != nil {
			return nil, fmt.Errorf("%s: %w", f.name, err)
		}
	}
	for i := range hand.Error {
		if hand.Error[i], err = d.Uint32(); err != nil {
			return nil, fmt.Errorf("error[%d]: %w", i, err)
		}
	}
	for i := 0; i < 2; i++ {
		if _, err := d.Uint32(); err != nil {
			return nil, fmt.Errorf("reserve[%d]: %w", i, err)
		}
	}
	if remaining := d.Remaining(); remaining != 0 {
		return nil, fmt.Errorf("rosmsg: unitree_hg HandState left %d bytes unread; the wire layout is not what this decoder expects", remaining)
	}
	return &hand, nil
}

// HottestMotorC returns the highest temperature any finger motor reports, from either of
// its two sensors. A hand losing travel as it warms is the reason this is worth a row of
// its own.
func (h HGHandState) HottestMotorC() (int16, int, bool) {
	var hottest int16
	index, found := 0, false
	for i, motor := range h.Motors {
		for _, celsius := range motor.TemperatureC {
			if !found || celsius > hottest {
				hottest, index, found = celsius, i, true
			}
		}
	}
	return hottest, index, found
}

func decodeHGPressSensor(d *cdr.Decoder, index int) (HGPressSensor, error) {
	var sensor HGPressSensor
	read := func(into []float32, name string) error {
		for i := range into {
			v, err := d.Float32()
			if err != nil {
				return fmt.Errorf("press_sensor_state[%d].%s[%d]: %w", index, name, i, err)
			}
			into[i] = v
		}
		return nil
	}
	if err := read(sensor.Pressure[:], "pressure"); err != nil {
		return sensor, err
	}
	if err := read(sensor.TemperatureC[:], "temperature"); err != nil {
		return sensor, err
	}
	lost, err := d.Uint32()
	if err != nil {
		return sensor, fmt.Errorf("press_sensor_state[%d].lost: %w", index, err)
	}
	sensor.Lost = lost
	if _, err := d.Uint32(); err != nil {
		return sensor, fmt.Errorf("press_sensor_state[%d].reserve: %w", index, err)
	}
	return sensor, nil
}
