//go:build linux

package services

import (
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

const efiImmutableFlag = 0x10 // FS_IMMUTABLE_FL, set by efivarfs for vendor variables.

// writeJetsonEFIVariable submits attributes and value in one write. O_TRUNC and
// remove/recreate are intentionally avoided: deleting an EFI variable is a
// separate firmware operation, not a prerequisite for changing its value.
func writeJetsonEFIVariable(path string, data []byte) (resultErr error) {
	existing, err := os.Open(path)
	if err == nil {
		defer existing.Close()
		flags, err := unix.IoctlGetInt(int(existing.Fd()), unix.FS_IOC_GETFLAGS)
		if err != nil && !errors.Is(err, unix.ENOTTY) && !errors.Is(err, unix.EOPNOTSUPP) {
			return fmt.Errorf("read EFI inode flags: %w", err)
		}
		if err == nil && flags&efiImmutableFlag != 0 {
			if err := unix.IoctlSetPointerInt(int(existing.Fd()), unix.FS_IOC_SETFLAGS, flags & ^efiImmutableFlag); err != nil {
				return fmt.Errorf("clear EFI immutable flag: %w", err)
			}
			defer func() {
				if err := unix.IoctlSetPointerInt(int(existing.Fd()), unix.FS_IOC_SETFLAGS, flags); err != nil {
					resultErr = errors.Join(resultErr, fmt.Errorf("restore EFI immutable flag: %w", err))
				}
			}()
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	n, writeErr := f.Write(data)
	if writeErr == nil && n != len(data) {
		writeErr = io.ErrShortWrite
	}
	return errors.Join(writeErr, f.Close())
}
