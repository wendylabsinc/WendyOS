package commands

import (
	"errors"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

func TestCloudDefaultIdentityMustMatchSavedPin(t *testing.T) {
	legacyAuth := &config.AuthConfig{Certificates: []config.CertificateInfo{{OrganizationID: 64}}}
	legacy := cloudDiscoveryIdentity(legacyAuth, cloudDiscoveryDevice{legacy: &cloudpb.Asset{}, key: "486"})
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
	actual := cloudDiscoveryIdentity(auth, cloudDiscoveryDevice{v2: &cloudpbv2.Asset{}, key: device})
	want := certs.DeviceSPIFFEURI(tenant, device)
	if actual.Principal != want {
		t.Fatalf("Cloud identity = %q, want %q", actual.Principal, want)
	}
	expected := certs.WendyIdentity{Principal: want}
	if err := verifyCloudDefaultIdentity("wendy-voice-agent", &expected, actual); err != nil {
		t.Fatalf("matching V2 pin refused: %v", err)
	}
}

func TestCloudDefaultIdentityUsesPKIBindingInsteadOfAssetID(t *testing.T) {
	const tenant = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	const assetID = "d41e332f-3e9e-43e7-a001-64d38d9708a0"
	deviceName := "c42efa0d-d443-452f-8128-b093fe0a3cc6"
	auth := &config.AuthConfig{Certificates: []config.CertificateInfo{{PrincipalURI: "spiffe://wendy.sh/tenant/" + tenant + "/operator/test"}}}
	asset := cloudDiscoveryDevice{v2: &cloudpbv2.Asset{Id: assetID, PkiDeviceName: &deviceName}, key: assetID}
	actual := cloudDiscoveryIdentity(auth, asset)
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
