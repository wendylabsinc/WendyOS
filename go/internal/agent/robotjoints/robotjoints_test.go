package robotjoints

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/rtps"
	"github.com/wendylabsinc/wendy/go/internal/shared/rosmsg"
)

// --- a synthetic unitree_hg/LowState -----------------------------------------
//
// Written out here rather than borrowed from the decoder so the two cannot share
// a misunderstanding about the layout: CDR aligns per field, not per struct, so a
// motor does not start on a four-byte boundary.

type hgWriter struct{ buf []byte }

func (w *hgWriter) align(n int) {
	for len(w.buf)%n != 0 {
		w.buf = append(w.buf, 0)
	}
}
func (w *hgWriter) u8(v uint8) { w.buf = append(w.buf, v) }
func (w *hgWriter) i16(v int16) {
	w.align(2)
	w.buf = binary.LittleEndian.AppendUint16(w.buf, uint16(v))
}
func (w *hgWriter) u32(v uint32) {
	w.align(4)
	w.buf = binary.LittleEndian.AppendUint32(w.buf, v)
}
func (w *hgWriter) f32(v float32) { w.align(4); w.u32x(math.Float32bits(v)) }
func (w *hgWriter) u32x(v uint32) { w.buf = binary.LittleEndian.AppendUint32(w.buf, v) }

