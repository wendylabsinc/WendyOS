package commands

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

const pinUUIDTenant = "22f35004-d638-4b5c-869f-b815974d6eae"
const pinUUIDOtherTenant = "11111111-1111-4111-8111-111111111111"
const pinUUIDDevice = "03ce308b-4c00-40c2-bc64-5f66a83d4f37"

func uuidPinObservation(tenant, device string) observedDeviceIdentity {
	return observedDeviceIdentity{mTLS: true, assetID: device, principal: certs.DeviceSPIFFEURI(tenant, device)}
}

func uuidPinConfig() *config.Config {
	return &config.Config{Auth: []config.AuthConfig{
		{CloudGRPC: "other-cloud.test:443", Certificates: []config.CertificateInfo{{PrincipalURI: "spiffe://wendy.sh/tenant/" + pinUUIDOtherTenant + "/operator/other"}}},
		{CloudGRPC: "api.wendy.dev:443", Certificates: []config.CertificateInfo{{PrincipalURI: "spiffe://wendy.sh/tenant/" + pinUUIDTenant + "/operator/test"}}},
	}}
}

func TestUUIDDevicePinSelectsTenantCloudAndSurvivesRenewal(t *testing.T) {
	cfg := uuidPinConfig()
	obs := uuidPinObservation(pinUUIDTenant, pinUUIDDevice)
	changed, err := applyDeviceIdentity(cfg, "uuid-device.local", "", obs)
	if err != nil || !changed {
		t.Fatalf("first use: changed=%v err=%v", changed, err)
	}
	pin, ok := cfg.DevicePinFor("uuid-device.local")
	if !ok || pin.CloudGRPC != "api.wendy.dev:443" || pin.OrgID != 0 || pin.AssetID != pinUUIDDevice || pin.Principal != obs.principal {
		t.Fatalf("wrong UUID pin or Cloud realm: %+v", pin)
	}
	// Certificate bytes/keys/expiry are deliberately absent from the durable
	// identity; a renewed leaf with this same principal cannot change the pin.
	if changed, err = applyDeviceIdentity(cfg, "uuid-device", "", obs); err != nil || changed {
		t.Fatalf("renewal: changed=%v err=%v", changed, err)
	}
	if got := cloudGRPCForOrg(cfg, 0); got != "" {
		t.Fatalf("numeric zero selected a UUID auth context: %s", got)
	}
}

func TestUUIDDevicePinRejectsSameDeviceUUIDInAnotherTenant(t *testing.T) {
	cfg := uuidPinConfig()
	// Both tenants can use the same Cloud endpoint and the same device name;
	// only the complete principal distinguishes their trust domains.
	cfg.Auth[0].CloudGRPC = "api.wendy.dev:443"
	obs := uuidPinObservation(pinUUIDTenant, pinUUIDDevice)
	cfg.SetDevicePin("uuid-device", 0, "api.wendy.dev:443", pinUUIDDevice, obs.principal)
	before, _ := cfg.DevicePinFor("uuid-device")
	for _, changedIdentity := range []observedDeviceIdentity{
		uuidPinObservation(pinUUIDOtherTenant, pinUUIDDevice),
		uuidPinObservation(pinUUIDTenant, "11111111-1111-4111-8111-111111111111"),
	} {
		if changed, err := applyDeviceIdentity(cfg, "uuid-device", "", changedIdentity); err == nil || changed {
			t.Fatalf("changed UUID identity accepted: changed=%v err=%v", changed, err)
		}
		if after, _ := cfg.DevicePinFor("uuid-device"); after != before {
			t.Fatal("refusal mutated the saved pin")
		}
	}
	// A UUID pin already naming its principal is authoritative even when an
	// earlier CLI failed to retain its separate device-name field.
	cfg.SetDevicePin("uuid-device", 0, "api.wendy.dev:443", "", obs.principal)
	if changed, err := applyDeviceIdentity(cfg, "uuid-device", "", uuidPinObservation(pinUUIDOtherTenant, pinUUIDDevice)); err == nil || changed {
		t.Fatalf("assetless UUID pin adopted another tenant: changed=%v err=%v", changed, err)
	}
}

