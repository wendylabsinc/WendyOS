// Package hoststats reads host-level CPU, memory, and GPU utilization on Linux
// for the `wendy device top` command. CPU and memory come from /proc; GPU comes
// from tegrastats or nvidia-smi (see gpu.go). All counters are reported raw and
// cumulative — callers compute rates from deltas.
package hoststats

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// CPUSample is a snapshot of cumulative host CPU jiffies from /proc/stat.
type CPUSample struct {
	TotalJiffies uint64
	IdleJiffies  uint64
	CPUCount     uint32
}

// MemSample is a snapshot of host memory from /proc/meminfo.
type MemSample struct {
	TotalBytes     int64
	AvailableBytes int64
}

// ParseProcStat parses /proc/stat contents into a CPUSample. TotalJiffies is the
// sum of user through steal on the aggregate "cpu " line. Guest counters are
// already included in user/nice and must not be counted twice. IdleJiffies is
// non-executing time: idle + iowait + steal (fields 4, 5, and 8). This keeps time
// waiting for the hypervisor out of CPU utilization. CPUCount is the number of
// per-core "cpuN" lines.
func ParseProcStat(data []byte) (CPUSample, error) {
	var s CPUSample
	sc := bufio.NewScanner(bytes.NewReader(data))
	foundAgg := false
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 {
			continue
		}
		switch {
		case fields[0] == "cpu":
			foundAgg = true
			for i := 1; i < len(fields); i++ {
				v, err := strconv.ParseUint(fields[i], 10, 64)
				if err != nil {
					return CPUSample{}, fmt.Errorf("parsing /proc/stat cpu field %d: %w", i, err)
				}
				if i <= 8 { // guest and guest_nice overlap user and nice
					s.TotalJiffies += v
				}
				if i == 4 || i == 5 || i == 8 { // idle, iowait, steal
					s.IdleJiffies += v
				}
			}
		case strings.HasPrefix(fields[0], "cpu"):
			s.CPUCount++
		}
	}
	if err := sc.Err(); err != nil {
		return CPUSample{}, err
	}
	if !foundAgg {
		return CPUSample{}, fmt.Errorf("no aggregate cpu line in /proc/stat")
	}
	return s, nil
}

// ParseMemInfo parses /proc/meminfo contents (values are in kB) into a MemSample.
func ParseMemInfo(data []byte) (MemSample, error) {
	var s MemSample
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			s.TotalBytes = kb * 1024
		case "MemAvailable:":
			s.AvailableBytes = kb * 1024
		}
	}
	if err := sc.Err(); err != nil {
		return MemSample{}, err
	}
	return s, nil
}

// ReadCPU reads and parses /proc/stat.
func ReadCPU() (CPUSample, error) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return CPUSample{}, err
	}
	return ParseProcStat(data)
}

// ReadMemory reads and parses /proc/meminfo.
func ReadMemory() (MemSample, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return MemSample{}, err
	}
	return ParseMemInfo(data)
}
