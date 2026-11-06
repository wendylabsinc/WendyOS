package meshcatalog

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"math/big"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/mesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

const testTenantID = "11111111-1111-4111-8111-111111111111"

const (
	p533 = "spiffe://wendy.sh/tenant/" + testTenantID + "/device/53333333-0000-4000-8000-000000000533"
	p534 = "spiffe://wendy.sh/tenant/" + testTenantID + "/device/53444444-0000-4000-8000-000000000534"
	p535 = "spiffe://wendy.sh/tenant/" + testTenantID + "/device/53555555-0000-4000-8000-000000000535"
	p536 = "spiffe://wendy.sh/tenant/" + testTenantID + "/device/53666666-0000-4000-8000-000000000536"
)

type fixture struct {
	now   time.Time
	creds map[string]*localmesh.Credentials
	chain string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	return newFixtureWithOptions(t, 24*time.Hour, nil)
}

func newFixtureWithOptions(t *testing.T, lifetime time.Duration, reuse map[string]*localmesh.Credentials) fixture {
	t.Helper()
	return newFixtureWithKeys(t, lifetime, reuse, false)
}

// newFixtureWithKeys issues ML-DSA-65 keys when mldsaKeys is set, as Cloud v2
// enrollment does. reuse applies to P-256 fixtures only.
func newFixtureWithKeys(t *testing.T, lifetime time.Duration, reuse map[string]*localmesh.Credentials, mldsaKeys bool) fixture {
	t.Helper()
	now := time.Now().Truncate(time.Second)
	newKey := func() crypto.Signer {
		var key crypto.Signer
		var err error
		if mldsaKeys {
			key, err = mldsa.GenerateKey(mldsa.MLDSA65())
		} else {
			key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		}
		if err != nil {
			t.Fatal(err)
		}
		return key
	}
	caKey := newKey()
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: now.Add(-time.Hour),
		NotAfter: now.Add(lifetime), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, caKey.Public(), caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	chain := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))
	creds := map[string]*localmesh.Credentials{}
	for _, principal := range []string{p533, p534, p535, p536} {
		key := newKey()
		if previous := reuse[principal]; previous != nil {
			key = previous.Signer.(*ecdsa.PrivateKey)
		}
		uri, _ := url.Parse(principal)
		leaf := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), NotBefore: caTmpl.NotBefore,
			NotAfter: caTmpl.NotAfter, URIs: []*url.URL{uri}, KeyUsage: x509.KeyUsageDigitalSignature,
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}}
		der, err := x509.CreateCertificate(rand.Reader, leaf, caCert, key.Public(), caKey)
		if err != nil {
			t.Fatal(err)
		}
		keyDER, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		certPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
		keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
		creds[principal], err = localmesh.NewCredentials(certPEM, chain, keyPEM)
		if err != nil {
			t.Fatal(err)
		}
	}
	return fixture{now, creds, chain}
}

func principalOf(t *testing.T, principal string) meshid.Device {
	t.Helper()
	device, err := meshid.DeviceFromPrincipal(principal)
	if err != nil {
		t.Fatal(err)
	}
	return device
}

func testVIPs(t *testing.T) *mesh.VIPTable {
	t.Helper()
	table, err := mesh.OpenVIPTable(filepath.Join(t.TempDir(), "mesh-vips.json"))
	if err != nil {
		t.Fatal(err)
	}
	return table
}

func (f fixture) newCatalog(t *testing.T, principal, meshName string, _ any, _ any) (*Catalog, *localmesh.IdentityCache) {
	t.Helper()
	cred := f.creds[principal]
	cache, err := localmesh.OpenIdentityCache("", localmesh.DefaultCacheLimits(), cred.Verify, f.now)
	if err != nil {
		t.Fatal(err)
	}
	view := localmesh.NodeSnapshot{Self: cred.Device}
	authorize := func(appID, typ string, port uint16) error {
		if appID != "com.wendy.test" || !(typ == "_http._tcp" && port == 18080 || typ == "_wendy-echo._udp" && port == 47777) {
			return errors.New("port not owned by mesh app")
		}
		return nil
	}
	catalog, err := NewManifestCatalog(meshName, cred.Device, authorize, func(services []localmesh.ManifestService) error {
		if err := localmesh.CheckManifestServices(services); err != nil {
			return err
		}
		revision := uint64(1)
		if len(view.Devices) > 0 {
			revision = view.Devices[0].Revision + 1
		}
		view.Devices = []localmesh.Manifest{{Device: cred.Device.UUID(), Revision: revision, Issued: f.now.UnixMilli(), Expires: f.now.Add(MaxLease).UnixMilli(), Services: services}}
		return nil
	}, func() localmesh.NodeSnapshot { return view })
	if err != nil {
		t.Fatal(err)
	}
	return catalog, cache
}
func testSpec() PublishSpec {
	return PublishSpec{AppID: "com.wendy.test", ServiceID: "http", Type: "_http._tcp", Instance: "Example", HostPort: 18080, TXT: []string{"path=/", "role=demo"}, Lease: 90 * time.Second}
}

// Frontend-only fixtures begin after router signature admission. Wire/signature
// interoperability is exercised by localmesh's complete-manifest tests.
func (c *Catalog) acceptTestRecord(r Record, now time.Time) (bool, error) {
	view := c.manifestSnapshot()
	device, err := meshid.DeviceFromPrincipal(r.Key.Device)
	if err != nil {
		return false, err
	}
	var old []localmesh.ManifestService
	var next []localmesh.Manifest
	for _, m := range view.Devices {
		if m.Device == device.UUID() {
			old = m.Services
		} else {
			next = append(next, m)
		}
	}
	var services []localmesh.ManifestService
	for _, s := range old {
		if s.Mesh != r.Key.Mesh || s.AppID != r.Key.AppID || s.ServiceID != r.Key.ServiceID {
			services = append(services, s)
		}
	}
	if !r.Withdraw {
		services = append(services, localmesh.ManifestService{Mesh: r.Key.Mesh, AppID: r.Key.AppID, ServiceID: r.Key.ServiceID, Type: r.Type, Instance: r.Instance, Port: r.HostPort, TXT: r.TXT})
	}
	view.Devices = append(next, localmesh.Manifest{Device: device.UUID(), Revision: r.Generation, Issued: r.Issued, Expires: r.Expires, Services: services})
	c.manifestSnapshot = func() localmesh.NodeSnapshot { return view }
	return true, nil
}
