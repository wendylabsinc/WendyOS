//go:build !windows

package services

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

type mountInfo struct {
	mountPoint string
	source     string
	filesystem string
	dedupeKey  string
}

func collectCPUInfo(ctx context.Context) *agentpb.GetSystemInfoResponse_CPUInfo {
	cpu := newBaseCPUInfo()

	switch runtime.GOOS {
	case "linux":
		if model := readLinuxCPUModel(); model != "" {
			cpu.ModelName = &model
		}
		if usage, ok := readLinuxCPUUsagePercent(ctx); ok {
			cpu.UsagePercent = &usage
		}
		if load, ok := readLinuxLoadAverage(); ok {
			cpu.LoadAverage_1M = &load[0]
			cpu.LoadAverage_5M = &load[1]
			cpu.LoadAverage_15M = &load[2]
		}
	case "darwin":
		if model := commandOutput(ctx, "sysctl", "-n", "machdep.cpu.brand_string"); model != "" {
			cpu.ModelName = &model
		}
	}

	return cpu
}

func collectMemoryInfo(ctx context.Context) *agentpb.GetSystemInfoResponse_MemoryInfo {
	switch runtime.GOOS {
	case "linux":
		if info, ok := readLinuxMemoryInfo("/proc/meminfo"); ok {
			return info
		}
	case "darwin":
		total, totalOK := parseUint(commandOutput(ctx, "sysctl", "-n", "hw.memsize"))
		if !totalOK {
			return &agentpb.GetSystemInfoResponse_MemoryInfo{}
		}
		available := readDarwinAvailableMemory(ctx)
		used := uint64(0)
		if total > available {
			used = total - available
		}
		return memoryInfo(total, used, available)
	}
	return &agentpb.GetSystemInfoResponse_MemoryInfo{}
}

func collectDiskInfo(ctx context.Context) []*agentpb.GetSystemInfoResponse_DiskInfo {
	mounts := []mountInfo{{mountPoint: "/", source: "/", filesystem: "", dedupeKey: "/"}}
	if runtime.GOOS == "linux" {
		if parsed, ok := readLinuxMountInfo("/proc/self/mountinfo"); ok && len(parsed) > 0 {
			mounts = parsed
		}
	}

	disks := make([]*agentpb.GetSystemInfoResponse_DiskInfo, 0, len(mounts))
	seen := make(map[string]struct{}, len(mounts))
	for _, mount := range mounts {
		if err := ctx.Err(); err != nil {
			break
		}
		key := mount.dedupeKey
		if key == "" {
			key = mount.mountPoint
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}

		disk, ok := statDisk(mount)
		if ok {
			disks = append(disks, disk)
		}
	}

	sort.Slice(disks, func(i, j int) bool {
		return disks[i].GetMountPoint() < disks[j].GetMountPoint()
	})
	return disks
}

func readLinuxCPUModel() string {
	file, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return ""
	}
	defer file.Close()

	fallback := ""
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(strings.ToLower(key))
		value = strings.TrimSpace(value)
		switch key {
		case "model name":
			return value
		case "hardware", "processor":
			if fallback == "" {
				fallback = value
			}
		}
	}
	return fallback
}

func readLinuxCPUUsagePercent(ctx context.Context) (float64, bool) {
	firstTotal, firstIdle, ok := readLinuxCPUStat()
	if !ok {
		return 0, false
	}

	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return 0, false
	case <-timer.C:
	}

	secondTotal, secondIdle, ok := readLinuxCPUStat()
	if !ok {
		return 0, false
	}
	totalDelta := secondTotal - firstTotal
	idleDelta := secondIdle - firstIdle
	if totalDelta == 0 || idleDelta > totalDelta {
		return 0, false
	}
	return (float64(totalDelta-idleDelta) / float64(totalDelta)) * 100, true
}

func readLinuxCPUStat() (total uint64, idle uint64, ok bool) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 5 {
			return 0, 0, false
		}
		values := make([]uint64, 0, len(fields)-1)
		for _, field := range fields[1:] {
			n, err := strconv.ParseUint(field, 10, 64)
			if err != nil {
				return 0, 0, false
			}
			values = append(values, n)
			total += n
		}
		idle = values[3]
		if len(values) > 4 {
			idle += values[4]
		}
		return total, idle, true
	}
	return 0, 0, false
}

func readLinuxLoadAverage() ([3]float64, bool) {
	var load [3]float64
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return load, false
	}
	fields := strings.Fields(string(data))
	if len(fields) < 3 {
		return load, false
	}
	for i := 0; i < 3; i++ {
		value, err := strconv.ParseFloat(fields[i], 64)
		if err != nil {
			return load, false
		}
		load[i] = value
	}
	return load, true
}

