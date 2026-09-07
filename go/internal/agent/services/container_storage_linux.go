//go:build linux

package services

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func containerStorageUsage() (partitionUsage, bool) {
	path, err := filepath.EvalSymlinks("/var/lib/containerd")
	if err != nil {
		return partitionUsage{}, false
	}
	var stat unix.Stat_t
	if err := unix.Stat(path, &stat); err != nil {
		return partitionUsage{}, false
	}
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return partitionUsage{}, false
	}
	id := fmt.Sprintf("%d:%d", unix.Major(uint64(stat.Dev)), unix.Minor(uint64(stat.Dev)))
	return discoverContainerStorage(string(data), path, id, statfsUsage)
}
