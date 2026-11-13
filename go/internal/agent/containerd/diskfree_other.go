//go:build !linux

package containerd

// containerdRootDir mirrors the Linux constant so PruneCache does not need a
// build tag of its own. The agent only ships for Linux; on other platforms
// (used by local tooling and tests) filesystemFreeBytes always reports the
// measurement as unavailable.
const containerdRootDir = "/var/lib/containerd"

// filesystemFreeBytes is unsupported outside Linux; ok is always false.
func filesystemFreeBytes(_ string) (uint64, bool) {
	return 0, false
}
