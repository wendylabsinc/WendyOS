package services

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/BurntSushi/toml"
)

// defaultBuildkitRoot is buildkitd's own rootful default for --root.
const defaultBuildkitRoot = "/var/lib/buildkit"

// defaultBuildkitConfigPath is where a rootful buildkitd looks for its config
// unless --config selects another file.
const defaultBuildkitConfigPath = "/etc/buildkit/buildkitd.toml"

type buildkitConfig struct {
	Root string `toml:"root"`
	GRPC struct {
		Address []string `toml:"address"`
	} `toml:"grpc"`
}

// buildkitRootLocation contains both the path an operator recognises and the
// path the agent must stat. The latter enters the selected daemon's mount
// namespace through /proc: /data inside a container is not necessarily /data
// in the agent's namespace.
type buildkitRootLocation struct {
	displayPath  string
	statPath     string
	statBoundary string
}

// buildkitRoot identifies the one running buildkitd whose effective address
// matches buildkitAddress and returns its effective state directory.
//
// Address matching matters because Docker/buildx and an administrator can run
// additional buildkitd processes on the same host. Selecting the first process
// from /proc can measure an unrelated cache and incorrectly allow the daemon
// the agent actually builds through to fill the root filesystem. More than one
// match, or any daemon whose address cannot be determined, is treated as
// ambiguous rather than guessed.
func buildkitRoot(procDir, buildkitAddress, configPath string) (buildkitRootLocation, bool) {
	entries, err := os.ReadDir(procDir)
	if err != nil {
		return buildkitRootLocation{}, false
	}

	var matches []buildkitRootLocation
	addressUnknown := false
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pidDir := filepath.Join(procDir, entry.Name())
		raw, err := os.ReadFile(filepath.Join(pidDir, "cmdline"))
		if err != nil || !isBuildkitDaemon(raw) {
			continue
		}

		addresses, location, addressKnown, rootKnown := inspectBuildkitDaemon(pidDir, raw, configPath)
		if !addressKnown {
			// This process may be the daemon behind the requested socket. Do not
			// let a different, inspectable process turn that uncertainty into a
			// confident answer.
			addressUnknown = true
			continue
		}
		matchesAddress, addressMatchKnown := daemonMatchesBuildkitAddress(pidDir, addresses, buildkitAddress)
		if !addressMatchKnown {
			addressUnknown = true
			continue
		}
		if !matchesAddress {
			continue
		}
		if !rootKnown {
			return buildkitRootLocation{}, false
		}
		matches = append(matches, location)
	}

	if addressUnknown || len(matches) != 1 {
		return buildkitRootLocation{}, false
	}
	return matches[0], true
}

// inspectBuildkitDaemon reconstructs the effective address and root using the
// same precedence as buildkitd: explicit flags, selected TOML, then defaults.
// Address and root knowledge are separate so an unrelated daemon with an
// explicit address can be ignored even if its config has disappeared.
func inspectBuildkitDaemon(pidDir string, raw []byte, defaultConfigPath string) (
	addresses []string,
	location buildkitRootLocation,
	addressKnown bool,
	rootKnown bool,
) {
	rootFlag, rootSet := procFlagValue(raw, "--root")
	addressFlags := procFlagValues(raw, "--addr")
	configFlag, configSet := procFlagValue(raw, "--config")

	var cfg buildkitConfig
	if !rootSet || len(addressFlags) == 0 {
		selectedConfig := defaultConfigPath
		if configSet {
			selectedConfig = configFlag
		}
		err := decodeBuildkitConfig(pathInProcess(pidDir, selectedConfig), &cfg)
		if err != nil {
			// An absent default config is normal. A selected custom config, or a
			// malformed/unreadable default, means values not overridden by flags
			// can no longer be inferred from the running process.
			if configSet || !errors.Is(err, os.ErrNotExist) {
				if len(addressFlags) > 0 {
					addresses, addressKnown = addressFlags, true
				}
				if rootSet {
					location, rootKnown = daemonRootLocation(pidDir, rootFlag)
				}
				return addresses, location, addressKnown, rootKnown
			}
		}
	}

	if len(addressFlags) > 0 {
		addresses = addressFlags
	} else if len(cfg.GRPC.Address) > 0 {
		addresses = cfg.GRPC.Address
	} else {
		addresses = []string{DefaultBuildkitAddress}
	}
	addressKnown = true

	root := rootFlag
	if !rootSet {
		root = cfg.Root
		if root == "" {
			root = defaultBuildkitRoot
		}
	}
	location, rootKnown = daemonRootLocation(pidDir, root)
	return addresses, location, addressKnown, rootKnown
}

