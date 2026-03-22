//go:build darwin

package commands

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func writeOSInstallPayloadToImage(imagePath string, payloadDir string) error {
	baseDevice, partitions, err := darwinAttachRawImage(imagePath)
	if err != nil {
		return err
	}
	defer darwinDetachDevice(baseDevice)

	partition, err := darwinFindPayloadPartition(partitions)
	if err != nil {
		return err
	}

	mountPoint, err := darwinMountPartition(partition)
	if err != nil {
		return err
	}
	defer darwinUnmountPartition(partition)

	dst := filepath.Join(mountPoint, osInstallPayloadDirName)
	if err := os.RemoveAll(dst); err != nil {
		return fmt.Errorf("clearing existing payload directory: %w", err)
	}
	if err := copyDir(payloadDir, dst); err != nil {
		return fmt.Errorf("copying payload to image: %w", err)
	}
	return nil
}

func darwinAttachRawImage(imagePath string) (string, []string, error) {
	out, err := exec.Command("hdiutil", "attach", "-nomount", imagePath).CombinedOutput()
	if err != nil {
		return "", nil, fmt.Errorf("attaching image: %s: %w", strings.TrimSpace(string(out)), err)
	}

	var base string
	var parts []string
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 || !strings.HasPrefix(fields[0], "/dev/disk") {
			continue
		}
		if base == "" {
			base = fields[0]
			continue
		}
		parts = append(parts, fields[0])
	}
	if base == "" {
		return "", nil, fmt.Errorf("unable to determine mounted device for %s", imagePath)
	}
	return base, parts, nil
}

func darwinDetachDevice(baseDevice string) {
	exec.Command("hdiutil", "detach", baseDevice).Run() //nolint:errcheck
}

func darwinFindPayloadPartition(partitions []string) (string, error) {
	for _, part := range partitions {
		out, err := exec.Command("diskutil", "info", part).CombinedOutput()
		if err != nil {
			continue
		}
		text := strings.ToLower(string(out))
		if strings.Contains(text, "file system personality:  ms-dos") ||
			strings.Contains(text, "type (bundle):  msdos") ||
			strings.Contains(text, "volume name:               efi") {
			return part, nil
		}
	}
	return "", fmt.Errorf("no FAT/EFI partition found in image")
}

func darwinMountPartition(partition string) (string, error) {
	if out, err := exec.Command("diskutil", "mount", partition).CombinedOutput(); err != nil {
		return "", fmt.Errorf("mounting %s: %s: %w", partition, strings.TrimSpace(string(out)), err)
	}

	out, err := exec.Command("diskutil", "info", partition).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("reading mount info for %s: %s: %w", partition, strings.TrimSpace(string(out)), err)
	}
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "Mount Point:") {
			mountPoint := strings.TrimSpace(strings.TrimPrefix(line, "Mount Point:"))
			if mountPoint != "" && mountPoint != "Not mounted" {
				return mountPoint, nil
			}
		}
	}
	return "", fmt.Errorf("could not determine mount point for %s", partition)
}

func darwinUnmountPartition(partition string) {
	exec.Command("diskutil", "unmount", partition).Run() //nolint:errcheck
}
