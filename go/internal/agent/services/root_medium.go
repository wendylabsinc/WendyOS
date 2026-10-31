package services

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"go.uber.org/zap"
)

// pickStorageMedium prefers the booted medium: the image's STORAGE value is
// missing on images that boot from several disks, and can be stale in /data.
func pickStorageMedium(booted, image string) string {
	if booted != "" {
		return booted
	}
	return image
}

func (s *AgentService) deviceTypeFile() string {
	if s.deviceTypePath != "" {
		return s.deviceTypePath
	}
	return deviceTypePath
}

// bootMedium probes once: the root disk cannot change without a reboot, and a
// reboot restarts the agent.
func (s *AgentService) bootMedium() string {
	s.rootMediumOnce.Do(func() {
		probe := s.rootMedium
		if probe == nil {
			probe = hostRootMedium
		}
		logger := s.logger
		if logger == nil {
			logger = zap.NewNop()
		}
		medium, disk, err := probe()
		if err != nil {
			logger.Warn("boot medium not detected; reporting the image's storage medium", zap.Error(err))
			return
		}
		logger.Info("boot medium", zap.String("disk", disk), zap.String("medium", medium))
		s.rootMediumName = medium
	})
	return s.rootMediumName
}

var (
	nvmeDiskName = regexp.MustCompile(`^nvme[0-9]+n[0-9]+$`)
	mmcDiskName  = regexp.MustCompile(`^mmcblk[0-9]+$`)
	scsiDiskName = regexp.MustCompile(`^sd[a-z]+$`)
	scsiHostName = regexp.MustCompile(`^host[0-9]+$`)
)

// maxRootStackDepth bounds the device-mapper walk in rootDiskFromSysfs.
const maxRootStackDepth = 8

// rootMediumFromSysfs returns the medium and kernel name of the disk behind id
// (MAJ:MIN). SATA, virtio and loop disks give "", which keeps the image's STORAGE.
func rootMediumFromSysfs(sysfs, id string) (medium, disk string, err error) {
	if root, err := filepath.EvalSymlinks(sysfs); err == nil {
		sysfs = root
	}
	dir, err := rootDiskFromSysfs(sysfs, id)
	if err != nil {
		return "", "", err
	}
	return classifyDisk(sysfs, dir), filepath.Base(dir), nil
}

// rootDiskFromSysfs resolves a device number to the sysfs directory of its
// whole disk, following device-mapper layers while each has one slave.
func rootDiskFromSysfs(sysfs, id string) (string, error) {
	entry := filepath.Join(sysfs, "dev", "block", id)
	for range maxRootStackDepth {
		dir, err := filepath.EvalSymlinks(entry)
		if err != nil {
			return "", fmt.Errorf("resolving block device %s: %w", id, err)
		}
		if _, err := os.Stat(filepath.Join(dir, "partition")); err == nil {
			return filepath.Dir(dir), nil
		}
		slaves, _ := os.ReadDir(filepath.Join(dir, "slaves"))
		switch len(slaves) {
		case 0:
			// A filesystem on the whole disk.
			return dir, nil
		case 1:
			entry = filepath.Join(dir, "slaves", slaves[0].Name())
		default:
			return "", fmt.Errorf("block device %s spans %d devices", id, len(slaves))
		}
	}
	return "", fmt.Errorf("block device %s: stack deeper than %d", id, maxRootStackDepth)
}

// classifyDisk names a disk's medium from kernel attributes, never from
// controller names: the Pi 4 SD slot sits on a node called emmc2.
func classifyDisk(sysfs, dir string) string {
	name := filepath.Base(dir)
	switch {
	case nvmeDiskName.MatchString(name):
		return "nvme"
	case mmcDiskName.MatchString(name):
		switch readSysfsValue(filepath.Join(dir, "device", "type")) {
		case "MMC":
			return "emmc"
		case "SD", "SDcombo":
			return "sd"
		}
	case scsiDiskName.MatchString(name):
		// UFS and USB disks are both sdX; only their ancestors tell them apart.
		for d := filepath.Dir(dir); strings.HasPrefix(d, sysfs+string(filepath.Separator)); d = filepath.Dir(d) {
			if sysfsSubsystem(d) == "usb" {
				return "usb"
			}
			if host := filepath.Base(d); scsiHostName.MatchString(host) &&
				readSysfsValue(filepath.Join(d, "scsi_host", host, "proc_name")) == "ufshcd" {
				return "ufs"
			}
		}
	}
	return ""
}

func readSysfsValue(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

func sysfsSubsystem(dir string) string {
	target, err := filepath.EvalSymlinks(filepath.Join(dir, "subsystem"))
	if err != nil {
		return ""
	}
	return filepath.Base(target)
}
