package oci

import (
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

// pidsPtr is a test helper: ResourceLimits.PIDs is a pointer so an absent field
// (nil → default cap) is distinguishable from an explicit, rejected 0.
func pidsPtr(v int64) *int64 { return &v }

func TestApplyResourceLimits_AllFields(t *testing.T) {
	spec := DefaultSpec("rootfs", []string{"/app"})
	limits := &appconfig.ResourceLimits{Memory: "512Mi", CPUs: "1.5", PIDs: pidsPtr(256)}

	if err := ApplyResourceLimits(spec, limits); err != nil {
		t.Fatalf("ApplyResourceLimits: %v", err)
	}

	res := spec.Linux.Resources
	if res.Memory == nil || res.Memory.Limit == nil || *res.Memory.Limit != 512*1024*1024 {
		t.Errorf("memory limit not applied: %+v", res.Memory)
	}
	if res.Memory.Swap == nil || *res.Memory.Swap != *res.Memory.Limit {
		t.Fatalf("explicit memory limit permits unbounded swap: %+v", res.Memory)
	}
	if res.CPU == nil || res.CPU.Quota == nil || *res.CPU.Quota != 150000 {
		t.Errorf("cpu quota not applied: %+v", res.CPU)
	}
	if res.CPU == nil || res.CPU.Period == nil || *res.CPU.Period != 100000 {
		t.Errorf("cpu period not applied: %+v", res.CPU)
	}
	if res.Pids == nil || res.Pids.Limit != 256 {
		t.Errorf("pids limit not applied: %+v", res.Pids)
	}
}

func TestApplyResourceLimits_NilAndEmpty(t *testing.T) {
	// nil limits: no-op, no panic.
	spec := DefaultSpec("rootfs", []string{"/app"})
	if err := ApplyResourceLimits(spec, nil); err != nil {
		t.Fatalf("nil limits should be a no-op: %v", err)
	}
	// Memory and CPU stay unbounded (WDY-1729); only the default PID guard
	// (WDY-1101) is applied — covered by TestApplyResourceLimits_DefaultsPIDs*.
	if m := spec.Linux.Resources.Memory; m != nil {
		t.Errorf("nil limits should not set memory, got %+v", m)
	}
	if c := spec.Linux.Resources.CPU; c != nil {
		t.Errorf("nil limits should not set cpu, got %+v", c)
	}

	// empty limits: leaves memory/cpu unset.
	spec2 := DefaultSpec("rootfs", []string{"/app"})
	if err := ApplyResourceLimits(spec2, &appconfig.ResourceLimits{}); err != nil {
		t.Fatalf("empty limits should be a no-op: %v", err)
	}
	res := spec2.Linux.Resources
	if res.Memory != nil || res.CPU != nil {
		t.Errorf("empty limits should leave memory/cpu unset, got %+v", res)
	}
}

func TestApplyResourceLimits_PartialPreservesDeviceRules(t *testing.T) {
	// Setting only memory must not clobber the device cgroup rules DefaultSpec
	// (and entitlements) populate on the same Resources struct.
	spec := DefaultSpec("rootfs", []string{"/app"})
	before := len(spec.Linux.Resources.Devices)
	if err := ApplyResourceLimits(spec, &appconfig.ResourceLimits{Memory: "128Mi"}); err != nil {
		t.Fatalf("ApplyResourceLimits: %v", err)
	}
	if got := len(spec.Linux.Resources.Devices); got != before {
		t.Errorf("device rules changed: before %d, after %d", before, got)
	}
	// Only memory was declared, so CPU stays unset; Pids carries the WDY-1101
	// default since the app declared none.
	if spec.Linux.Resources.CPU != nil {
		t.Errorf("only memory was declared; cpu should be unset, got %+v", spec.Linux.Resources.CPU)
	}
	if p := spec.Linux.Resources.Pids; p == nil || p.Limit != DefaultMaxPIDs {
		t.Errorf("expected default PID limit when undeclared, got %+v", p)
	}
}

// TestApplyResourceLimits_DefaultsPIDsWhenUndeclared is the WDY-1101 guard: a
// container that declares no PID limit still gets a conservative default so a
// fork bomb cannot exhaust host PIDs. Memory and CPU intentionally remain
// unbounded by default (WDY-1729 backward compatibility).
func TestApplyResourceLimits_DefaultsPIDsWhenUndeclared(t *testing.T) {
	for _, tc := range []struct {
		name   string
		limits *appconfig.ResourceLimits
	}{
		{"nil", nil},
		{"empty", &appconfig.ResourceLimits{}},
		{"only-memory", &appconfig.ResourceLimits{Memory: "128Mi"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := DefaultSpec("rootfs", []string{"/app"})
			if err := ApplyResourceLimits(spec, tc.limits); err != nil {
				t.Fatalf("ApplyResourceLimits: %v", err)
			}
			p := spec.Linux.Resources.Pids
			if p == nil || p.Limit != DefaultMaxPIDs {
				t.Errorf("expected default PID limit %d, got %+v", DefaultMaxPIDs, p)
			}
		})
	}
}

// TestApplyResourceLimits_DeclaredPIDsOverridesDefault ensures an explicit PID
// limit always wins over the default — including a deliberately high value.
func TestApplyResourceLimits_DeclaredPIDsOverridesDefault(t *testing.T) {
	spec := DefaultSpec("rootfs", []string{"/app"})
	if err := ApplyResourceLimits(spec, &appconfig.ResourceLimits{PIDs: pidsPtr(DefaultMaxPIDs * 4)}); err != nil {
		t.Fatalf("ApplyResourceLimits: %v", err)
	}
	if p := spec.Linux.Resources.Pids; p == nil || p.Limit != DefaultMaxPIDs*4 {
		t.Errorf("declared PID limit must override the default, got %+v", p)
	}
}

func TestApplyResourceLimits_InvalidValue(t *testing.T) {
	spec := DefaultSpec("rootfs", []string{"/app"})
	err := ApplyResourceLimits(spec, &appconfig.ResourceLimits{Memory: "garbage"})
	if err == nil {
		t.Fatalf("expected error for invalid memory value")
	}
}
