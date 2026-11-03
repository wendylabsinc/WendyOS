package containerd

import (
	"github.com/wendylabsinc/wendy/go/internal/agent/services"
	"testing"
)

func TestReleaseIDsFollowEnrollmentProtocol(t *testing.T) {
	c := &Client{}
	uuid := "0b6f2c1e-8a3d-4c5e-9f00-1234567890ab"
	if !c.validReleaseID(uuid) || c.validReleaseID("123") {
		t.Fatal("default release validation changed")
	}
	c.SetCloudEnrollment(func() (services.CloudEnrollment, error) {
		return services.CloudEnrollment{Mode: services.CloudLegacy, OrganizationID: 64, AssetID: 674}, nil
	})
	if !c.validReleaseID("123") || c.validReleaseID(uuid) {
		t.Fatal("legacy release validation mixed protocols")
	}
	for _, bad := range []string{"0", "-1", "01", "2147483648", "+1", ""} {
		if c.validReleaseID(bad) {
			t.Fatalf("accepted %q", bad)
		}
	}
	c.SetCloudEnrollment(func() (services.CloudEnrollment, error) {
		return services.CloudEnrollment{Mode: services.CloudPKI}, nil
	})
	if !c.validReleaseID(uuid) || c.validReleaseID("123") {
		t.Fatal("re-enrollment kept legacy IDs")
	}
}
