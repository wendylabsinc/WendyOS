package cloudmcp

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"math/big"
	"net/url"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/delegation"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
)

func TestInspectDelegatedCredential(t *testing.T) {
	tenant := "11111111-1111-4111-8111-111111111111"
	owner := "spiffe://wendy.sh/tenant/" + tenant + "/operator/alice"
	gateway := "spiffe://wendy.sh/tenant/" + tenant + "/service/mcp"
	device := "spiffe://wendy.sh/tenant/" + tenant + "/device/robot"
	id := "22222222-2222-4222-8222-222222222222"
	audience := "https://mcp.example/orgs/" + tenant + "/mcp"
	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rootTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "root"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	rootDER, err := x509.CreateCertificate(rand.Reader, rootTemplate, rootTemplate, rootKey.Public(), rootKey)
	if err != nil {
		t.Fatal(err)
	}
	root, err := x509.ParseCertificate(rootDER)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM, err := certs.GenerateMLDSAKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	key, err := certs.ParseSigningPrivateKeyPEM([]byte(keyPEM))
	if err != nil {
		t.Fatal(err)
	}
	rawString := func(v string) asn1.RawValue { return asn1.RawValue{Tag: asn1.TagUTF8String, Bytes: []byte(v)} }
	wire := struct {
		Version  int
		ID       string `asn1:"utf8"`
		Owner    string `asn1:"utf8"`
		Devices  []asn1.RawValue
		Apps     []asn1.RawValue
		Audience string `asn1:"optional,utf8"`
		Gateway  string `asn1:"optional,utf8"`
	}{2, id, owner, []asn1.RawValue{rawString(device)}, []asn1.RawValue{}, audience, gateway}
	scopeDER, err := asn1.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	right := "entitlement:wendy.agent.services.v2.WendyDeviceInfoService:GetDeviceInfo:allow"
	rightsDER, err := asn1.Marshal([]asn1.RawValue{rawString(right)})
	if err != nil {
		t.Fatal(err)
	}
	uri, _ := url.Parse(owner)
	leafTemplate := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: time.Now().Add(-time.Second), NotAfter: time.Now().Add(4 * time.Minute), URIs: []*url.URL{uri}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, ExtraExtensions: []pkix.Extension{{Id: delegation.ScopeOID, Critical: true, Value: scopeDER}, {Id: delegation.EntitlementsOID, Value: rightsDER}}}
	der, err := x509.CreateCertificate(rand.Reader, leafTemplate, root, key.Public(), rootKey)
	if err != nil {
		t.Fatal(err)
	}
	issued := issuedDelegation{ID: id, Certificate: der, Chain: rootDER, Devices: map[string]string{"asset": device}}
	issued.Specification.Delegation.Version = 2
	issued.Specification.Delegation.ID = id
	issued.Specification.Delegation.Owner = owner
	issued.Specification.Delegation.Devices = []string{device}
	issued.Specification.Delegation.Apps = []string{}
	issued.Specification.Delegation.Audience = audience
	issued.Specification.Delegation.Gateway = gateway
	issued.Specification.Entitlements = []string{right}
	issued.Specification.Exp = time.Now().Add(time.Hour).Unix()
	a := Access{OrganizationID: tenant, TenantID: tenant, UserID: "alice", OwnerPrincipal: owner, ServiceSubject: "mcp", DelegationID: id}
	for _, name := range []string{"valid", "wrong key", "wrong delegation", "wrong gateway", "wrong audience", "wrong owner", "wrong scope", "untrusted chain", "expired consent"} {
		t.Run(name, func(t *testing.T) {
			backend := &CloudBackend{}
			if err := backend.ConfigureDelegations(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER}), "https://cloud.example", "https://mcp.example"); err != nil {
				t.Fatal(err)
			}
			entry := &userDelegatedKey{privatePEM: keyPEM, owner: owner, id: id}
			access := a
			value := issued
			switch name {
			case "wrong key":
				entry.privatePEM, err = certs.GenerateMLDSAKeyPair()
				if err != nil {
					t.Fatal(err)
				}
			case "wrong delegation":
				access.DelegationID = "33333333-3333-4333-8333-333333333333"
			case "wrong gateway":
				access.ServiceSubject = "other"
			case "wrong audience":
				backend.mcpOrigin = "https://other.example"
			case "wrong owner":
				entry.owner = owner + "2"
			case "wrong scope":
				value.Specification.Delegation.Devices = []string{device + "2"}
			case "untrusted chain":
				backend.roots = x509.NewCertPool()
			case "expired consent":
				value.Specification.Exp = time.Now().Add(-time.Minute).Unix()
			}
			_, _, _, err := backend.inspectIssued(access, entry, value)
			if name == "valid" && err != nil {
				t.Fatal(err)
			}
			if name != "valid" && err == nil {
				t.Fatal("accepted substituted credential")
			}
		})
	}
	// Cache ownership must not collapse different users onto the org service key.
	other := a
	other.UserID = "bob"
	if delegatedCacheKey(a) == delegatedCacheKey(other) {
		t.Fatal("cache mixes users")
	}
}