// lowStatePayload encodes a whole message, shaping each motor with motor(i).
func lowStatePayload(motor func(i int) rosmsg.HGMotor) []byte {
	var w hgWriter
	w.u32(1)
	w.u32(2)  // version[2]
	w.u8(0)   // mode_pr
	w.u8(0)   // mode_machine
	w.u32(17) // tick
	for i := 0; i < 13; i++ {
		w.f32(0) // imu quaternion/gyro/accel/rpy
	}
	w.i16(40) // imu temperature
	for i := 0; i < rosmsg.HGMotorCount; i++ {
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
	w.u32(0) // crc
	return append([]byte{0x00, 0x01, 0x00, 0x00}, w.buf...)
}

// g1Payload is the shape the real robot has: 35 slots, 27 driven. Slots 13 and
// 14 (waist roll and pitch) and everything past 28 report nothing.
//
// Slot 0 is deliberately driven and sitting at exactly zero radians — the case
// that makes "absent" and "zero" different answers rather than a distinction
// nobody can observe.
var g1IdleSlots = []int{13, 14, 29, 30, 31, 32, 33, 34}

func g1Payload() []byte {
	return lowStatePayload(func(i int) rosmsg.HGMotor {
		if slices.Contains(g1IdleSlots, i) {
			return rosmsg.HGMotor{}
		}
		m := rosmsg.HGMotor{
			Voltage:      48,
			TemperatureC: [2]int16{35, 36},
		}
		if i != 0 {
			m.Position = float32(i) / 10
		}
		return m
	})
}

func TestIdleSlotsComeBackAbsentAndNotAsZero(t *testing.T) {
	reading, err := decodeUnitreeLowState(g1Payload())
	if err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if reading.SlotCount != rosmsg.HGMotorCount {
		t.Errorf("SlotCount = %d, want %d — the total has to stay visible beside the live ones",
			reading.SlotCount, rosmsg.HGMotorCount)
	}
	if reading.Unit != "rad" {
		t.Errorf("Unit = %q, want rad", reading.Unit)
	}

	got := map[int]float64{}
	for _, slot := range reading.Slots {
		got[slot.Index] = slot.Position
	}
	if len(got) != rosmsg.HGMotorCount-len(g1IdleSlots) {
		t.Fatalf("reported %d slots, want %d", len(got), rosmsg.HGMotorCount-len(g1IdleSlots))
	}
	for _, idle := range g1IdleSlots {
		if position, present := got[idle]; present {
			t.Errorf("slot %d is idle on this robot and came back as %v: a zero angle reads as a joint "+
				"sitting perfectly still, which is the false calibration the wizard's rules exist to prevent",
				idle, position)
		}
	}
	// The distinction is only worth anything if a driven joint really at zero
	// still arrives.
	if position, present := got[0]; !present || position != 0 {
		t.Errorf("slot 0 is driven and at zero radians; got (%v, present=%v) — a real zero must not be "+
			"mistaken for an idle slot", position, present)
	}
	// Indices must not shift to close the gaps: the client resolves a slot to a
	// joint name through the profile's order, by index.
	if position := got[15]; position != 1.5 {
		t.Errorf("slot 15 = %v, want 1.5 — indices must survive the idle slots either side of them", position)
	}
}

func TestATruncatedPayloadIsAFailureNotAnAbsence(t *testing.T) {
	payload := g1Payload()
	if _, err := decodeUnitreeLowState(payload[:len(payload)-8]); err == nil {
		t.Fatal("want a decode failure for a payload the layout does not fit")
	} else if errors.Is(err, ErrSourceAbsent) {
		t.Fatalf("a payload that does not decode is not an absence: %v", err)
	}
}

// --- a fake lease -------------------------------------------------------------

type fakeLease struct {
	endpoints []rtps.Endpoint
	changed   chan struct{}
	samples   chan rtps.Sample
	done      chan struct{}
	subscribe error

	subscribed []rtps.Endpoint
	closed     bool
}

func newFakeLease() *fakeLease {
	return &fakeLease{
		changed: make(chan struct{}, 1),
		samples: make(chan rtps.Sample, 16),
		done:    make(chan struct{}),
	}
}

func (f *fakeLease) Endpoints() []rtps.Endpoint  { return f.endpoints }
func (f *fakeLease) Changed() <-chan struct{}    { return f.changed }
func (f *fakeLease) Samples() <-chan rtps.Sample { return f.samples }
func (f *fakeLease) Done() <-chan struct{}       { return f.done }
func (f *fakeLease) Close() error                { f.closed = true; return nil }
func (f *fakeLease) Subscribe(ep rtps.Endpoint) error {
	f.subscribed = append(f.subscribed, ep)
	return f.subscribe
}

func (f *fakeLease) announce(ep rtps.Endpoint) {
	f.endpoints = append(f.endpoints, ep)
	select {
	case f.changed <- struct{}{}:
	default:
	}
}

func lowStateEndpoint(topic string) rtps.Endpoint {
	return rtps.Endpoint{
		GUID:  rtps.GUID{EntityID: 1},
		Topic: topic,
		Type:  rosmsg.TypeHGLowState,
	}
}

// testReader builds a reader over lease, with windows short enough that the
// absence rules can be exercised without waiting on a robot.
func testReader(t *testing.T, cfg Config, lease Lease) *Reader {
	t.Helper()
	if cfg.Backend == "" {
		cfg.Backend = BackendUnitreeLowState
	}
	if cfg.Interface == "" {
		cfg.Interface = "eth0"
	}
	r, err := NewReader(cfg, rtps.NewPool(), nil)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	r.acquire = func(context.Context, rtps.Config) (Lease, error) { return lease, nil }
	r.discoverWindow = 50 * time.Millisecond
	r.silenceWindow = 100 * time.Millisecond
	return r
}

func TestARobotThatPublishesNothingIsAnAbsence(t *testing.T) {
	lease := newFakeLease()
	// Something else is on the graph, so this is a robot that is reachable and
	// quiet rather than a device that could not listen.
	lease.announce(rtps.Endpoint{Topic: "rt/tf", Type: "tf2_msgs::msg::dds_::TFMessage_"})

	err := testReader(t, Config{}, lease).Stream(t.Context(), func(Reading) error { return nil })
	if !errors.Is(err, ErrSourceAbsent) {
		t.Fatalf("Stream() = %v, want ErrSourceAbsent", err)
	}
	if errors.Is(err, ErrNoInterface) {
		t.Fatal("a silent robot must not be reported as a device that could not listen")
	}
	if len(lease.subscribed) != 0 {
		t.Errorf("subscribed to %v with no matching writer", lease.subscribed)
	}
	if !lease.closed {
		t.Error("the participant lease was left open after the stream ended")
	}
}

func TestJoiningFailureIsNotAnAbsence(t *testing.T) {
	r := testReader(t, Config{}, newFakeLease())
	r.acquire = func(context.Context, rtps.Config) (Lease, error) {
		return nil, errors.New("bind: cannot assign requested address")
	}
	err := r.Stream(t.Context(), func(Reading) error { return nil })
	if !errors.Is(err, ErrNoInterface) {
		t.Fatalf("Stream() = %v, want ErrNoInterface", err)
	}
	if errors.Is(err, ErrSourceAbsent) {
		t.Fatal("a device that could not join a DDS domain heard nothing because it was not " +
			"listening; reporting that as a silent robot sends the operator to the wrong machine")
	}
}

func TestAStreamThatStopsMidSweepEndsRatherThanFreezing(t *testing.T) {
	lease := newFakeLease()
	endpoint := lowStateEndpoint("rt/lowstate")
	lease.announce(endpoint)
	payload := g1Payload()
	for range 3 {
		lease.samples <- rtps.Sample{Writer: endpoint.GUID, Payload: payload}
	}

	r := testReader(t, Config{MinInterval: time.Nanosecond}, lease)
	var seen int
	err := r.Stream(t.Context(), func(Reading) error { seen++; return nil })
	if !errors.Is(err, ErrSourceAbsent) {
		t.Fatalf("Stream() = %v, want ErrSourceAbsent once the publisher goes quiet", err)
	}
	if seen != 3 {
		t.Fatalf("delivered %d readings, want 3", seen)
	}
	// The count belongs in the message: "it stopped after three samples" and "it
	// never started" are different things to have to explain to an operator.
	if want := "after 3 samples"; !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to mention %q", err, want)
	}
}

