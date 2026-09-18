package commands

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/wendylabsinc/wendy/go/internal/shared/robotcal"
	"github.com/wendylabsinc/wendy/go/internal/shared/streamreason"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// g1Order is the shape of the real robot's profile: 29 named joints over a
// message that carries 35 slots.
var g1Order = []string{
	"left_hip_pitch_joint", "left_hip_roll_joint", "left_hip_yaw_joint",
	"left_knee_joint", "left_ankle_pitch_joint", "left_ankle_roll_joint",
	"right_hip_pitch_joint", "right_hip_roll_joint", "right_hip_yaw_joint",
	"right_knee_joint", "right_ankle_pitch_joint", "right_ankle_roll_joint",
	"waist_yaw_joint", "waist_roll_joint", "waist_pitch_joint",
	"left_shoulder_pitch_joint", "left_shoulder_roll_joint", "left_shoulder_yaw_joint",
	"left_elbow_joint", "left_wrist_roll_joint", "left_wrist_pitch_joint", "left_wrist_yaw_joint",
	"right_shoulder_pitch_joint", "right_shoulder_roll_joint", "right_shoulder_yaw_joint",
	"right_elbow_joint", "right_wrist_roll_joint", "right_wrist_pitch_joint", "right_wrist_yaw_joint",
}

// fakeRobotAgent serves StreamJointPositions from a script, so the CLI half can
// be driven without a robot, a device or a DDS domain.
type fakeRobotAgent struct {
	agentpbv2.UnimplementedWendyRobotServiceServer
	samples []*agentpbv2.JointPositionSample
	// endWith, when set, is how the stream finishes after the samples run out.
	endWith error
	// hold keeps the stream open after the samples instead of ending it.
	hold chan struct{}

	requests []*agentpbv2.StreamJointPositionsRequest
}

func (f *fakeRobotAgent) StreamJointPositions(
	req *agentpbv2.StreamJointPositionsRequest,
	stream agentpbv2.WendyRobotService_StreamJointPositionsServer,
) error {
	f.requests = append(f.requests, req)
	for _, sample := range f.samples {
		if err := stream.Send(sample); err != nil {
			return err
		}
	}
	if f.hold != nil {
		select {
		case <-f.hold:
		case <-stream.Context().Done():
		}
	}
	return f.endWith
}

func dialFakeRobotAgent(t *testing.T, agent *fakeRobotAgent) *grpc.ClientConn {
	t.Helper()
	lis := bufconn.Listen(1024 * 1024)
	srv := grpc.NewServer()
	agentpbv2.RegisterWendyRobotServiceServer(srv, agent)
	go func() { _ = srv.Serve(lis) }()
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() {
		conn.Close()
		srv.Stop()
		lis.Close()
	})
	return conn
}

// g1Sample is what the agent sends for a G1: 35 slots, 27 of them reporting.
// Slots 13 and 14 — waist roll and pitch — are absent because the robot does not
// actuate them, and slot 0 is present at exactly zero.
func g1Sample() *agentpbv2.JointPositionSample {
	sample := &agentpbv2.JointPositionSample{
		ObservedAtUnixNanos: time.Now().UnixNano(),
		SlotCount:           35,
		Unit:                "rad",
	}
	for i := range 29 {
		if i == 13 || i == 14 {
			continue
		}
		sample.Slots = append(sample.Slots, &agentpbv2.JointSlot{
			Index:    uint32(i),
			Position: float64(i) / 10,
		})
	}
	return sample
}

func openG1JointSource(t *testing.T, agent *fakeRobotAgent, params map[string]string) (*unitreeLowStateJointSource, error) {
	t.Helper()
	source, err := openJointSource(dialFakeRobotAgent(t, agent))(t.Context(), robotcal.Joints{
		Unit:   "rad",
		Order:  g1Order,
		Source: robotcal.JointSourceSpec{Backend: backendUnitreeLowState, Params: params},
	})
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { _ = source.Close() })
	return source.(*unitreeLowStateJointSource), nil
}

