package containerd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	containerdclient "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/typeurl/v2"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"

	"github.com/wendylabsinc/wendy/go/internal/agent/dbusproxy"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

const serialByIDPattern = "/dev/serial/by-id/*"

var (
	globSerialByID = filepath.Glob
	evalSerialPath = filepath.EvalSymlinks
	statSerialPath = func(path string) (major, minor int64, err error) {
		info, err := os.Lstat(path)
		if err != nil {
			return 0, 0, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return 0, 0, fmt.Errorf("%s is a symlink; want resolved device node", path)
		}
		var stat syscall.Stat_t
		if err := syscall.Stat(path, &stat); err != nil {
			return 0, 0, err
		}
		return int64(unix.Major(uint64(stat.Rdev))), int64(unix.Minor(uint64(stat.Rdev))), nil
	}
)

func (c *Client) ensureDBusProxyForStart(ctx context.Context, containerName string, mounts []specs.Mount) (bool, error) {
	for _, mount := range mounts {
		if mount.Destination != "/var/run/dbus" {
			continue
		}
		expected := dbusproxy.SocketDir(containerName)
		if filepath.Clean(mount.Source) != expected {
			return false, fmt.Errorf("Bluetooth D-Bus mount source %q does not match scoped proxy %q", mount.Source, expected)
		}
		if c.proxyManager == nil {
			return false, fmt.Errorf("cannot start container %q: persisted Bluetooth mount requires xdg-dbus-proxy", containerName)
		}
		dir, err := c.proxyManager.Start(ctx, containerName)
		if err != nil {
			return false, fmt.Errorf("recreating D-Bus proxy for %q: %w", containerName, err)
		}
		if filepath.Clean(dir) != expected {
			_ = c.proxyManager.Stop(containerName)
			return false, fmt.Errorf("D-Bus proxy for %q returned %q, want %q", containerName, dir, expected)
		}
		return true, nil
	}
	return false, nil
}

func (c *Client) restoreDBusProxyForRunningTask(
	ctx context.Context,
	containerName string,
	running bool,
	mounts []specs.Mount,
) (bool, error) {
	if !running {
		return false, nil
	}
	return c.ensureDBusProxyForStart(ctx, containerName, mounts)
}

// captureSerialIdentities records stable by-id names at container creation.
// Missing by-id entries are left uncaptured: the exact tty mount continues to
// work while present, but it cannot distinguish a different physical device
// that later appears under the same tty name. A missing or incompatible node
// still fails clearly instead of broadening access beyond the declared class.
func captureSerialIdentities(entitlements []appconfig.Entitlement) map[string]string {
	paths, err := globSerialByID(serialByIDPattern)
	if err != nil || len(paths) == 0 {
		return nil
	}
	wanted := make(map[string]struct{})
	for _, entitlement := range entitlements {
		if entitlement.Type == appconfig.EntitlementSerial {
			wanted[entitlement.Device] = struct{}{}
		}
	}
	identities := make(map[string]string)
	for _, byIDPath := range paths {
		resolved, err := evalSerialPath(byIDPath)
		if err != nil {
			continue
		}
		device := filepath.Base(filepath.Clean(resolved))
		if _, ok := wanted[device]; !ok {
			continue
		}
		major, _, err := statSerialPath(resolved)
		wantMajor, ok := serialMajorForName(device)
		if err != nil || !ok || major != wantMajor {
			continue
		}
		identities[device] = filepath.Base(byIDPath)
	}
	if len(identities) == 0 {
		return nil
	}
	return identities
}

func decodeSerialIdentities(encoded string) (map[string]string, error) {
	if encoded == "" {
		return nil, nil
	}
	var identities map[string]string
	if err := json.Unmarshal([]byte(encoded), &identities); err != nil {
		return nil, err
	}
	return identities, nil
}

