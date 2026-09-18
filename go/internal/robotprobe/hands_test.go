package robotprobe

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/robotinspect"
)

func handBytes(motors, sensors int, powerVolts float32, hottest int16) []byte {
	var w partsWriter
	w.u32(uint32(motors))
	for i := 0; i < motors; i++ {
		w.u8(0)
		w.f32(float32(i) * 0.3)
		w.f32(0)
		w.f32(0)
		w.f32(float32(i) * 0.02)
		temperature := int16(30 + i)
		if i == motors-1 {
			temperature = hottest
		}
		w.i16(temperature)
		w.i16(temperature - 2)
		w.f32(12)
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
			w.f32(float32(p))
		}
		for p := 0; p < 12; p++ {
			w.f32(29)
		}
		w.u32(uint32(i)) // lost
		w.u32(0)
	}
	for i := 0; i < 13; i++ {
		w.f32(0)
	}
	w.i16(30)
	w.f32(powerVolts)
	w.f32(2.5)
	w.f32(5)
	w.f32(3.3)
	w.u32(0)
	w.u32(0)
	w.u32(0)
	w.u32(0)
	return w.payload()
}

// A hand loses travel as it warms — the campaign recorded 97% falling to 36% within a
// session. Motor temperature and supply voltage are what show that happening, and
// neither is in the body's LowState.
func TestHandsReportTemperatureAndSupplyPerHand(t *testing.T) {
	reader := &topicBytes{byTopic: map[string][][]byte{
		"/dex3/left/state":  {handBytes(7, 5, 24.1, 74)},
		"/dex3/right/state": {handBytes(7, 5, 23.8, 61)},
	}}

	properties, err := Hands{}.Observe(context.Background(), ddsEnv(reader))
	if err != nil {
		t.Fatal(err)
	}

	left := findIn(t, properties, "hand.left.temperature.max").Observations[0]
	if left.Quantity.Value() != 74 {
		t.Errorf("left hottest = %v, want 74", left.Quantity.Value())
	}
	if got := left.Conditions["motor"]; got != "6" {
		t.Errorf("hottest motor = %q, want 6", got)
	}
	if got := findIn(t, properties, "hand.right.temperature.max").Observations[0].Quantity.Value(); got != 61 {
		t.Errorf("right hottest = %v, want 61", got)
	}

	// The supply rails: where a hand browning out under load appears.
	if got := findIn(t, properties, "hand.left.power.volts").Observations[0].Quantity.Value(); math.Abs(got-24.1) > 1e-4 {
		t.Errorf("left supply = %v V, want 24.1", got)
	}
	if got := findIn(t, properties, "hand.left.power.amps").Observations[0].Quantity.Value(); math.Abs(got-2.5) > 1e-4 {
		t.Errorf("left draw = %v A, want 2.5", got)
	}

	if got := findIn(t, properties, "hand.left.motors").Observations[0].Quantity.Value(); got != 7 {
		t.Errorf("left motors = %v, want 7", got)
	}
	if got := findIn(t, properties, "hand.left.pressure_sensors").Observations[0].Quantity.Value(); got != 5 {
		t.Errorf("left pressure sensors = %v, want 5", got)
	}
	// A pad dropping readings is a sensor failing, not a hand at rest.
	if got := findIn(t, properties, "hand.left.pressure.lost").Observations[0].Quantity.Value(); got != 10 {
		t.Errorf("lost readings = %v, want the sum across sensors", got)
	}

	// Per-finger angles carry the hand in the axis, so left and right cannot be
	// confused for one another.
	position := findIn(t, properties, "hand.left.motor.3.position").Observations[0]
	if got := position.Quantity.Axis(); got != "left3" {
		t.Errorf("axis = %q, want left3", got)
	}
}

// Two hands must never share an identifier — the lesson from the camera collision.
func TestHandsKeepEachHandOnItsOwnIdentifiers(t *testing.T) {
	reader := &topicBytes{byTopic: map[string][][]byte{
		"/dex3/left/state":  {handBytes(7, 1, 24, 50)},
		"/dex3/right/state": {handBytes(7, 1, 24, 50)},
	}}
	properties, err := Hands{}.Observe(context.Background(), ddsEnv(reader))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, p := range properties {
		seen[p.ID]++
	}
	for id, count := range seen {
		if count > 1 {
			t.Errorf("property %q appeared %d times across two hands", id, count)
		}
	}
}

// A robot with no hands fitted publishes nothing, and every promise still resolves.
func TestHandsReportAbsenceWithoutFailing(t *testing.T) {
	properties, err := Hands{}.Observe(context.Background(), ddsEnv(&topicBytes{}))
	if err != nil {
		t.Fatalf("a robot without hands is a finding, not a failure: %v", err)
	}
	for _, promised := range (Hands{}).Provides() {
		p := findIn(t, properties, promised)
		if p.Unknown == nil || p.Unknown.Reason != robotinspect.ReasonSourceAbsent {
			t.Errorf("%s = %+v, want an unknown", promised, p.Unknown)
		}
	}
}

func TestHandsSurfaceATransportError(t *testing.T) {
	if _, err := (Hands{}).Observe(context.Background(),
		ddsEnv(&topicBytes{err: errors.New("participant closed")})); err == nil {
		t.Error("a transport failure was swallowed")
	}
}

func TestHandsHonourTopicOverrides(t *testing.T) {
	reader := &topicBytes{byTopic: map[string][][]byte{"/custom/hand": {handBytes(3, 0, 12, 40)}}}
	properties, err := Hands{Topics: map[string]string{"gripper": "/custom/hand"}}.Observe(context.Background(), ddsEnv(reader))
	if err != nil {
		t.Fatal(err)
	}
	if got := findIn(t, properties, "hand.gripper.motors").Observations[0].Quantity.Value(); got != 3 {
		t.Errorf("gripper motors = %v, want 3", got)
	}
}
