//go:build darwin

package commands

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/shared/wendyconf"
)

// configPartitionSupported reports whether writeConfigPartition has a working
// implementation on this OS. Callers gate the agent download + config write
// on this so non-supported platforms don't pay the network cost just to fail.
const configPartitionSupported = true

// writeConfigPartition finds, mounts, and populates the FAT32 config partition
// on d after a dd write. agentBinary is the arm64 agent binary content. creds
// and deviceName are written to wendy.conf when non-empty.
func writeConfigPartition(d drive, agentBinary []byte, creds []wendyconf.WifiCredential, deviceName string, provisioningJSON []byte) error {
	partDev, err := findConfigPartition(d.DevicePath)
	if err != nil {
		return fmt.Errorf("locating config partition on %s: %w", d.DevicePath, err)
	}

	m, err := mountConfigPartition(partDev)
	if err != nil {
		return fmt.Errorf("mounting config partition %s: %w", partDev, err)
	}
	defer m.release()

	var target configTarget = dirTarget(m.path)
	if m.elevated {
		target = sudoDirTarget(m.path)
	}
	return writeConfigFilesTo(target, agentBinary, creds, deviceName, provisioningJSON)
}

// darwinPartitionRe matches the slice device nodes diskutil reports for
// partitions (e.g. disk4s2, without the /dev/ prefix). findConfigPartition
// only returns device paths matching it, so everything downstream — diskutil
// mount/unmount and the elevated writes — operates on a well-formed /dev node.
var darwinPartitionRe = regexp.MustCompile(`^disk[0-9]+s[0-9]+$`)

// findConfigPartition runs `diskutil list <diskDev>` (which also rescans the
// partition table after dd) and returns the device node for the partition
// labelled "config".
func findConfigPartition(diskDev string) (string, error) {
	out, err := exec.Command("diskutil", "list", diskDev).Output()
	if err != nil {
		return "", fmt.Errorf("diskutil list %s: %w", diskDev, err)
	}

	// diskutil list output contains lines like:
	//    2:  Microsoft Basic Data  config      67.1 MB    disk4s2
	// We look for a field equal to "config" and take the last field as the
	// partition device (without the /dev/ prefix).
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		for i, f := range fields {
			if strings.EqualFold(f, "config") && i > 0 {
				last := fields[len(fields)-1]
				if darwinPartitionRe.MatchString(last) {
					return "/dev/" + last, nil
				}
			}
		}
	}
	return "", fmt.Errorf("config partition not found on %s (is the image fully written?)", diskDev)
}

// rescanPartitionTable is a no-op on macOS: the `diskutil list` in
// readDriveIdentity already rescans the partition table.
func rescanPartitionTable(_ drive) {}

// readDriveIdentity fingerprints d from what diskutil reports without
// elevation: the disk's size and media name, and the unique GUID of its config
// partition ("Disk / Partition UUID"). diskutil does not report the GPT disk
// GUID, and reading it from the raw disk would need root.
func readDriveIdentity(d drive) (driveIdentity, error) {
	info, err := getDiskInfo(d.DevicePath)
	if err != nil {
		return driveIdentity{}, fmt.Errorf("diskutil info %s: %w", d.DevicePath, err)
	}
	partDev, err := findConfigPartition(d.DevicePath)
	if err != nil {
		return driveIdentity{}, err
	}
	out, err := exec.Command("diskutil", "info", partDev).Output()
	if err != nil {
		return driveIdentity{}, fmt.Errorf("diskutil info %s: %w", partDev, err)
	}
	return driveIdentity{
		SizeBytes:           info.sizeBytes,
		ConfigPartitionGUID: normalizeGUID(diskInfoField(string(out), "Disk / Partition UUID:")),
		Model:               info.name,
	}, nil
}

// configMount is a mounted config partition: where it lives, whether writing to
// it needs elevation, and how to let it go.
type configMount struct {
	path string
	// elevated is set when the mount is not writable by the invoking user.
	//
	// What decides it is per-volume ownership enforcement, not the bus or the
	// media type: `diskutil enableOwnership`/`disableOwnership` toggle it and it
	// persists in the OS volume database. It is off by default for plug-in disks
	// (so SD cards and most USB enclosures mount `noowners` and the caller owns
	// the view) and ON by default for built-in disks — which is why an internal
	// NVMe target hits it, as PR #1262 found.
	//
	// When enforced, a vfat volume's files come out owned by an unmapped uid
	// (-101/nobody, not root), so the unprivileged CLI cannot write — or even
	// stat — inside the mount. Measured on an SSD in a USB enclosure with
	// ownership enabled.
	elevated bool
	release  func()
}

