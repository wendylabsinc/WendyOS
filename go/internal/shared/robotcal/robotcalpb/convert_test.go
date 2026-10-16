package robotcalpb

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/robotcal"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// A nil residual must survive as nil. Everything downstream turns on this: a
// zero residual fits inside every budget, so a skipped step encoded as zero
// would come back QUALIFIED.
func TestAnUnmeasuredResidualStaysAbsent(t *testing.T) {
	rec := robotcal.Record{ID: "joint-range", Budget: robotcal.Quantity{Value: 10, Unit: "tick"}}
	got := RecordFromProto(RecordToProto(rec))
	if got.Residual != nil {
		t.Fatalf("residual = %+v, want nil", got.Residual)
	}
	if got.Verdict() != robotcal.VerdictNotMeasured {
		t.Errorf("verdict = %q, want %q", got.Verdict(), robotcal.VerdictNotMeasured)
	}

	// And a residual that really is zero stays zero, distinguishable from the
	// above.
	rec.Residual = &robotcal.Quantity{Value: 0, Unit: "tick"}
	got = RecordFromProto(RecordToProto(rec))
	if got.Residual == nil || got.Residual.Value != 0 {
		t.Fatalf("residual = %+v, want a present zero", got.Residual)
	}
	if got.Verdict() != robotcal.VerdictQualified {
		t.Errorf("verdict = %q, want %q", got.Verdict(), robotcal.VerdictQualified)
	}
}

// The Go zero time is year 1, whose nanosecond count overflows int64. Without
// an explicit guard an unset timestamp comes back as a plausible-looking date
// centuries away, which is worse than an obviously missing one.
func TestTheZeroTimeSurvivesTheRoundTrip(t *testing.T) {
	rec := RecordFromProto(RecordToProto(robotcal.Record{ID: "x"}))
	if !rec.MeasuredAt.IsZero() {
		t.Errorf("measured_at = %v, want the zero time", rec.MeasuredAt)
	}
	sess := SessionFromProto(SessionToProto(robotcal.Session{ProcedureID: "x"}))
	if !sess.StartedAt.IsZero() || !sess.UpdatedAt.IsZero() {
		t.Errorf("times = %v/%v, want both zero", sess.StartedAt, sess.UpdatedAt)
	}
	unit := UnitFromProto(UnitToProto(robotcal.UnitRecord{Unit: "default"}))
	if !unit.UpdatedAt.IsZero() {
		t.Errorf("updated_at = %v, want the zero time", unit.UpdatedAt)
	}

	// A real time survives to the nanosecond.
	at := time.Unix(1_700_000_000, 987_654_321).UTC()
	rec = RecordFromProto(RecordToProto(robotcal.Record{ID: "x", MeasuredAt: at}))
	if !rec.MeasuredAt.Equal(at) {
		t.Errorf("measured_at = %v, want %v", rec.MeasuredAt, at)
	}
}

// The conversion does not interpret what a method measured: it carries the
// payload through as bytes, whatever they are, and it carries a unit string
// nobody enumerated.
//
// Note what this does NOT claim. The record file the store writes is JSON, so
// the store refuses a payload that is not a JSON value (the service says so in
// InvalidArgument). That constraint belongs to the store, not to this
// conversion, which is why it is tested against the stores and not here — the
// transport must not be the thing that narrows what a method may produce.
func TestTheConversionDoesNotInterpretThePayload(t *testing.T) {
	// Not valid JSON, and not valid UTF-8: the conversion still must not care.
	payload := json.RawMessage([]byte{0x00, 0xff, 0x7b, 0x27, 0xc3, 0x28})
	rec := robotcal.Record{
		ID:       "so101-joint-homing",
		Residual: &robotcal.Quantity{Value: 3, Unit: "encoder-tick-of-some-vendor"},
		Budget:   robotcal.Quantity{Value: 10, Unit: "encoder-tick-of-some-vendor"},
		Payload:  payload,
	}
	got := RecordFromProto(RecordToProto(rec))
	if string(got.Payload) != string(payload) {
		t.Errorf("payload = %v, want it byte-for-byte: %v", []byte(got.Payload), []byte(payload))
	}
	if got.Residual.Unit != "encoder-tick-of-some-vendor" {
		t.Errorf("unit = %q, want the free string it was given", got.Residual.Unit)
	}
	if got.Verdict() != robotcal.VerdictQualified {
		t.Errorf("verdict = %q: a unit nobody enumerated must still compare against its own budget", got.Verdict())
	}
}

