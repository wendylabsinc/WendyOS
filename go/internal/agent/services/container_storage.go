package services

import "strings"

// discoverContainerStorage matches the path's device identity against mountinfo,
// including bind mounts whose mount root is a subdirectory of the filesystem.
// Prefer the filesystem's root mount so the result identifies the same partition
// as the ordinary partition list. All host inspection is supplied by the caller.
func discoverContainerStorage(mountinfo, path, deviceID string, usage func(string) (diskUsage, bool)) (partitionUsage, bool) {
	type mount struct {
		mountEntry
		id, root string
	}
	var mounts []mount
	var containing *mount
	for line := range strings.SplitSeq(mountinfo, "\n") {
		left, right, ok := strings.Cut(line, " - ")
		if !ok {
			continue
		}
		fields, fs := strings.Fields(left), strings.Fields(right)
		if len(fields) < 6 || len(fs) < 3 || fields[2] != deviceID {
			continue
		}
		m := mount{mountEntry{device: unescapeMountField(fs[1]), mountpoint: unescapeMountField(fields[4]), filesystem: fs[0]}, fields[2], unescapeMountField(fields[3])}
		mounts = append(mounts, m)
		if path == m.mountpoint || strings.HasPrefix(path, strings.TrimSuffix(m.mountpoint, "/")+"/") {
			if containing == nil || len(m.mountpoint) > len(containing.mountpoint) {
				copy := m
				containing = &copy
			}
		}
	}
	if containing == nil {
		return partitionUsage{}, false
	}
	chosen := *containing
	for _, m := range mounts {
		if m.root == "/" && (chosen.root != "/" || len(m.mountpoint) < len(chosen.mountpoint)) {
			chosen = m
		}
	}
	if !strings.HasPrefix(chosen.device, "/dev/") {
		return partitionUsage{}, false
	}
	u, ok := usage(path)
	if !ok {
		return partitionUsage{}, false
	}
	return partitionUsage{mountpoint: chosen.mountpoint, filesystem: chosen.filesystem, device: chosen.device, usedBytes: u.usedBytes, totalBytes: u.totalBytes}, true
}
