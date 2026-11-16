package hoststats

import (
	"context"
	"encoding/csv"
	"encoding/xml"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// GPUAdmission is conservative ownership evidence, separate from display metrics.
// A missing observation is never represented as an idle device.
type GPUAdmission struct {
	Index       uint32
	UUID        string
	Foreign     bool
	Supported   bool
	Unified     bool
	Utilization *float64
	FreeBytes   *uint64
}

type gpuInventory struct {
	GPUs []struct {
		UUID      string `xml:"uuid"`
		Display   string `xml:"display_active"`
		MIG       string `xml:"mig_mode>current_mig"`
		Free      string `xml:"fb_memory_usage>free"`
		Util      string `xml:"utilization>gpu_util"`
		Processes *struct {
			Text    string `xml:",chardata"`
			Entries []struct {
				PID string `xml:"pid"`
			} `xml:"process_info"`
		} `xml:"processes"`
	} `xml:"gpu"`
}

// ProcessCgroup reads host cgroup v2 identity. A vanished/unreadable process
// cannot be claimed as our workload.
func ProcessCgroup(pid uint32) (string, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "0::/") && line != "0::/" {
			return strings.TrimPrefix(line, "0::"), nil
		}
	}
	return "", fmt.Errorf("isolated cgroup v2 required")
}

// ReadGPUAdmission runs fixed, bounded NVIDIA queries. No caller-supplied command
// or filesystem path is accepted. Graphics and idle resident contexts are included.
func ReadGPUAdmission(ctx context.Context, owned []string) ([]GPUAdmission, error) {
	if len(owned) == 0 {
		return nil, fmt.Errorf("app cgroups unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	path, err := exec.LookPath("nvidia-smi")
	if err != nil {
		return nil, fmt.Errorf("NVIDIA inventory unavailable")
	}
	indices, err := runBounded(exec.CommandContext(ctx, path, "--query-gpu=index,uuid", "--format=csv,noheader,nounits"))
	if err != nil {
		return nil, fmt.Errorf("NVIDIA index query failed: %w", err)
	}
	inventory, err := runLimited(exec.CommandContext(ctx, path, "-q", "-x"), maxGPUInventoryOutput)
	if err != nil {
		return nil, fmt.Errorf("NVIDIA process query failed: %w", err)
	}
	model, _ := os.ReadFile("/proc/device-tree/model")
	unified := strings.Contains(string(model), "Jetson")
	return parseGPUAdmission(indices, inventory, owned, unified, ProcessCgroup)
}

// maxGPUInventoryOutput bounds `nvidia-smi -q -x`. Unlike the CSV queries, the
// full XML lists every supported clock pair: one RTX 4090 on driver 535 emits
// about 73 KiB, past maxGPUToolOutput, and every further GPU adds as much.
const maxGPUInventoryOutput = 4 << 20 // 4 MiB

// runLimited reads at most limit bytes of cmd's stdout. Unlike runBounded,
// output past the limit is an error rather than a prefix: a truncated XML
// document is no observation. The process is killed at once instead of being
// left blocked on a full pipe until the context deadline.
func runLimited(cmd *exec.Cmd, limit int64) ([]byte, error) {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(stdout, limit+1))
	if int64(len(data)) > limit {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("output exceeds %d bytes", limit)
	}
	waitErr := cmd.Wait()
	if readErr != nil {
		return nil, readErr
	}
	return data, waitErr
}

func parseGPUAdmission(indices, inventory []byte, owned []string, unified bool, cgroup func(uint32) (string, error)) ([]GPUAdmission, error) {
	rows, err := csv.NewReader(strings.NewReader(string(indices))).ReadAll()
	if err != nil || len(rows) == 0 {
		return nil, fmt.Errorf("invalid NVIDIA index inventory")
	}
	ids := map[string]uint32{}
	for _, row := range rows {
		if len(row) != 2 {
			return nil, fmt.Errorf("invalid NVIDIA index row")
		}
		n, err := strconv.ParseUint(strings.TrimSpace(row[0]), 10, 32)
		id := strings.TrimSpace(row[1])
		if err != nil || id == "" {
			return nil, fmt.Errorf("invalid NVIDIA identity")
		}
		if _, ok := ids[id]; ok {
			return nil, fmt.Errorf("duplicate NVIDIA identity")
		}
		ids[id] = uint32(n)
	}
	var parsed gpuInventory
	if err := xml.Unmarshal(inventory, &parsed); err != nil {
		return nil, fmt.Errorf("invalid NVIDIA XML: %w", err)
	}
	if len(parsed.GPUs) != len(ids) {
		return nil, fmt.Errorf("NVIDIA inventories disagree")
	}
	var result []GPUAdmission
	for _, gpu := range parsed.GPUs {
		index, ok := ids[gpu.UUID]
		if !ok {
			return nil, fmt.Errorf("NVIDIA inventories disagree")
		}
		delete(ids, gpu.UUID)
		if gpu.Processes == nil || strings.TrimSpace(gpu.Processes.Text) != "" {
			return nil, fmt.Errorf("GPU process inventory unavailable")
		}
		g := GPUAdmission{Index: index, UUID: gpu.UUID, Unified: unified,
			Supported: gpu.Display == "Disabled" && (gpu.MIG == "Disabled" || gpu.MIG == "N/A")}
		util := strings.TrimSpace(strings.TrimSuffix(gpu.Util, "%"))
		if n, err := strconv.ParseFloat(util, 64); err == nil && !math.IsNaN(n) && n >= 0 && n <= 100 {
			g.Utilization = &n
		}
		memory := strings.Fields(gpu.Free)
		if len(memory) == 2 && memory[1] == "MiB" {
			if n, err := strconv.ParseUint(memory[0], 10, 44); err == nil {
				v := n * 1024 * 1024
				g.FreeBytes = &v
			}
		}
		for _, proc := range gpu.Processes.Entries {
			pid, err := strconv.ParseUint(proc.PID, 10, 32)
			if err != nil || pid == 0 {
				return nil, fmt.Errorf("invalid GPU PID")
			}
			group, err := cgroup(uint32(pid))
			ours := false
			if err == nil {
				for _, own := range owned {
					if own != "" && own != "/" && (group == own || strings.HasPrefix(group, own+"/")) {
						ours = true
						break
					}
				}
			}
			if !ours {
				g.Foreign = true
			}
		}
		result = append(result, g)
	}
	return result, nil
}
