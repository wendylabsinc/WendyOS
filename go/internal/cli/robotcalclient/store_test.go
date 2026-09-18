package robotcalclient_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/wendylabsinc/wendy/go/internal/agent/services"
	"github.com/wendylabsinc/wendy/go/internal/cli/robotcalclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/robotcal"
	"github.com/wendylabsinc/wendy/go/internal/shared/robotcal/robotcalpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// dialAgent starts the agent's RobotService over an in-process listener, backed
// by a FileStore under root, and returns a connection to it.
func dialAgent(t *testing.T, root string) *grpc.ClientConn {
	t.Helper()
	lis := bufconn.Listen(1024 * 1024)
	srv := grpc.NewServer()
	agentpbv2.RegisterWendyRobotServiceServer(srv, services.NewRobotService(zap.NewNop(), root))
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

// implementation is one way of reaching a robot's calibration store. There are
// two, and the whole point of this file is that they are driven by one table:
// the on-device path and the remote path disagreeing about whether a robot is
// calibrated is a failure nobody would see until a robot moved on a
// calibration that was never really stored.
type implementation struct {
	name string
	open func(t *testing.T) robotcal.Store
	// boundedByTransport is the one place the two legitimately differ: a local
	// file has no message-size limit, and a gRPC call does. Stated here rather
	// than hidden in a skip, because "the local path can store what the remote
	// path refuses" is a real property of the system and should be visible.
	boundedByTransport bool
}

func implementations() []implementation {
	return []implementation{
		{
			name: "FileStore (on the device)",
			open: func(t *testing.T) robotcal.Store {
				return robotcal.NewFileStore(t.TempDir())
			},
		},
		{
			name:               "gRPC (from a laptop)",
			boundedByTransport: true,
			open: func(t *testing.T) robotcal.Store {
				conn := dialAgent(t, t.TempDir())
				s, err := robotcalclient.Open(t.Context(), conn, "test-device")
				if err != nil {
					t.Fatalf("opening the store over gRPC: %v", err)
				}
				return s
			},
		},
	}
}

// sameJSON compares two payloads as JSON values rather than as bytes. What the
// store promises about a payload is that it does not interpret it, not that it
// preserves its whitespace.
func sameJSON(t *testing.T, got, want json.RawMessage) bool {
	t.Helper()
	var a, b any
	if err := json.Unmarshal(got, &a); err != nil {
		t.Errorf("the payload that came back is not JSON at all: %v", err)
		return false
	}
	if err := json.Unmarshal(want, &b); err != nil {
		t.Fatalf("this test's fixture is not JSON: %v", err)
	}
	return reflect.DeepEqual(a, b)
}

// eachStore runs one case against both implementations.
func eachStore(t *testing.T, name string, run func(t *testing.T, impl implementation, store robotcal.Store)) {
	t.Helper()
	for _, impl := range implementations() {
		t.Run(name+"/"+impl.name, func(t *testing.T) {
			run(t, impl, impl.open(t))
		})
	}
}

func TestStoresAgreeOnARecordRoundTrip(t *testing.T) {
	eachStore(t, "record round-trip", func(t *testing.T, _ implementation, store robotcal.Store) {
		ctx := t.Context()
		measured := time.Unix(1_700_000_000, 123).UTC()
		// The payload is deliberately not the shape any method produces, and
		// deliberately not something the platform could interpret: it is opaque
		// bytes, and the store's job is to hand back exactly what it was given.
		payload := json.RawMessage(`{"transform":[1,0,0,0.08],"note":"opaque to WendyOS"}`)
		want := robotcal.Record{
			ID:         "head-rgbd-extrinsics",
			Qualified:  true,
			Residual:   &robotcal.Quantity{Value: 0.006, Unit: "m", Frame: "torso_link"},
			Budget:     robotcal.Quantity{Value: 0.010, Unit: "m", Frame: "torso_link"},
			MeasuredAt: measured,
			Method:     "fiducial-extrinsics/1.0",
			Payload:    payload,
		}
		if err := store.PutRecord(ctx, "default", want); err != nil {
			t.Fatalf("PutRecord: %v", err)
		}
		unit, err := store.Load(ctx, "default")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		got, ok := unit.Calibrations[want.ID]
		if !ok {
			t.Fatalf("the record did not come back; got %+v", unit.Calibrations)
		}
		if got.ID != want.ID || got.Qualified != want.Qualified || got.Method != want.Method {
			t.Errorf("record = %+v, want %+v", got, want)
		}
		if got.Residual == nil || *got.Residual != *want.Residual {
			t.Errorf("residual = %+v, want %+v", got.Residual, want.Residual)
		}
		if got.Budget != want.Budget {
			t.Errorf("budget = %+v, want %+v", got.Budget, want.Budget)
		}
		if !got.MeasuredAt.Equal(want.MeasuredAt) {
			t.Errorf("measured_at = %v, want %v", got.MeasuredAt, want.MeasuredAt)
		}
		// The payload comes back as the same JSON value, not the same bytes:
		// the store writes the whole record with MarshalIndent, which re-indents
		// anything embedded in it. That is a property of the record file, and
		// running this table against both implementations is what shows they
		// share it — the remote path does not re-encode it any further.
		if !sameJSON(t, got.Payload, payload) {
			t.Errorf("payload = %s, want the same JSON value as %s", got.Payload, payload)
		}
		if got.Verdict() != robotcal.VerdictQualified {
			t.Errorf("verdict = %q, want %q", got.Verdict(), robotcal.VerdictQualified)
		}
	})
}

// A residual that was never measured must not arrive as a zero. A zero fits
// inside every budget, so a skipped step would come back qualified — the exact
// failure a skip exists to make visible.
func TestStoresKeepAnUnmeasuredResidualAbsent(t *testing.T) {
	eachStore(t, "not measured is not zero", func(t *testing.T, _ implementation, store robotcal.Store) {
		ctx := t.Context()
		rec := robotcal.Record{
			ID:       "joint-range",
			Residual: nil,
			Budget:   robotcal.Quantity{Value: 10, Unit: "tick"},
		}
		if err := store.PutRecord(ctx, "default", rec); err != nil {
			t.Fatalf("PutRecord: %v", err)
		}
		unit, err := store.Load(ctx, "default")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		got := unit.Calibrations["joint-range"]
		if got.Residual != nil {
			t.Fatalf("residual = %+v, want it still absent: a skip that came back as zero would qualify", got.Residual)
		}
		if got.Verdict() != robotcal.VerdictNotMeasured {
			t.Errorf("verdict = %q, want %q", got.Verdict(), robotcal.VerdictNotMeasured)
		}
	})
}

// The resume path depends on this one: a seven-joint sweep interrupted at joint
// four must come back with the four answers it had, and with a skip still
// telling itself apart from a measurement.
func TestStoresAgreeOnASessionRoundTrip(t *testing.T) {
	eachStore(t, "session round-trip", func(t *testing.T, _ implementation, store robotcal.Store) {
		ctx := t.Context()
		sess := robotcal.Session{
			ProcedureID: "joint-range",
			Method:      robotcal.MethodJointRangeSweep,
			ProfileKind: "so101",
			StartedAt:   time.Unix(1_700_000_000, 0).UTC(),
			UpdatedAt:   time.Unix(1_700_000_500, 0).UTC(),
		}
		sess.Record("shoulder_pan", json.RawMessage(`{"min":-2.94,"max":1.62}`))
		sess.Record("shoulder_lift", json.RawMessage(`{"min":-1.1,"max":1.1}`))
		sess.Skip("elbow_flex")

		if err := store.PutSession(ctx, "default", sess); err != nil {
			t.Fatalf("PutSession: %v", err)
		}
		unit, err := store.Load(ctx, "default")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		got, ok := unit.Sessions["joint-range"]
		if !ok {
			t.Fatalf("the checkpoint did not survive; a resume would start the sweep over")
		}
		if got.Method != sess.Method || got.ProfileKind != sess.ProfileKind {
			t.Errorf("session = %+v, want method %q / profile %q", got, sess.Method, sess.ProfileKind)
		}
		if !got.StartedAt.Equal(sess.StartedAt) || !got.UpdatedAt.Equal(sess.UpdatedAt) {
			t.Errorf("times = %v/%v, want %v/%v", got.StartedAt, got.UpdatedAt, sess.StartedAt, sess.UpdatedAt)
		}
		if !sameJSON(t, got.Steps["shoulder_pan"], json.RawMessage(`{"min":-2.94,"max":1.62}`)) {
			t.Errorf("step result = %s, want the same JSON value back", got.Steps["shoulder_pan"])
		}
		if !got.WasSkipped("elbow_flex") {
			t.Error("a skipped joint must still read as skipped after a round trip")
		}
		if got.Done("shoulder_pan") != true || got.Done("wrist_roll") != false {
			t.Error("the resume point moved across the round trip")
		}
		if n := got.Progress([]string{"shoulder_pan", "shoulder_lift", "elbow_flex", "wrist_roll"}); n != 3 {
			t.Errorf("progress = %d, want 3 (two measured, one skipped)", n)
		}

		if err := store.ClearSession(ctx, "default", "joint-range"); err != nil {
			t.Fatalf("ClearSession: %v", err)
		}
		unit, err = store.Load(ctx, "default")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if _, ok := unit.Sessions["joint-range"]; ok {
			t.Error("a finished procedure's checkpoint must be dropped")
		}
	})
}

// Clearing an id that is not there must say so. A typo that reported success
// would leave an operator believing a calibration was invalidated after a
// repair, which is the case `clear` exists for.
func TestStoresRefuseToClearACalibrationThatIsNotThere(t *testing.T) {
	eachStore(t, "clear an absent id", func(t *testing.T, _ implementation, store robotcal.Store) {
		ctx := t.Context()
		if err := store.PutRecord(ctx, "default", robotcal.Record{ID: "joint-range"}); err != nil {
			t.Fatalf("PutRecord: %v", err)
		}
		err := store.ClearRecord(ctx, "default", "never-existed")
		if err == nil {
			t.Fatal("clearing a calibration that does not exist must be an error, not a silent success")
		}
		if !strings.Contains(err.Error(), "never-existed") {
			t.Errorf("the refusal must name what was not found, got: %v", err)
		}
		// The record that does exist is untouched.
		unit, err := store.Load(ctx, "default")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if _, ok := unit.Calibrations["joint-range"]; !ok {
			t.Error("a failed clear must not have removed anything")
		}
	})
}

// Clearing a record takes its half-finished attempt with it: resuming a sweep
// into a calibration that was just invalidated would re-install the numbers the
// repair invalidated.
func TestStoresClearARecordWithItsSession(t *testing.T) {
	eachStore(t, "clear drops the session too", func(t *testing.T, _ implementation, store robotcal.Store) {
		ctx := t.Context()
		if err := store.PutRecord(ctx, "default", robotcal.Record{ID: "dex3-homing"}); err != nil {
			t.Fatalf("PutRecord: %v", err)
		}
		if err := store.PutSession(ctx, "default", robotcal.Session{ProcedureID: "dex3-homing"}); err != nil {
			t.Fatalf("PutSession: %v", err)
		}
		if err := store.ClearRecord(ctx, "default", "dex3-homing"); err != nil {
			t.Fatalf("ClearRecord: %v", err)
		}
		unit, err := store.Load(ctx, "default")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if _, ok := unit.Calibrations["dex3-homing"]; ok {
			t.Error("the record survived clear")
		}
		if _, ok := unit.Sessions["dex3-homing"]; ok {
			t.Error("a half-finished attempt at a cleared calibration must go with it")
		}
	})
}

func TestStoresTreatAnUncalibratedUnitAsNormal(t *testing.T) {
	eachStore(t, "an uncalibrated unit", func(t *testing.T, _ implementation, store robotcal.Store) {
		got, err := store.Load(t.Context(), "leader")
		if err != nil {
			t.Fatalf("a robot nobody has calibrated is the normal state, not an error: %v", err)
		}
		if got.Unit != "leader" {
			t.Errorf("unit = %q, want the name it was asked for", got.Unit)
		}
		if len(got.Calibrations) != 0 || len(got.Sessions) != 0 {
			t.Errorf("expected an empty record, got %+v", got)
		}
	})
}

// An SO-101 rig is two arms on one Jetson with different measured travel on
// every joint. One shared record would be wrong for both.
func TestStoresKeepUnitsApart(t *testing.T) {
	eachStore(t, "per unit, not per device", func(t *testing.T, _ implementation, store robotcal.Store) {
		ctx := t.Context()
		put := func(unit string, v float64) {
			t.Helper()
			err := store.PutRecord(ctx, unit, robotcal.Record{
				ID: "joint-range", Residual: &robotcal.Quantity{Value: v, Unit: "tick"},
				Budget: robotcal.Quantity{Value: 10, Unit: "tick"},
			})
			if err != nil {
				t.Fatalf("PutRecord(%s): %v", unit, err)
			}
		}
		put("leader", 4)
		put("follower", 40)

		leader, err := store.Load(ctx, "leader")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got := leader.Calibrations["joint-range"].Residual.Value; got != 4 {
			t.Fatalf("the leader picked up the follower's calibration: residual = %v", got)
		}
		if v := leader.Calibrations["joint-range"].Verdict(); v != robotcal.VerdictQualified {
			t.Errorf("leader verdict = %q, want %q", v, robotcal.VerdictQualified)
		}
		follower, err := store.Load(ctx, "follower")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if v := follower.Calibrations["joint-range"].Verdict(); v != robotcal.VerdictOverBudget {
			t.Errorf("follower verdict = %q, want %q", v, robotcal.VerdictOverBudget)
		}
	})
}

func TestStoresRecordWhatAUnitIsAndWhatBacksIt(t *testing.T) {
	eachStore(t, "profile kind and stable id", func(t *testing.T, _ implementation, store robotcal.Store) {
		ctx := t.Context()
		const stableID = "by-id:usb-Intel_RealSense_338622073335-video-index0"
		if err := store.SetProfileKind(ctx, "default", "unitree-g1"); err != nil {
			t.Fatalf("SetProfileKind: %v", err)
		}
		if err := store.SetStableID(ctx, "default", stableID); err != nil {
			t.Fatalf("SetStableID: %v", err)
		}
		got, err := store.Load(ctx, "default")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got.ProfileKind != "unitree-g1" {
			t.Errorf("profile kind = %q, want unitree-g1", got.ProfileKind)
		}
		if got.StableID != stableID {
			t.Errorf("stable id = %q, want %q", got.StableID, stableID)
		}
	})
}

// A unit name that could escape the store's directory is refused before it can
// reach a filesystem, on both paths.
func TestStoresRefuseAUnitNameThatCouldEscape(t *testing.T) {
	eachStore(t, "a bad unit name", func(t *testing.T, _ implementation, store robotcal.Store) {
		ctx := t.Context()
		for _, unit := range []string{"../escape", "a/b", "Leader", ""} {
			if _, err := store.Load(ctx, unit); err == nil {
				t.Errorf("Load(%q) should be refused", unit)
			}
			if err := store.PutRecord(ctx, unit, robotcal.Record{ID: "x"}); err == nil {
				t.Errorf("PutRecord(%q) should be refused", unit)
			}
		}
	})
}

// The payload is opaque — WendyOS never reads what a method measured — but the
// record it lands in is a JSON file, so it has to be a JSON value. Both
// implementations refuse bytes that are not, rather than one storing something
// the other could never read back.
func TestStoresRefuseAPayloadTheRecordCouldNotHold(t *testing.T) {
	eachStore(t, "a payload that is not JSON", func(t *testing.T, _ implementation, store robotcal.Store) {
		ctx := t.Context()
		rec := robotcal.Record{ID: "head-rgbd-extrinsics", Payload: json.RawMessage([]byte{0x00, 0xff, 0x28})}
		if err := store.PutRecord(ctx, "default", rec); err == nil {
			t.Fatal("a payload that cannot be written into the record file must be refused, not half-stored")
		}
		unit, err := store.Load(ctx, "default")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if _, ok := unit.Calibrations["head-rgbd-extrinsics"]; ok {
			t.Error("a refused record must leave nothing behind")
		}
	})
}

// The one property the two implementations do not share, stated rather than
// skipped: a session too big for a gRPC message is refused with an error that
// names the limit, while the same session written to a local file is fine.
// Refused, never truncated — a resume built from samples that were silently
// dropped would be worse than no resume at all.
func TestOversizeSessionIsRefusedNotTruncated(t *testing.T) {
	eachStore(t, "an oversize session", func(t *testing.T, impl implementation, store robotcal.Store) {
		ctx := t.Context()
		sess := robotcal.Session{ProcedureID: "joint-range", Method: robotcal.MethodJointRangeSweep}
		// Raw samples where a checkpoint only needs a per-step result — the
		// realistic way a session gets this big.
		blob := make([]byte, 64*1024)
		for i := range blob {
			blob[i] = 'a'
		}
		for i := 0; i < 64; i++ {
			sess.Record(fmt.Sprintf("joint_%02d", i), json.RawMessage(`"`+string(blob)+`"`))
		}

		err := store.PutSession(ctx, "default", sess)
		if !impl.boundedByTransport {
			if err != nil {
				t.Fatalf("a local file has no message limit, so this must succeed: %v", err)
			}
			got, lerr := store.Load(ctx, "default")
			if lerr != nil {
				t.Fatalf("Load: %v", lerr)
			}
			if n := len(got.Sessions["joint-range"].Steps); n != 64 {
				t.Fatalf("steps = %d, want all 64 back", n)
			}
			return
		}

		if err == nil {
			t.Fatal("an oversize session must be refused, not silently sent")
		}
		if !robotcalclient.TooLarge(err) {
			t.Errorf("the refusal must be recognisable as a size refusal, got: %v", err)
		}
		for _, want := range []string{"joint-range", "MiB"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal must name %q so an operator knows what did not fit; got: %v", want, err)
			}
		}
		// And nothing was written: a partial checkpoint is the failure this
		// refusal exists to prevent.
		got, lerr := store.Load(ctx, "default")
		if lerr != nil {
			t.Fatalf("Load: %v", lerr)
		}
		if _, ok := got.Sessions["joint-range"]; ok {
			t.Error("a refused session must leave no partial checkpoint behind")
		}
	})
}

