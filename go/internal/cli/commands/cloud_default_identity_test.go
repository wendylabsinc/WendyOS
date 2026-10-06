package commands

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/protobuf/proto"
)

func TestCloudDefaultIdentityMustMatchSavedPin(t *testing.T) {
	legacyAuth := &config.AuthConfig{Certificates: []config.CertificateInfo{{OrganizationID: 64}}}
	legacy, err := cloudDiscoveryIdentity(legacyAuth, cloudDiscoveryDevice{legacy: &cloudpb.Asset{}, key: "486"})
	if err != nil {
		t.Fatal(err)
	}
	matching := certs.WendyIdentity{OrgID: 64, EntityType: certs.EntityAsset, EntityID: "486"}
	otherAsset := certs.WendyIdentity{OrgID: 64, EntityType: certs.EntityAsset, EntityID: "566"}
	otherOrg := certs.WendyIdentity{OrgID: 2, EntityType: certs.EntityAsset, EntityID: "486"}
	for _, tc := range []struct {
		name     string
		expected *certs.WendyIdentity
		wantErr  bool
	}{
		{"no saved pin", nil, false},
		{"same asset", &matching, false},
		{"re-enrolled asset", &otherAsset, true},
		{"same name in another org", &otherOrg, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := verifyCloudDefaultIdentity("wendy-voice-agent", tc.expected, legacy)
			if (err != nil) != tc.wantErr {
				t.Fatalf("identity check = %v, want error %t", err, tc.wantErr)
			}
			if tc.wantErr && (!errors.Is(err, errDeviceIdentityRefused) || !strings.Contains(err.Error(), "saved for the default device")) {
				t.Fatalf("identity mismatch = %v, want actionable identity refusal", err)
			}
		})
	}
}

func TestCloudDiscoveryIdentityRefusesInvalidV2Binding(t *testing.T) {
	const tenant = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	auth := &config.AuthConfig{Certificates: []config.CertificateInfo{{PrincipalURI: "spiffe://wendy.sh/tenant/" + tenant + "/operator/test"}}}
	for _, tc := range []struct {
		name  string
		asset *cloudpbv2.Asset
	}{
		{"absent binding", &cloudpbv2.Asset{OrganizationId: tenant}},
		{"empty binding", &cloudpbv2.Asset{OrganizationId: tenant, PkiDeviceName: proto.String("")}},
		{"wrong tenant", &cloudpbv2.Asset{OrganizationId: "bbbbbbbb-bbbb-cccc-dddd-eeeeeeeeeeee", PkiDeviceName: proto.String("device")}},
		{"missing tenant", &cloudpbv2.Asset{PkiDeviceName: proto.String("device")}},
		{"invalid path", &cloudpbv2.Asset{OrganizationId: tenant, PkiDeviceName: proto.String("other//device")}},
		{"traversal", &cloudpbv2.Asset{OrganizationId: tenant, PkiDeviceName: proto.String("fleet/../device")}},
		{"dot segment", &cloudpbv2.Asset{OrganizationId: tenant, PkiDeviceName: proto.String("fleet/./device")}},
		{"trailing slash", &cloudpbv2.Asset{OrganizationId: tenant, PkiDeviceName: proto.String("device/")}},
		{"control", &cloudpbv2.Asset{OrganizationId: tenant, PkiDeviceName: proto.String("device\n")}},
		{"unicode", &cloudpbv2.Asset{OrganizationId: tenant, PkiDeviceName: proto.String("dév")}},
		{"fragment", &cloudpbv2.Asset{OrganizationId: tenant, PkiDeviceName: proto.String("device#fragment")}},
		{"escape", &cloudpbv2.Asset{OrganizationId: tenant, PkiDeviceName: proto.String("fleet%2fdevice")}},
		{"overlong segment", &cloudpbv2.Asset{OrganizationId: tenant, PkiDeviceName: proto.String(strings.Repeat("a", 65))}},
		{"query", &cloudpbv2.Asset{OrganizationId: tenant, PkiDeviceName: proto.String("device?query")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.asset.Id = "asset-uuid"
			if _, err := cloudDiscoveryIdentity(auth, cloudDiscoveryDevice{v2: tc.asset, key: "asset-uuid"}); err == nil {
				t.Fatal("accepted invalid binding or derived a peer from the asset UUID")
			}
		})
	}
	for _, auth := range []*config.AuthConfig{nil, {}} {
		if _, err := cloudDiscoveryIdentity(auth, cloudDiscoveryDevice{}); err == nil {
			t.Fatal("accepted missing operator certificate")
		}
	}
}

