package commands

import "os"

// A build on this computer that runs out of disk fails partway, after
// minutes of work, with whatever error the tool writing at that moment
// reports (WDY-4049). These floors apply to the disks a local build writes
// to, checked before it starts. A build host has its own, larger floors
// (buildkitRootMinFreeBytes), because there a full cache can fill the disk the
// device boots from.
const (
	// Below this, refuse: an image build, its exported copy and its cache
	// rarely fit.
	localBuildMinFreeBytes int64 = 3 << 30 // 3 GiB
	// Between the two, build but say so: small images fit, large ones may not.
	localBuildWarnFreeBytes int64 = 10 << 30 // 10 GiB
)

var (
	localBuildDiskAvail = diskAvailBytes
	// dockerDataRoot is Docker Engine's default data directory on Linux.
	dockerDataRoot = "/var/lib/docker"
)

const wendyCacheClearStep = "Run `wendy cache clear` to delete Wendy's cached builds and downloads."

// checkLocalBuildSpace refuses, or warns about, a build on this computer when
// a disk it writes to is nearly full. dirs are the directories the build
// writes under, and the one with the least free space decides. A directory
// whose free space cannot be read is skipped: not knowing is no reason to stop
// a build that may well fit. steps are the fixes a refusal offers.
func checkLocalBuildSpace(dirs, steps []string) error {
	fullest, free, known := "", int64(0), false
	for _, dir := range dirs {
		if avail, ok := localBuildDiskAvail(dir); ok && (!known || avail < free) {
			fullest, free, known = dir, avail, true
		}
	}
	switch {
	case !known:
		return nil
	case free < localBuildMinFreeBytes:
		return withNextSteps(commandErrorf(errDiskSpaceLow,
			"not enough free disk space to build: %s free on the disk holding %s, and a build needs at least %s",
			humanBytes(uint64(free)), fullest, humanBytes(uint64(localBuildMinFreeBytes))), steps...)
	case free < localBuildWarnFreeBytes:
		cliNotice("Warning: only %s free on the disk holding %s; a large build may run out of space.", humanBytes(uint64(free)), fullest)
	}
	return nil
}

// imageBuildSpaceDirs returns where an image build on this computer writes:
// Wendy's cache, which holds the exported image and the build cache, and on
// Linux Docker's data directory, often a partition of its own. On macOS and
// Windows Docker keeps its data in a virtual disk under the user's home, so
// the cache directory's disk already covers it.
func imageBuildSpaceDirs() []string {
	var dirs []string
	if cache, err := os.UserCacheDir(); err == nil {
		dirs = append(dirs, cache)
	}
	if imageBuilderHostGOOS() == "linux" {
		if _, err := os.Stat(dockerDataRoot); err == nil {
			dirs = append(dirs, dockerDataRoot)
		}
	}
	return dirs
}

// toolchainBuildSpaceDirs returns where a Swift or Xcode build on this
// computer writes: its output goes into the project, and toolchain caches
// under the user's home.
func toolchainBuildSpaceDirs(projectDir string) []string {
	dirs := []string{projectDir}
	if cache, err := os.UserCacheDir(); err == nil {
		dirs = append(dirs, cache)
	}
	return dirs
}

// imageBuildSpaceSteps returns the fixes offered when an image build is
// refused for disk space: pruning Docker when Docker does the build, clearing
// Wendy's cache, and building on another device where the project allows it.
func imageBuildSpaceSteps(builder string, buildHost bool) []string {
	var steps []string
	if b, err := resolveOCIExportBuilder(builder); err != nil || b == imageBuilderDocker {
		steps = append(steps, "Run `docker system prune` to delete Docker data nothing uses: stopped containers, dangling images and build cache.")
	}
	steps = append(steps, wendyCacheClearStep)
	if buildHost {
		steps = append(steps, "Or build on another WendyOS device: wendy run --build-host")
	}
	return steps
}
