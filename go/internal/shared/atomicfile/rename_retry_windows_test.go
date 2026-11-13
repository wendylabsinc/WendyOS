//go:build windows

package atomicfile

import (
	"errors"
	"os"
	"testing"

	"golang.org/x/sys/windows"
)

func TestRetryableRenameErrMatchesTheTransientWindowsRefusals(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{windows.ERROR_ACCESS_DENIED, true},
		{windows.ERROR_SHARING_VIOLATION, true},
		{windows.ERROR_FILE_NOT_FOUND, true},
		{&os.LinkError{Op: "rename", Err: windows.ERROR_SHARING_VIOLATION}, true},
		{windows.ERROR_PATH_NOT_FOUND, false},
		{windows.ERROR_DIR_NOT_EMPTY, false},
		{errors.New("Access is denied."), false},
		{nil, false},
	} {
		if got := retryableRenameErr(tc.err); got != tc.want {
			t.Errorf("retryableRenameErr(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}
