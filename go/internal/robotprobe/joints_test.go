package robotprobe

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/robotinspect"
	"github.com/wendylabsinc/wendy/go/internal/shared/rosmsg"
)

// lowStatePayload encodes a unitree_hg LowState, paying CDR alignment per field. Written
// here independently of the decoder for the same reason as in the rosmsg tests.
func lowStatePayload(modeMachine uint8, motor func(i int) rosmsg.HGMotor) []byte {
	var buf []byte
	align := func(n int) {
		for len(buf)%n != 0 {
			buf = append(buf, 0)
		}
	}
	u8 := func(v uint8) { buf = append(buf, v) }
	i16 := func(v int16) { align(2); buf = binary.LittleEndian.AppendUint16(buf, uint16(v)) }
	u32 := func(v uint32) { align(4); buf = binary.LittleEndian.AppendUint32(buf, v) }
	f32 := func(v float32) { align(4); buf = binary.LittleEndian.AppendUint32(buf, math.Float32bits(v)) }

	u32(1)
	u32(2)
	u8(0)
	u8(modeMachine)
	u32(7)
	for i := 0; i < 13; i++ {
		f32(float32(i)) // imu quaternion, gyro, accel, rpy
	}
	i16(39) // imu temperature
	for i := 0; i < rosmsg.HGMotorCount; i++ {
		m := motor(i)
		u8(m.Mode)
		f32(m.Position)
		f32(m.Velocity)
		f32(m.Acceleration)
		f32(m.Torque)
		i16(m.TemperatureC[0])
		i16(m.TemperatureC[1])
		f32(m.Voltage)
		u32(0)
		u32(0)
		u32(m.State)
		for r := 0; r < 4; r++ {
			u32(0)
		}
	}
	buf = append(buf, make([]byte, 40)...)
	for i := 0; i < 4; i++ {
		u32(0)
	}
	u32(0)
	return append([]byte{0x00, 0x01, 0x00, 0x00}, buf...)
}

// g1Motors shapes a G1: 29 live body motors, the rest of the fixed array idle.
func g1Motors(i int) rosmsg.HGMotor {
	if i >= 29 {
		return rosmsg.HGMotor{} // unused slot: no volts, no heat
	}
	return rosmsg.HGMotor{
		Position:     float32(i) * 0.05,
		Torque:       float32(i) * 0.5,
		Voltage:      48,
		TemperatureC: [2]int16{int16(40 + i), int16(45 + i)},
	}
}

type lowStateReader struct {
	payloads [][]byte
	err      error
	asked    string
	askedFor string
}

func (r *lowStateReader) Sample(_ context.Context, topic, typeName string, _ time.Duration, _ int) ([][]byte, error) {
	r.asked, r.askedFor = topic, typeName
	return r.payloads, r.err
}

func jointsEnv(reader TopicReader) *robotinspect.Env {
	return robotinspect.NewEnv().Offer(robotinspect.RequirementDDSDomain, reader)
}

func TestJointsReportsEveryLiveMotor(t *testing.T) {
	reader := &lowStateReader{payloads: [][]byte{lowStatePayload(9, g1Motors)}}
	properties, err := Joints{}.Observe(context.Background(), jointsEnv(reader))
	if err != nil {
		t.Fatal(err)
	}

	if reader.asked != "/lowstate" || reader.askedFor != rosmsg.TypeHGLowState {
		t.Errorf("sampled %q as %q", reader.asked, reader.askedFor)
	}

	// Both counts are reported, so the liveness heuristic hides nothing.
	if got := findIn(t, properties, "joints.slots").Observations[0].Quantity.Value(); got != 35 {
		t.Errorf("slots = %v, want 35", got)
	}
	if got := findIn(t, properties, "joints.live").Observations[0].Quantity.Value(); got != 29 {
		t.Errorf("live = %v, want 29", got)
	}

	position := findIn(t, properties, "joint.10.position").Observations[0]
	if math.Abs(position.Quantity.Value()-0.5) > 1e-6 {
		t.Errorf("joint 10 position = %v, want 0.5", position.Quantity.Value())
	}
	// An angle carries its axis, and for a bare joint reading the axis is the joint.
	if got := position.Quantity.Axis(); got != "joint10" {
		t.Errorf("axis = %q, want joint10", got)
	}
	if position.Kind != robotinspect.Measured {
		t.Errorf("kind = %q, want measured", position.Kind)
	}

	// Both temperature sensors survive, under their wire index.
	for sensor, want := range map[int]float64{0: 50, 1: 55} {
		id := fmt.Sprintf("joint.10.temperature.%d", sensor)
		if got := findIn(t, properties, id).Observations[0].Quantity.Value(); got != want {
			t.Errorf("%s = %v, want %v", id, got, want)
		}
	}

	// An idle slot produces no rows at all.
	for _, p := range properties {
		if strings.HasPrefix(p.ID, "joint.30.") {
			t.Errorf("idle slot %s was reported as a joint", p.ID)
		}
	}
}

