package robotcal

import (
	"strings"
	"testing"
)

// TestResolveClassBelongsToTheProfileAndMethodTogether is the generalisation
// the whole safety model depends on: a joint sweep on a back-drivable arm
// energises nothing, and the same sweep on a robot that must hold a brake open
// does. If the class were a property of the method alone, one robot would
// inherit the other's gates.
func TestResolveClassBelongsToTheProfileAndMethodTogether(t *testing.T) {
	tests := []struct {
		name      string
		floor     SafetyClass
		declared  SafetyClass
		want      SafetyClass
		wantErr   bool
		errSubstr string
	}{
		{
			name:  "a profile that declares nothing takes the method's floor",
			floor: ClassNothingPowered,
			want:  ClassNothingPowered,
		},
		{
			name:     "an arm keeps the sweep unpowered",
			floor:    ClassNothingPowered,
			declared: ClassNothingPowered,
			want:     ClassNothingPowered,
		},
		{
			name:     "a robot where the same sweep needs power raises it",
			floor:    ClassNothingPowered,
			declared: ClassPoweredMotion,
			want:     ClassPoweredMotion,
		},
		{
			name:     "a profile can raise to static observation too",
			floor:    ClassNothingPowered,
			declared: ClassStaticObservation,
			want:     ClassStaticObservation,
		},
		{
			// A profile that could call a powered procedure unpowered is a
			// profile that can switch the gate off.
			name:      "a profile can never lower the class",
			floor:     ClassPoweredMotion,
			declared:  ClassNothingPowered,
			wantErr:   true,
			errSubstr: "never lowered",
		},
		{
			name:      "an unknown class is refused rather than ignored",
			floor:     ClassNothingPowered,
			declared:  "mostly-safe",
			wantErr:   true,
			errSubstr: "unknown safety class",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Resolve(tt.floor, tt.declared)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Resolve(%q, %q) = %q, want an error", tt.floor, tt.declared, got)
				}
				if !strings.Contains(err.Error(), tt.errSubstr) {
					t.Fatalf("error = %v, want it to mention %q", err, tt.errSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve(%q, %q): %v", tt.floor, tt.declared, err)
			}
			if got != tt.want {
				t.Fatalf("Resolve(%q, %q) = %q, want %q", tt.floor, tt.declared, got, tt.want)
			}
		})
	}
}

func TestGateRefusesRatherThanWarns(t *testing.T) {
	tests := []struct {
		name      string
		class     SafetyClass
		in        GateInput
		wantErr   bool
		errSubstr string
	}{
		{
			name:  "an unpowered procedure runs on day one",
			class: ClassNothingPowered,
			in:    GateInput{Interactive: true},
		},
		{
			name:  "so does a static observation",
			class: ClassStaticObservation,
			in:    GateInput{Interactive: true},
		},
		{
			// Rule 6: no --non-interactive for steps that need a human.
			name:      "no procedure runs without a person at the terminal",
			class:     ClassNothingPowered,
			in:        GateInput{Interactive: false},
			wantErr:   true,
			errSubstr: "needs a person at the terminal",
		},
		{
			// WDY-3128 has not landed. A CLI-side gate with no agent-side
			// enforcement is a gate any other gRPC client walks past.
			name:      "powered motion refuses while the motion entitlement does not exist",
			class:     ClassPoweredMotion,
			in:        GateInput{Interactive: true},
			wantErr:   true,
			errSubstr: "motion entitlement",
		},
		{
			name:  "and runs once it does",
			class: ClassPoweredMotion,
			in:    GateInput{Interactive: true, MotionEntitlementAvailable: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.class.Gate(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatal("want a refusal, got none")
				}
				if !strings.Contains(err.Error(), tt.errSubstr) {
					t.Fatalf("error = %v, want it to mention %q", err, tt.errSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected refusal: %v", err)
			}
		})
	}
}

// TestMethodSetIsClosed keeps the fixed set fixed: a profile that names a method
// WendyOS does not implement is told what the set is, not handed a mechanism to
// describe one.
func TestMethodSetIsClosed(t *testing.T) {
	if _, err := Method("wave-a-tape-measure-about"); err == nil {
		t.Fatal("an unknown method must be refused")
	} else if !strings.Contains(err.Error(), string(MethodJointRangeSweep)) {
		t.Fatalf("the refusal must list the set it does implement, got: %v", err)
	}
	want := []string{"fiducial-extrinsics", "hand-eye-extrinsics", "homing-offset", "joint-range-sweep"}
	got := MethodIDs()
	if len(got) != len(want) {
		t.Fatalf("method set = %v, want exactly %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("method set = %v, want %v", got, want)
		}
	}
}