// refreshSerialMountsForStart keeps the container-visible tty path stable but
// re-resolves its host source and cgroup minor from the captured USB by-id name.
func refreshSerialMountsForStart(spec *specs.Spec, identities map[string]string) (bool, error) {
	changed := false
	for index := range spec.Mounts {
		mount := &spec.Mounts[index]
		device := filepath.Base(filepath.Clean(mount.Destination))
		wantMajor, isSerial := serialMajorForName(device)
		if !isSerial || filepath.Dir(filepath.Clean(mount.Destination)) != "/dev" {
			continue
		}

		identity := identities[device]
		if identity == "" {
			major, _, err := statSerialPath(mount.Source)
			if err != nil {
				return false, fmt.Errorf("serial mount %s is unavailable and has no stable by-id identity: %w", mount.Source, err)
			}
			if major != wantMajor {
				return false, fmt.Errorf("serial mount %s has major %d, want %d", mount.Source, major, wantMajor)
			}
			continue
		}
		if identity != filepath.Base(identity) || identity == "." || identity == string(filepath.Separator) {
			return false, fmt.Errorf("invalid serial by-id identity %q", identity)
		}
		byIDPath := filepath.Join(filepath.Dir(serialByIDPattern), identity)
		resolved, err := evalSerialPath(byIDPath)
		if err != nil {
			return false, fmt.Errorf("resolving serial identity %q: %w", identity, err)
		}
		resolved = filepath.Clean(resolved)
		if filepath.Dir(resolved) != "/dev" {
			return false, fmt.Errorf("serial identity %q resolved outside /dev: %s", identity, resolved)
		}
		resolvedDevice := filepath.Base(resolved)
		resolvedMajor, ok := serialMajorForName(resolvedDevice)
		if !ok || resolvedMajor != wantMajor {
			return false, fmt.Errorf("serial identity %q resolved to incompatible device %s", identity, resolved)
		}
		major, minor, err := statSerialPath(resolved)
		if err != nil {
			return false, fmt.Errorf("stat serial identity %q: %w", identity, err)
		}
		if major != wantMajor {
			return false, fmt.Errorf("serial identity %q has major %d, want %d", identity, major, wantMajor)
		}
		if mount.Source == resolved {
			continue
		}
		oldMinor, err := serialMinorFromName(filepath.Base(filepath.Clean(mount.Source)))
		if err != nil {
			return false, err
		}
		if err := replaceSerialCgroupMinor(spec, wantMajor, oldMinor, minor); err != nil {
			return false, err
		}
		mount.Source = resolved
		changed = true
	}
	return changed, nil
}

func serialMajorForName(device string) (int64, bool) {
	for prefix, major := range map[string]int64{"ttyACM": 166, "ttyUSB": 188} {
		if !strings.HasPrefix(device, prefix) {
			continue
		}
		if _, err := strconv.ParseUint(strings.TrimPrefix(device, prefix), 10, 32); err != nil {
			return 0, false
		}
		return major, true
	}
	return 0, false
}

func serialMinorFromName(device string) (int64, error) {
	for _, prefix := range []string{"ttyACM", "ttyUSB"} {
		if strings.HasPrefix(device, prefix) {
			minor, err := strconv.ParseInt(strings.TrimPrefix(device, prefix), 10, 64)
			if err != nil {
				return 0, fmt.Errorf("invalid serial device name %q", device)
			}
			return minor, nil
		}
	}
	return 0, fmt.Errorf("invalid serial device name %q", device)
}

func replaceSerialCgroupMinor(spec *specs.Spec, major, oldMinor, newMinor int64) error {
	if spec.Linux == nil || spec.Linux.Resources == nil {
		return errors.New("serial mount has no Linux cgroup device rules")
	}
	for index := range spec.Linux.Resources.Devices {
		rule := &spec.Linux.Resources.Devices[index]
		if rule.Allow && rule.Type == "c" && rule.Major != nil && rule.Minor != nil &&
			*rule.Major == major && *rule.Minor == oldMinor {
			minor := newMinor
			rule.Minor = &minor
			return nil
		}
	}
	return fmt.Errorf("serial cgroup rule %d:%d not found", major, oldMinor)
}

func withUpdatedContainerSpec(spec *specs.Spec) containerdclient.UpdateContainerOpts {
	return func(_ context.Context, _ *containerdclient.Client, container *containers.Container) error {
		encoded, err := typeurl.MarshalAnyToProto(spec)
		if err != nil {
			return err
		}
		container.Spec = encoded
		return nil
	}
}
