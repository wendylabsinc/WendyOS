package robotprobe

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/robotinspect"
	"github.com/wendylabsinc/wendy/go/internal/shared/rosmsg"
)

type partsWriter struct{ buf []byte }

func (w *partsWriter) align(n int) {
	for len(w.buf)%n != 0 {
		w.buf = append(w.buf, 0)
	}
}
func (w *partsWriter) u8(v uint8)   { w.buf = append(w.buf, v) }
func (w *partsWriter) u16(v uint16) { w.align(2); w.buf = binary.LittleEndian.AppendUint16(w.buf, v) }
func (w *partsWriter) i16(v int16) {
	w.align(2)
	w.buf = binary.LittleEndian.AppendUint16(w.buf, uint16(v))
}
func (w *partsWriter) u32(v uint32) { w.align(4); w.buf = binary.LittleEndian.AppendUint32(w.buf, v) }
func (w *partsWriter) i32(v int32) {
	w.align(4)
	w.buf = binary.LittleEndian.AppendUint32(w.buf, uint32(v))
}
func (w *partsWriter) f32(v float32) {
	w.align(4)
	w.buf = binary.LittleEndian.AppendUint32(w.buf, math.Float32bits(v))
}
func (w *partsWriter) payload() []byte {
	return append([]byte{0x00, 0x01, 0x00, 0x00}, w.buf...)
}

func bmsBytes(charge, health uint8, cycles uint16, cells []uint16, temps []int16) []byte {
	var w partsWriter
	w.u8(2)
	w.u8(7)
	w.u8(0)
	for i := 0; i < 40; i++ {
		if i < len(cells) {
			w.u16(cells[i])
		} else {
			w.u16(0)
		}
	}
	for i := 0; i < 3; i++ {
		w.u32(0)
	}
	w.i32(-6100)
	w.u8(charge)
	w.u8(health)
	for i := 0; i < 12; i++ {
		if i < len(temps) {
			w.i16(temps[i])
		} else {
			w.i16(0)
		}
	}
	w.u16(cycles)
	w.u16(0)
	for i := 0; i < 5; i++ {
		w.u32(0)
	}
	for i := 0; i < 3; i++ {
		w.u32(0)
	}
	return w.payload()
}

type topicBytes struct {
	byTopic map[string][][]byte
	err     error
	asked   []string
}

func (t *topicBytes) Sample(_ context.Context, topic, _ string, _ time.Duration, _ int) ([][]byte, error) {
	t.asked = append(t.asked, topic)
	if t.err != nil {
		return nil, t.err
	}
	return t.byTopic[topic], nil
}

func ddsEnv(reader TopicReader) *robotinspect.Env {
	return robotinspect.NewEnv().Offer(robotinspect.RequirementDDSDomain, reader)
}

// Charge alone does not describe a pack. Health, cycles and the spread between cells are
// what separate a worn battery from a new one at the same percentage.
func TestRobotBatteryReportsWearNotJustCharge(t *testing.T) {
	cells := []uint16{4100, 4105, 4190, 4102, 4108}
	reader := &topicBytes{byTopic: map[string][][]byte{
		"/lf/bmsstate": {bmsBytes(78, 91, 412, cells, []int16{31, 33, 38})},
	}}

	properties, err := RobotBattery{}.Observe(context.Background(), ddsEnv(reader))
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]float64{
		"battery.robot.charge": 78,
		"battery.robot.health": 91,
		"battery.robot.cycles": 412,
	} {
		if got := findIn(t, properties, id).Observations[0].Quantity.Value(); got != want {
			t.Errorf("%s = %v, want %v", id, got, want)
		}
	}

	// 4190 minus 4100 across fitted cells; the 35 empty slots are not flat cells.
	if got := findIn(t, properties, "battery.robot.cell.imbalance").Observations[0].Quantity.Value(); math.Abs(got-0.09) > 1e-6 {
		t.Errorf("imbalance = %v V, want 0.09 across fitted cells only", got)
	}
	if got := findIn(t, properties, "battery.robot.temperature.max").Observations[0].Quantity.Value(); got != 38 {
		t.Errorf("hottest = %v, want 38", got)
	}
	// Negative current means discharging, and the sign has to survive.
	if got := findIn(t, properties, "battery.robot.current").Observations[0].Quantity.Value(); got != -6.1 {
		t.Errorf("current = %v A, want -6.1", got)
	}
	// Firmware, which is readable here and nowhere else without the vendor SDK.
	if got := findIn(t, properties, "battery.robot.firmware").Observations[0].Text; got != "2.7" {
		t.Errorf("firmware = %q, want 2.7", got)
	}
}

// The robot's battery and the computer's are different things and must never share a row.
func TestRobotBatteryIsNamespacedAwayFromTheHostBattery(t *testing.T) {
	reader := &topicBytes{byTopic: map[string][][]byte{
		"/lf/bmsstate": {bmsBytes(50, 90, 10, []uint16{4000}, []int16{25})},
	}}
	properties, err := RobotBattery{}.Observe(context.Background(), ddsEnv(reader))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range properties {
		if p.ID == "battery.host.charge" {
			t.Error("the robot battery collided with the host battery identifier")
		}
	}
	findIn(t, properties, "battery.robot.charge")
}

// A robot that is not a Unitree humanoid publishes nothing here, and every promised row
// still appears saying so.
func TestRobotBatteryKeepsItsPromisesWhenAbsent(t *testing.T) {
	properties, err := RobotBattery{}.Observe(context.Background(), ddsEnv(&topicBytes{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, promised := range (RobotBattery{}).Provides() {
		p := findIn(t, properties, promised)
		if p.Unknown == nil || p.Unknown.Reason != robotinspect.ReasonSourceAbsent {
			t.Errorf("%s = %+v, want an unknown", promised, p.Unknown)
		}
	}
}

func TestRobotBatteryDistinguishesAnUnreadableLayout(t *testing.T) {
	properties, err := RobotBattery{}.Observe(context.Background(),
		ddsEnv(&topicBytes{byTopic: map[string][][]byte{"/lf/bmsstate": {{0x00, 0x01, 0x00, 0x00, 1, 2}}}}))
	if err != nil {
		t.Fatal(err)
	}
	charge := findIn(t, properties, "battery.robot.charge")
	if charge.Unknown == nil || charge.Unknown.Reason != robotinspect.ReasonProbeFailed {
		t.Errorf("charge = %+v, want %q", charge.Unknown, robotinspect.ReasonProbeFailed)
	}
}

func TestRobotBatterySurfacesATransportError(t *testing.T) {
	if _, err := (RobotBattery{}).Observe(context.Background(),
		ddsEnv(&topicBytes{err: errors.New("participant closed")})); err == nil {
		t.Error("a transport failure was swallowed")
	}
}

func TestRobotBatteryIsPassive(t *testing.T) {
	if got := (RobotBattery{}).Class(); got != robotinspect.ClassPassive {
		t.Errorf("class = %q, want passive", got)
	}
	if got := (Hands{}).Class(); got != robotinspect.ClassPassive {
		t.Errorf("hands class = %q, want passive", got)
	}
}

var _ = rosmsg.TypeHGBmsState