// TestIdleSlotsNeverArriveAsAZeroAngle is the rule the whole backend turns on: a
// slot the robot does not actuate must be missing from the reading, because a
// zero would read as a joint that was swept and did not move.
func TestIdleSlotsNeverArriveAsAZeroAngle(t *testing.T) {
	agent := &fakeRobotAgent{samples: []*agentpbv2.JointPositionSample{g1Sample()}, hold: make(chan struct{})}
	source, err := openG1JointSource(t, agent, nil)
	if err != nil {
		t.Fatalf("opening: %v", err)
	}

	reading, err := source.Read(t.Context())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	for _, absent := range []string{"waist_roll_joint", "waist_pitch_joint"} {
		if position, present := reading.Positions[absent]; present {
			t.Errorf("%s is not actuated on this robot and arrived as %v: a zero angle is "+
				"indistinguishable from a joint that was swept and did not move", absent, position)
		}
	}
	if len(reading.Positions) != 27 {
		t.Errorf("reported %d joints, want 27", len(reading.Positions))
	}
	// The names are the profile's, resolved by index — the message carries none.
	if got, want := reading.Positions["right_shoulder_pitch_joint"], 2.2; got != want {
		t.Errorf("right_shoulder_pitch_joint = %v, want %v (slot 22)", got, want)
	}
	if _, present := reading.Positions["left_hip_pitch_joint"]; !present {
		t.Error("slot 0 is driven and at exactly zero radians; it must still be reported")
	}
}

// TestTheSourceReportsNoJointOrderOfItsOwn: the wizard compares a source's
// reported order against the profile's, and this source has none — the wire
// message carries indices and no names. Handing the profile's own order back
// would make that check agree with itself and look like evidence.
func TestTheSourceReportsNoJointOrderOfItsOwn(t *testing.T) {
	agent := &fakeRobotAgent{samples: []*agentpbv2.JointPositionSample{g1Sample()}, hold: make(chan struct{})}
	source, err := openG1JointSource(t, agent, nil)
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	reading, err := source.Read(t.Context())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(reading.Order) != 0 {
		t.Fatalf("Order = %v, want none: this robot publishes no joint names, so claiming an "+
			"order would turn the joint-map check into a tautology", reading.Order)
	}
}

// TestASlotTheProfileDoesNotNameIsStillVisible: a robot driving a slot beyond
// the profile's order is a disagreement worth surfacing, under a key nobody
// could mistake for a joint name.
func TestASlotTheProfileDoesNotNameIsStillVisible(t *testing.T) {
	sample := g1Sample()
	sample.Slots = append(sample.Slots, &agentpbv2.JointSlot{Index: 31, Position: 0.75})
	agent := &fakeRobotAgent{samples: []*agentpbv2.JointPositionSample{sample}, hold: make(chan struct{})}
	source, err := openG1JointSource(t, agent, nil)
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	reading, err := source.Read(t.Context())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got, present := reading.Positions["slot[31]"]; !present || got != 0.75 {
		t.Fatalf("slot 31 = (%v, present=%v), want it reported under a key that is not a joint "+
			"name — dropping it would hide a robot driving something the profile has never heard of",
			got, present)
	}
}

// TestAStreamThatStopsMidSweepStopsReading: once the stream has ended, Read must
// say so rather than hand back the pose it was holding. A frozen reading is
// measured as a joint holding perfectly still.
func TestAStreamThatStopsMidSweepStopsReading(t *testing.T) {
	agent := &fakeRobotAgent{samples: []*agentpbv2.JointPositionSample{g1Sample()}}
	source, err := openG1JointSource(t, agent, nil)
	if err != nil {
		t.Fatalf("opening: %v", err)
	}

	// The first read may land before or after the server closed the stream, so
	// wait for the end rather than racing it.
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err = source.Read(t.Context())
		if err != nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err == nil {
		t.Fatal("Read kept returning a pose after the stream ended: a frozen reading is measured " +
			"as a joint that did not travel")
	}
	if !strings.Contains(err.Error(), "/lowstate") {
		t.Errorf("error = %v, want it to name the topic that stopped", err)
	}
}

// TestASilentRobotIsToldApartFromAnUnreachableOne: the agent marks "we listened
// and heard nothing" with a reason, and the CLI says so in those words rather
// than repeating a bare NOT_FOUND that a lost tunnel also produces.
func TestASilentRobotIsToldApartFromAnUnreachableOne(t *testing.T) {
	silent := &fakeRobotAgent{endWith: streamreason.New(codes.NotFound,
		"nothing published the joint topic: unitree-lowstate on /lowstate did not appear on \"eth0\"",
		streamreason.RobotJointSourceAbsent, map[string]string{"source": "unitree-lowstate on /lowstate"})}
	source, err := openG1JointSource(t, silent, nil)
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	_, err = source.Read(t.Context())
	if err == nil {
		t.Fatal("want a refusal from a robot that published nothing")
	}
	if !strings.Contains(err.Error(), "heard nothing") {
		t.Errorf("error = %v, want it to say the device listened and heard nothing", err)
	}

	unreachable := &fakeRobotAgent{endWith: streamreason.New(codes.NotFound,
		"device not found", "NOT_OURS", nil)}
	other, err := openG1JointSource(t, unreachable, nil)
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	_, err = other.Read(t.Context())
	if err == nil {
		t.Fatal("want a refusal")
	}
	if strings.Contains(err.Error(), "heard nothing") {
		t.Errorf("error = %v: a NOT_FOUND without the agent's reason is not evidence that the "+
			"robot is quiet", err)
	}
}