// The client-side size check is a courtesy, not the enforcement. A caller that
// skipped it — an older CLI, or something that is not the CLI at all — must be
// refused by the device too, and the device must not have written anything.
func TestTheDeviceRefusesAnOversizeSessionItself(t *testing.T) {
	conn := dialAgent(t, t.TempDir())
	client := agentpbv2.NewWendyRobotServiceClient(conn)

	steps := map[string][]byte{}
	blob := make([]byte, 128*1024)
	for i := 0; i < 40; i++ {
		steps[fmt.Sprintf("joint_%02d", i)] = blob
	}
	req := &agentpbv2.PutSessionRequest{
		Unit:    "default",
		Session: &agentpbv2.CalibrationSession{ProcedureId: "joint-range", Steps: steps},
	}
	if robotcalpb.CheckSize("probe", req) == nil {
		t.Fatal("this test's fixture is not actually oversize any more")
	}

	_, err := client.PutSession(t.Context(), req)
	if err == nil {
		t.Fatal("the device must refuse an oversize session from a client that skipped its own check")
	}
	// gRPC's own receive limit rejecting it first is also a refusal, and also
	// correct — what must not happen is a partial write.
	if code := status.Code(err); code != codes.ResourceExhausted {
		t.Errorf("code = %v, want %v", code, codes.ResourceExhausted)
	}

	resp, err := client.LoadUnit(t.Context(), &agentpbv2.LoadUnitRequest{Unit: "default"})
	if err != nil {
		t.Fatalf("LoadUnit: %v", err)
	}
	if len(resp.GetUnit().GetSessions()) != 0 {
		t.Error("a refused session must leave nothing on disk")
	}
}