func TestTheWireSpellingOfATopicMatches(t *testing.T) {
	for _, announced := range []string{"rt/lowstate", "/lowstate"} {
		t.Run(announced, func(t *testing.T) {
			lease := newFakeLease()
			endpoint := lowStateEndpoint(announced)
			lease.announce(endpoint)
			lease.samples <- rtps.Sample{Writer: endpoint.GUID, Payload: g1Payload()}

			r := testReader(t, Config{Topic: "/lowstate"}, lease)
			var seen int
			_ = r.Stream(t.Context(), func(Reading) error { seen++; return nil })
			if seen == 0 {
				t.Fatalf("nothing was read from a writer announced as %q", announced)
			}
		})
	}
}

func TestSurplusSamplesAreDroppedAtTheSource(t *testing.T) {
	lease := newFakeLease()
	endpoint := lowStateEndpoint("rt/lowstate")
	lease.announce(endpoint)
	for range 8 {
		lease.samples <- rtps.Sample{Writer: endpoint.GUID, Payload: g1Payload()}
	}

	r := testReader(t, Config{MinInterval: 50 * time.Millisecond}, lease)
	// A clock that does not advance: every sample after the first is inside the
	// interval, so exactly one should be delivered.
	frozen := time.Now()
	r.now = func() time.Time { return frozen }

	var seen int
	_ = r.Stream(t.Context(), func(Reading) error { seen++; return nil })
	if seen != 1 {
		t.Fatalf("delivered %d readings from 8 samples at a frozen clock, want 1", seen)
	}
}

func TestSamplesFromAnotherWriterAreIgnored(t *testing.T) {
	lease := newFakeLease()
	endpoint := lowStateEndpoint("rt/lowstate")
	lease.announce(endpoint)
	lease.samples <- rtps.Sample{Writer: rtps.GUID{EntityID: 99}, Payload: g1Payload()}

	r := testReader(t, Config{MinInterval: time.Nanosecond}, lease)
	var seen int
	err := r.Stream(t.Context(), func(Reading) error { seen++; return nil })
	if seen != 0 {
		t.Fatalf("delivered %d readings from a writer we did not subscribe to", seen)
	}
	if !errors.Is(err, ErrSourceAbsent) {
		t.Fatalf("Stream() = %v, want ErrSourceAbsent", err)
	}
}

func TestAnUndecodablePayloadIsReportedNotSmoothedOver(t *testing.T) {
	lease := newFakeLease()
	endpoint := lowStateEndpoint("rt/lowstate")
	lease.announce(endpoint)
	lease.samples <- rtps.Sample{Writer: endpoint.GUID, Payload: []byte{0x00, 0x01, 0x00, 0x00, 0xff}}

	err := testReader(t, Config{MinInterval: time.Nanosecond}, lease).
		Stream(t.Context(), func(Reading) error { return nil })
	if err == nil {
		t.Fatal("want an error for bytes the decoder cannot read")
	}
	if errors.Is(err, ErrSourceAbsent) || errors.Is(err, ErrNoInterface) {
		t.Fatalf("a topic that is there and unreadable is neither an absence nor a transport "+
			"failure, got %v", err)
	}
}