func TestUUIDDevicePinBackfillsExistingOrgZeroPin(t *testing.T) {
	cfg := uuidPinConfig()
	cfg.SetDevicePin("uuid-device", 0, "api.wendy.dev:443", "", "")
	obs := uuidPinObservation(pinUUIDTenant, pinUUIDDevice)
	if changed, err := applyDeviceIdentity(cfg, "uuid-device", "", obs); err != nil || !changed {
		t.Fatalf("existing incomplete org-zero pin was not upgraded: changed=%v err=%v", changed, err)
	}
	if pin, _ := cfg.DevicePinFor("uuid-device"); pin.AssetID != pinUUIDDevice || pin.Principal != obs.principal {
		t.Fatalf("existing pin did not gain the certificate identity: %+v", pin)
	}
}

func TestUUIDDevicePinRetainsLegacyOperatorCloudOnRenewal(t *testing.T) {
	cfg := &config.Config{Auth: []config.AuthConfig{{CloudGRPC: "legacy-cloud.test:443", Certificates: []config.CertificateInfo{{OrganizationID: 7}}}}}
	cfg.SetDevicePin("renewed-device", 7, "legacy-cloud.test:443", "42", "")
	obs := observedDeviceIdentity{mTLS: true, orgID: 7, assetID: "42", principal: certs.DeviceSPIFFEURI(pinUUIDTenant, "42")}
	if changed, err := applyDeviceIdentity(cfg, "renewed-device", "", obs); err != nil || !changed {
		t.Fatalf("legacy operator lost its Cloud realm after device renewal: changed=%v err=%v", changed, err)
	}
}

func TestObserveUUIDDeviceIdentityRequiresAuthenticatedMatchingTenant(t *testing.T) {
	valid, _ := certs.ParsePrincipal(certs.DeviceSPIFFEURI(pinUUIDTenant, pinUUIDDevice))
	operator := &config.CertificateInfo{PrincipalURI: "spiffe://wendy.sh/tenant/" + pinUUIDTenant + "/operator/test"}
	for _, tc := range []struct {
		name string
		edit func(*certs.WendyIdentity)
		want bool
	}{
		{"matching verified device", func(*certs.WendyIdentity) {}, true},
		{"other tenant", func(id *certs.WendyIdentity) {
			*id, _ = certs.ParsePrincipal(certs.DeviceSPIFFEURI(pinUUIDOtherTenant, pinUUIDDevice))
		}, false},
		{"operator as server", func(id *certs.WendyIdentity) { *id, _ = certs.ParsePrincipal(operator.PrincipalURI) }, false},
		{"missing identity", func(id *certs.WendyIdentity) { *id = certs.WendyIdentity{} }, false},
		{"inconsistent entity", func(id *certs.WendyIdentity) { id.EntityID = "other" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			identity := valid
			tc.edit(&identity)
			// Supply the metadata of a trusted broker connection without opening
			// a socket. A separate grpcclient integration test uses real TLS.
			conn, err := grpcclient.ConnectSessionProxy(context.Background(), filepath.Join(t.TempDir(), "unused.sock"), "uuid-device", "uuid-device:50052", operator, identity)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			obs := observeDeviceIdentity(conn)
			if tc.want {
				if obs.identityErr != nil || obs.principal != valid.Principal || obs.assetID != valid.EntityID {
					t.Fatalf("verified identity lost: %+v", obs)
				}
				conn.IsMTLS = false
				if unauthenticated := observeDeviceIdentity(conn); unauthenticated.mTLS || unauthenticated.principal != "" || unauthenticated.assetID != "" {
					t.Fatalf("unauthenticated metadata became an identity: %+v", unauthenticated)
				}
			} else {
				cfg := uuidPinConfig()
				if obs.identityErr == nil {
					t.Fatal("invalid UUID connection was not rejected")
				}
				if changed, err := applyDeviceIdentity(cfg, "uuid-device", "", obs); err == nil || changed || len(cfg.DevicePins) != 0 {
					t.Fatalf("invalid observation created a pin: changed=%v err=%v", changed, err)
				}
			}
		})
	}
}
