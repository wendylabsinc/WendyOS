//go:build (!darwin && !linux) || ios

package rcm

import "fmt"

// ListRecoveryDevices is unsupported off macOS/Linux.
func ListRecoveryDevices() ([]RecoveryDevice, error) {
	return nil, fmt.Errorf("Jetson USB recovery flashing is not supported on this platform")
}

// WaitForDeviceAt is unsupported off macOS/Linux.
func WaitForDeviceAt(string, uint16) (*Device, error) {
	return nil, fmt.Errorf("Jetson USB recovery flashing is not supported on this platform")
}
