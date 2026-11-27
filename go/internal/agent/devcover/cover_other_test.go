//go:build !linux

package devcover

import "testing"

func TestOpen_UnsupportedOffLinux(t *testing.T) {
	if _, err := Open(1); err != ErrUnsupported {
		t.Fatalf("Open = %v, want ErrUnsupported", err)
	}
}
