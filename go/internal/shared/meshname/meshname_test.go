package meshname

import (
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// TestDevice covers the legacy v1 asset-ID form the CLI cloud-tunnel path
// still uses for pre-v2 assets.
func TestDevice(t *testing.T) {
	if got, want := Device(216), "device-216.mesh.wendy.internal"; got != want {
		t.Fatalf("Device(216) = %q, want %q", got, want)
	}
}

func TestDeviceFor(t *testing.T) {
	tenant, err := meshid.ParseTenant("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	device, err := meshid.ParseDevice(tenant, "00000001-4000-8000-0000-000000000001")
	if err != nil {
		t.Fatal(err)
	}
	want := "device-00000001-4000-8000-0000-000000000001.mesh.wendy.internal"
	if got := DeviceFor(device); got != want {
		t.Fatalf("DeviceFor = %q, want %q", got, want)
	}
}
