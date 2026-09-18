package commands

import (
	"context"
	"encoding/binary"
	"math"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/wendylabsinc/wendy/go/internal/cli/robotwizard"
	"github.com/wendylabsinc/wendy/go/internal/shared/robotcal"
	"github.com/wendylabsinc/wendy/go/internal/shared/rosmsg"
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

// --- the robot's own bytes -------------------------------------------------
//
// Every payload in this file starts as a unitree_hg/msg/LowState that
// unitree-g1-nx-2 published on 2026-09-18, captured with the robot idle. Reading
// the wire is now this side's job, so the tests that check what a reading
// contains are pinned to bytes a robot actually sent rather than to a proto
// message a test wrote for itself.
//
// The offsets below are computed from the IDL, independently of the decoder, so
// the two cannot share a misunderstanding; TestThePatchHelpersAgreeWithTheDecoder
// is what holds them to that.

// hgEncapsulation is the CDR encapsulation header every payload starts with.
const hgEncapsulation = 4

// hgMotorOffset is where a motor begins in the body.
//
// motor[0] starts at 70 and is 54 bytes long, because its leading uint8 sits at
// an offset that only the float32 after it realigns; every later motor is a full
// 56. Treating all of them as 56 is the mistake that decodes into plausible wrong
// numbers instead of failing.
func hgMotorOffset(index int) int {
	if index == 0 {
		return 70
	}
	return 124 + (index-1)*56
}

func hgAlign4(n int) int { return (n + 3) &^ 3 }

// hgPositionOffset and hgVoltageOffset locate q and the motor's supply voltage —
// the field the position is read from, and the one that decides whether a slot is
// reporting at all.
func hgPositionOffset(index int) int {
	return hgEncapsulation + hgAlign4(hgMotorOffset(index)+1)
}

func hgVoltageOffset(index int) int {
	// position, velocity, acceleration, torque (16 bytes) then both int16
	// temperatures (4).
	return hgPositionOffset(index) + 20
}

// hgModeOffset locates the motor's mode word — the first byte of the motor, and
// the field that says whether it is driving or limp.
func hgModeOffset(index int) int {
	return hgEncapsulation + hgMotorOffset(index)
}

// realG1Payload is the captured message, copied so a patch cannot leak between
// tests.
func realG1Payload(t *testing.T) []byte {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join("..", "..", "shared", "rosmsg", "testdata", "unitree_hg_lowstate.bin"))
	if err != nil {
		t.Fatalf("reading the captured LowState: %v", err)
	}
	return payload
}

func putFloat32(payload []byte, offset int, v float32) {
	binary.LittleEndian.PutUint32(payload[offset:], math.Float32bits(v))
}

// driveSlot makes a slot report: a supply voltage is what tells the decoder
// something is behind it, and the position is what the sweep reads.
func driveSlot(payload []byte, index int, position float32) {
	putFloat32(payload, hgVoltageOffset(index), 48)
	putFloat32(payload, hgPositionOffset(index), position)
}

// limpSlot clears a motor's mode word, which is the difference between an arm a
// person can move and one that resists them. The capture was taken with every
// motor energised, so this is how a test gets the other state.
func limpSlot(payload []byte, index int) {
	payload[hgModeOffset(index)] = 0
}

