//go:build windows

package discovery

import "fmt"

// ResolveESP32SerialPort is not yet implemented on Windows.
func ResolveESP32SerialPort() (string, error) {
	return "", fmt.Errorf("ESP32 serial port detection is not yet supported on Windows")
}
