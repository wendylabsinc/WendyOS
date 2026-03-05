//go:build windows

package commands

import "fmt"

// drive represents an external disk suitable for image writing.
type drive struct {
	DevicePath  string
	RawPath     string
	Name        string
	Size        string
	IsRemovable bool
}

// listExternalDrives is not yet implemented on Windows.
func listExternalDrives() ([]drive, error) {
	return nil, fmt.Errorf("OS image writing is not yet supported on Windows")
}

// writeImageToDisk is not yet implemented on Windows.
func writeImageToDisk(imagePath string, d drive, progressFn func(written int64)) error {
	return fmt.Errorf("OS image writing is not yet supported on Windows")
}
