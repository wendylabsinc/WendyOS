package containerd

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	specs "github.com/opencontainers/runtime-spec/specs-go"

	sharedenv "github.com/wendylabsinc/wendy/go/internal/shared/env"
)

const appCgroupParent = "wendy-apps"

// appCgroupPath retains the historical basename because the app data socket
// authenticates peer identity from it. With cgroupfs, the colons are literal:
// they do NOT place an app under containerd.service or system.slice.
func appCgroupPath(appID, serviceName string, unified bool) string {
	suffix := appID
	if serviceName != "" {
		suffix += "@" + serviceName
	}
	name := fmt.Sprintf("system.slice:%s:%s", sharedenv.SystemdServiceName(), suffix)
	if unified {
		return "/" + appCgroupParent + "/" + name
	}
	return name
}

func hasUnifiedAppCgroup() bool {
	_, err := os.Stat("/sys/fs/cgroup/cgroup.controllers")
	return runtime.GOOS == "linux" && err == nil
}

// appMemoryBudget reserves max(2 GiB, 20% RAM) for the agent and host services.
// Small boards keep half their RAM rather than receiving a zero app budget.
// Apps cannot consume host swap; memory.high starts reclaim before their shared
// hard ceiling. Per-app limits remain additional, independent ceilings.
func appMemoryBudget(total uint64) (high, max uint64, err error) {
	if total < 2 {
		return 0, 0, fmt.Errorf("invalid host memory total %d", total)
	}
	reserve := total / 5
	if reserve < 2<<30 {
		reserve = 2 << 30
	}
	if reserve > total/2 {
		reserve = total / 2
	}
	max = total - reserve
	high = max - max/10
	return high, max, nil
}

func hostMemoryTotal(meminfo []byte) (uint64, error) {
	for _, line := range strings.Split(string(meminfo), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == "MemTotal:" && fields[2] == "kB" {
			kb, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil || kb == 0 || kb > ^uint64(0)/1024 {
				return 0, fmt.Errorf("invalid MemTotal in /proc/meminfo")
			}
			return kb * 1024, nil
		}
	}
	return 0, fmt.Errorf("missing MemTotal in /proc/meminfo")
}

// ensureAppMemoryBudget is deliberately called on every task start, including
// starts from a persisted spec after reboot. A v2 configuration error must stop
// the start: silently running without the parent cap defeats host protection.
// Older cgroup-v1 devices retain their existing placement and get an explicit
// warning at the call site; this v2 policy is not claimed to cover them.
func ensureAppMemoryBudget() (bool, error) {
	if runtime.GOOS != "linux" {
		return false, nil
	}
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); os.IsNotExist(err) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	meminfo, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return false, err
	}
	total, err := hostMemoryTotal(meminfo)
	if err != nil {
		return false, err
	}
	if err := configureAppMemoryBudget("/sys/fs/cgroup", total); err != nil {
		return false, err
	}
	return true, nil
}

func configureAppMemoryBudget(root string, total uint64) error {
	high, max, err := appMemoryBudget(total)
	if err != nil {
		return err
	}
	if err := enableAppMemoryController(root); err != nil {
		return err
	}
	parent := filepath.Join(root, appCgroupParent)
	if err := os.MkdirAll(parent, 0755); err != nil {
		return fmt.Errorf("creating app cgroup parent: %w", err)
	}
	for _, setting := range []struct{ name, value string }{
		{"memory.high", strconv.FormatUint(high, 10)},
		{"memory.max", strconv.FormatUint(max, 10)},
		{"memory.swap.max", "0"},
	} {
		if err := writeAppCgroupControl(filepath.Join(parent, setting.name), setting.value); err != nil {
			return err
		}
	}
	return enableAppMemoryController(parent)
}

func enableAppMemoryController(dir string) error {
	controllers, err := os.ReadFile(filepath.Join(dir, "cgroup.controllers"))
	if err != nil {
		return fmt.Errorf("reading cgroup memory controller: %w", err)
	}
	available := false
	for _, controller := range strings.Fields(string(controllers)) {
		available = available || controller == "memory"
	}
	if !available {
		return fmt.Errorf("cgroup memory controller unavailable in %s", dir)
	}
	path := filepath.Join(dir, "cgroup.subtree_control")
	enabled, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	for _, controller := range strings.Fields(string(enabled)) {
		if controller == "memory" {
			return nil
		}
	}
	return writeAppCgroupControl(path, "+memory")
}

func writeAppCgroupControl(path, value string) error {
	// Do not create missing control files: unsupported kernels must fail rather
	// than appear protected. Avoid re-writing an unchanged memory.max, which
	// can trigger reclaim and delay an otherwise unrelated app start.
	current, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading app cgroup control %s: %w", path, err)
	}
	if strings.TrimSpace(string(current)) == value {
		return nil
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return fmt.Errorf("opening app cgroup control %s: %w", path, err)
	}
	_, writeErr := f.WriteString(value)
	closeErr := f.Close()
	if writeErr != nil {
		return fmt.Errorf("setting app cgroup control %s: %w", path, writeErr)
	}
	return closeErr
}

// refreshAppResourcePolicy migrates persisted specs on their next task start.
// Updating the spec alone does not move any already-running task; agent restart
// recovery explicitly warns when such a task still needs an app restart.
func refreshAppResourcePolicy(spec *specs.Spec, appID, serviceName string, unified bool) (bool, error) {
	if spec == nil || spec.Linux == nil || spec.Process == nil {
		return false, fmt.Errorf("app spec lacks Linux or process configuration")
	}
	changed := false
	path := appCgroupPath(appID, serviceName, unified)
	if spec.Linux.CgroupsPath != path {
		spec.Linux.CgroupsPath = path
		changed = true
	}
	if spec.Process.OOMScoreAdj == nil || *spec.Process.OOMScoreAdj != 0 {
		score := 0
		spec.Process.OOMScoreAdj = &score
		changed = true
	}
	return changed, nil
}

// Runtime membership is authoritative: a failed restart may already have saved
// a new spec while the old task remains alive in its original cgroup.
func appTaskHasBudgetCgroup(membership string) bool {
	for _, line := range strings.Split(membership, "\n") {
		if strings.HasPrefix(line, "0::/"+appCgroupParent+"/") {
			return true
		}
	}
	return false
}
