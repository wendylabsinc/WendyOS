package hardware

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// GPU driver states reported in the gpu capability's properties.
const (
	driverStatusResponding    = "responding"     // the driver answered
	driverStatusNotResponding = "not_responding" // nodes exist but the driver refused
	driverStatusUnknown       = "unknown"        // probe failed, timed out, or was inconclusive
	driverStatusAbsent        = "absent"         // no driver nodes to ask
)

// gpuProbeTimeout bounds the external query. A wedged driver can leave
// nvidia-smi hanging, and hardware discovery must not hang with it.
const gpuProbeTimeout = 3 * time.Second

// GPUDriverHealth describes a management query or control-node open, not CUDA
// context creation or kernel execution. Hardware presence remains a separate
// board fact, used by image builds even when a probe is inconclusive.
type GPUDriverHealth struct {
	Vendor string
	Status string
	// Probe names what was actually tried, so a "not_responding" verdict can be
	// argued with rather than taken on faith.
	Probe string
	// Detail carries the probe's own words on failure, truncated.
	Detail string
}

var computeCapRe = regexp.MustCompile(`^\d+\.\d+$`)

// nvidiaControlNodes are the NVIDIA control devices worth opening, discrete
// first then Tegra. Opening one read-only is the cheapest question that only a
// live driver can answer: stat sees a node the kernel left behind, open(2) goes
// to the driver.
var nvidiaControlNodes = []string{
	"/dev/nvidiactl",
	"/dev/nvhost-ctrl-gpu",
	"/dev/nvgpu/igpu0/ctrl",
}

// ProbeGPUDriver reports whether the accelerator driver on this host is
// answering. Probe errors are reported as unknown; they are not evidence that
// the driver failed. Hardware discovery remains available when probes stall.
func ProbeGPUDriver(ctx context.Context) (GPUDriverHealth, bool) {
	if h, ok := probeNVIDIA(ctx); ok {
		return h, true
	}
	if h, ok := probeAMD(ctx); ok {
		return h, true
	}
	return GPUDriverHealth{}, false
}

func probeNVIDIA(ctx context.Context) (GPUDriverHealth, bool) {
	_, tegra := os.Stat("/etc/nv_tegra_release")
	nodes := existingNodes(nvidiaControlNodes)
	if tegra != nil && len(nodes) == 0 {
		return GPUDriverHealth{}, false
	}

	h := GPUDriverHealth{Vendor: "nvidia"}

	if smi, err := exec.LookPath("nvidia-smi"); err == nil {
		return gpuDriverProbes.run(ctx, "nvidia", "nvidia-smi", func(probeCtx context.Context) GPUDriverHealth {
			return probeNvidiaSMI(probeCtx, smi)
		}), true
	}

	// No nvidia-smi (the common Jetson case): ask the driver by opening its
	// control node. A node the driver has abandoned still stats fine but fails
	// to open, which is exactly the state a stat-based check cannot see.
	if len(nodes) == 0 {
		return GPUDriverHealth{Vendor: "nvidia", Status: driverStatusAbsent, Probe: "device nodes"}, true
	}
	h.Probe = "open " + nodes[0]
	return gpuDriverProbes.run(ctx, h.Vendor, h.Probe, func(context.Context) GPUDriverHealth {
		return probeControlNode(h.Vendor, nodes[0])
	}), true
}

func probeAMD(ctx context.Context) (GPUDriverHealth, bool) {
	const kfd = "/dev/kfd"
	if _, err := os.Stat(kfd); err != nil {
		return GPUDriverHealth{}, false
	}
	return gpuDriverProbes.run(ctx, "amd", "open "+kfd, func(context.Context) GPUDriverHealth {
		return probeControlNode("amd", kfd)
	}), true
}

func probeNvidiaSMI(ctx context.Context, smi string) GPUDriverHealth {
	h := GPUDriverHealth{Vendor: "nvidia", Probe: "nvidia-smi", Status: driverStatusUnknown}
	cmd := exec.CommandContext(ctx, smi, "--query-gpu=compute_cap", "--format=csv,noheader,nounits")
	cmd.WaitDelay = 100 * time.Millisecond
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
			out = exitErr.Stderr
		}
		h.Detail = probeDetail(err, out)
		return h
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, line := range lines {
		if !computeCapRe.MatchString(strings.TrimSpace(line)) {
			h.Detail = "unsupported or incomplete compute capability response: " + truncate(string(out))
			return h
		}
	}
	h.Status = driverStatusResponding
	h.Detail = "management query succeeded; CUDA execution not tested"
	return h
}

func probeControlNode(vendor, path string) GPUDriverHealth {
	h := GPUDriverHealth{Vendor: vendor, Probe: "open " + path, Status: driverStatusResponding}
	if err := openable(path); err != nil {
		h.Status = driverStatusUnknown
		if errors.Is(err, syscall.ENODEV) || errors.Is(err, syscall.ENXIO) || errors.Is(err, syscall.EIO) {
			h.Status = driverStatusNotResponding
		}
		h.Detail = truncate(err.Error())
	} else {
		h.Detail = "control device opened; CUDA execution not tested"
	}
	return h
}

// A driver open/close can block even with O_NONBLOCK, and a subprocess can be
// stuck in an uninterruptible kernel wait after cancellation. Bound the caller
// separately and allow at most one outstanding worker. A wedged worker retains
// its slot: later requests return unknown instead of accumulating stuck threads
// or child processes. The agent itself remains responsive.
type gpuProbeLimiter struct{ slots chan struct{} }

var gpuDriverProbes = gpuProbeLimiter{slots: make(chan struct{}, 1)}

func (l gpuProbeLimiter) run(ctx context.Context, vendor, probe string, fn func(context.Context) GPUDriverHealth) GPUDriverHealth {
	unknown := GPUDriverHealth{Vendor: vendor, Probe: probe, Status: driverStatusUnknown}
	ctx, cancel := context.WithTimeout(ctx, gpuProbeTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		unknown.Detail = err.Error()
		return unknown
	}
	select {
	case l.slots <- struct{}{}:
	default:
		unknown.Detail = "another GPU driver probe is still in progress"
		return unknown
	}
	result := make(chan GPUDriverHealth, 1)
	go func() {
		defer func() { <-l.slots }()
		result <- fn(ctx)
	}()
	select {
	case h := <-result:
		return h
	case <-ctx.Done():
		unknown.Detail = ctx.Err().Error()
		return unknown
	}
}

// O_NONBLOCK is a hint to the driver, not a deadline. Call through the limiter.
func openable(path string) error {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	return syscall.Close(fd)
}

func existingNodes(paths []string) []string {
	var out []string
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}

func probeDetail(err error, out []byte) string {
	if msg := strings.TrimSpace(string(out)); msg != "" {
		return truncate(msg)
	}
	if err != nil {
		return truncate(err.Error())
	}
	return ""
}

// truncate keeps a driver's own words without letting a verbose failure become
// the whole response.
func truncate(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	const max = 200
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