// An agent that predates this service must be named, not guessed at: the wizard
// walks an operator through a physical procedure before it writes anything, and
// discovering at the end that there is nowhere to put the result is the worst
// possible moment.
func TestOpeningAgainstAnAgentWithoutTheServiceSaysSo(t *testing.T) {
	lis := bufconn.Listen(1024 * 1024)
	srv := grpc.NewServer() // no RobotService registered
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() { srv.Stop(); lis.Close() })

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	defer conn.Close()

	_, err = robotcalclient.Open(t.Context(), conn, "old-device")
	if err == nil {
		t.Fatal("opening a store an agent does not serve must fail at open, not at the first write")
	}
	for _, want := range []string{"RobotService", robotcal.DefaultRoot, "wendy device update"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must mention %q, got: %v", want, err)
		}
	}
}

// Describe is what error messages use to send an operator to the right file, so
// the remote case has to say which machine as well as which path — and the path
// has to be the one the device reported, not one this process assumed.
func TestDescribeNamesThePathAndTheMachine(t *testing.T) {
	root := t.TempDir()
	conn := dialAgent(t, root)
	store, err := robotcalclient.Open(t.Context(), conn, "unitree-g1-nx-2")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got := store.Describe()
	if !strings.Contains(got, root) || !strings.Contains(got, "unitree-g1-nx-2") {
		t.Errorf("Describe() = %q, want it to name both %q and the device", got, root)
	}
}