func TestCloudDiscoveryIdentityPreservesPathShapedPKIBinding(t *testing.T) {
	const tenant = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	auth := &config.AuthConfig{Certificates: []config.CertificateInfo{{PrincipalURI: "spiffe://wendy.sh/tenant/" + tenant + "/operator/test"}}}
	for _, name := range []string{"fleet/box-01", "fleet/subfleet/box_02.v1", strings.Repeat("a", 64)} {
		asset := &cloudpbv2.Asset{Id: "unrelated-asset-uuid", OrganizationId: tenant, PkiDeviceName: proto.String(name)}
		identity, err := cloudDiscoveryIdentity(auth, cloudDiscoveryDevice{v2: asset, key: "unrelated-asset-uuid"})
		if err != nil || identity.EntityID != name || identity.Principal != certs.DeviceSPIFFEURI(tenant, name) {
			t.Fatalf("valid PKI binding %q changed/refused: %+v %v", name, identity, err)
		}
	}
}

func TestCloudDefaultIdentityUsesPKIBindingInsteadOfAssetID(t *testing.T) {
	const tenant = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	const assetID = "d41e332f-3e9e-43e7-a001-64d38d9708a0"
	deviceName := "c42efa0d-d443-452f-8128-b093fe0a3cc6"
	auth := &config.AuthConfig{Certificates: []config.CertificateInfo{{PrincipalURI: "spiffe://wendy.sh/tenant/" + tenant + "/operator/test"}}}
	asset := cloudDiscoveryDevice{v2: &cloudpbv2.Asset{Id: assetID, OrganizationId: tenant, PkiDeviceName: &deviceName}, key: assetID}
	actual, err := cloudDiscoveryIdentity(auth, asset)
	if err != nil {
		t.Fatal(err)
	}
	want := certs.DeviceSPIFFEURI(tenant, deviceName)
	if actual.Principal != want || actual.EntityID != deviceName {
		t.Fatalf("Cloud identity = %+v, want bound device %s", actual, want)
	}
	if err := verifyCloudDefaultIdentity("simsim", &certs.WendyIdentity{Principal: want}, actual); err != nil {
		t.Fatalf("bound device refused: %v", err)
	}
	wrong := certs.WendyIdentity{Principal: certs.DeviceSPIFFEURI(tenant, assetID)}
	if err := verifyCloudDefaultIdentity("simsim", &wrong, actual); !errors.Is(err, errDeviceIdentityRefused) {
		t.Fatalf("database ID accepted as device identity: %v", err)
	}
}

func TestCloudDiscoveryIdentityRequiresSelectedAssetAndOperatorTenant(t *testing.T) {
	const tenant = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	for _, tc := range []struct{ name, selected, returned, principal string }{
		{"different asset", "selected", "other", "spiffe://wendy.sh/tenant/" + tenant + "/operator/test"},
		{"missing returned asset", "selected", "", "spiffe://wendy.sh/tenant/" + tenant + "/operator/test"},
		{"empty selected asset", "", "", "spiffe://wendy.sh/tenant/" + tenant + "/operator/test"},
		{"numeric operator cannot verify v2", "selected", "selected", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auth := &config.AuthConfig{Certificates: []config.CertificateInfo{{PrincipalURI: tc.principal, OrganizationID: 64}}}
			asset := cloudDiscoveryDevice{v2: &cloudpbv2.Asset{Id: tc.returned, OrganizationId: tenant, PkiDeviceName: proto.String("fleet/device")}, key: tc.selected}
			if _, err := cloudDiscoveryIdentity(auth, asset); err == nil {
				t.Fatal("accepted wrong asset or missing operator tenant")
			}
			if conn, err := connectCloudDiscoveryDevice(context.Background(), auth, asset, ""); err == nil || conn != nil {
				t.Fatal("connection attempted without a verified binding")
			}
		})
	}
	if conn, err := connectCloudDiscoveryDevice(context.Background(), nil, cloudDiscoveryDevice{}, ""); err == nil || conn != nil {
		t.Fatal("connection accepted missing operator certificate")
	}
}

func TestCloudDefaultSelectorUsesSavedAssetID(t *testing.T) {
	defaultName := "wendyos-voice-agent.local"
	legacy := certs.WendyIdentity{OrgID: 64, EntityType: certs.EntityAsset, EntityID: "486"}
	if got := cloudDefaultSelector(defaultName, &legacy); got != "486" {
		t.Fatalf("pinned Cloud selector = %q, want asset ID 486", got)
	}
	if got := cloudDefaultSelector(defaultName, nil); got != defaultName {
		t.Fatalf("unpinned Cloud selector = %q, want saved name", got)
	}
}
