package robotcal

import (
	"strings"
	"testing"
)

// TestEmbeddedProfilesAreValid is the generalisation test with teeth: both
// shipped profiles have to satisfy the same schema, and they agree on none of
// the contents — 29 joints in radians against 6 in encoder ticks, declared
// vendor limits against none at all, a vendor state machine against torque
// disabled.
func TestEmbeddedProfilesAreValid(t *testing.T) {
	kinds := ProfileKinds()
	if len(kinds) < 2 {
		t.Fatalf("expected at least two shipped profiles (a humanoid and an arm), got %v", kinds)
	}
	for _, kind := range kinds {
		t.Run(kind, func(t *testing.T) {
			p, err := LoadProfile(kind)
			if err != nil {
				t.Fatalf("loading %q: %v", kind, err)
			}
			if p.Kind != kind {
				t.Errorf("profile file %q declares kind %q", kind, p.Kind)
			}
			if p.Control.Unpowered.Instruction == "" {
				t.Error("a profile must say how to put this robot into its unpowered state, in its own words")
			}
			for _, proc := range p.RequiresCalibration {
				if _, err := Method(proc.Method); err != nil {
					t.Errorf("calibration %q: %v", proc.ID, err)
				}
			}
		})
	}
}

func TestShippedProfilesDifferInContentNotShape(t *testing.T) {
	g1, err := LoadProfile("unitree-g1")
	if err != nil {
		t.Fatal(err)
	}
	arm, err := LoadProfile("so101")
	if err != nil {
		t.Fatal(err)
	}

	if g1.Joints.Unit == arm.Joints.Unit {
		t.Errorf("both profiles report joints in %q; the point of the unit being a free string is "+
			"that an arm reports encoder ticks and a humanoid radians", g1.Joints.Unit)
	}
	if len(g1.Joints.Declared) == 0 {
		t.Error("the humanoid's travel is a datasheet fact and should be declared")
	}
	if len(arm.Joints.Declared) != 0 {
		t.Error("the arm's travel is measured per unit, so it must not be declared in the profile")
	}
	if g1.Control.Unpowered.Name == arm.Control.Unpowered.Name {
		t.Error("the two robots call their unpowered state the same thing, which means the platform " +
			"is probably enumerating it rather than quoting the profile")
	}
	// Both robots run the same method, and it is the one that runs on day one.
	for _, p := range []*Profile{g1, arm} {
		var found bool
		for _, proc := range p.RequiresCalibration {
			if proc.Method != MethodJointRangeSweep {
				continue
			}
			found = true
			desc, err := Method(proc.Method)
			if err != nil {
				t.Fatal(err)
			}
			class, err := Resolve(desc.ClassFloor, proc.Class)
			if err != nil {
				t.Fatal(err)
			}
			if class != ClassNothingPowered {
				t.Errorf("%s: joint-range-sweep resolved to %q, want %q", p.Kind, class, ClassNothingPowered)
			}
			if proc.Budget.Unit != p.Joints.Unit {
				t.Errorf("%s: budget is in %q but joints are in %q", p.Kind, proc.Budget.Unit, p.Joints.Unit)
			}
		}
		if !found {
			t.Errorf("%s declares no joint-range sweep", p.Kind)
		}
	}
}

func TestProfileValidationRefusesWhatThePlatformCannotActOn(t *testing.T) {
	const base = `
kind: test-bot
control:
  unpowered: {name: limp, instruction: hold it}
joints:
  unit: rad
  source: {backend: fake}
  order: [a, b]
`
	tests := []struct {
		name      string
		yaml      string
		errSubstr string
	}{
		{
			name: "valid",
			yaml: base + `
requires_calibration:
  - id: range
    method: joint-range-sweep
    budget: {value: 0.1, unit: rad}
`,
		},
		{
			name: "a reserved id would be unreachable from the CLI",
			yaml: base + `
requires_calibration:
  - id: status
    method: joint-range-sweep
    budget: {value: 0.1, unit: rad}
`,
			errSubstr: "reserved calibration id",
		},
		{
			name: "a method outside the fixed set",
			yaml: base + `
requires_calibration:
  - id: range
    method: vibes-based-alignment
    budget: {value: 0.1, unit: rad}
`,
			errSubstr: "unknown calibration method",
		},
		{
			name: "no budget means nothing to pass",
			yaml: base + `
requires_calibration:
  - id: range
    method: joint-range-sweep
`,
			errSubstr: "declares no budget",
		},
		{
			name: "a class below the method's floor",
			yaml: base + `
requires_calibration:
  - id: handeye
    method: hand-eye-extrinsics
    class: nothing-powered
    budget: {value: 0.01, unit: m}
`,
			errSubstr: "never lowered",
		},
		{
			name: "a joint nobody declared",
			yaml: base + `
requires_calibration:
  - id: range
    method: joint-range-sweep
    budget: {value: 0.1, unit: rad}
    joints: [c]
`,
			errSubstr: `joint "c"`,
		},
		{
			name: "declared limits for a joint that does not exist",
			yaml: `
kind: test-bot
control:
  unpowered: {name: limp, instruction: hold it}
joints:
  unit: rad
  source: {backend: fake}
  order: [a]
  declared:
    zzz: {min: -1, max: 1}
`,
			errSubstr: `joint "zzz"`,
		},
		{
			name: "no joint order at all",
			yaml: `
kind: test-bot
joints:
  unit: rad
  source: {backend: fake}
`,
			errSubstr: "declares no joints.order",
		},
		{
			name: "no unit on the joints",
			yaml: `
kind: test-bot
joints:
  order: [a]
  source: {backend: fake}
`,
			errSubstr: "declares no joints.unit",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseProfile([]byte(tt.yaml))
			if tt.errSubstr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("want a refusal mentioning %q, got none", tt.errSubstr)
			}
			if !strings.Contains(err.Error(), tt.errSubstr) {
				t.Fatalf("error = %v, want it to mention %q", err, tt.errSubstr)
			}
		})
	}
}

// TestOpaqueFieldsAreCarriedNotInterpreted checks that a profile can say things
// the platform has no opinion about without the platform choking on them.
func TestOpaqueFieldsAreCarriedNotInterpreted(t *testing.T) {
	p, err := ParseProfile([]byte(`
kind: test-bot
joints:
  unit: rad
  source: {backend: fake}
  order: [a]
bringup:
  sequence: [500, DAMP, 801]
limits:
  torque: {a: 88}
description: <robot name="x"/>
`))
	if err != nil {
		t.Fatalf("a profile with fields the platform does not interpret must still load: %v", err)
	}
	for _, key := range []string{"bringup", "limits", "description"} {
		if _, ok := p.Opaque[key]; !ok {
			t.Errorf("%q was dropped rather than carried", key)
		}
	}
}
