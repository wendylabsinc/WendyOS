package mcp

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net/url"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

func TestMCPCloudTLSRequiresSelectedInventoryIdentity(t *testing.T) {
	const tenant = "11111111-1111-4111-8111-111111111111"
	const deviceID = "22222222-2222-4222-8222-222222222222"
	for _, tc := range []struct {
		name, operator, presented, cn string
		device                        mcpCloudDevice
		want                          bool
	}{
		{name: "matching legacy URN", presented: certs.AssetURN(7, 42), device: mcpCloudDevice{legacyID: 42}, want: true},
		{name: "matching legacy CN", cn: "sh/wendy/7/42", device: mcpCloudDevice{legacyID: 42}, want: true},
		{name: "wrong legacy asset same org", presented: certs.AssetURN(7, 43), device: mcpCloudDevice{legacyID: 42}},
		{name: "identityless certificate", device: mcpCloudDevice{legacyID: 42}},
		{name: "authoritative PKI binding", operator: "spiffe://wendy.sh/tenant/" + tenant + "/operator/test", presented: certs.DeviceSPIFFEURI(tenant, deviceID), device: mcpCloudDevice{isV2: true, key: "different-cloud-row-id", pkiDeviceName: deviceID}, want: true},
		{name: "wrong device same tenant", operator: "spiffe://wendy.sh/tenant/" + tenant + "/operator/test", presented: certs.DeviceSPIFFEURI(tenant, "33333333-3333-4333-8333-333333333333"), device: mcpCloudDevice{isV2: true, key: deviceID}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, peer := mcpTLSIdentityFixture(t, tc.operator, tc.presented, tc.cn)
			var verified grpcclient.VerifiedServerIdentity
			tlsConfig, err := mcpCloudAgentTLSConfig(client, tc.device, &verified)
			if err != nil {
				t.Fatal(err)
			}
			err = tlsConfig.VerifyConnection(tls.ConnectionState{PeerCertificates: []*x509.Certificate{peer}})
			if (err == nil) != tc.want {
				t.Fatalf("MCP verification error=%v, want accepted=%v", err, tc.want)
			}
			if !tc.want {
				var mismatch *certs.IdentityMismatchError
				if !errors.As(err, &mismatch) {
					t.Fatalf("want exact-device rejection, got %v", err)
				}
			}
		})
	}
}

func TestMCPCloudTLSRejectsMissingInventoryBinding(t *testing.T) {
	client, _ := mcpTLSIdentityFixture(t, "", certs.AssetURN(7, 42), "")
	var verified grpcclient.VerifiedServerIdentity
	for _, device := range []mcpCloudDevice{{}, {legacyID: -1}, {isV2: true}, {isV2: true, key: "some-device"}} {
		_, err := mcpCloudAgentTLSConfig(client, device, &verified)
		if !errors.Is(err, certs.ErrExpectedDeviceIdentity) {
			t.Fatalf("device %+v without a complete inventory identity: %v", device, err)
		}
	}
}

// Both client and device chain to the same CA. Tests deliberately substitute a
// valid device leaf so rejection cannot be explained by a broken certificate.
func mcpTLSIdentityFixture(t *testing.T, operator, device, cn string) (config.CertificateInfo, *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "MCP test CA"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageCertSign, IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	caPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	ca, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	issue := func(principal, cn string, serial int64) (*x509.Certificate, string, string) {
		t.Helper()
		leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		leaf := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: cn},
			NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, KeyUsage: x509.KeyUsageDigitalSignature,
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}}
		if principal != "" {
			u, err := url.Parse(principal)
			if err != nil {
				t.Fatal(err)
			}
			leaf.URIs = []*url.URL{u}
		}
		der, err := x509.CreateCertificate(rand.Reader, leaf, ca, leafKey.Public(), key)
		if err != nil {
			t.Fatal(err)
		}
		leaf, err = x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		keyDER, err := x509.MarshalECPrivateKey(leafKey)
		if err != nil {
			t.Fatal(err)
		}
		return leaf, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	}
	if operator == "" {
		operator = certs.UserURN(7, "test")
	}
	_, clientPEM, clientKey := issue(operator, "", 2)
	peer, _, _ := issue(device, cn, 3)
	return config.CertificateInfo{OrganizationID: 7, PrincipalURI: operator, PemCertificate: clientPEM, PemPrivateKey: clientKey, PemCertificateChain: caPEM}, peer
}
