package browserauth

import (
	"context"
	"crypto/x509"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"net"
	"net/url"
	"testing"
	"time"
)

func TestSlowDeviceConnectionDoesNotLockSessionProfile(t *testing.T) {
	s, _ := setup(t, nil)
	if _, err := s.Complete(context.Background(), "valid-code", s.state, testIssuer); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	dialing := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, _ := s.ConnectDevice(ctx, "00000000-0000-4000-8000-000000000001", func(ctx context.Context, _ string) (net.Conn, error) {
			select {
			case dialing <- struct{}{}:
			default:
			}
			<-ctx.Done()
			return nil, ctx.Err()
		}, nil)
		if conn != nil {
			conn.Close()
		}
	}()
	select {
	case <-dialing:
	case <-done:
		t.Fatal("connection did not attempt its authenticated Cloud lookup")
	case <-ctx.Done():
		t.Fatal("connection did not begin")
	}
	profile := make(chan Profile, 1)
	go func() { profile <- s.Profile() }()
	select {
	case got := <-profile:
		if got.Tenant == "" {
			t.Fatal("lost authenticated profile")
		}
	case <-time.After(time.Second):
		t.Fatal("slow device locked unrelated session operations")
	}
	cancel()
	<-done
}

func TestCloudDeviceIdentityUsesEnrollmentBinding(t *testing.T) {
	const tenant = "8a53be77-2a69-464f-8f73-83643fe0beaa"
	const assetID = "00699be8-ee8b-4e81-a4af-47969d52251f"
	name := "fleet/box-01"
	record := &cloudpbv2.Asset{Id: assetID, OrganizationId: tenant, PkiDeviceName: &name}
	want, err := cloudDeviceIdentity(record, assetID, tenant)
	if err != nil {
		t.Fatal(err)
	}
	if want.EntityType != certs.EntityAsset || want.EntityID != name || want.TenantUUID != tenant {
		t.Fatalf("incomplete identity: %+v", want)
	}
	for _, test := range []struct {
		principal string
		match     bool
	}{
		{"spiffe://wendy.sh/tenant/" + tenant + "/device/" + name, true},
		{"spiffe://wendy.sh/tenant/" + tenant + "/device/" + assetID, false},
		{"spiffe://wendy.sh/tenant/" + tenant + "/device/other", false},
		{"spiffe://wendy.sh/tenant/00000000-0000-4000-8000-000000000001/device/" + name, false},
	} {
		uri, _ := url.Parse(test.principal)
		got, found, err := certs.IdentityFromCert(&x509.Certificate{URIs: []*url.URL{uri}})
		if err != nil || !found {
			t.Fatalf("parse certificate: %v", err)
		}
		if got.SameEntity(*want) != test.match {
			t.Errorf("unexpected acceptance for %s", test.principal)
		}
	}
	record.OrganizationId = "00000000-0000-4000-8000-000000000001"
	if _, err := cloudDeviceIdentity(record, assetID, tenant); err == nil {
		t.Fatal("accepted another tenant")
	}
	record.OrganizationId = tenant
	for _, name = range []string{"../device", "device/../other", "/device", "device/", "device?other"} {
		if _, err := cloudDeviceIdentity(record, assetID, tenant); err == nil {
			t.Errorf("accepted missing/invalid binding %q", name)
		}
	}
}

// After cloud #873 the asset id is the device UUID and pki_device_name is empty.
func TestCloudDeviceIdentityUsesTheAssetIDWithoutPKIDeviceName(t *testing.T) {
	const tenant = "8a53be77-2a69-464f-8f73-83643fe0beaa"
	const assetID = "00699be8-ee8b-4e81-a4af-47969d52251f"
	for name, record := range map[string]*cloudpbv2.Asset{
		"post-873":   {Id: assetID, OrganizationId: tenant},
		"empty name": {Id: assetID, OrganizationId: tenant, PkiDeviceName: new(string)},
	} {
		got, err := cloudDeviceIdentity(record, assetID, tenant)
		if err != nil || got.EntityID != assetID || got.Principal != "spiffe://wendy.sh/tenant/"+tenant+"/device/"+assetID {
			t.Errorf("%s: identity %+v, %v; want the asset id", name, got, err)
		}
	}
}