func TestThePatchHelpersAgreeWithTheDecoder(t *testing.T) {
	payload := realG1Payload(t)
	if len(payload) != 2092 {
		t.Fatalf("the capture is %d bytes, want 2092", len(payload))
	}
	driveSlot(payload, 0, 0)
	driveSlot(payload, 31, 0.75)

	state, err := rosmsg.DecodeHGLowState(payload)
	if err != nil {
		t.Fatalf("the patched capture no longer decodes, so the offsets are wrong: %v", err)
	}
	if got := state.Motors[0].Position; got != 0 {
		t.Errorf("slot 0 position = %v, want the patched 0", got)
	}
	if got := state.Motors[31].Position; got != 0.75 {
		t.Errorf("slot 31 position = %v, want the patched 0.75", got)
	}
	if got := state.Motors[31].Voltage; got != 48 {
		t.Errorf("slot 31 voltage = %v, want the patched 48", got)
	}
	if got := state.Motors[22].Mode; got != 1 {
		t.Errorf("slot 22 mode = %d, want the 1 the robot published", got)
	}
	limpSlot(payload, 22)
	state, err = rosmsg.DecodeHGLowState(payload)
	if err != nil {
		t.Fatalf("clearing a mode word broke the payload, so the offset is wrong: %v", err)
	}
	if got := state.Motors[22].Mode; got != 0 {
		t.Errorf("slot 22 mode = %d after being made limp, want 0", got)
	}
	if got := state.Motors[22].Position; got == 0 {
		t.Error("clearing the mode word moved the position, so the two offsets overlap")
	}
}

// --- a robot on the other end of the agent's raw topic stream ---------------

// fakeRawTopicAgent serves ListRawTopics and StreamRawTopic from a script, so the
// CLI half — which is now where the decoding, the throttle and the silence rule
// live — can be driven without a robot, a device or a DDS domain.
type fakeRawTopicAgent struct {
	agentpbv2.UnimplementedROS2ServiceServer

	// listed is the graph the device can see, and listErr is it failing to look.
	listed  []*agentpbv2.RawTopic
	listErr error

	// rounds is what each successive subscription delivers. Once the script runs
	// out the stream is simply held open, so a renewal loop cannot spin.
	rounds [][][]byte
	// endWith is how a scripted subscription finishes. nil ends it cleanly, which
	// is what the agent's own duration bound does.
	endWith error
	// hold keeps a scripted subscription open after its payloads instead of
	// ending it, which is a robot that has gone quiet.
	hold bool
	// firstSampleAfter delays a subscription's first payload, which is what a
	// cold DDS participant on the device looks like from here.
	firstSampleAfter time.Duration

	mu             sync.Mutex
	listRequests   []*agentpbv2.ListRawTopicsRequest
	streamRequests []*agentpbv2.StreamRawTopicRequest
}

func (f *fakeRawTopicAgent) ListRawTopics(_ context.Context, req *agentpbv2.ListRawTopicsRequest) (*agentpbv2.ListRawTopicsResponse, error) {
	f.mu.Lock()
	f.listRequests = append(f.listRequests, req)
	f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	return &agentpbv2.ListRawTopicsResponse{Topics: f.listed}, nil
}

func (f *fakeRawTopicAgent) StreamRawTopic(req *agentpbv2.StreamRawTopicRequest, stream agentpbv2.ROS2Service_StreamRawTopicServer) error {
	f.mu.Lock()
	round := len(f.streamRequests)
	f.streamRequests = append(f.streamRequests, req)
	f.mu.Unlock()

	if round < len(f.rounds) {
		if f.firstSampleAfter > 0 {
			select {
			case <-time.After(f.firstSampleAfter):
			case <-stream.Context().Done():
				return nil
			}
		}
		for _, payload := range f.rounds[round] {
			if err := stream.Send(&agentpbv2.RawTopicSample{
				Payload:           payload,
				ReceivedUnixNanos: time.Now().UnixNano(),
				Topic:             req.GetTopic(),
				Type:              rosmsg.TypeHGLowState,
			}); err != nil {
				return err
			}
		}
		if f.endWith != nil {
			return f.endWith
		}
		if !f.hold {
			return nil
		}
	}
	<-stream.Context().Done()
	return nil
}

func (f *fakeRawTopicAgent) streams() []*agentpbv2.StreamRawTopicRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*agentpbv2.StreamRawTopicRequest(nil), f.streamRequests...)
}

func (f *fakeRawTopicAgent) listings() []*agentpbv2.ListRawTopicsRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*agentpbv2.ListRawTopicsRequest(nil), f.listRequests...)
}