// TestAnOlderAgentIsNamedAsSuch: the RPC is new, so a device that has not been
// updated answers UNIMPLEMENTED, and the operator is told what to do about it
// rather than shown a gRPC envelope.
func TestAnOlderAgentIsNamedAsSuch(t *testing.T) {
	err := unitreeJointStreamError(status.Error(codes.Unimplemented,
		"unknown method StreamJointPositions"))
	if !strings.Contains(err.Error(), "does not serve joint positions") ||
		!strings.Contains(err.Error(), "wendy device update") {
		t.Errorf("error = %v, want it to name an agent that is too old and what to do", err)
	}
}

func TestUnitreeJointSourceParameters(t *testing.T) {
	t.Run("the shipped G1 profile's parameters are accepted", func(t *testing.T) {
		profile, err := robotcal.LoadProfile("unitree-g1")
		if err != nil {
			t.Fatal(err)
		}
		if profile.Joints.Source.Backend != backendUnitreeLowState {
			t.Skipf("the G1 profile no longer selects %s", backendUnitreeLowState)
		}
		if _, err := parseUnitreeLowStateParams(profile.Joints.Source.Params); err != nil {
			t.Fatalf("the shipped profile's own parameters are refused: %v", err)
		}
	})

	t.Run("an unknown parameter is refused rather than ignored", func(t *testing.T) {
		_, err := parseUnitreeLowStateParams(map[string]string{"baud": "1000000"})
		if err == nil || !strings.Contains(err.Error(), "baud") {
			t.Fatalf("err = %v, want it to name the parameter nothing reads", err)
		}
	})

	t.Run("a position field this backend does not read is refused", func(t *testing.T) {
		_, err := parseUnitreeLowStateParams(map[string]string{"position_field": "motor_state.dq"})
		if err == nil || !strings.Contains(err.Error(), "motor_state.q") {
			t.Fatalf("err = %v, want it to say which field is actually read", err)
		}
	})

	t.Run("parameters reach the agent", func(t *testing.T) {
		agent := &fakeRobotAgent{samples: []*agentpbv2.JointPositionSample{g1Sample()}, hold: make(chan struct{})}
		source, err := openG1JointSource(t, agent, map[string]string{
			"topic":           "/lf/lowstate",
			"domain_id":       "7",
			"interface":       "enP8p1s0",
			"min_interval_ms": "40",
			"position_field":  "motor_state.q",
		})
		if err != nil {
			t.Fatalf("opening: %v", err)
		}
		if _, err := source.Read(t.Context()); err != nil {
			t.Fatalf("Read: %v", err)
		}
		if len(agent.requests) != 1 {
			t.Fatalf("the agent saw %d requests, want 1", len(agent.requests))
		}
		req := agent.requests[0]
		if req.GetBackend() != backendUnitreeLowState || req.GetTopic() != "/lf/lowstate" ||
			req.GetDomainId() != 7 || req.GetInterface() != "enP8p1s0" || req.GetMinIntervalMillis() != 40 {
			t.Errorf("request = %+v, want the profile's parameters carried through", req)
		}
		if !strings.Contains(source.Describe(), "/lf/lowstate") {
			t.Errorf("Describe() = %q, want it to name the topic being read", source.Describe())
		}
	})

	t.Run("a profile with no joint order is refused", func(t *testing.T) {
		agent := &fakeRobotAgent{hold: make(chan struct{})}
		_, err := openJointSource(dialFakeRobotAgent(t, agent))(t.Context(), robotcal.Joints{
			Unit:   "rad",
			Source: robotcal.JointSourceSpec{Backend: backendUnitreeLowState},
		})
		if err == nil || !strings.Contains(err.Error(), "joints.order") {
			t.Fatalf("err = %v, want a refusal naming the missing joint order: the message carries "+
				"no joint names, so there would be nothing to call what it reads", err)
		}
		if len(agent.requests) != 0 {
			t.Error("the robot was asked for joints before we could name them")
		}
	})
}