func readLinuxMemoryInfo(path string) (*agentpb.GetSystemInfoResponse_MemoryInfo, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	values := parseLinuxMemInfo(string(data))
	total := values["MemTotal"] * 1024
	available := values["MemAvailable"] * 1024
	if available == 0 {
		available = (values["MemFree"] + values["Buffers"] + values["Cached"]) * 1024
	}
	if total == 0 {
		return nil, false
	}
	used := uint64(0)
	if total > available {
		used = total - available
	}
	return memoryInfo(total, used, available), true
}

func parseLinuxMemInfo(data string) map[string]uint64 {
	values := make(map[string]uint64)
	scanner := bufio.NewScanner(strings.NewReader(data))
	for scanner.Scan() {
		key, rest, ok := strings.Cut(scanner.Text(), ":")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		value, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		values[key] = value
	}
	return values
}

func readDarwinAvailableMemory(ctx context.Context) uint64 {
	out := commandOutput(ctx, "vm_stat")
	if out == "" {
		return 0
	}

	pageSize := uint64(os.Getpagesize())
	pages := uint64(0)
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		line := scanner.Text()
		key, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "Pages free", "Pages inactive", "Pages speculative":
			value := strings.Trim(strings.TrimSpace(rest), ".")
			n, err := strconv.ParseUint(value, 10, 64)
			if err == nil {
				pages += n
			}
		}
	}
	return pages * pageSize
}

func memoryInfo(total, used, available uint64) *agentpb.GetSystemInfoResponse_MemoryInfo {
	usedPercent := 0.0
	if total > 0 {
		usedPercent = (float64(used) / float64(total)) * 100
	}
	return &agentpb.GetSystemInfoResponse_MemoryInfo{
		TotalBytes:     total,
		UsedBytes:      used,
		AvailableBytes: available,
		UsedPercent:    usedPercent,
	}
}

func readLinuxMountInfo(path string) ([]mountInfo, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	return parseLinuxMountInfo(string(data)), true
}

func parseLinuxMountInfo(data string) []mountInfo {
	var mounts []mountInfo
	scanner := bufio.NewScanner(strings.NewReader(data))
	for scanner.Scan() {
		line := scanner.Text()
		before, after, ok := strings.Cut(line, " - ")
		if !ok {
			continue
		}
		preFields := strings.Fields(before)
		postFields := strings.Fields(after)
		if len(preFields) < 5 || len(postFields) < 2 {
			continue
		}
		fsType := postFields[0]
		if shouldSkipFilesystem(fsType) {
			continue
		}
		source := postFields[1]
		mountPoint := unescapeMountPath(preFields[4])
		root := preFields[3]
		if mountPoint == "" {
			continue
		}
		mounts = append(mounts, mountInfo{
			mountPoint: mountPoint,
			source:     source,
			filesystem: fsType,
			dedupeKey:  source + "\x00" + root + "\x00" + fsType,
		})
	}
	return mounts
}

func shouldSkipFilesystem(fsType string) bool {
	switch fsType {
	case "autofs", "binfmt_misc", "bpf", "cgroup", "cgroup2", "configfs",
		"debugfs", "devpts", "devtmpfs", "efivarfs", "fusectl", "hugetlbfs",
		"mqueue", "proc", "pstore", "ramfs", "rpc_pipefs", "securityfs",
		"sysfs", "tmpfs", "tracefs":
		return true
	default:
		return false
	}
}

func unescapeMountPath(path string) string {
	replacer := strings.NewReplacer(
		`\\`, `\`,
		`\040`, " ",
		`\011`, "\t",
		`\012`, "\n",
		`\134`, `\`,
	)
	return replacer.Replace(path)
}

func statDisk(mount mountInfo) (*agentpb.GetSystemInfoResponse_DiskInfo, bool) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(mount.mountPoint, &stat); err != nil {
		return nil, false
	}
	blockSize := uint64(stat.Bsize)
	total := stat.Blocks * blockSize
	free := stat.Bfree * blockSize
	available := stat.Bavail * blockSize
	used := uint64(0)
	if total > free {
		used = total - free
	}
	usedPercent := 0.0
	if total > 0 {
		usedPercent = (float64(used) / float64(total)) * 100
	}
	return &agentpb.GetSystemInfoResponse_DiskInfo{
		MountPoint:     mount.mountPoint,
		Source:         mount.source,
		FilesystemType: mount.filesystem,
		TotalBytes:     total,
		UsedBytes:      used,
		FreeBytes:      free,
		AvailableBytes: available,
		UsedPercent:    usedPercent,
	}, total > 0
}

func commandOutput(ctx context.Context, name string, args ...string) string {
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func parseUint(value string) (uint64, bool) {
	n, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
	return n, err == nil
}
