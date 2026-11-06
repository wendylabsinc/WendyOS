package appconfig

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// cpuCFSPeriod is the CFS scheduler period (in microseconds) used to express a
// fractional-core CPU limit as a quota. 100ms matches the OCI/Docker default,
// so a `cpus` of "1.5" becomes quota=150000 over period=100000.
const cpuCFSPeriod uint64 = 100000

// maxPIDsLimit is the upper bound accepted for an explicit `pids` limit. It
// matches the common Linux kernel.pid_max ceiling; above it the cgroup pids
// controller may clamp to "max" (unbounded), silently defeating the fork-bomb
// guard, so we reject such values at validation time instead.
const maxPIDsLimit int64 = 4194304

// ResourceLimits declares the resource ceilings the agent enforces on a
// container via cgroups. All fields are optional; an omitted field leaves that
// resource unbounded (the historical behaviour). Limits may be set at the app
// level and overridden per service (see AppConfig.ResolveResourcesForService).
type ResourceLimits struct {
	// Memory is the hard memory limit as a byte count, optionally with a unit
	// suffix: binary (Ki, Mi, Gi, Ti) or decimal (K, M, G, T). A bare number is
	// interpreted as bytes. Explicit memory budgets also cap combined RAM and
	// swap at this value (no swap); exceeding the budget can OOM-kill the app.
	Memory string `json:"memory,omitempty"`
	// CPUs is the maximum number of CPU cores, as a decimal (e.g. "0.5", "1.5",
	// "2"). It is enforced as a CFS quota over a fixed 100ms period.
	CPUs string `json:"cpus,omitempty"`
	// PIDs is the maximum number of processes/threads the container may create —
	// a cheap guard against fork bombs. It is a pointer so an absent field is
	// distinguishable from an explicit (and rejected) 0: when omitted (nil) the
	// agent applies its conservative default cap (oci.DefaultMaxPIDs), set it
	// explicitly to raise or lower that ceiling.
	PIDs *int64 `json:"pids,omitempty"`
}

// memorySuffixes maps a (lower-cased) unit suffix to its multiplier. The "i"
// variants are binary (powers of 1024); the bare variants are decimal (powers
// of 1000), matching the convention used by Kubernetes resource quantities.
var memorySuffixes = map[string]int64{
	"ki": 1 << 10, "mi": 1 << 20, "gi": 1 << 30, "ti": 1 << 40,
	"k": 1e3, "m": 1e6, "g": 1e9, "t": 1e12,
}

// MemoryLimitBytes parses Memory into a byte count. It returns (nil, nil) when
// Memory is empty (unbounded) and an error when the value is malformed or not a
// positive whole number of bytes.
func (r *ResourceLimits) MemoryLimitBytes() (*int64, error) {
	s := strings.TrimSpace(r.Memory)
	if s == "" {
		return nil, nil
	}

	mult := int64(1)
	// Match the longest unit suffix first so "Mi" is not mistaken for "M".
	lower := strings.ToLower(s)
	for _, suffix := range []string{"ki", "mi", "gi", "ti", "k", "m", "g", "t"} {
		if strings.HasSuffix(lower, suffix) {
			mult = memorySuffixes[suffix]
			s = s[:len(s)-len(suffix)]
			break
		}
	}

	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("memory %q is not a valid byte quantity (e.g. \"512Mi\", \"1Gi\", or a number of bytes)", r.Memory)
	}
	if n <= 0 {
		return nil, fmt.Errorf("memory must be a positive quantity, got %q", r.Memory)
	}
	// Guard the suffix multiplication against int64 overflow: a value like
	// "9223372036854775807Ti" would otherwise wrap to a negative or tiny number
	// and silently undersize (or unbound) the cgroup limit. n>0 and mult>=1, so
	// the division is safe.
	if n > math.MaxInt64/mult {
		return nil, fmt.Errorf("memory %q is too large (overflows the maximum byte count)", r.Memory)
	}
	bytes := n * mult
	return &bytes, nil
}

