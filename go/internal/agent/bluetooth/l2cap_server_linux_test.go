//go:build linux

package bluetooth

import (
	"errors"
	"fmt"
	"testing"

	"golang.org/x/sys/unix"
)

func TestL2CAPRetriesPreemptionWithoutEndingSession(t *testing.T) {
	calls := 0
	n, err := retryL2CAPInterrupted(func() (int, error) {
		calls++
		if calls <= 3 {
			return -1, fmt.Errorf("socket: %w", unix.EINTR)
		}
		return 42, nil
	})
	if n != 42 || err != nil || calls != 4 {
		t.Fatalf("result = %d, %v after %d calls", n, err, calls)
	}
}

func TestL2CAPDoesNotRetryDisconnectOrTimeout(t *testing.T) {
	for _, failure := range []error{unix.ECONNABORTED, unix.ECONNRESET, unix.EAGAIN} {
		calls := 0
		n, err := retryL2CAPInterrupted(func() (int, error) {
			calls++
			return -1, failure
		})
		if n != -1 || !errors.Is(err, failure) || calls != 1 {
			t.Fatalf("%v: result = %d, %v after %d calls", failure, n, err, calls)
		}
	}
}
