//go:build linux

package commands

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type imageLSBLKOutput struct {
	Blockdevices []imageLSBLKDevice `json:"blockdevices"`
}

type imageLSBLKDevice struct {
	Path     string             `json:"path"`
	FSType   string             `json:"fstype"`
	Children []imageLSBLKDevice `json:"children"`
}

func writeOSInstallPayloadToImage(imagePath string, payloadDir string) error {
	loopDevice, err := linuxAttachImage(imagePath)
	if err != nil {
		return err
	}
	defer linuxDetachImage(loopDevice)

	partition, err := linuxFindPayloadPartition(loopDevice)
	if err != nil {
		return err
	}

	mountDir, err := os.MkdirTemp("", "wendy-image-mount-*")
	if err != nil {
		return fmt.Errorf("creating image mount dir: %w", err)
	}
	defer os.RemoveAll(mountDir)

	if err := linuxMountPartition(partition, mountDir); err != nil {
		return err
	}
	defer linuxUnmountPartition(mountDir)

	dst := filepath.Join(mountDir, osInstallPayloadDirName)
	if err := os.RemoveAll(dst); err != nil {
		return fmt.Errorf("clearing existing payload directory: %w", err)
	}
	if err := copyDir(payloadDir, dst); err != nil {
		return fmt.Errorf("copying payload to image: %w", err)
	}

	return nil
}

func linuxAttachImage(imagePath string) (string, error) {
	out, err := exec.Command("sudo", "losetup", "--find", "--partscan", "--show", imagePath).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("attaching image loop device: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return strings.TrimSpace(string(out)), nil
}

func linuxDetachImage(loopDevice string) {
	exec.Command("sudo", "losetup", "-d", loopDevice).Run() //nolint:errcheck
}

func linuxFindPayloadPartition(loopDevice string) (string, error) {
	out, err := exec.Command("lsblk", "--json", "-o", "PATH,FSTYPE", loopDevice).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("inspecting image partitions: %s: %w", strings.TrimSpace(string(out)), err)
	}

	var data imageLSBLKOutput
	if err := json.Unmarshal(out, &data); err != nil {
		return "", fmt.Errorf("parsing lsblk output: %w", err)
	}
	for _, device := range data.Blockdevices {
		for _, child := range device.Children {
			if strings.EqualFold(child.FSType, "vfat") {
				return child.Path, nil
			}
		}
	}
	return "", fmt.Errorf("no FAT/EFI partition found in image")
}

func linuxMountPartition(partition string, mountDir string) error {
	opts := fmt.Sprintf("uid=%d,gid=%d,umask=022", os.Getuid(), os.Getgid())
	out, err := exec.Command("sudo", "mount", "-o", opts, partition, mountDir).CombinedOutput()
	if err != nil {
		return fmt.Errorf("mounting %s: %s: %w", partition, strings.TrimSpace(string(out)), err)
	}
	return nil
}

func linuxUnmountPartition(mountDir string) {
	exec.Command("sudo", "umount", mountDir).Run() //nolint:errcheck
}