// Two quantities are comparable only when unit, axis and frame all match —
// comparing a travel shortfall in ticks against a budget in radians would
// produce a perfectly valid number and a meaningless answer. The qualifiers
// have to make it across the wire for that refusal to still happen.
func TestAxisAndFrameSurviveSoIncomparableStaysIncomparable(t *testing.T) {
	rec := robotcal.Record{
		ID:       "head-rgbd-extrinsics",
		Residual: &robotcal.Quantity{Value: 0.001, Unit: "m", Frame: "torso_link"},
		Budget:   robotcal.Quantity{Value: 0.010, Unit: "m", Frame: "pelvis"},
	}
	got := RecordFromProto(RecordToProto(rec))
	if got.Residual.Frame != "torso_link" || got.Budget.Frame != "pelvis" {
		t.Fatalf("frames = %q/%q, want them preserved", got.Residual.Frame, got.Budget.Frame)
	}
	if got.Verdict() != robotcal.VerdictIncomparable {
		t.Errorf("verdict = %q, want %q", got.Verdict(), robotcal.VerdictIncomparable)
	}
}

// Empty and nil are one state on both sides (omitempty in the JSON store, an
// unset field in protobuf), so a round trip must not invent a difference.
func TestEmptyCollectionsRoundTripAsNil(t *testing.T) {
	sess := robotcal.Session{ProcedureID: "x", Steps: map[string]json.RawMessage{}, Skipped: []string{}}
	got := SessionFromProto(SessionToProto(sess))
	if got.Steps != nil || got.Skipped != nil {
		t.Errorf("steps/skipped = %v/%v, want both nil", got.Steps, got.Skipped)
	}
	unit := UnitFromProto(UnitToProto(robotcal.UnitRecord{
		Unit:         "default",
		Calibrations: map[string]robotcal.Record{},
		Sessions:     map[string]robotcal.Session{},
	}))
	if unit.Calibrations != nil || unit.Sessions != nil {
		t.Errorf("maps = %v/%v, want both nil", unit.Calibrations, unit.Sessions)
	}
}

func TestNilMessagesDecodeToZeroValues(t *testing.T) {
	if got := RecordFromProto(nil); got.ID != "" || got.Residual != nil {
		t.Errorf("RecordFromProto(nil) = %+v, want the zero record", got)
	}
	if got := SessionFromProto(nil); got.ProcedureID != "" {
		t.Errorf("SessionFromProto(nil) = %+v, want the zero session", got)
	}
	if got := UnitFromProto(nil); got.Unit != "" {
		t.Errorf("UnitFromProto(nil) = %+v, want the zero unit record", got)
	}
}

// The limit has to sit below gRPC's 4 MiB default, or the refusal it exists to
// produce never happens and the transport's own opaque error wins.
func TestTheSizeLimitLeavesHeadroomUnderTheTransport(t *testing.T) {
	const grpcDefault = 4 << 20
	if MaxMessageBytes >= grpcDefault {
		t.Fatalf("MaxMessageBytes = %d, which is not below gRPC's default receive limit of %d: "+
			"the refusal would come from the transport, which names neither the unit nor the procedure",
			MaxMessageBytes, grpcDefault)
	}
}

func TestCheckSizeSaysWhatDidNotFit(t *testing.T) {
	small := &agentpbv2.PutRecordRequest{Unit: "default", Record: &agentpbv2.CalibrationRecord{Id: "x"}}
	if err := CheckSize("the calibration record \"x\"", small); err != nil {
		t.Fatalf("a record is tiny and must pass: %v", err)
	}

	big := &agentpbv2.PutSessionRequest{
		Unit: "default",
		Session: &agentpbv2.CalibrationSession{
			ProcedureId: "joint-range",
			Steps:       map[string][]byte{"joint_00": make([]byte, MaxMessageBytes+1)},
		},
	}
	err := CheckSize("the calibration session for procedure \"joint-range\"", big)
	if err == nil {
		t.Fatal("an oversize session must be refused")
	}
	var tooLarge *ErrTooLarge
	if !errors.As(err, &tooLarge) {
		t.Fatalf("error = %T, want *ErrTooLarge so callers can tell this apart from a device failure", err)
	}
	if tooLarge.What != `the calibration session for procedure "joint-range"` {
		t.Errorf("What = %q, want it to name the procedure", tooLarge.What)
	}
	// The message has to be actionable, not just "too big".
	for _, want := range []string{"joint-range", "MiB", "refused rather than truncated", "summarise per step"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the message must contain %q, got: %v", want, err)
		}
	}
}
