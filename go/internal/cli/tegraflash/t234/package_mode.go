//go:build darwin || linux || windows

package t234

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"

	backendfile "github.com/diskfs/go-diskfs/backend/file"
	"github.com/diskfs/go-diskfs/filesystem/ext4"
)

type USBMode string

const (
	USBModeLegacy               USBMode = "legacy"
	USBModeSingle               USBMode = "single"
	SingleEnumerationCapability         = "usb-single-enumeration-v1"
	usbModePath                         = "flashpkg/conf/usb-mode"
)

func readPackageUSBMode(image io.ReaderAt) (USBMode, error) {
	data, err := Ext4ReadFile(image, usbModePath)
	if errors.Is(err, ErrExt4FileNotFound) {
		return USBModeLegacy, nil
	}
	if err != nil {
		return "", fmt.Errorf("reading USB mode: %w", err)
	}
	switch string(data) {
	case "legacy", "legacy\n":
		return USBModeLegacy, nil
	case "single", "single\n":
		return USBModeSingle, nil
	default:
		return "", fmt.Errorf("unsupported command-package USB mode %q", data)
	}
}

func hasSingleEnumeration(capabilities []string) bool {
	for _, capability := range capabilities {
		if capability == SingleEnumerationCapability {
			return true
		}
	}
	return false
}

// prepareUSBMode leaves the integrity-checked cache intact. Only a capable
// initrd receives the opt-in, authored through go-diskfs's normal ext4 APIs.
// Already-selected packages require no editing.
func prepareUSBMode(sourcePath, tempDir string, supportsSingle bool) (string, USBMode, error) {
	source, err := os.Open(sourcePath)
	if err != nil {
		return "", "", err
	}
	defer source.Close()
	mode, err := readPackageUSBMode(source)
	if err != nil {
		return "", "", err
	}
	if mode == USBModeSingle {
		if !supportsSingle {
			return "", "", fmt.Errorf("command package selects single enumeration but the recovery initrd does not advertise %s", SingleEnumerationCapability)
		}
		return sourcePath, mode, nil
	}
	if !supportsSingle {
		return sourcePath, USBModeLegacy, nil
	}
	info, err := source.Stat()
	if err != nil {
		return "", "", err
	}
	if !info.Mode().IsRegular() {
		return "", "", fmt.Errorf("command package is not a regular image file")
	}
	// Offline editing requires a clean filesystem, with no journal replay.
	sb := make([]byte, 1024)
	if _, err := source.ReadAt(sb, ext4SuperOffset); err != nil {
		return "", "", err
	}
	if binary.LittleEndian.Uint16(sb[58:60]) != 1 || binary.LittleEndian.Uint32(sb[96:100])&4 != 0 {
		return "", "", fmt.Errorf("command-package ext4 filesystem needs recovery; refusing to edit it")
	}
	copy, err := os.CreateTemp(tempDir, "t234-commands-*.ext4")
	if err != nil {
		return "", "", err
	}
	copyPath := copy.Name()
	success := false
	defer func() {
		copy.Close()
		if !success {
			os.Remove(copyPath)
		}
	}()
	if _, err := io.Copy(copy, source); err != nil {
		return "", "", fmt.Errorf("copying command package: %w", err)
	}
	fs, err := ext4.Read(backendfile.New(copy, false), info.Size(), 0, 512)
	if err != nil {
		return "", "", fmt.Errorf("opening command-package ext4 for writing: %w", err)
	}
	file, err := fs.OpenFile(usbModePath, os.O_RDWR|os.O_CREATE)
	if err != nil {
		fs.Close()
		return "", "", fmt.Errorf("opening USB mode file: %w", err)
	}
	n, writeErr := file.Write([]byte("single\n"))
	closeErr := file.Close()
	fsErr := fs.Close()
	if writeErr != nil {
		return "", "", fmt.Errorf("writing USB mode: %w", writeErr)
	}
	if n != 7 {
		return "", "", io.ErrShortWrite
	}
	if closeErr != nil {
		return "", "", closeErr
	}
	if fsErr != nil {
		return "", "", fsErr
	}
	if err := copy.Sync(); err != nil {
		return "", "", err
	}
	got, err := readPackageUSBMode(copy)
	if err != nil || got != USBModeSingle {
		return "", "", fmt.Errorf("verifying prepared USB mode: got %q, error %v", got, err)
	}
	if err := copy.Close(); err != nil {
		return "", "", err
	}
	success = true
	return copyPath, USBModeSingle, nil
}
