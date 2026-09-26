package browserauth

import (
	"crypto/x509"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"net/url"
	"testing"
)

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
	for _, name = range []string{"", "../device", "device/../other", "/device", "device/", "device?other"} {
		if _, err := cloudDeviceIdentity(record, assetID, tenant); err == nil {
			t.Errorf("accepted missing/invalid binding %q", name)
		}
	}
}
