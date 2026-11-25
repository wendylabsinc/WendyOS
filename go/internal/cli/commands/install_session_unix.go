//go:build !windows

package commands

import "os"

// openPromptTTY opens the controlling terminal, which still reaches the person
// when both stdout and stderr are piped.
func openPromptTTY() (*os.File, func(), bool) {
	f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return nil, nil, false
	}
	return f, func() { _ = f.Close() }, true
}
