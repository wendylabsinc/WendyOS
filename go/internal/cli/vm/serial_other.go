//go:build !windows && !darwin && !linux

package vm

import (
	"io"
	"os"
	"strings"
)

func validateHostSerialPort(string) error { return errSerialHostUnsupported }

func NormalizeSerialPort(port string) string { return strings.TrimSpace(port) }

// RunForeground adds the VM name and the --clear fix to this error.
func openHostSerial(SerialConfig) (io.ReadWriteCloser, error) {
	return nil, errSerialHostUnsupported
}

func guardSerialProcess(*os.Process) (io.Closer, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