func isBuildkitDaemon(raw []byte) bool {
	if len(raw) == 0 {
		return false
	}
	argv0 := string(raw)
	if i := strings.IndexByte(argv0, 0); i >= 0 {
		argv0 = argv0[:i]
	}
	return filepath.Base(argv0) == "buildkitd"
}

// daemonRootLocation resolves relative roots exactly where buildkitd does:
// against that process's working directory. statPath deliberately traverses
// /proc/<pid>/root or /proc/<pid>/cwd so Statfs sees the daemon's mount
// namespace, not merely a same-named path in the agent's namespace.
func daemonRootLocation(pidDir, root string) (buildkitRootLocation, bool) {
	if root == "" {
		return buildkitRootLocation{}, false
	}
	if filepath.IsAbs(root) {
		return buildkitRootLocation{
			displayPath:  filepath.Clean(root),
			statPath:     pathInProcess(pidDir, root),
			statBoundary: filepath.Join(pidDir, "root"),
		}, true
	}

	cwd, err := os.Readlink(filepath.Join(pidDir, "cwd"))
	if err != nil || !filepath.IsAbs(cwd) {
		return buildkitRootLocation{}, false
	}
	return buildkitRootLocation{
		displayPath:  filepath.Clean(filepath.Join(cwd, root)),
		statPath:     filepath.Join(pidDir, "cwd", root),
		statBoundary: filepath.Join(pidDir, "cwd"),
	}, true
}

// pathInProcess maps a buildkitd path into that process's filesystem view.
func pathInProcess(pidDir, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Join(pidDir, "root", strings.TrimPrefix(filepath.Clean(path), string(filepath.Separator)))
	}
	return filepath.Join(pidDir, "cwd", path)
}

func daemonMatchesBuildkitAddress(pidDir string, addresses []string, want string) (matches, known bool) {
	hasInheritedFD := false
	for _, address := range addresses {
		if sameBuildkitAddress(address, want) {
			if socketPath, unixAddress := strings.CutPrefix(strings.TrimSpace(want), "unix://"); unixAddress {
				return daemonSharesBuildkitSocket(pidDir, socketPath)
			}
			return true, true
		}
		if strings.TrimSpace(address) == "fd://" {
			hasInheritedFD = true
		}
	}
	if !hasInheritedFD {
		return false, true
	}

	socketPath, unixAddress := strings.CutPrefix(strings.TrimSpace(want), "unix://")
	if !unixAddress {
		// An inherited descriptor may be a TCP listener, but /proc/net/unix
		// cannot prove or disprove that association.
		return false, false
	}
	if same, known := daemonSharesBuildkitSocket(pidDir, socketPath); !same || !known {
		return same, known
	}
	matches, err := daemonOwnsUnixSocket(pidDir, socketPath)
	if err != nil {
		return false, false
	}
	return matches, true
}

// daemonSharesBuildkitSocket distinguishes identical socket paths in separate
// containers. A pathname alone does not identify the socket the agent uses.
func daemonSharesBuildkitSocket(pidDir, socketPath string) (same, known bool) {
	// In the same mount namespace and root, an absolute path identifies the
	// same object. Checking both also handles a daemon running in a chroot.
	selfNS, selfErr := os.Readlink(filepath.Join(filepath.Dir(pidDir), "self", "ns", "mnt"))
	daemonNS, daemonErr := os.Readlink(filepath.Join(pidDir, "ns", "mnt"))
	if filepath.IsAbs(socketPath) && selfErr == nil && daemonErr == nil && selfNS == daemonNS {
		selfRoot, selfErr := os.Stat("/")
		daemonRoot, daemonErr := os.Stat(filepath.Join(pidDir, "root"))
		if selfErr == nil && daemonErr == nil && os.SameFile(selfRoot, daemonRoot) {
			return true, true
		}
	}

	// Different namespaces can still share a socket through a bind mount.
	// Compare filesystem identity through the daemon's root, not path text.
	agentSocket, err := os.Stat(socketPath)
	if err != nil {
		return false, false
	}
	daemonSocket, err := os.Stat(pathInProcess(pidDir, socketPath))
	if err != nil {
		return false, false
	}
	return os.SameFile(agentSocket, daemonSocket), true
}

