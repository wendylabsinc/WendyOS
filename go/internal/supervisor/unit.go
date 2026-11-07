//go:build linux

package supervisor

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// UnitProcs lists the live processes in this process's cgroup (v2), or nil
// when it cannot be read.
func UnitProcs() []int {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return nil
	}
	var group string
	for _, line := range strings.Split(string(data), "\n") {
		if path, ok := strings.CutPrefix(line, "0::"); ok {
			group = path
		}
	}
	if group == "" {
		return nil
	}
	f, err := os.Open(filepath.Join("/sys/fs/cgroup", group, "cgroup.procs"))
	if err != nil {
		return nil
	}
	defer f.Close()
	procs := []int{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if pid, err := strconv.Atoi(strings.TrimSpace(sc.Text())); err == nil && !zombie(pid) {
			procs = append(procs, pid)
		}
	}
	return procs
}

func zombie(pid int) bool {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return true
	}
	_, after, _ := strings.Cut(string(stat), ") ")
	return strings.HasPrefix(after, "Z")
}
