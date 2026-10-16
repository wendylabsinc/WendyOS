package robotcal

import (
	"testing"
	"time"
)

// TestQualifyIsTheOnlyGate pins the rule the whole wizard turns on: completing a
// procedure qualifies nothing, and fitting inside the budget is the only thing
// that does.
func TestQualifyIsTheOnlyGate(t *testing.T) {
	rad := func(v float64) *Quantity { return &Quantity{Value: v, Unit: "rad"} }

	tests := []struct {
		name         string
		residual     *Quantity
		budget       Quantity
		wantQualifed bool
		wantVerdict  Verdict
	}{
		{
			name:         "inside the budget qualifies",
			residual:     rad(0.004),
			budget:       Quantity{Value: 0.010, Unit: "rad"},
			wantQualifed: true,
			wantVerdict:  VerdictQualified,
		},
		{
			name:        "outside the budget does not",
			residual:    rad(0.11),
			budget:      Quantity{Value: 0.010, Unit: "rad"},
			wantVerdict: VerdictOverBudget,
		},
		{
			name:         "exactly on the budget qualifies",
			residual:     rad(0.010),
			budget:       Quantity{Value: 0.010, Unit: "rad"},
			wantQualifed: true,
			wantVerdict:  VerdictQualified,
		},
		{
			// The rule a skip depends on. A skipped step must never be storable
			// as a zero, because a zero residual is the best possible result.
			name:        "a missing residual never qualifies",
			residual:    nil,
			budget:      Quantity{Value: 0.010, Unit: "rad"},
			wantVerdict: VerdictNotMeasured,
		},
		{
			name:         "a measured zero does qualify, unlike a missing one",
			residual:     rad(0),
			budget:       Quantity{Value: 0.010, Unit: "rad"},
			wantQualifed: true,
			wantVerdict:  VerdictQualified,
		},
		{
			// The SO-101 reports ticks and the G1 radians. Comparing across
			// them would produce a number, and the number would be meaningless.
			name:        "a residual in another unit is incomparable, not large",
			residual:    &Quantity{Value: 900, Unit: "tick"},
			budget:      Quantity{Value: 0.010, Unit: "rad"},
			wantVerdict: VerdictIncomparable,
		},
		{
			name:        "a residual on another axis is incomparable",
			residual:    &Quantity{Value: 0.001, Unit: "m", Axis: "vertical"},
			budget:      Quantity{Value: 0.010, Unit: "m", Axis: "horizontal"},
			wantVerdict: VerdictIncomparable,
		},
		{
			name:        "no budget means nothing to pass",
			residual:    rad(0.001),
			budget:      Quantity{},
			wantVerdict: VerdictNoBudget,
		},
		{
			name:        "a zero budget is no budget",
			residual:    rad(0),
			budget:      Quantity{Value: 0, Unit: "rad"},
			wantVerdict: VerdictNoBudget,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotQualified, gotVerdict := Qualify(tt.residual, tt.budget)
			if gotQualified != tt.wantQualifed {
				t.Errorf("qualified = %v, want %v", gotQualified, tt.wantQualifed)
			}
			if gotVerdict != tt.wantVerdict {
				t.Errorf("verdict = %q, want %q", gotVerdict, tt.wantVerdict)
			}
		})
	}
}

// TestNewRecordCannotDisagreeWithItself checks that no caller can build a record
// whose Qualified flag contradicts its own numbers.
func TestNewRecordCannotDisagreeWithItself(t *testing.T) {
	over := &Quantity{Value: 5, Unit: "tick"}
	rec, verdict := NewRecord("joint-range", "joint-range-sweep/1.0", over,
		Quantity{Value: 1, Unit: "tick"}, time.Unix(0, 0), nil)
	if rec.Qualified {
		t.Fatal("a record over budget must not be qualified")
	}
	if verdict != VerdictOverBudget {
		t.Fatalf("verdict = %q, want %q", verdict, VerdictOverBudget)
	}
	if rec.Method != "joint-range-sweep/1.0" {
		t.Fatalf("method = %q; the method has to be recorded, because trust differs between them", rec.Method)
	}
	// A stored flag is never trusted over the numbers beside it.
	rec.Qualified = true
	if got := rec.Verdict(); got != VerdictOverBudget {
		t.Fatalf("re-derived verdict = %q, want %q", got, VerdictOverBudget)
	}
}
