//go:build linux

package t234

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Match mountinfo's device numbers rather than source names, which can use
// aliases. Limit the operation to this disk and its sysfs partition children.
func unmountLinuxDisk(devPath, sysfsRoot string, mounts io.Reader, unmount func(target, device string) error) error {
	name := filepath.Base(devPath)
	disk := filepath.Join(sysfsRoot, name)
	partitions, err := filepath.Glob(filepath.Join(disk, name+"*", "dev"))
	if err != nil {
		return err
	}
	devices := make(map[string]bool)
	for _, path := range append(partitions, filepath.Join(disk, "dev")) {
		dev, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("identifying %s mounts: %w", devPath, err)
		}
		devices[strings.TrimSpace(string(dev))] = true
	}
	type diskMount struct{ target, device string }
	var targets []diskMount
	decode := strings.NewReplacer("\\040", " ", "\\011", "\t", "\\012", "\n", "\\134", "\\")
	scanner := bufio.NewScanner(mounts)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 6 {
			return fmt.Errorf("invalid mountinfo entry")
		}
		if devices[fields[2]] {
			targets = append(targets, diskMount{decode.Replace(fields[4]), fields[2]})
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("reading mounts: %w", err)
	}
	// Child mounts and bind mounts can share a device; unmount deeper paths first.
	sort.SliceStable(targets, func(i, j int) bool { return len(targets[i].target) > len(targets[j].target) })
	for _, mount := range targets {
		if err := unmount(mount.target, mount.device); err != nil {
			return fmt.Errorf("unmounting %s from %s: %w", devPath, mount.target, err)
		}
	}
	return nil
}
