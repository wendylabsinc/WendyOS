//go:build !darwin

package commands

// Spotlight preparation and compatibility marking are macOS-only. New images
// should carry markers from the builder regardless of the flashing host.
func markFATVolumes(_ drive) {}