// lowStateGraph is a robot that publishes the body topic, plus a couple of others
// so a refusal has something to name.
func lowStateGraph(topic string) []*agentpbv2.RawTopic {
	return []*agentpbv2.RawTopic{
		{Name: topic, Type: rosmsg.TypeHGLowState, WriterCount: 1},
		{Name: "/lf/lowstate", Type: rosmsg.TypeHGLowState, WriterCount: 1},
		{Name: "/rt/imu", Type: "sensor_msgs::msg::dds_::Imu_", WriterCount: 1},
	}
}

func dialFakeRawTopicAgent(t *testing.T, agent *fakeRawTopicAgent) *grpc.ClientConn {
	t.Helper()
	lis := bufconn.Listen(1024 * 1024)
	srv := grpc.NewServer()
	agentpbv2.RegisterROS2ServiceServer(srv, agent)
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

func openG1JointSource(t *testing.T, agent *fakeRawTopicAgent, params map[string]string) (*unitreeLowStateJointSource, error) {
	t.Helper()
	source, err := openJointSource(dialFakeRawTopicAgent(t, agent))(t.Context(), robotcal.Joints{
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

// g1Streaming is the common case: a robot publishing /lowstate, held open so a
// read lands on a live stream rather than racing its end.
func g1Streaming(t *testing.T, payloads ...[]byte) *fakeRawTopicAgent {
	t.Helper()
	return &fakeRawTopicAgent{
		listed: lowStateGraph("/lowstate"),
		rounds: [][][]byte{payloads},
		hold:   true,
	}
}

// --- the rules the backend exists to keep ----------------------------------

// TestIdleSlotsNeverArriveAsAZeroAngle is the rule the whole backend turns on: a
// slot the robot does not actuate must be missing from the reading, because a
// zero would read as a joint that was swept and did not move.
//
// The capture makes the point by itself — this robot reports nothing on slots 13
// and 14, the waist roll and pitch — and slot 0 is patched to exactly zero to
// show that the rule is about what a slot reports, not about its angle.
func TestIdleSlotsNeverArriveAsAZeroAngle(t *testing.T) {
	payload := realG1Payload(t)
	driveSlot(payload, 0, 0)

	source, err := openG1JointSource(t, g1Streaming(t, payload), nil)
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
	if got, want := reading.Positions["right_shoulder_pitch_joint"], 0.07829294; math.Abs(got-want) > 1e-6 {
		t.Errorf("right_shoulder_pitch_joint = %v, want %v (slot 22 of the capture)", got, want)
	}
	if got, present := reading.Positions["left_hip_pitch_joint"]; !present || got != 0 {
		t.Errorf("slot 0 = (%v, present=%v); it is driven and at exactly zero radians, "+
			"so it must still be reported", got, present)
	}
}

// TestWhetherAJointCanBeMovedByHand covers the three states measured on
// unitree-g1-nx-2, and the translation from this robot's vocabulary into the
// wizard's.
//
// The capture is the robot as it actually was when an operator spent five
// minutes sweeping it: every fitted motor at mode 1, 34–52 °C, 52 V, holding
// position against their hands. Clearing a mode word is the only difference
// between that and an arm they could move.
func TestWhetherAJointCanBeMovedByHand(t *testing.T) {
	// The joints of the right arm, which is the limb that sweep was run on.
	rightArm := []int{22, 23, 24, 25, 26, 27, 28}

	tests := []struct {
		name string
		// patch is what is done to the robot's own bytes before decoding.
		patch func(payload []byte)
		// want is the condition each named joint must be reported in.
		want map[string]robotwizard.Mobility
		// absent are joints that must carry no condition at all, because the
		// robot is not reporting them.
		absent []string
		// reason, when set, must appear in the joint's hold reason.
		reason string
	}{
		{
			name:   "the robot as it was swept: every arm motor energised",
			want:   map[string]robotwizard.Mobility{"right_shoulder_pitch_joint": robotwizard.MobilityHeld, "right_elbow_joint": robotwizard.MobilityHeld},
			reason: "still energised (motor mode 1)",
		},
		{
			name: "the same arm at zero torque",
			patch: func(payload []byte) {
				for _, index := range rightArm {
					limpSlot(payload, index)
				}
			},
			want: map[string]robotwizard.Mobility{"right_shoulder_pitch_joint": robotwizard.MobilityFree, "right_elbow_joint": robotwizard.MobilityFree},
		},
		{
			name: "one motor left energised on an otherwise limp arm",
			patch: func(payload []byte) {
				for _, index := range rightArm {
					if index != 25 {
						limpSlot(payload, index)
					}
				}
			},
			want: map[string]robotwizard.Mobility{
				"right_shoulder_pitch_joint": robotwizard.MobilityFree,
				"right_elbow_joint":          robotwizard.MobilityHeld,
			},
			reason: "still energised (motor mode 1)",
		},
		{
			// Mode 0 and nothing else: the waist roll and pitch slots are
			// reserved on this unit and report no voltage and no temperature.
			// They are absent from the reading entirely rather than free, so
			// the sweep's "this robot does not have that joint" refusal is the
			// one that fires, not "it is energised".
			name:   "slots that are not fitted say nothing at all",
			absent: []string{"waist_roll_joint", "waist_pitch_joint"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := realG1Payload(t)
			if tt.patch != nil {
				tt.patch(payload)
			}
			source, err := openG1JointSource(t, g1Streaming(t, payload), nil)
			if err != nil {
				t.Fatalf("opening: %v", err)
			}
			reading, err := source.Read(t.Context())
			if err != nil {
				t.Fatalf("Read: %v", err)
			}

			for joint, want := range tt.want {
				status, ok := reading.Status[joint]
				if !ok {
					t.Fatalf("%s carries no condition at all", joint)
				}
				if status.Mobility != want {
					t.Errorf("%s = %v, want %v", joint, status.Mobility, want)
				}
				if want == robotwizard.MobilityHeld {
					if !strings.Contains(status.HoldReason, tt.reason) {
						t.Errorf("%s hold reason = %q, want it to contain %q", joint, status.HoldReason, tt.reason)
					}
					if status.HoldRemedy == "" {
						t.Errorf("%s is refused with no way out; the operator needs to be told "+
							"what to do about it", joint)
					}
				} else if status.HoldReason != "" {
					t.Errorf("%s is free and carries the hold reason %q", joint, status.HoldReason)
				}
			}
			for _, joint := range tt.absent {
				if status, ok := reading.Status[joint]; ok {
					t.Errorf("%s is not fitted on this robot and arrived as %v: an absent joint "+
						"and a driven one are different failures with different answers", joint, status.Mobility)
				}
			}
		})
	}
}

// TestAJointCarriesWhatTheMotorMeasured: mode alone cannot tell "not fitted"
// from "free", so all three numbers travel with the reading. They were dropped
// before, and rebuilding them from a raw wire capture is what turned a
// five-minute sweep into an hour of diagnosis.
func TestAJointCarriesWhatTheMotorMeasured(t *testing.T) {
	source, err := openG1JointSource(t, g1Streaming(t, realG1Payload(t)), nil)
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	reading, err := source.Read(t.Context())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	status, ok := reading.Status["right_shoulder_pitch_joint"]
	if !ok {
		t.Fatal("the joint carries no condition")
	}
	// Slot 22 of the capture: mode 1, both sensors at 49 and 48 °C, 52 V.
	if got := status.Vendor["mode"]; got != "1" {
		t.Errorf("mode = %q, want the robot's own word carried verbatim", got)
	}
	if want := []float64{49, 48}; len(status.TemperaturesC) != 2 ||
		status.TemperaturesC[0] != want[0] || status.TemperaturesC[1] != want[1] {
		t.Errorf("temperatures = %v, want %v — both sensors, because averaging them hides "+
			"whichever runs hotter", status.TemperaturesC, want)
	}
	if status.Volts == nil || *status.Volts != 52 {
		t.Errorf("volts = %v, want 52", status.Volts)
	}
}

// TestTheSourceReportsNoJointOrderOfItsOwn: the wizard compares a source's
// reported order against the profile's, and this source has none — the wire
// message carries indices and no names. Handing the profile's own order back
// would make that check agree with itself and look like evidence.
func TestTheSourceReportsNoJointOrderOfItsOwn(t *testing.T) {
	source, err := openG1JointSource(t, g1Streaming(t, realG1Payload(t)), nil)
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
	payload := realG1Payload(t)
	driveSlot(payload, 31, 0.75)

	source, err := openG1JointSource(t, g1Streaming(t, payload), nil)
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
	agent := &fakeRawTopicAgent{
		listed:  lowStateGraph("/lowstate"),
		rounds:  [][][]byte{{realG1Payload(t)}},
		endWith: status.Error(codes.Unavailable, "the participant closed while reading /lowstate"),
	}
	source, err := openG1JointSource(t, agent, nil)
	if err != nil {
		t.Fatalf("opening: %v", err)
	}

	// The first read may land before or after the server closed the stream, so
	// wait for the end rather than racing it.
	err = readUntilError(t, source, 5*time.Second)
	if err == nil {
		t.Fatal("Read kept returning a pose after the stream ended: a frozen reading is measured " +
			"as a joint that did not travel")
	}
	if !strings.Contains(err.Error(), "/lowstate") {
		t.Errorf("error = %v, want it to name the topic that stopped", err)
	}
}

// TestARobotThatGoesQuietEndsTheStream is the same rule in its other shape. The
// old device-side RPC watched for silence on the robot's behalf; the raw topic
// stream does not, so this side does, and a subscription that stops carrying
// samples has to end rather than leave the last pose standing.
func TestARobotThatGoesQuietEndsTheStream(t *testing.T) {
	restore := unitreeSilenceWindow
	unitreeSilenceWindow = 50 * time.Millisecond
	t.Cleanup(func() { unitreeSilenceWindow = restore })

	source, err := openG1JointSource(t, g1Streaming(t, realG1Payload(t)), nil)
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	err = readUntilError(t, source, 5*time.Second)
	if err == nil {
		t.Fatal("the robot stopped publishing and Read kept answering with the pose it was holding")
	}
	if !strings.Contains(err.Error(), "stopped publishing") {
		t.Errorf("error = %v, want it to say the robot went quiet", err)
	}
}

// TestTheWatchdogDoesNotFireBeforeTheFirstSample: silence is a robot that was
// talking and stopped. A subscription that has not delivered anything yet is the
// device's discovery still running, which the agent bounds itself — calling that
// a robot that stopped publishing would refuse a perfectly live one.
func TestTheWatchdogDoesNotFireBeforeTheFirstSample(t *testing.T) {
	restore := unitreeSilenceWindow
	unitreeSilenceWindow = 50 * time.Millisecond
	t.Cleanup(func() { unitreeSilenceWindow = restore })

	agent := &fakeRawTopicAgent{
		listed:           lowStateGraph("/lowstate"),
		rounds:           [][][]byte{{realG1Payload(t)}},
		hold:             true,
		firstSampleAfter: 250 * time.Millisecond,
	}
	source, err := openG1JointSource(t, agent, nil)
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	if _, err := source.Read(t.Context()); err != nil {
		t.Fatalf("Read: %v — the first sample was slow to arrive, which is discovery on the "+
			"device rather than a robot that stopped", err)
	}
}

// TestTheSubscriptionIsRenewedWhenTheAgentEndsIt: StreamRawTopic bounds every
// subscription — two minutes is the agent's ceiling — and a sweep of a humanoid's
// joints runs far longer. A stream the agent closed cleanly is therefore not the
// robot stopping, and must not be reported as one.
func TestTheSubscriptionIsRenewedWhenTheAgentEndsIt(t *testing.T) {
	first := realG1Payload(t)
	driveSlot(first, 0, 0.1)
	second := realG1Payload(t)
	driveSlot(second, 0, 0.2)

	agent := &fakeRawTopicAgent{
		listed: lowStateGraph("/lowstate"),
		rounds: [][][]byte{{first}, {second}},
	}
	source, err := openG1JointSource(t, agent, nil)
	if err != nil {
		t.Fatalf("opening: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		reading, err := source.Read(t.Context())
		if err != nil {
			t.Fatalf("a subscription the agent ended cleanly was reported as a failure: %v", err)
		}
		if math.Abs(reading.Positions["left_hip_pitch_joint"]-0.2) < 1e-6 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the second subscription's sample never arrived; %d subscriptions were made",
				len(agent.streams()))
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := len(agent.streams()); got < 2 {
		t.Fatalf("%d subscriptions; the agent's duration bound has to be renewed, not taken as "+
			"the robot stopping", got)
	}
}

// TestAnUndecodablePayloadIsReportedNotSmoothedOver: bytes arrived and are not
// this layout. That is neither an absence nor a transport failure — the topic is
// there and we could not read it — and it must never be decoded into plausible
// wrong angles.
func TestAnUndecodablePayloadIsReportedNotSmoothedOver(t *testing.T) {
	quadruped := append([]byte{0x00, 0x01, 0x00, 0x00}, make([]byte, 64)...)
	source, err := openG1JointSource(t, g1Streaming(t, quadruped), nil)
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	err = readUntilError(t, source, 5*time.Second)
	if err == nil {
		t.Fatal("a payload of another layout produced a reading")
	}
	if !strings.Contains(err.Error(), "decoding") {
		t.Errorf("error = %v, want it to say the bytes would not decode rather than that the "+
			"robot is silent", err)
	}
}

// TestASilentRobotIsToldApartFromAnUnreachableOne.
//
// The device-side RPC this replaced marked "we listened and heard nothing" with a
// machine-readable reason, because a cloud tunnel that has lost the device
// answers NOT_FOUND too. StreamRawTopic answers a robot that publishes nothing
// with a bare NOT_FOUND and no reason, so the distinction is re-established from
// the other side: a listing that succeeds is proof the device was reachable and
// could hear, and a topic missing from it is the robot's own silence.
func TestASilentRobotIsToldApartFromAnUnreachableOne(t *testing.T) {
	t.Run("the device heard the graph and the robot was not in it", func(t *testing.T) {
		silent := &fakeRawTopicAgent{listed: []*agentpbv2.RawTopic{
			{Name: "/camera/color/image_raw", Type: "sensor_msgs::msg::dds_::Image_", WriterCount: 1},
		}}
		_, err := openG1JointSource(t, silent, nil)
		if err == nil {
			t.Fatal("want a refusal from a robot that published nothing")
		}
		if !strings.Contains(err.Error(), "nothing published /lowstate") {
			t.Errorf("error = %v, want it to say the device listened and heard nothing", err)
		}
		if !strings.Contains(err.Error(), "/camera/color/image_raw") {
			t.Errorf("error = %v, want it to name what the device did hear — that is what "+
				"separates a quiet robot from listening on the wrong wire", err)
		}
		if len(silent.streams()) != 0 {
			t.Error("the robot was subscribed to before we knew it published anything")
		}
	})

	t.Run("a device that could not be reached is not called a quiet robot", func(t *testing.T) {
		unreachable := &fakeRawTopicAgent{listErr: status.Error(codes.NotFound, "no device named 501 found")}
		_, err := openG1JointSource(t, unreachable, nil)
		if err == nil {
			t.Fatal("want a refusal")
		}
		if strings.Contains(err.Error(), "nothing published") {
			t.Errorf("error = %v: a NOT_FOUND from the call that never reached the device is not "+
				"evidence that the robot is quiet", err)
		}
	})

	t.Run("a device that could not join the domain is not called a quiet robot", func(t *testing.T) {
		deaf := &fakeRawTopicAgent{listErr: status.Error(codes.Unavailable,
			"joining DDS domain 0: no eligible interface")}
		_, err := openG1JointSource(t, deaf, nil)
		if err == nil {
			t.Fatal("want a refusal")
		}
		if strings.Contains(err.Error(), "nothing published") {
			t.Errorf("error = %v: nothing was heard because nothing listened, which sends an "+
				"operator to a different machine", err)
		}
		if !strings.Contains(err.Error(), "joining DDS domain") {
			t.Errorf("error = %v, want the agent's own account of why it could not listen", err)
		}
	})

	t.Run("a topic carrying another type is refused rather than decoded", func(t *testing.T) {
		quadruped := &fakeRawTopicAgent{listed: []*agentpbv2.RawTopic{
			{Name: "/lowstate", Type: "unitree_go::msg::dds_::LowState_", WriterCount: 1},
		}}
		_, err := openG1JointSource(t, quadruped, nil)
		if err == nil {
			t.Fatal("want a refusal: unitree_go is a different layout with a different motor count")
		}
		if !strings.Contains(err.Error(), "unitree_go") {
			t.Errorf("error = %v, want it to name the type the robot actually publishes", err)
		}
	})
}

// TestAnOlderAgentIsNamedAsSuch: raw topic sampling is new, so a device that has
// not been updated answers UNIMPLEMENTED, and the operator is told what to do
// about it rather than shown a gRPC envelope.
func TestAnOlderAgentIsNamedAsSuch(t *testing.T) {
	old := &fakeRawTopicAgent{listErr: status.Error(codes.Unimplemented,
		"unknown method ListRawTopics")}
	_, err := openG1JointSource(t, old, nil)
	if err == nil {
		t.Fatal("want a refusal")
	}
	if !strings.Contains(err.Error(), "does not serve raw topic samples") ||
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

	t.Run("the throttle is bounded", func(t *testing.T) {
		params, err := parseUnitreeLowStateParams(map[string]string{"min_interval_ms": "60000"})
		if err != nil {
			t.Fatal(err)
		}
		if params.MinInterval != unitreeMaxMinInterval {
			t.Errorf("MinInterval = %v, want it held to %v: a sweep that samples slower than "+
				"that is not measuring a moving joint", params.MinInterval, unitreeMaxMinInterval)
		}
		params, err = parseUnitreeLowStateParams(nil)
		if err != nil {
			t.Fatal(err)
		}
		if params.MinInterval != unitreeDefaultMinInterval {
			t.Errorf("MinInterval = %v, want the default %v", params.MinInterval, unitreeDefaultMinInterval)
		}
	})

	t.Run("parameters reach the agent", func(t *testing.T) {
		agent := &fakeRawTopicAgent{
			listed: lowStateGraph("/lf/lowstate"),
			rounds: [][][]byte{{realG1Payload(t)}},
			hold:   true,
		}
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

		listings := agent.listings()
		if len(listings) != 1 {
			t.Fatalf("the agent saw %d listings, want 1", len(listings))
		}
		if listings[0].GetDomainId() != 7 || listings[0].GetInterface() != "enP8p1s0" {
			t.Errorf("listing = %+v, want the profile's domain and interface", listings[0])
		}

		streams := agent.streams()
		if len(streams) != 1 {
			t.Fatalf("the agent saw %d subscriptions, want 1", len(streams))
		}
		req := streams[0]
		if req.GetTopic() != "/lf/lowstate" || req.GetDomainId() != 7 ||
			req.GetInterface() != "enP8p1s0" || req.GetType() != rosmsg.TypeHGLowState {
			t.Errorf("request = %+v, want the profile's parameters carried through, and the type "+
				"pinned so another layout on the same topic cannot be read as this one", req)
		}
		if req.GetDurationMs() == 0 {
			t.Error("the subscription asked for the agent's default window, which is seconds — a " +
				"sweep needs the ceiling and a renewal, not a five-second read")
		}
		if !strings.Contains(source.Describe(), "/lf/lowstate") {
			t.Errorf("Describe() = %q, want it to name the topic being read", source.Describe())
		}
	})

	t.Run("either spelling of the topic is accepted", func(t *testing.T) {
		agent := g1Streaming(t, realG1Payload(t))
		source, err := openG1JointSource(t, agent, map[string]string{"topic": "rt/lowstate"})
		if err != nil {
			t.Fatalf("opening: %v", err)
		}
		if _, err := source.Read(t.Context()); err != nil {
			t.Fatalf("Read: %v", err)
		}
		if got := agent.streams()[0].GetTopic(); got != "/lowstate" {
			t.Errorf("subscribed to %q; ROS 2 mangles a topic on the DDS wire and a profile "+
				"should never have to know that", got)
		}
	})

	t.Run("a profile with no joint order is refused", func(t *testing.T) {
		agent := &fakeRawTopicAgent{listed: lowStateGraph("/lowstate")}
		_, err := openJointSource(dialFakeRawTopicAgent(t, agent))(t.Context(), robotcal.Joints{
			Unit:   "rad",
			Source: robotcal.JointSourceSpec{Backend: backendUnitreeLowState},
		})
		if err == nil || !strings.Contains(err.Error(), "joints.order") {
			t.Fatalf("err = %v, want a refusal naming the missing joint order: the message carries "+
				"no joint names, so there would be nothing to call what it reads", err)
		}
		if len(agent.listings()) != 0 || len(agent.streams()) != 0 {
			t.Error("the robot was asked for joints before we could name them")
		}
	})

	t.Run("a profile whose unit is not the wire's is refused", func(t *testing.T) {
		agent := &fakeRawTopicAgent{listed: lowStateGraph("/lowstate")}
		_, err := openJointSource(dialFakeRawTopicAgent(t, agent))(t.Context(), robotcal.Joints{
			Unit:   "deg",
			Order:  g1Order,
			Source: robotcal.JointSourceSpec{Backend: backendUnitreeLowState},
		})
		if err == nil || !strings.Contains(err.Error(), "rad") {
			t.Fatalf("err = %v, want a refusal: unitree_hg publishes radians, and nothing here "+
				"converts", err)
		}
		if len(agent.streams()) != 0 {
			t.Error("the robot was read before the units were reconciled")
		}
	})
}

// TestSurplusSamplesAreDropped: /lowstate publishes far faster than a hand moves.
// The throttle is on this side because the raw topic stream has no rate control
// of its own, so it saves the CDR walk and nothing else — see the PR body for
// what the bytes cost.
func TestSurplusSamplesAreDropped(t *testing.T) {
	first := realG1Payload(t)
	driveSlot(first, 0, 0.1)
	surplus := realG1Payload(t)
	driveSlot(surplus, 0, 0.2)

	agent := &fakeRawTopicAgent{
		listed: lowStateGraph("/lowstate"),
		rounds: [][][]byte{{first, surplus}},
		hold:   true,
	}
	source, err := openG1JointSource(t, agent, map[string]string{"min_interval_ms": "1000"})
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	reading, err := source.Read(t.Context())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got := reading.Positions["left_hip_pitch_joint"]; math.Abs(got-0.1) > 1e-6 {
		t.Errorf("left_hip_pitch_joint = %v, want the first sample's 0.1: the second arrived "+
			"inside the minimum interval and should not have been decoded", got)
	}
}

// readUntilError reads until the source reports how the stream ended, which is
// what a sweep does: it polls for the newest pose while an operator moves a
// joint.
func readUntilError(t *testing.T, source *unitreeLowStateJointSource, within time.Duration) error {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		_, err := source.Read(t.Context())
		if err != nil {
			return err
		}
		if time.Now().After(deadline) {
			return nil
		}
		time.Sleep(5 * time.Millisecond)
	}
}