// CPUQuota parses CPUs into a CFS (quota, period) pair in microseconds. It
// returns (nil, nil, nil) when CPUs is empty (unbounded) and an error when the
// value is malformed or not positive.
func (r *ResourceLimits) CPUQuota() (*int64, *uint64, error) {
	s := strings.TrimSpace(r.CPUs)
	if s == "" {
		return nil, nil, nil
	}
	cores, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil, nil, fmt.Errorf("cpus %q is not a valid number of cores (e.g. \"0.5\", \"1\", \"2\")", r.CPUs)
	}
	// ParseFloat accepts "Inf"/"NaN" without error; reject non-finite values
	// explicitly (NaN also slips past the cores<=0 check below).
	if math.IsInf(cores, 0) || math.IsNaN(cores) {
		return nil, nil, fmt.Errorf("cpus %q is not a finite number of cores", r.CPUs)
	}
	if cores <= 0 {
		return nil, nil, fmt.Errorf("cpus must be a positive number of cores, got %q", r.CPUs)
	}
	period := cpuCFSPeriod
	// Guard the quota against int64 overflow before casting: a value like
	// "1e308" would otherwise wrap to a tiny or negative CFS quota and silently
	// throttle the container to near-zero CPU (or be treated as unlimited).
	quotaF := cores*float64(period) + 0.5 // round to nearest microsecond
	if quotaF > float64(math.MaxInt64) {
		return nil, nil, fmt.Errorf("cpus %q is too large (overflows the maximum CFS quota)", r.CPUs)
	}
	quota := int64(quotaF)
	return &quota, &period, nil
}

// PIDsLimit returns the process-count limit, or nil when unset (the agent then
// applies its default cap, see oci.DefaultMaxPIDs).
func (r *ResourceLimits) PIDsLimit() *int64 {
	if r.PIDs == nil {
		return nil
	}
	limit := *r.PIDs
	return &limit
}

// validate checks that every set field parses and is within range. The prefix
// is used in error messages (e.g. "resources" or "services[\"worker\"].resources").
func (r *ResourceLimits) validate(prefix string) error {
	if r == nil {
		return nil
	}
	if _, err := r.MemoryLimitBytes(); err != nil {
		return fmt.Errorf("%s.%w", prefix, err)
	}
	if _, _, err := r.CPUQuota(); err != nil {
		return fmt.Errorf("%s.%w", prefix, err)
	}
	// nil means "use the default cap"; an explicit value must be a positive
	// process count within the kernel's pid ceiling. Reject 0 (and negatives)
	// with a clear hint rather than silently treating 0 as "unset" — schema
	// enforces minimum:1, but a legacy or hand-rolled config could still reach
	// the agent. Reject absurdly large values too: above kernel.pid_max the
	// cgroup may clamp to "max" and silently defeat the fork-bomb guard.
	if r.PIDs != nil {
		if *r.PIDs < 1 {
			return fmt.Errorf("%s.pids must be a positive process count (omit the field to use the default cap), got %d", prefix, *r.PIDs)
		}
		if *r.PIDs > maxPIDsLimit {
			return fmt.Errorf("%s.pids %d exceeds the maximum supported value (%d)", prefix, *r.PIDs, maxPIDsLimit)
		}
	}
	return nil
}

// ResolveResourcesForService returns the resource limits that apply to the
// named service, merging service-level over app-level limits PER FIELD: a field
// the service sets wins, and a field the service leaves unset inherits the
// app-level value. This prevents a service block that overrides only one field
// (e.g. memory) from silently dropping a security-relevant app-level constraint
// such as a PID cap. Returns nil when neither level sets anything.
func (a *AppConfig) ResolveResourcesForService(serviceName string) *ResourceLimits {
	var svc *ResourceLimits
	if serviceName != "" {
		if s, ok := a.Services[serviceName]; ok && s != nil {
			svc = s.Resources
		}
	}
	switch {
	case svc == nil:
		return a.Resources
	case a.Resources == nil:
		return svc
	}
	// Both set: start from the app-level limits and overlay the fields the
	// service actually declares. Copy by value so neither input is mutated.
	merged := *a.Resources
	if strings.TrimSpace(svc.Memory) != "" {
		merged.Memory = svc.Memory
	}
	if strings.TrimSpace(svc.CPUs) != "" {
		merged.CPUs = svc.CPUs
	}
	if svc.PIDs != nil {
		merged.PIDs = svc.PIDs
	}
	return &merged
}
