package robotcal

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func testStore(t *testing.T) *FileStore {
	t.Helper()
	s := NewFileStore(t.TempDir())
	s.Now = func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }
	return s
}

// TestRecordsAreKeyedPerUnitNotPerDevice is the two-arms-on-one-Jetson case: an
// SO-101 rig is a leader and a follower with different measured travel on every
// joint, and one shared record would be wrong for both.
func TestRecordsAreKeyedPerUnitNotPerDevice(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)

	leader := Record{ID: "joint-range", Qualified: true,
		Residual: &Quantity{Value: 4, Unit: "tick"}, Budget: Quantity{Value: 10, Unit: "tick"}}
	follower := Record{ID: "joint-range", Qualified: false,
		Residual: &Quantity{Value: 40, Unit: "tick"}, Budget: Quantity{Value: 10, Unit: "tick"}}

	if err := store.PutRecord(ctx, "leader", leader); err != nil {
		t.Fatal(err)
	}
	if err := store.PutRecord(ctx, "follower", follower); err != nil {
		t.Fatal(err)
	}

	got, err := store.Load(ctx, "leader")
	if err != nil {
		t.Fatal(err)
	}
	if got.Calibrations["joint-range"].Residual.Value != 4 {
		t.Fatalf("leader picked up the follower's calibration: %+v", got.Calibrations["joint-range"])
	}
	units, err := store.Units()
	if err != nil {
		t.Fatal(err)
	}
	if len(units) != 2 {
		t.Fatalf("units = %v, want both arms", units)
	}
}

func TestLoadingAnUncalibratedUnitIsNotAnError(t *testing.T) {
	got, err := testStore(t).Load(context.Background(), "default")
	if err != nil {
		t.Fatalf("a robot nobody has calibrated is the normal state, not an error: %v", err)
	}
	if len(got.Calibrations) != 0 {
		t.Fatalf("expected no calibrations, got %+v", got.Calibrations)
	}
}

// TestClearMattersMoreThanItLooks: after a repair the old calibration is worse
// than none, and any half-finished attempt at it goes with it.
func TestClearDropsTheRecordAndItsSession(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	if err := store.PutRecord(ctx, "default", Record{ID: "joint-range"}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutSession(ctx, "default", Session{ProcedureID: "joint-range"}); err != nil {
		t.Fatal(err)
	}
	if err := store.ClearRecord(ctx, "default", "joint-range"); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Calibrations["joint-range"]; ok {
		t.Error("the record survived clear")
	}
	if _, ok := got.Sessions["joint-range"]; ok {
		t.Error("a half-finished attempt at a cleared calibration must go with it")
	}
	if err := store.ClearRecord(ctx, "default", "never-existed"); err == nil {
		t.Error("clearing a calibration that does not exist should say so")
	}
}

func TestInvalidUnitNamesAreRefused(t *testing.T) {
	for _, unit := range []string{"", "../escape", "Leader", "a/b", "x y"} {
		if err := ValidUnit(unit); err == nil {
			t.Errorf("unit %q should be refused", unit)
		}
	}
	for _, unit := range []string{"default", "leader", "follower", "arm-2", "g1.left"} {
		if err := ValidUnit(unit); err != nil {
			t.Errorf("unit %q should be accepted: %v", unit, err)
		}
	}
}

func TestSessionKeepsSkippedAndMeasuredApart(t *testing.T) {
	var s Session
	s.Record("joint_a", json.RawMessage(`{"travel":1}`))
	s.Skip("joint_b")

	if !s.Done("joint_a") || !s.Done("joint_b") {
		t.Fatal("both an answered and a skipped step count as answered")
	}
	if s.WasSkipped("joint_a") {
		t.Error("a measured joint must not read as skipped")
	}
	if !s.WasSkipped("joint_b") {
		t.Error("a skipped joint must stay distinguishable from a measured one")
	}
	// Re-doing a skipped step clears the skip rather than leaving both.
	s.Record("joint_b", json.RawMessage(`{"travel":2}`))
	if s.WasSkipped("joint_b") {
		t.Error("measuring a previously skipped joint must clear the skip")
	}
	if got := s.Progress([]string{"joint_a", "joint_b", "joint_c"}); got != 2 {
		t.Fatalf("progress = %d, want 2", got)
	}
}

// TestStatusesShowWhatWasNeverRun: a listing that only showed stored records
// would hide the calibrations that matter most.
func TestStatusesShowWhatWasNeverRun(t *testing.T) {
	p, err := LoadProfile("unitree-g1")
	if err != nil {
		t.Fatal(err)
	}
	statuses := Statuses(p, UnitRecord{})
	if len(statuses) != len(p.RequiresCalibration) {
		t.Fatalf("statuses = %d, want one per declared calibration (%d)", len(statuses), len(p.RequiresCalibration))
	}
	for _, st := range statuses {
		if st.Verdict != VerdictNeverRun {
			t.Errorf("%s: verdict = %q, want %q", st.ID, st.Verdict, VerdictNeverRun)
		}
		if !st.Runnable && st.Reason == "" {
			t.Errorf("%s: a procedure this build cannot run must say why", st.ID)
		}
	}
}

func TestStatusMarksARecordTakenAgainstAnOldBudgetStale(t *testing.T) {
	p, err := LoadProfile("so101")
	if err != nil {
		t.Fatal(err)
	}
	proc := p.RequiresCalibration[0]
	unit := UnitRecord{Calibrations: map[string]Record{
		proc.ID: {
			ID:        proc.ID,
			Qualified: true,
			Residual:  &Quantity{Value: 0, Unit: "tick"},
			Budget:    Quantity{Value: proc.Budget.Value + 100, Unit: "tick"},
		},
	}}
	statuses := Statuses(p, unit)
	if statuses[0].Verdict != VerdictStale {
		t.Fatalf("verdict = %q, want %q: a record judged against a budget the profile no longer asks "+
			"for was judged by a gate that no longer applies", statuses[0].Verdict, VerdictStale)
	}
}
