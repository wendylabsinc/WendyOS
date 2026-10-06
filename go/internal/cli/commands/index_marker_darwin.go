//go:build darwin

package commands

import (
	"errors"
	"fmt"
	"path/filepath"
)

// markFATVolumesUnindexed writes the Spotlight marker to every FAT slice of d.
func markFATVolumesUnindexed(d drive) error {
	out, err := runWithTimeout(markerTimeout, "/usr/sbin/diskutil", "list", d.DevicePath)
	if err != nil {
		return fmt.Errorf("diskutil list %s: %w", d.DevicePath, err)
	}
	var errs []error
	for _, slice := range parseDiskutilSlices(string(out), d.DevicePath) {
		info, err := runWithTimeout(markerTimeout, "/usr/sbin/diskutil", "info", slice)
		if err != nil || parseBundleType(string(info)) != "msdos" {
			continue
		}
		if err := markDarwinVolume(slice); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", slice, err))
		}
	}
	return errors.Join(errs...)
}

func markDarwinVolume(slice string) error {
	info, err := runWithTimeout(markerTimeout, "/usr/sbin/diskutil", "info", slice)
	if err != nil {
		return err
	}
	mp := diskInfoField(string(info), "Mount Point:")
	if mp == "" || mp == "Not applicable" {
		if _, err := runWithTimeout(markerTimeout, "/usr/sbin/diskutil", "mount", slice); err != nil {
			return err
		}
		defer runWithTimeout(markerTimeout, "/usr/sbin/diskutil", "unmount", slice) //nolint:errcheck
		info, err = runWithTimeout(markerTimeout, "/usr/sbin/diskutil", "info", slice)
		if err != nil {
			return err
		}
		mp = diskInfoField(string(info), "Mount Point:")
	}
	if mp == "" || mp == "Not applicable" {
		return fmt.Errorf("%s has no mount point", slice)
	}
	if err := touchMarker(mp, false); err == nil {
		return nil
	}
	// The install has already authenticated sudo; do not prompt during cleanup.
	return touchMarker(mp, true)
}

// touchMarker creates the marker from a subprocess, so a wedged FAT driver
// costs a timeout rather than the install.
func touchMarker(dir string, elevated bool) error {
	p := filepath.Join(dir, spotlightMarker)
	if elevated {
		_, err := runWithTimeout(markerTimeout, "/usr/bin/sudo", "-n", "/usr/bin/touch", p)
		return err
	}
	_, err := runWithTimeout(markerTimeout, "/usr/bin/touch", p)
	return err
}