// daemonOwnsUnixSocket maps a filesystem Unix socket through /proc/net/unix to
// its kernel socket inode, then confirms that the daemon holds that inode. This
// is how a systemd-activated buildkitd using --addr fd:// is associated with the
// unix:/// address the Wendy agent uses.
func daemonOwnsUnixSocket(pidDir, socketPath string) (bool, error) {
	raw, err := os.ReadFile(filepath.Join(pidDir, "net", "unix"))
	if err != nil {
		return false, err
	}
	wantedInodes := make(map[string]struct{})
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 8 || filepath.Clean(fields[7]) != filepath.Clean(socketPath) {
			continue
		}
		wantedInodes[fields[6]] = struct{}{}
	}
	if len(wantedInodes) == 0 {
		return false, nil
	}

	fds, err := os.ReadDir(filepath.Join(pidDir, "fd"))
	if err != nil {
		return false, err
	}
	for _, fd := range fds {
		target, err := os.Readlink(filepath.Join(pidDir, "fd", fd.Name()))
		if err != nil {
			continue
		}
		inode, ok := strings.CutSuffix(strings.TrimPrefix(target, "socket:["), "]")
		if !ok {
			continue
		}
		if _, ok := wantedInodes[inode]; ok {
			return true, nil
		}
	}
	return false, nil
}

func sameBuildkitAddress(a, b string) bool {
	aPath, aUnix := strings.CutPrefix(strings.TrimSpace(a), "unix://")
	bPath, bUnix := strings.CutPrefix(strings.TrimSpace(b), "unix://")
	if aUnix || bUnix {
		return aUnix && bUnix && filepath.Clean(aPath) == filepath.Clean(bPath)
	}
	return strings.TrimSpace(a) == strings.TrimSpace(b)
}

// procFlagValue returns the last occurrence, matching CLI flag semantics when
// a scalar flag is repeated. Both --name value and --name=value are accepted.
func procFlagValue(raw []byte, name string) (string, bool) {
	values := procFlagValues(raw, name)
	if len(values) == 0 {
		return "", false
	}
	return values[len(values)-1], true
}

func procFlagValues(raw []byte, name string) []string {
	argv := strings.Split(string(raw), "\x00")
	var values []string
	for i := 1; i < len(argv); i++ {
		if argv[i] == name && i+1 < len(argv) {
			values = append(values, strings.TrimSpace(argv[i+1]))
			i++
			continue
		}
		if value, ok := strings.CutPrefix(argv[i], name+"="); ok {
			values = append(values, strings.TrimSpace(value))
		}
	}
	return values
}

func decodeBuildkitConfig(path string, cfg *buildkitConfig) error {
	_, err := toml.DecodeFile(path, cfg)
	return err
}

// rootFromConfig is the small parser seam used by focused TOML tests.
func rootFromConfig(path string) (string, error) {
	var cfg buildkitConfig
	if err := decodeBuildkitConfig(path, &cfg); err != nil {
		return "", err
	}
	return cfg.Root, nil
}

// buildkitRootSpace reports total and available bytes for the filesystem
// holding path, walking up to the nearest existing ancestor.
func buildkitRootSpace(path string) (total, free uint64) {
	return buildkitRootSpaceWithin(path, "")
}

// buildkitRootSpaceWithin is buildkitRootSpace with a floor. The floor keeps a
// vanished process from making a failed /proc/<pid>/root lookup walk upward and
// accidentally report the proc filesystem as BuildKit's cache filesystem.
func buildkitRootSpaceWithin(path, boundary string) (total, free uint64) {
	if path == "" {
		return 0, 0
	}
	boundary = filepath.Clean(boundary)
	for p := filepath.Clean(path); ; p = filepath.Dir(p) {
		var st syscall.Statfs_t
		if err := syscall.Statfs(p, &st); err == nil {
			bs := uint64(st.Bsize)
			if bs == 0 {
				return 0, 0
			}
			return st.Blocks * bs, st.Bavail * bs
		}
		if (boundary != "." && p == boundary) || p == "/" || p == "." {
			return 0, 0
		}
	}
}
