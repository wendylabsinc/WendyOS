//go:build !darwin && !linux

package rcm

import "fmt"

// Device is a stub on platforms where direct Jetson recovery USB access is not
// implemented.
type Device struct{}

func WaitForDevice() (*Device, error) {
	return nil, fmt.Errorf("Jetson USB recovery flashing is only supported on macOS and Linux")
}

func WaitForNv3p() (*Device, error) {
	return nil, fmt.Errorf("Jetson USB recovery flashing is only supported on macOS and Linux")
}

func (d *Device) String() string { return "" }
func (d *Device) Close()         {}
func (d *Device) Read([]byte) (int, error) {
	return 0, fmt.Errorf("Jetson USB recovery flashing is only supported on macOS and Linux")
}
func (d *Device) Write([]byte) error {
	return fmt.Errorf("Jetson USB recovery flashing is only supported on macOS and Linux")
}
func (d *Device) ReadUID() ([]byte, error) {
	return nil, fmt.Errorf("Jetson USB recovery flashing is only supported on macOS and Linux")
}
func (d *Device) LoadApplet([]byte) error {
	return fmt.Errorf("Jetson USB recovery flashing is only supported on macOS and Linux")
}