// mountConfigPartition returns a writable view of the FAT32 config partition.
//
// macOS auto-mounts FAT volumes when the partition table is rescanned (by the
// `diskutil list` in findConfigPartition), so the usual case is that the volume
// is already mounted and — on removable media — already writable by us. We use
// that mount as-is.
//
// Do NOT reintroduce `mount_msdos` here. It loads the legacy msdosfs kext
// (`kmutil load -p /System/Library/Extensions/msdosfs.kext`) while
// DiskArbitration serves the same volume through FSKit (`com.apple.fskit.msdos`),
// and it can only take a device nothing else holds — so it required tearing
// down the auto-mount first, which is how WendyOS#1562 produced
// "mount_msdos: /dev/diskNsM: Resource busy: exit status 71" and silently
// dropped wendy.conf. `diskutil mount` needs no elevation, no kext, and no
// teardown.
func mountConfigPartition(partDev string) (configMount, error) {
	// Already mounted by DiskArbitration: use it where it stands.
	if mp, err := currentMountPoint(partDev); err != nil {
		return configMount{}, err
	} else if mp != "" {
		return configMount{
			path:     mp,
			elevated: !userWritable(mp),
			// Not ours to unmount — we did not mount it, and os install ejects
			// the whole disk once provisioning is done.
			release: func() {},
		}, nil
	}

	// Not mounted (the volume was ejected, or auto-mount is disabled). Mount it
	// ourselves at a private mount point, via the same FSKit path.
	tmpDir, err := os.MkdirTemp("", "wendyos-config-*")
	if err != nil {
		return configMount{}, fmt.Errorf("creating temp mount dir: %w", err)
	}
	out, err := exec.Command("diskutil", "mount", "-mountPoint", tmpDir, partDev).CombinedOutput()
	if err != nil {
		os.RemoveAll(tmpDir) //nolint:errcheck
		return configMount{}, fmt.Errorf("diskutil mount %s: %s: %w", partDev, strings.TrimSpace(string(out)), err)
	}

	return configMount{
		path:     tmpDir,
		elevated: !userWritable(tmpDir),
		release: func() {
			if out, err := exec.Command("diskutil", "unmount", tmpDir).CombinedOutput(); err != nil {
				// Leave tmpDir alone: with the volume still mounted there,
				// RemoveAll would delete the partition's contents, not the
				// mount point. ejectDisk tears the mount down later anyway.
				fmt.Printf("Warning: could not unmount config partition at %s: %s: %v\n", tmpDir, strings.TrimSpace(string(out)), err)
				return
			}
			if err := os.RemoveAll(tmpDir); err != nil {
				fmt.Printf("Warning: could not remove temporary mount dir %s: %v\n", tmpDir, err)
			}
		},
	}, nil
}

// currentMountPoint reports where partDev is mounted, or "" if it is not.
func currentMountPoint(partDev string) (string, error) {
	out, err := exec.Command("/sbin/mount").Output()
	if err != nil {
		return "", fmt.Errorf("listing mounts: %w", err)
	}
	return parseMountPoint(string(out), partDev), nil
}

// userWritable reports whether the invoking user can create files in dir.
// Probing beats inspecting st_uid: vfat synthesizes ownership from mount
// options, so the mode bits alone do not say whether a write will land.
func userWritable(dir string) bool {
	f, err := os.CreateTemp(dir, ".wendy-write-probe-*")
	if err != nil {
		return false
	}
	name := f.Name()
	f.Close() //nolint:errcheck
	if err := os.Remove(name); err != nil {
		fmt.Printf("Warning: could not remove write probe %s: %v\n", name, err)
	}
	return true
}

// sudoDirTarget writes into a config-partition mount the invoking user does not
// own — the fixed/SSD-backed case where macOS honours ownership and
// diskarbitrationd mounted the volume root-owned. Each file is staged as the
// user and copied into place with one elevated `cp`, rather than remounting the
// volume to change the ownership of the view.
//
// `wendy os install` has already cached a sudo timestamp via preAuthElevation
// (and keeps it warm with keepElevationAlive), so this does not add a password
// prompt. vfat carries no on-disk ownership, so the device applies its own uid
// when it boots — this only affects the host-side write.
type sudoDirTarget string

func (s sudoDirTarget) WriteFile(name string, data []byte, perm os.FileMode) error {
	// The cp below runs as root, so refuse any destination that could land
	// outside the config mount: name must be a bare file name (today the
	// callers only pass constants like "wendy.conf") and the mount point must
	// be an absolute path.
	if name != filepath.Base(name) || name == "." || name == ".." {
		return fmt.Errorf("config file name %q is not a bare file name", name)
	}
	if !filepath.IsAbs(string(s)) {
		return fmt.Errorf("config mount point %q is not an absolute path", string(s))
	}

	staged, err := os.CreateTemp("", "wendyos-cfg-*")
	if err != nil {
		return fmt.Errorf("staging %s: %w", name, err)
	}
	stagedPath := staged.Name()
	defer os.Remove(stagedPath) //nolint:errcheck

	if _, err := staged.Write(data); err != nil {
		staged.Close() //nolint:errcheck
		return fmt.Errorf("staging %s: %w", name, err)
	}
	if err := staged.Chmod(perm); err != nil {
		staged.Close() //nolint:errcheck
		return fmt.Errorf("staging %s: %w", name, err)
	}
	if err := staged.Close(); err != nil {
		return fmt.Errorf("staging %s: %w", name, err)
	}

	dst := filepath.Join(string(s), name)
	if out, err := exec.Command("sudo", "/bin/cp", stagedPath, dst).CombinedOutput(); err != nil {
		return fmt.Errorf("writing %s to config partition: %s: %w", name, strings.TrimSpace(string(out)), err)
	}
	// Best-effort: vfat has no permission bits to set, and the copy already
	// succeeded, so a chmod failure must not fail the write.
	exec.Command("sudo", "/bin/chmod", fmt.Sprintf("%o", perm.Perm()), dst).Run() //nolint:errcheck
	return nil
}
