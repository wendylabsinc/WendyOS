package modelhub

import "strings"

// Hardware is a point-in-time snapshot. Zero capacity means unknown; available
// bytes may legitimately be zero when the corresponding total is known.
type Hardware struct {
	OS, Architecture, GPU, JetPack, Warning                        string
	MemoryTotal, MemoryAvailable, DiskFree, GPUTotal, GPUAvailable int64
	DiskKnown, CUDA, SharedMemory                                  bool
}

// Accelerator selects the managed runtime from capabilities, not memory size.
// Jetson shares system RAM and may not report a separate GPU memory total.
func (h Hardware) Accelerator() uint32 {
	if !h.CUDA {
		return 0
	}
	if strings.HasPrefix(h.JetPack, "5") {
		return 2
	}
	if strings.HasPrefix(h.JetPack, "6") {
		return 3
	}
	return 1
}

const GiB int64 = 1 << 30

// MemoryEstimate budgets weights plus 25% and 4 GiB for the runtime/KV cache at
// a 32,768-token context. This is a conservative heuristic, not a model benchmark.
func MemoryEstimate(weights int64) int64 { return weights + weights/4 + 4*GiB }
func (h Hardware) Budget(accelerator uint32) (int64, int64) {
	if accelerator != 0 && !h.SharedMemory {
		return h.GPUTotal, h.GPUAvailable
	}
	return h.MemoryTotal, h.MemoryAvailable
}

// Fit levels: 0 unknown, 1 comfortable estimate, 2 close to available capacity,
// 3 exceeds currently available memory/storage or physical memory.
func (h Hardware) Fit(weights int64, accelerator uint32, install bool) int {
	if weights <= 0 {
		return 0
	}
	disk := weights
	if install {
		disk += 12 * GiB
	}
	if h.DiskKnown && disk > h.DiskFree {
		return 3
	}
	total, available := h.Budget(accelerator)
	if total <= 0 {
		return 0
	}
	estimate := MemoryEstimate(weights)
	if estimate > total || estimate > available {
		return 3
	}
	if estimate > available-available/5 {
		return 2
	}
	if !h.DiskKnown {
		return 0
	}
	return 1
}
