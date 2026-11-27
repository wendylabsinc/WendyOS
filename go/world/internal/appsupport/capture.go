package appsupport

import (
	"errors"
	"os"
	"syscall"
)

// OpenCapture confines symlink resolution to an already-open capture directory.
// Nonblocking open lets us reject FIFOs/devices before a read can block.
func OpenCapture(root *os.Root, name string) (*os.File, error) {
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !st.Mode().IsRegular() {
		f.Close()
		return nil, errors.New("capture input must be a regular file")
	}
	return f, nil
}
