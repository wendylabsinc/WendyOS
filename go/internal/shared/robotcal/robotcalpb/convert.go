// Package robotcalpb converts robotcal's records to and from the wire form
// WendyRobotService carries.
//
// It exists as its own package for the reason streamreason does: it is the only
// place that both builds and parses this encoding, so the agent serving the
// store and the CLI reaching it cannot disagree about what a record means. A
// second spelling of the record on one side of the wire is a silent
// disagreement about whether a robot is calibrated.
//
// It is separate from robotcal itself so that robotcal stays free of the
// generated protobuf types: the store's semantics are the device's, not the
// transport's, and the on-device path opens a FileStore with no gRPC anywhere
// in reach.
package robotcalpb

import (
	"encoding/json"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/wendylabsinc/wendy/go/internal/shared/robotcal"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// MaxMessageBytes is the largest robot-store message this transport will send
// or accept.
//
// gRPC's default receive limit is 4 MiB, and a message over it is rejected by
// the transport with an error that names neither the unit nor the procedure.
// This limit sits below that on purpose, so the refusal comes from here — where
// there is enough context to say which session was too big and what to do about
// it — rather than from the transport. Records are tiny; a session carrying raw
// samples instead of a per-step summary is the case that reaches this.
//
// Raising it would need both peers' gRPC limits raised in step, which is why it
// is a constant here rather than a knob: one side raising it alone converts a
// clear refusal into an opaque transport error.
const MaxMessageBytes = 3 << 20 // 3 MiB

// ErrTooLarge is returned when a message will not fit. It is deliberately a
// refusal and never a truncation: half a calibration session would resume a
// procedure from samples that were silently dropped, and the operator would
// have no way to tell.
type ErrTooLarge struct {
	// What names the thing that did not fit, e.g. `session "joint-range"`.
	What string
	Size int
}

func (e *ErrTooLarge) Error() string {
	return fmt.Sprintf("%s is %.1f MiB, over the %.0f MiB this transport carries. "+
		"It is refused rather than truncated: a calibration missing samples nobody was told about is "+
		"worse than one that failed to save. A session this large is recording raw samples where a "+
		"checkpoint only needs each step's result — summarise per step, or run the procedure from the "+
		"device itself where the store is a local file",
		e.What, float64(e.Size)/(1<<20), float64(MaxMessageBytes)/(1<<20))
}

// CheckSize refuses a message that would not survive the transport.
func CheckSize(what string, m proto.Message) error {
	if n := proto.Size(m); n > MaxMessageBytes {
		return &ErrTooLarge{What: what, Size: n}
	}
	return nil
}

// nanos encodes a time as Unix nanoseconds, mapping the Go zero time to 0.
//
// The guard is not cosmetic: time.Time{} is year 1, whose nanosecond count
// overflows int64 into a large negative number, so an unset timestamp would
// come back as a real-looking date in the far future or past.
func nanos(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

// unnanos is the inverse: 0 is the unset marker, not the epoch.
func unnanos(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n).UTC()
}

// bytesOrNil normalises an empty slice to nil, which is how both the JSON store
// (omitempty) and protobuf (an unset field) spell "nothing here". Without it an
// empty-but-non-nil payload would come back nil and a round-trip comparison
// would report a difference that does not exist.
func bytesOrNil(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return b
}

// QuantityToProto encodes a quantity. A nil quantity stays nil: a residual that
// was never measured must not arrive as a zero, which would qualify.
func QuantityToProto(q *robotcal.Quantity) *agentpbv2.Quantity {
	if q == nil {
		return nil
	}
	return &agentpbv2.Quantity{Value: q.Value, Unit: q.Unit, Axis: q.Axis, Frame: q.Frame}
}

// QuantityFromProto decodes a quantity, preserving nil.
func QuantityFromProto(q *agentpbv2.Quantity) *robotcal.Quantity {
	if q == nil {
		return nil
	}
	return &robotcal.Quantity{Value: q.GetValue(), Unit: q.GetUnit(), Axis: q.GetAxis(), Frame: q.GetFrame()}
}

// RecordToProto encodes one calibration result.
func RecordToProto(r robotcal.Record) *agentpbv2.CalibrationRecord {
	return &agentpbv2.CalibrationRecord{
		Id:                  r.ID,
		Qualified:           r.Qualified,
		Residual:            QuantityToProto(r.Residual),
		Budget:              QuantityToProto(&r.Budget),
		MeasuredAtUnixNanos: nanos(r.MeasuredAt),
		Method:              r.Method,
		Payload:             bytesOrNil(r.Payload),
	}
}

// RecordFromProto decodes one calibration result.
//
// Qualified is carried rather than re-derived, because robotcal.Record.Verdict
// re-judges every stored record against the same gate anyway — the flag is
// data, not the decision.
func RecordFromProto(r *agentpbv2.CalibrationRecord) robotcal.Record {
	if r == nil {
		return robotcal.Record{}
	}
	out := robotcal.Record{
		ID:         r.GetId(),
		Qualified:  r.GetQualified(),
		Residual:   QuantityFromProto(r.GetResidual()),
		MeasuredAt: unnanos(r.GetMeasuredAtUnixNanos()),
		Method:     r.GetMethod(),
	}
	if b := bytesOrNil(r.GetPayload()); b != nil {
		out.Payload = json.RawMessage(b)
	}
	if q := QuantityFromProto(r.GetBudget()); q != nil {
		out.Budget = *q
	}
	return out
}

// SessionToProto encodes an interrupted procedure's checkpoint.
func SessionToProto(s robotcal.Session) *agentpbv2.CalibrationSession {
	out := &agentpbv2.CalibrationSession{
		ProcedureId:        s.ProcedureID,
		Method:             string(s.Method),
		ProfileKind:        s.ProfileKind,
		StartedAtUnixNanos: nanos(s.StartedAt),
		UpdatedAtUnixNanos: nanos(s.UpdatedAt),
		Skipped:            s.Skipped,
	}
	if len(s.Steps) > 0 {
		out.Steps = make(map[string][]byte, len(s.Steps))
		for k, v := range s.Steps {
			out.Steps[k] = v
		}
	}
	return out
}

// SessionFromProto decodes a checkpoint.
func SessionFromProto(s *agentpbv2.CalibrationSession) robotcal.Session {
	if s == nil {
		return robotcal.Session{}
	}
	out := robotcal.Session{
		ProcedureID: s.GetProcedureId(),
		Method:      robotcal.MethodID(s.GetMethod()),
		ProfileKind: s.GetProfileKind(),
		StartedAt:   unnanos(s.GetStartedAtUnixNanos()),
		UpdatedAt:   unnanos(s.GetUpdatedAtUnixNanos()),
	}
	if len(s.GetSkipped()) > 0 {
		out.Skipped = s.GetSkipped()
	}
	if len(s.GetSteps()) > 0 {
		out.Steps = make(map[string]json.RawMessage, len(s.GetSteps()))
		for k, v := range s.GetSteps() {
			out.Steps[k] = json.RawMessage(v)
		}
	}
	return out
}

// UnitToProto encodes everything one robot knows about itself.
func UnitToProto(u robotcal.UnitRecord) *agentpbv2.UnitRecord {
	out := &agentpbv2.UnitRecord{
		Unit:               u.Unit,
		ProfileKind:        u.ProfileKind,
		StableId:           u.StableID,
		UpdatedAtUnixNanos: nanos(u.UpdatedAt),
	}
	if len(u.Calibrations) > 0 {
		out.Calibrations = make(map[string]*agentpbv2.CalibrationRecord, len(u.Calibrations))
		for k, v := range u.Calibrations {
			out.Calibrations[k] = RecordToProto(v)
		}
	}
	if len(u.Sessions) > 0 {
		out.Sessions = make(map[string]*agentpbv2.CalibrationSession, len(u.Sessions))
		for k, v := range u.Sessions {
			out.Sessions[k] = SessionToProto(v)
		}
	}
	return out
}

// UnitFromProto decodes a unit record.
func UnitFromProto(u *agentpbv2.UnitRecord) robotcal.UnitRecord {
	if u == nil {
		return robotcal.UnitRecord{}
	}
	out := robotcal.UnitRecord{
		Unit:        u.GetUnit(),
		ProfileKind: u.GetProfileKind(),
		StableID:    u.GetStableId(),
		UpdatedAt:   unnanos(u.GetUpdatedAtUnixNanos()),
	}
	if len(u.GetCalibrations()) > 0 {
		out.Calibrations = make(map[string]robotcal.Record, len(u.GetCalibrations()))
		for k, v := range u.GetCalibrations() {
			out.Calibrations[k] = RecordFromProto(v)
		}
	}
	if len(u.GetSessions()) > 0 {
		out.Sessions = make(map[string]robotcal.Session, len(u.GetSessions()))
		for k, v := range u.GetSessions() {
			out.Sessions[k] = SessionFromProto(v)
		}
	}
	return out
}
