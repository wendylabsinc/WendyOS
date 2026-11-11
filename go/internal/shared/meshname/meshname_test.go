package meshname

import (
	"strings"
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
	want := "device-00000001-4000-8000-0000-000000000001.wendy.internal"
	if got := DeviceFor(device); got != want {
		t.Fatalf("DeviceFor = %q, want %q", got, want)
	}
}

func TestFriendlyPreservesLabelsAndReservesIdentityNames(t *testing.T) {
	for _, name := range []string{"jetson", "Jetson--A", "a"} {
		host, ok := Friendly(name)
		if !ok || host != strings.ToLower(name)+".wendy.internal" {
			t.Fatalf("Friendly(%q) = %q,%v", name, host, ok)
		}
	}
	for _, name := range []string{"", "jetson.local", " jetson", "1jetson", "jetson-", "jetson_a", "Kelvin", strings.Repeat("a", 64), "device-00000001-4000-8000-0000-000000000001"} {
		if host, ok := Friendly(name); ok {
			t.Fatalf("accepted %q as %q", name, host)
		}
	}
}
