//go:build !windows && !darwin && !linux

package vm

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
)

func validateHostSerialPort(string) error {
	return fmt.Errorf("VM serial passthrough is not supported on %s", runtime.GOOS)
}

func NormalizeSerialPort(port string) string { return strings.TrimSpace(port) }

func openHostSerial(SerialConfig) (io.ReadWriteCloser, error) {
	return nil, fmt.Errorf("VM serial passthrough is not supported on %s; remove the mapping with 'wendy vm serial NAME --clear'", runtime.GOOS)
}

func guardSerialProcess(*os.Process) (io.Closer, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
