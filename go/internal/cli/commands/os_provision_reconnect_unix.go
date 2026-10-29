//go:build darwin || linux

package commands

import (
	"errors"
	"os"
)

// listIdentifiedDrives fingerprints every drive the installer would offer as a
// target. A drive whose identity cannot be read — no config partition, or
// still enumerating — can never match, so it is left out.
func listIdentifiedDrives() ([]identifiedDrive, error) {
	drives, err := listAllDrives()
	if err != nil {
		return nil, err
	}
	var out []identifiedDrive
	for _, d := range drives {
		if id, err := readDriveIdentity(d); err == nil {
			out = append(out, identifiedDrive{drive: d, id: id})
		}
	}
	return out, nil
}

// drivePresent reports whether d's device node still exists. The node goes
// away when the drive drops off the bus.
func drivePresent(d drive) bool {
	_, err := os.Stat(d.DevicePath)
	return !errors.Is(err, os.ErrNotExist)
}
