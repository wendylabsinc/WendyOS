package rosmsg

import (
	"encoding/binary"
	"strings"
	"testing"
)

// The BmsState fields the LowState helpers do not cover: an unsigned 16-bit cell
// voltage, and a signed 32-bit current that is negative while discharging.
func (w *hgBody) u16(v uint16) { w.align(2); w.buf = binary.LittleEndian.AppendUint16(w.buf, v) }
func (w *hgBody) i32(v int32)  { w.align(4); w.buf = binary.LittleEndian.AppendUint32(w.buf, uint32(v)) }

// payload prefixes the CDR_LE encapsulation header these decoders expect.
func (w *hgBody) payload() []byte {
	return append([]byte{0x00, 0x01, 0x00, 0x00}, w.buf...)
}

// bmsPayload encodes a unitree_hg BmsState, alignment paid per field.
func bmsPayload(charge, health uint8, cells [HGBmsCells]uint16, temps [HGBmsTemperatures]int16, cycles uint16) []byte {
	var w hgBody
	w.u8(2)
	w.u8(7)
	w.u8(0)
	for _, mv := range cells {
		w.u16(mv)
	}
	for i := 0; i < 3; i++ {
		w.u32(0)
	}
	w.i32(-4200) // discharging
	w.u8(charge)
	w.u8(health)
	for _, c := range temps {
		w.i16(c)
	}
	w.u16(cycles)
	w.u16(0) // manufacturer_date
	for i := 0; i < 5; i++ {
		w.u32(0) // bmsstate
	}
	for i := 0; i < 3; i++ {
		w.u32(0) // reserve
	}
	return w.payload()
}

func TestDecodeHGBmsStateReadsTheRobotsBattery(t *testing.T) {
	var cells [HGBmsCells]uint16
	// A 15-cell pack in a 40-slot array: the rest are unfitted, not flat.
	for i := 0; i < 15; i++ {
		cells[i] = uint16(4100 + i*3)
	}
	var temps [HGBmsTemperatures]int16
	for i := 0; i < 6; i++ {
		temps[i] = int16(30 + i)
	}

	bms, err := DecodeHGBmsState(bmsPayload(78, 96, cells, temps, 412))
	if err != nil {
		t.Fatal(err)
	}
	if bms.ChargePercent != 78 || bms.HealthPercent != 96 {
		t.Errorf("charge/health = %d/%d, want 78/96", bms.ChargePercent, bms.HealthPercent)
	}
	if bms.Cycles != 412 {
		t.Errorf("cycles = %d, want 412", bms.Cycles)
	}
	if bms.CurrentMilliamps != -4200 {
		t.Errorf("current = %d, want -4200 (discharging)", bms.CurrentMilliamps)
	}
	if bms.VersionHigh != 2 || bms.VersionLow != 7 {
		t.Errorf("firmware = %d.%d, want 2.7", bms.VersionHigh, bms.VersionLow)
	}

	// An unfitted slot reads zero and must not be mistaken for a dead cell.
	highest, ok := bms.HighestCellMillivolts()
	if !ok || highest != 4142 {
		t.Errorf("highest cell = %d (%v), want 4142", highest, ok)
	}
	lowest, ok := bms.LowestCellMillivolts()
	if !ok || lowest != 4100 {
		t.Errorf("lowest cell = %d (%v), want 4100 — a zero slot is unfitted, not flat", lowest, ok)
	}
	hottest, ok := bms.HottestCelsius()
	if !ok || hottest != 35 {
		t.Errorf("hottest = %d (%v), want 35", hottest, ok)
	}
}

func TestDecodeHGBmsStateRejectsAWrongLayout(t *testing.T) {
	full := bmsPayload(50, 90, [HGBmsCells]uint16{}, [HGBmsTemperatures]int16{}, 1)
	if _, err := DecodeHGBmsState(full[:len(full)-6]); err == nil {
		t.Error("a truncated payload decoded without error")
	}
	longer := append(append([]byte{}, full...), 0, 0, 0, 0)
	if _, err := DecodeHGBmsState(longer); err == nil {
		t.Error("a payload with a trailing tail decoded without error")
	}
}

