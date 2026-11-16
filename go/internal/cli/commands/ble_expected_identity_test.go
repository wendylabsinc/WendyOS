package commands

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/discoverycache"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

// Exercise the production BLE TLS setup. The certificate for another asset is
// legitimately signed by the selected login's CA, so only the expected-device
// check can reject it.
func TestBLEAgentTLSRequiresSavedDeviceIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, presented string
		want            bool
	}{
		{"matching legacy asset", certs.AssetURN(2, 606), true},
		{"different same-org asset", certs.AssetURN(2, 607), false},
		{"missing identity", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auth, peer := legacyLiteTestPKI(t, 2, tc.presented)
			cfg := &config.Config{}
			cfg.SetDevicePin("robot", 2, "cloud.test:443", "606", "")
			setTempConfig(t, cfg)
			setPinCache(t)
			client, err := bleAgentTLSConfig(&models.BluetoothDevice{DisplayName: "robot.local"}, auth.Certificates[0], nil)
			if err != nil {
				t.Fatal(err)
			}
			leaf, err := x509.ParseCertificate(peer.Certificates[0].Certificate[0])
			if err != nil {
				t.Fatal(err)
			}
			err = client.VerifyConnection(tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}})
			if (err == nil) != tc.want {
				t.Fatalf("BLE verification error = %v, want accepted=%v", err, tc.want)
			}
			if !tc.want {
				var mismatch *certs.IdentityMismatchError
				if !errors.As(err, &mismatch) {
					t.Fatalf("want identity rejection, got %v", err)
				}
			}
		})
	}
}

// A Bluetooth device with no saved identity is a first contact: its
// certificate is still checked against the sign-in's trust roots and tenant.
func TestBLEAgentTLSFirstContact(t *testing.T) {
	verify := func(t *testing.T, auth *config.AuthConfig, peer *tls.Config) error {
		t.Helper()
		setTempConfig(t, &config.Config{})
		setPinCache(t)
		client, err := bleAgentTLSConfig(&models.BluetoothDevice{DisplayName: "robot"}, auth.Certificates[0], nil)
		if err != nil {
			t.Fatalf("no saved identity refused before connecting: %v", err)
		}
		leaf, err := x509.ParseCertificate(peer.Certificates[0].Certificate[0])
		if err != nil {
			t.Fatal(err)
		}
		return client.VerifyConnection(tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}})
	}
	auth, peer := legacyLiteTestPKI(t, 2, certs.AssetURN(2, 606))
	if err := verify(t, auth, peer); err != nil {
		t.Fatalf("device under the sign-in's roots was rejected: %v", err)
	}
	_, otherPKI := legacyLiteTestPKI(t, 2, certs.AssetURN(2, 606))
	if err := verify(t, auth, otherPKI); err == nil {
		t.Fatal("device from another CA was accepted")
	}
	tenantAuth, sameTenant := localLiteTestPKI(t)
	if err := verify(t, tenantAuth, sameTenant); err != nil {
		t.Fatalf("same-tenant device was rejected: %v", err)
	}
	otherAuth, otherTenant := localLiteTestPKIForTenant(t, "11111111-1111-4111-8111-111111111111")
	if err := verify(t, otherAuth, otherTenant); err == nil {
		t.Fatal("device from another tenant was accepted")
	}
}

func TestBLEAgentTLSUsesCloudInventoryName(t *testing.T) {
	auth, peer := localLiteTestPKI(t)
	auth.CloudGRPC = "cloud.test:443"
	setTempConfig(t, &config.Config{Auth: []config.AuthConfig{*auth}})
	setPinCache(t, discoverycache.Entry{Hostname: "wendyos-robot.local", DisplayName: "robot"})
	deviceID := liteTestAssetID
	if err := seedPinsFromCloudAssetsV2(auth, []*cloudpbv2.Asset{{Id: "cloud-row-id", Name: "robot", OrganizationId: liteTestTenant, PkiDeviceName: &deviceID}}); err != nil {
		t.Fatal(err)
	}
	client, err := bleAgentTLSConfig(&models.BluetoothDevice{DisplayName: "robot"}, auth.Certificates[0], nil)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(peer.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := client.VerifyConnection(tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}); err != nil {
		t.Fatalf("Cloud inventory name was not usable over BLE: %v", err)
	}
	// A changed expected device in the same trusted tenant must reject this
	// still-valid leaf rather than silently learn its presented identity.
	changed := &config.Config{}
	changed.SetDevicePin("robot", 0, auth.CloudGRPC, "another-device", certs.DeviceSPIFFEURI(liteTestTenant, "another-device"))
	if err := config.Save(changed); err != nil {
		t.Fatal(err)
	}
	client, err = bleAgentTLSConfig(&models.BluetoothDevice{DisplayName: "robot"}, auth.Certificates[0], nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.VerifyConnection(tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}); err == nil {
		t.Fatal("different device with a valid same-tenant leaf was accepted")
	}
}

// With no saved identity, a plaintext answer is used only when the device
// proves it is unenrolled; this fake cannot.
func TestMissingExpectedIdentityNeedsUnenrolledPlaintext(t *testing.T) {
	orig := plaintextConnectFn
	plaintextConnectFn = func(context.Context, string) (*grpcclient.AgentConnection, error) {
		return grpcclient.NewFromConn(nil), nil
	}
	t.Cleanup(func() { plaintextConnectFn = orig })
	conn, _, err := dialAgentLadderWithCerts(context.Background(), dialTarget{Addr: "127.0.0.1:1"}, []config.CertificateInfo{{}})
	if conn != nil || err == nil {
		t.Fatalf("unproven plaintext answer returned connection=%v err=%v", conn != nil, err)
	}
}
