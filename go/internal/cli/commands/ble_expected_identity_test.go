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

func TestBLEAgentTLSUnknownAdvertisementCannotCreateBinding(t *testing.T) {
	setTempConfig(t, &config.Config{})
	setPinCache(t)
	// No credentials are loaded or any BLE connection attempted before refusal.
	_, err := bleAgentTLSConfig(&models.BluetoothDevice{DisplayName: "robot", ID: "606"}, config.CertificateInfo{}, nil)
	if !errors.Is(err, certs.ErrExpectedDeviceIdentity) || !blocksUnauthenticatedFallback(err) {
		t.Fatalf("missing saved identity did not fail closed: %v", err)
	}
}

func TestBLEAgentTLSUsesCloudSeededAliasBinding(t *testing.T) {
	auth, peer := localLiteTestPKI(t)
	auth.CloudGRPC = "cloud.test:443"
	setTempConfig(t, &config.Config{Auth: []config.AuthConfig{*auth}})
	setPinCache(t, discoverycache.Entry{Hostname: "wendyos-robot.local", DisplayName: "robot"})
	deviceID := liteTestAssetID
	if err := seedPinsFromCloudAssetsV2(auth, []*cloudpbv2.Asset{{Id: "cloud-row-id", Name: "robot", OrganizationId: liteTestTenant, PkiDeviceName: &deviceID}}); err != nil {
		t.Fatal(err)
	}
	client, err := bleAgentTLSConfig(&models.BluetoothDevice{DisplayName: "wendyos-robot"}, auth.Certificates[0], nil)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(peer.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := client.VerifyConnection(tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}); err != nil {
		t.Fatalf("Cloud inventory binding was not usable over BLE alias: %v", err)
	}
	// A changed expected device in the same trusted tenant must reject this
	// still-valid leaf rather than silently learn its presented identity.
	changed := &config.Config{}
	changed.SetDevicePin("wendyos-robot", 0, auth.CloudGRPC, "another-device", certs.DeviceSPIFFEURI(liteTestTenant, "another-device"))
	if err := config.Save(changed); err != nil {
		t.Fatal(err)
	}
	client, err = bleAgentTLSConfig(&models.BluetoothDevice{DisplayName: "wendyos-robot"}, auth.Certificates[0], nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.VerifyConnection(tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}); err == nil {
		t.Fatal("different device with a valid same-tenant leaf was accepted")
	}
}

func TestMissingExpectedIdentityNeverDialsPlaintext(t *testing.T) {
	orig := plaintextConnectFn
	plaintextConnectFn = func(context.Context, string) (*grpcclient.AgentConnection, error) {
		t.Fatal("missing expected identity caused a plaintext downgrade")
		return nil, nil
	}
	t.Cleanup(func() { plaintextConnectFn = orig })
	conn, _, err := dialAgentLadderWithCerts(context.Background(), dialTarget{Addr: "127.0.0.1:50051"}, []config.CertificateInfo{{}})
	if conn != nil || !errors.Is(err, certs.ErrExpectedDeviceIdentity) || !blocksUnauthenticatedFallback(err) {
		t.Fatalf("missing expectation returned connection=%v err=%v", conn != nil, err)
	}
}
