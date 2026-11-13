package providers

import (
	"context"
	"crypto/tls"
	"slices"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/liteclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

func TestLegacyLiteDiscoveryPreservesEnrollment(t *testing.T) {
	device := (&MicroWendyProvider{}).mdnsExternalDevice(discovery.MDNSService{
		Hostname: "board.local", IPAddress: "192.0.2.1", Port: 5054,
		TXTRecords: map[string]string{"id": "020000000001", "mtls": "true", "orgid": "2", "assetid": "609"},
	})
	identity, err := legacyLiteIdentity(device)
	if err != nil || identity == nil || identity.orgID != 2 || identity.assetID != 609 {
		t.Fatalf("discovered legacy identity = %+v, %v", identity, err)
	}
	if device.ConnectionInfo["deviceId"] != "020000000001" {
		t.Fatal("legacy Cloud asset ID replaced the board's hardware ID")
	}
}

func TestLegacyLiteIdentitySelection(t *testing.T) {
	for _, tc := range []struct {
		name, tenant, org, asset string
		legacy, invalid          bool
	}{
		{name: "legacy enrollment", org: "2", asset: "609", legacy: true},
		{name: "PKI enrollment takes precedence", tenant: localLiteTenant, org: "2", asset: "609"},
		{name: "missing hints keeps PKI path"},
		{name: "missing organization", asset: "609", invalid: true},
		{name: "missing asset", org: "2", invalid: true},
		{name: "negative organization", org: "-2", asset: "609", invalid: true},
		{name: "zero asset", org: "2", asset: "0", invalid: true},
		{name: "invalid asset", org: "2", asset: "board", invalid: true},
		{name: "overflow", org: "2147483648", asset: "609", invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			identity, err := legacyLiteIdentity(models.ExternalDevice{ConnectionInfo: map[string]string{
				"tenantId": tc.tenant, "orgId": tc.org, "assetId": tc.asset,
			}})
			if (err != nil) != tc.invalid || (identity != nil) != tc.legacy {
				t.Fatalf("identity = %+v, err = %v", identity, err)
			}
		})
	}
}

func TestLegacyLiteSelectsMatchingUserCertificate(t *testing.T) {
	wrongOrg := localLiteTestCertificate(t, "urn:wendy:org:77:user:42", false)
	wrongOrg.OrganizationID = 2 // Metadata cannot override the signed identity.
	primary := localLiteTestCertificate(t, localLiteOperator, true)
	device := localLiteTestCertificate(t, "urn:wendy:org:2:asset:609", false)
	user := localLiteTestCertificate(t, "urn:wendy:org:2:user:42", false)
	localLiteTestSessions(t,
		config.AuthConfig{Name: "wrong-org", Certificates: []config.CertificateInfo{wrongOrg}},
		config.AuthConfig{Name: "tenant", Certificates: []config.CertificateInfo{primary}},
		config.AuthConfig{Name: "device", Certificates: []config.CertificateInfo{device}},
		config.AuthConfig{Name: "org-2", Certificates: []config.CertificateInfo{user}},
	)
	var selected []string
	LiteIdentityResolver = func(_ context.Context, auth *config.AuthConfig) (config.CertificateInfo, string, error) {
		selected = append(selected, auth.Name)
		return auth.Certificates[0], "device trust", nil
	}
	err := connectWithLegacyCLIIdentities(context.Background(), 2, func(_ tls.Certificate, trust string, _ liteclient.LiteDeviceCAChain) error {
		if trust != "device trust" {
			t.Fatal("lost the selected device trust")
		}
		return nil
	})
	if err != nil || !slices.Equal(selected, []string{"org-2"}) {
		t.Fatalf("selected contexts = %v, err = %v", selected, err)
	}
}
