package commands

import (
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

func TestCloudDefaultIdentityUsesV2DevicePrincipal(t *testing.T) {
	const tenant = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	const device = "11111111-2222-3333-4444-555555555555"
	auth := &config.AuthConfig{Certificates: []config.CertificateInfo{{PrincipalURI: "spiffe://wendy.sh/tenant/" + tenant + "/operator/test"}}}
	const assetID = "66666666-7777-8888-9999-aaaaaaaaaaaa"
	actual, err := cloudDiscoveryIdentity(auth, cloudDiscoveryDevice{v2: &cloudpbv2.Asset{Id: assetID, OrganizationId: tenant, PkiDeviceName: proto.String(device)}, key: assetID})
	if err != nil {
		t.Fatal(err)
	}
	if actual.EntityID != device || actual.EntityID == assetID {
		t.Fatalf("peer identity = %q; must use the PKI binding, not Cloud asset ID", actual.EntityID)
	}
	want := certs.DeviceSPIFFEURI(tenant, device)
	if actual.Principal != want {
		t.Fatalf("Cloud identity = %q, want %q", actual.Principal, want)
	}
	expected := certs.WendyIdentity{Principal: want}
	if err := verifyCloudDefaultIdentity("wendy-voice-agent", &expected, actual); err != nil {
		t.Fatalf("matching V2 pin refused: %v", err)
	}
	wrongPin := certs.WendyIdentity{Principal: certs.DeviceSPIFFEURI(tenant, assetID)}
	if err := verifyCloudDefaultIdentity("wendy-voice-agent", &wrongPin, actual); !errors.Is(err, errDeviceIdentityRefused) {
		t.Fatalf("asset-ID-derived pin accepted: %v", err)
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
		{"fragment", &cloudpbv2.Asset{OrganizationId: tenant, PkiDeviceName: proto.String("device#fragment")}},
		{"escape", &cloudpbv2.Asset{OrganizationId: tenant, PkiDeviceName: proto.String("fleet%2fdevice")}},
		{"overlong segment", &cloudpbv2.Asset{OrganizationId: tenant, PkiDeviceName: proto.String(strings.Repeat("a", 65))}},
		{"query", &cloudpbv2.Asset{OrganizationId: tenant, PkiDeviceName: proto.String("device?query")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
		asset := &cloudpbv2.Asset{OrganizationId: tenant, PkiDeviceName: proto.String(name)}
		identity, err := cloudDiscoveryIdentity(auth, cloudDiscoveryDevice{v2: asset, key: "unrelated-asset-uuid"})
		if err != nil || identity.EntityID != name || identity.Principal != certs.DeviceSPIFFEURI(tenant, name) {
			t.Fatalf("valid PKI binding %q changed/refused: %+v %v", name, identity, err)
		}
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
