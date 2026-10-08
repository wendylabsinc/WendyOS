package browserauth

import (
	"context"
	"crypto/x509"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	grpcmetadata "google.golang.org/grpc/metadata"
	"net/url"
	"testing"
)

func TestDeviceDiagnosticContextDoesNotForwardCredentials(t *testing.T) {
	source := grpcmetadata.NewOutgoingContext(context.Background(), grpcmetadata.Pairs(
		"traceparent", "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01",
		"x-correlation-id", "00699be8-ee8b-4e81-a4af-47969d52251f",
		"authorization", "Bearer secret", "dpop", "secret-proof", "baggage", "secret-data",
		"x-wendy-client-cert", "URI=forged",
	))
	ctx := deviceDiagnosticContext(context.Background(), source, "verified-principal")
	md, _ := grpcmetadata.FromOutgoingContext(ctx)
	if len(md) != 4 || md.Get("x-wendy-client-cert")[0] != "URI=verified-principal" || len(md.Get("traceparent")) != 1 || len(md.Get("x-correlation-id")) != 1 {
		t.Fatalf("unexpected diagnostic metadata keys: %v", md)
	}
	for _, key := range []string{"authorization", "dpop", "baggage"} {
		if len(md.Get(key)) != 0 {
			t.Fatalf("forwarded forbidden metadata %s", key)
		}
	}
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
	for _, name = range []string{"", "../device", "device/../other", "/device", "device/", "device?other"} {
		if _, err := cloudDeviceIdentity(record, assetID, tenant); err == nil {
			t.Errorf("accepted missing/invalid binding %q", name)
		}
	}
}
