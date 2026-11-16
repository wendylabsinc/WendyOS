// Package appcgroup puts every app container under one cgroup v2 parent whose
// memory.max leaves a small reserve for the agent and host services.
package appcgroup

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	sharedenv "github.com/wendylabsinc/wendy/go/internal/shared/env"
)

const (
	// Parent is the cgroup, below the cgroup v2 root, that holds every app.
	Parent = "wendy-apps"
	// OverrideFile, in the agent config dir, holds a reserve quantity or "off".
	OverrideFile = "host-memory-reserve"

	minAppMemory = 256 << 20
	// minReserve catches a unitless override such as "768", read as bytes.
	minReserve = 256 << 20

	mib = 1 << 20
	gib = 1 << 30
)

var (
	cgroupRoot  = "/sys/fs/cgroup"
	meminfoPath = "/proc/meminfo"
	ensureMu    sync.Mutex
)

// Status reports what Configure or Ensure applied. Apps start whatever it says.
type Status struct {
	Unified bool   // cgroup v2 is mounted
	Limited bool   // a numeric memory.max was applied to Parent
	Reserve uint64 // bytes kept for the host; 0 means no budget
	Max     uint64 // the app budget in bytes; 0 means unlimited
	Note    string // why there is no budget, or not the configured one
	Err     error  // a cgroup read or write failed
}

// DefaultReserve is a fixed part for host services plus a share for kernel and
// driver memory that grows with RAM, capped at half for small boards.
func DefaultReserve(total uint64) uint64 {
	reserve := (768*mib + total*3/100 + mib - 1) / mib * mib
	return min(reserve, total/2/mib*mib)
}

// Reserve resolves an override: empty means the default, "off" means none.
// It fails when an override is under minReserve or apps would keep less than
// minAppMemory.
func Reserve(total uint64, override string) (uint64, error) {
	s := strings.TrimSpace(override)
	var reserve uint64
	switch {
	case s == "":
		reserve = DefaultReserve(total)
	case strings.EqualFold(s, "off"):
		return 0, nil
	default:
		n, err := (&appconfig.ResourceLimits{Memory: s}).MemoryLimitBytes()
		if err != nil {
			return 0, err
		}
		reserve = uint64(*n)
		if reserve < minReserve {
			return 0, fmt.Errorf("a %d byte reserve is under %d MiB: add a unit such as 768Mi, or use off to remove the budget",
				reserve, minReserve/mib)
		}
	}
	if reserve >= total || total-reserve < minAppMemory {
		return 0, fmt.Errorf("a %d MiB reserve leaves apps less than %d MiB of %d MiB",
			reserve/mib, minAppMemory/mib, total/mib)
	}
	return reserve, nil
}

// Configure sets Parent's memory.max under root to total-reserve, or to max
// when reserve is 0. Without a cgroup v2 memory controller it writes nothing.
func Configure(root string, total, reserve uint64) Status {
	st, ok := probe(root)
	if !ok {
		return st
	}
	if reserve >= total && reserve > 0 {
		st.Err = fmt.Errorf("host memory reserve %d exceeds total %d", reserve, total)
		return st
	}
	st.Reserve = reserve
	value := "max"
	if reserve > 0 {
		st.Max = total - reserve
		value = strconv.FormatUint(st.Max, 10)
	}
	if err := enableMemory(root); err != nil {
		st.Err = err
		return st
	}
	parent := filepath.Join(root, Parent)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		st.Err = fmt.Errorf("creating app cgroup: %w", err)
		return st
	}
	errMax := writeControl(filepath.Join(parent, "memory.max"), value)
	// A stale memory.high would throttle every app well before memory.max.
	errHigh := writeControl(filepath.Join(parent, "memory.high"), "max")
	if errors.Is(errHigh, fs.ErrNotExist) {
		errHigh = nil
	}
	st.Limited = errMax == nil && reserve > 0
	st.Err = errors.Join(errMax, errHigh, enableMemory(parent))
	return st
}

// Ensure applies the budget for this host: its RAM and the override file in
// configDir. A bad override falls back to the default reserve.
func Ensure(configDir string) Status {
	ensureMu.Lock()
	defer ensureMu.Unlock()
	if runtime.GOOS != "linux" {
		return Status{}
	}
	if st, ok := probe(cgroupRoot); !ok {
		return st
	}
	meminfo, err := os.ReadFile(meminfoPath)
	if err != nil {
		return Status{Unified: true, Err: err}
	}
	total, err := hostMemoryTotal(meminfo)
	if err != nil {
		return Status{Unified: true, Err: err}
	}
	reserve, note := reserveFor(configDir, total)
	st := Configure(cgroupRoot, total, reserve)
	st.Note = joinNotes(st.Note, note)
	return st
}

