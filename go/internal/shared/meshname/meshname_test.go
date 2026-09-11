package meshname

import "testing"

func TestDevice(t *testing.T) {
	if got, want := Device(216), "device-216.mesh.wendy.internal"; got != want {
		t.Fatalf("Device(216) = %q, want %q", got, want)
	}
}