func TestCancellingTheClientEndsTheStreamCleanly(t *testing.T) {
	lease := newFakeLease()
	lease.announce(lowStateEndpoint("rt/lowstate"))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := testReader(t, Config{}, lease).Stream(ctx, func(Reading) error { return nil }); err != nil {
		t.Fatalf("Stream() = %v, want nil when the client stopped listening", err)
	}
}

// TestAnInterfaceThatWillNotOpenDoesNotStopTheNextOne: a device runs container
// bridges beside the NIC the robot is on, so giving up at the first interface
// that refuses to join would find the robot only by luck.
func TestAnInterfaceThatWillNotOpenDoesNotStopTheNextOne(t *testing.T) {
	lease := newFakeLease()
	endpoint := lowStateEndpoint("rt/lowstate")
	lease.announce(endpoint)
	lease.samples <- rtps.Sample{Writer: endpoint.GUID, Payload: g1Payload()}

	r := testReader(t, Config{MinInterval: time.Nanosecond}, lease)
	r.cfg.Interface = ""
	r.candidateInterfaces = func() ([]string, error) { return []string{"docker0", "eth0"}, nil }
	var tried []string
	r.acquire = func(_ context.Context, cfg rtps.Config) (Lease, error) {
		tried = append(tried, cfg.Interface)
		if cfg.Interface == "docker0" {
			return nil, errors.New("bind: cannot assign requested address")
		}
		return lease, nil
	}

	var seen int
	_ = r.Stream(t.Context(), func(Reading) error { seen++; return nil })
	if seen == 0 {
		t.Fatalf("read nothing after trying %v; the robot was on the second interface", tried)
	}
	if len(tried) != 2 || tried[1] != "eth0" {
		t.Errorf("tried = %v, want both interfaces in order", tried)
	}
}

// TestADeviceWithNowhereToListenSaysSo: no eligible interface is a device that
// cannot listen, never a robot that is quiet.
func TestADeviceWithNowhereToListenSaysSo(t *testing.T) {
	r := testReader(t, Config{}, newFakeLease())
	r.cfg.Interface = ""
	r.candidateInterfaces = func() ([]string, error) { return nil, nil }
	err := r.Stream(t.Context(), func(Reading) error { return nil })
	if !errors.Is(err, ErrNoInterface) {
		t.Fatalf("Stream() = %v, want ErrNoInterface", err)
	}
}

func TestAnUnknownBackendIsRefusedByName(t *testing.T) {
	for _, backend := range []string{"", "feetech-serial"} {
		_, err := NewReader(Config{Backend: backend}, rtps.NewPool(), nil)
		if !errors.Is(err, ErrUnknownBackend) {
			t.Fatalf("NewReader(%q) = %v, want ErrUnknownBackend", backend, err)
		}
		if !strings.Contains(err.Error(), BackendUnitreeLowState) {
			t.Errorf("refusal for %q = %v, want it to name what this build can read", backend, err)
		}
	}
}

func TestAnAgentWithNoParticipantPoolRefusesRatherThanReportingSilence(t *testing.T) {
	if _, err := NewReader(Config{Backend: BackendUnitreeLowState}, nil, nil); err == nil {
		t.Fatal("want a refusal when there is no pool to join a domain with")
	}
}

func TestThrottleIsBounded(t *testing.T) {
	r, err := NewReader(Config{Backend: BackendUnitreeLowState, MinInterval: time.Hour}, rtps.NewPool(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.cfg.MinInterval != MaxMinInterval {
		t.Errorf("MinInterval = %v, want it clamped to %v — a sweep sampled slower than that is not "+
			"measuring a moving joint", r.cfg.MinInterval, MaxMinInterval)
	}
	r, err = NewReader(Config{Backend: BackendUnitreeLowState}, rtps.NewPool(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.cfg.MinInterval != DefaultMinInterval {
		t.Errorf("MinInterval = %v, want the default %v", r.cfg.MinInterval, DefaultMinInterval)
	}
	if r.cfg.Topic != "/lowstate" {
		t.Errorf("Topic = %q, want the backend's own default", r.cfg.Topic)
	}
}