func reserveFor(configDir string, total uint64) (uint64, string) {
	path := filepath.Join(configDir, OverrideFile)
	var note string
	data, err := os.ReadFile(path)
	switch {
	case err == nil && strings.TrimSpace(string(data)) != "":
		reserve, err := Reserve(total, string(data))
		if err == nil && reserve == 0 {
			return 0, "host memory reserve is off in " + path
		}
		if err == nil {
			return reserve, ""
		}
		note = fmt.Sprintf("%s: %v; using the default reserve", path, err)
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		note = fmt.Sprintf("%v; using the default reserve", err)
	}
	reserve, err := Reserve(total, "")
	if err != nil {
		return 0, joinNotes(note, "no app memory budget: "+err.Error())
	}
	return reserve, note
}

func joinNotes(a, b string) string {
	if a == "" || b == "" {
		return a + b
	}
	return a + "; " + b
}

// probe reports whether root is cgroup v2 with a memory controller to use.
func probe(root string) (Status, bool) {
	controllers, err := os.ReadFile(filepath.Join(root, "cgroup.controllers"))
	if errors.Is(err, fs.ErrNotExist) {
		return Status{Note: "cgroup v2 unavailable"}, false
	}
	st := Status{Unified: true}
	if err != nil {
		st.Err = fmt.Errorf("reading cgroup controllers: %w", err)
		return st, false
	}
	if !hasMemory(controllers) {
		st.Note = "memory controller unavailable"
		return st, false
	}
	return st, true
}

func hasMemory(list []byte) bool {
	for _, controller := range strings.Fields(string(list)) {
		if controller == "memory" {
			return true
		}
	}
	return false
}

func enableMemory(dir string) error {
	path := filepath.Join(dir, "cgroup.subtree_control")
	enabled, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading cgroup control: %w", err)
	}
	if hasMemory(enabled) {
		return nil
	}
	return writeFile(path, "+memory")
}

// writeControl skips an unchanged value: rewriting memory.max can trigger
// reclaim and delay an unrelated app start.
func writeControl(path, value string) error {
	current, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading cgroup control: %w", err)
	}
	if strings.TrimSpace(string(current)) == value {
		return nil
	}
	return writeFile(path, value)
}

// writeFile never creates the file: a missing control file means the kernel
// does not support it, and creating one would fake a limit.
func writeFile(path, value string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return fmt.Errorf("opening cgroup control: %w", err)
	}
	_, writeErr := f.WriteString(value)
	closeErr := f.Close()
	if writeErr != nil {
		return fmt.Errorf("setting cgroup control: %w", writeErr)
	}
	return closeErr
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

// Unified reports whether this host runs cgroup v2.
func Unified() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	_, err := os.Stat(filepath.Join(cgroupRoot, "cgroup.controllers"))
	return err == nil
}

// ContainerPath keeps the historical basename because the app data socket
// identifies apps by it. With cgroupfs the colons are literal, not slices.
func ContainerPath(appID, serviceName string, unified bool) string {
	suffix := appID
	if serviceName != "" {
		suffix += "@" + serviceName
	}
	name := fmt.Sprintf("system.slice:%s:%s", sharedenv.SystemdServiceName(), suffix)
	if unified {
		return "/" + Parent + "/" + name
	}
	return name
}

// HelperPath places an agent-started helper under Parent. The system.slice:
// prefix is refused so a helper can never be mistaken for an app.
func HelperPath(name string, unified bool) (string, bool) {
	if !unified || name == "" || name == "." || name == ".." ||
		strings.Contains(name, "/") || strings.HasPrefix(name, "system.slice:") {
		return "", false
	}
	return "/" + Parent + "/" + name, true
}

// InBudget reports whether /proc/<pid>/cgroup content places a task under
// Parent. Runtime membership wins over the saved spec, which may be newer.
func InBudget(procCgroup string) bool {
	for _, line := range strings.Split(procCgroup, "\n") {
		if strings.HasPrefix(line, "0::/"+Parent+"/") {
			return true
		}
	}
	return false
}
