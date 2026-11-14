package main

import (
	"testing"

	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

type advertisedDevice struct {
	port      int
	principal string
}

func recordAdvertisement(into *[]advertisedDevice) func(*zap.Logger, int, string) {
	return func(_ *zap.Logger, port int, principal string) {
		*into = append(*into, advertisedDevice{port: port, principal: principal})
	}
}

// A device enrolled with a legacy numeric-organization certificate serves only
// mTLS, like any enrolled device. Its leaf has no tenant principal, so there is
// no Wendy Net identity to advertise, but the advertisement must still point at
// the mTLS port. Left alone, a file in the image's default form (or one an OS
// update reset) keeps announcing the unencrypted setup port, which an enrolled
// agent does not serve, and the device looks unenrolled to everything that
// discovers it.
func TestLegacyEnrolledDeviceAdvertisesTheMTLSPortWithoutAPrincipal(t *testing.T) {
	var published []advertisedDevice
	advertiseProvisionedDevice(zap.NewNop(), 50052, meshid.Device{}, recordAdvertisement(&published))

	if len(published) != 1 {
		t.Fatalf("advertisement updated %d times, want once: %+v", len(published), published)
	}
	if got := published[0]; got.port != 50052 || got.principal != "" {
		t.Fatalf("advertised %+v, want the mTLS port 50052 with no principal", got)
	}
}

func TestTenantEnrolledDeviceAdvertisesItsPrincipal(t *testing.T) {
	const principal = "spiffe://wendy.sh/tenant/2f6a8a10-1111-4aaa-8bbb-000000000001/device/3fa85f64-5717-4562-b3fc-2c963f66afa6"
	device, err := meshid.DeviceFromPrincipal(principal)
	if err != nil {
		t.Fatalf("DeviceFromPrincipal: %v", err)
	}
	var published []advertisedDevice
	advertiseProvisionedDevice(zap.NewNop(), 50052, device, recordAdvertisement(&published))

	if len(published) != 1 {
		t.Fatalf("advertisement updated %d times, want once: %+v", len(published), published)
	}
	if got := published[0]; got.port != 50052 || got.principal != principal {
		t.Fatalf("advertised %+v, want port 50052 with principal %s", got, principal)
	}
}
