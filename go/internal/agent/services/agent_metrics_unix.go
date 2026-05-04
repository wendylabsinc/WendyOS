//go:build !windows

package services

import (
	"bufio"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

func agentCPUNanos() (userNanos, sysNanos int64) {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		return 0, 0
	}
	userNanos = usage.Utime.Sec*1_000_000_000 + int64(usage.Utime.Usec)*1_000
	sysNanos = usage.Stime.Sec*1_000_000_000 + int64(usage.Stime.Usec)*1_000
	return
}

// agentMemBytes returns the resident set size of the process in bytes.
// On Linux it reads VmRSS from /proc/self/status; on other Unix systems
// it falls back to an approximation derived from the Go runtime.
func agentMemBytes() int64 {
	if f, err := os.Open("/proc/self/status"); err == nil {
		defer f.Close()
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "VmRSS:") {
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					if kb, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
						return kb * 1024
					}
				}
			}
		}
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return int64(ms.HeapInuse + ms.StackInuse)
}
