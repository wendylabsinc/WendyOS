//go:build !windows

package vm

import (
	"fmt"
	"io"
	"os"
	"strings"
)

func openHostSerial(SerialConfig) (io.ReadWriteCloser, error) {
	return nil, fmt.Errorf("managed VM serial passthrough requires a Windows host; remove the mapping with 'wendy vm serial NAME --clear'")
}

func guardSerialProcess(*os.Process) (io.Closer, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