// The hottest motor is the number an operator acts on, and it names which joint it was.
func TestJointsSurfacesTheHottestMotor(t *testing.T) {
	hot := func(i int) rosmsg.HGMotor {
		m := g1Motors(i)
		if i == 17 {
			m.TemperatureC = [2]int16{74, 91}
		}
		return m
	}
	properties, err := Joints{}.Observe(context.Background(),
		jointsEnv(&lowStateReader{payloads: [][]byte{lowStatePayload(9, hot)}}))
	if err != nil {
		t.Fatal(err)
	}
	hottest := findIn(t, properties, "joints.temperature.max").Observations[0]
	if hottest.Quantity.Value() != 91 {
		t.Errorf("hottest = %v, want 91", hottest.Quantity.Value())
	}
	if got := hottest.Conditions["joint"]; got != "17" {
		t.Errorf("hottest joint = %q, want 17", got)
	}
}

// The mode words are carried verbatim and never presented as the execution FSM id, which
// is larger than these fields and is not published here.
func TestJointsCarriesTheModeWordsWithoutInterpretingThem(t *testing.T) {
	properties, err := Joints{}.Observe(context.Background(),
		jointsEnv(&lowStateReader{payloads: [][]byte{lowStatePayload(4, g1Motors)}}))
	if err != nil {
		t.Fatal(err)
	}
	if got := findIn(t, properties, "robot.mode_machine").Observations[0].Text; got != "4" {
		t.Errorf("mode_machine = %q, want 4", got)
	}
	findIn(t, properties, "robot.mode_pr")
}

// A motor reporting a fault gets a row; a healthy one does not.
func TestJointsReportsAMotorFaultVerbatim(t *testing.T) {
	faulty := func(i int) rosmsg.HGMotor {
		m := g1Motors(i)
		if i == 3 {
			m.State = 0x00000041
		}
		return m
	}
	properties, err := Joints{}.Observe(context.Background(),
		jointsEnv(&lowStateReader{payloads: [][]byte{lowStatePayload(9, faulty)}}))
	if err != nil {
		t.Fatal(err)
	}
	if got := findIn(t, properties, "joint.3.fault").Observations[0].Text; got != "0x00000041" {
		t.Errorf("fault = %q, want the raw word", got)
	}
	for _, p := range properties {
		if p.ID == "joint.4.fault" {
			t.Error("a healthy motor was given a fault row")
		}
	}
}

// A robot that is not a Unitree humanoid does not publish this. That is the answer, and
// it must be distinguishable from nobody having looked.
func TestJointsReportsAbsenceOnAnyOtherRobot(t *testing.T) {
	properties, err := Joints{}.Observe(context.Background(), jointsEnv(&lowStateReader{}))
	if err != nil {
		t.Fatalf("a robot without this topic is a finding, not a failure: %v", err)
	}
	slots := findIn(t, properties, "joints.slots")
	if slots.Unknown == nil || slots.Unknown.Reason != robotinspect.ReasonSourceAbsent {
		t.Errorf("slots = %+v, want an unknown", slots.Unknown)
	}
}

// A payload this decoder does not recognise is reported as a failure to read, not as an
// absence: the topic was there and we could not make sense of it.
func TestJointsDistinguishesAnUnreadableLayoutFromAnAbsentTopic(t *testing.T) {
	properties, err := Joints{}.Observe(context.Background(),
		jointsEnv(&lowStateReader{payloads: [][]byte{{0x00, 0x01, 0x00, 0x00, 1, 2, 3}}}))
	if err != nil {
		t.Fatal(err)
	}
	slots := findIn(t, properties, "joints.slots")
	if slots.Unknown == nil || slots.Unknown.Reason != robotinspect.ReasonProbeFailed {
		t.Errorf("slots = %+v, want %q", slots.Unknown, robotinspect.ReasonProbeFailed)
	}
}

func TestJointsSurfacesATransportError(t *testing.T) {
	reader := &lowStateReader{err: errors.New("participant closed")}
	if _, err := (Joints{}).Observe(context.Background(), jointsEnv(reader)); err == nil {
		t.Error("a transport failure was swallowed")
	}
}

func TestJointsIsPassive(t *testing.T) {
	if got := (Joints{}).Class(); got != robotinspect.ClassPassive {
		t.Errorf("class = %q, want passive; reading a topic must never be able to actuate", got)
	}
}

func TestJointsHonoursATopicOverride(t *testing.T) {
	reader := &lowStateReader{payloads: [][]byte{lowStatePayload(9, g1Motors)}}
	if _, err := (Joints{Topic: "/lf/lowstate"}).Observe(context.Background(), jointsEnv(reader)); err != nil {
		t.Fatal(err)
	}
	if reader.asked != "/lf/lowstate" {
		t.Errorf("sampled %q, want the override", reader.asked)
	}
}