// handPayload encodes a HandState. Unlike LowState these are sequences, so each carries
// a length.
func handPayload(motors int, sensors int, powerVolts float32) []byte {
	var w hgBody
	w.u32(uint32(motors))
	for i := 0; i < motors; i++ {
		w.u8(0)
		w.f32(float32(i) * 0.2)  // q
		w.f32(0)                 // dq
		w.f32(0)                 // ddq
		w.f32(float32(i) * 0.05) // tau_est
		w.i16(int16(30 + i))
		w.i16(int16(34 + i))
		w.f32(12.0) // vol
		w.u32(0)
		w.u32(0)
		w.u32(0)
		for r := 0; r < 4; r++ {
			w.u32(0)
		}
	}
	w.u32(uint32(sensors))
	for i := 0; i < sensors; i++ {
		for p := 0; p < 12; p++ {
			w.f32(float32(p) * 1.5)
		}
		for p := 0; p < 12; p++ {
			w.f32(float32(28 + p))
		}
		w.u32(0) // lost
		w.u32(0) // reserve
	}
	for i := 0; i < 13; i++ {
		w.f32(0) // imu quaternion, gyro, accel, rpy
	}
	w.i16(33) // imu temperature
	w.f32(powerVolts)
	w.f32(1.25) // power_a
	w.f32(5.0)  // system_v
	w.f32(3.3)  // device_v
	w.u32(0)
	w.u32(0) // error
	w.u32(0)
	w.u32(0) // reserve
	return w.payload()
}

func TestDecodeHGHandStateReadsFingersPressureAndPower(t *testing.T) {
	hand, err := DecodeHGHandState(handPayload(7, 2, 24.5))
	if err != nil {
		t.Fatal(err)
	}
	if len(hand.Motors) != 7 {
		t.Fatalf("motors = %d, want 7", len(hand.Motors))
	}
	if len(hand.PressSensors) != 2 {
		t.Fatalf("pressure sensors = %d, want 2", len(hand.PressSensors))
	}
	if hand.PowerVolts != 24.5 || hand.PowerAmps != 1.25 {
		t.Errorf("power = %v V %v A, want 24.5/1.25", hand.PowerVolts, hand.PowerAmps)
	}
	if got := hand.PressSensors[0].TemperatureC[0]; got != 28 {
		t.Errorf("pressure sensor temperature = %v, want 28", got)
	}
	if got := hand.PressSensors[1].Pressure[3]; got != 4.5 {
		t.Errorf("pressure = %v, want 4.5", got)
	}

	// The hand that loses travel as it warms is why this has a row of its own.
	hottest, index, ok := hand.HottestMotorC()
	if !ok || hottest != 40 || index != 6 {
		t.Errorf("hottest = %d on motor %d (%v), want 40 on 6", hottest, index, ok)
	}
	if hand.IMU.TemperatureC != 33 {
		t.Errorf("hand IMU temperature = %d, want 33", hand.IMU.TemperatureC)
	}
}

// A hand with nothing attached is a real state and must decode, not fail.
func TestDecodeHGHandStateHandlesEmptySequences(t *testing.T) {
	hand, err := DecodeHGHandState(handPayload(0, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(hand.Motors) != 0 || len(hand.PressSensors) != 0 {
		t.Errorf("expected empty sequences, got %d motors and %d sensors", len(hand.Motors), len(hand.PressSensors))
	}
}

// A corrupt length prefix must be refused before it is used to size anything. LowState's
// fixed arrays cannot hit this; HandState's sequences can.
func TestDecodeHGHandStateRefusesAnAbsurdSequenceLength(t *testing.T) {
	payload := []byte{0x00, 0x01, 0x00, 0x00, 0xff, 0xff, 0xff, 0xff}
	_, err := DecodeHGHandState(payload)
	if err == nil {
		t.Fatal("a four-billion-element sequence was accepted")
	}
	if !strings.Contains(err.Error(), "plausible") {
		t.Errorf("error should name the implausible length: %v", err)
	}
}

func TestDecodeHGHandStateRejectsATrailingTail(t *testing.T) {
	full := handPayload(7, 2, 24.5)
	longer := append(append([]byte{}, full...), 0, 0, 0, 0)
	if _, err := DecodeHGHandState(longer); err == nil {
		t.Error("a payload with a trailing tail decoded without error")
	}
}
