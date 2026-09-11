package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const identityTenant = "8a53be77-2a69-464f-8f73-83643fe0beaa"

func TestLoadRecoversPrincipalFromCertificate(t *testing.T) {
	home := overrideHome(t)
	principal := "spiffe://wendy.sh/tenant/" + identityTenant + "/operator/test"
	uri, _ := url.Parse(principal)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), URIs: []*url.URL{uri}}
	raw, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	c := Config{Auth: []AuthConfig{{CloudGRPC: "cloud:443", Certificates: []CertificateInfo{{PemCertificate: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: raw}))}}}}}
	encoded, _ := json.Marshal(c)
	dir := filepath.Join(home, ".wendy")
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "config.json"), encoded, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	cert := loaded.Auth[0].Certificates[0]
	if cert.PrincipalURI != principal || cert.TenantUUID() != identityTenant {
		t.Fatalf("identity was not recovered: %q", cert.PrincipalURI)
	}
	if loaded.Auth[0].OAuthIssuer != "" {
		t.Fatal("invented OAuth metadata")
	}
	after, _ := os.ReadFile(filepath.Join(dir, "config.json"))
	if string(after) != string(encoded) {
		t.Fatal("loading rewrote the user's configuration")
	}
}
func TestUUIDOrganizationsDoNotCollapseToZero(t *testing.T) {
	makeAuth := func(tenant string) AuthConfig {
		return AuthConfig{CloudGRPC: "cloud:443", OAuthIssuer: "https://issuer.test", Certificates: []CertificateInfo{{PrincipalURI: "spiffe://wendy.sh/tenant/" + tenant + "/operator/test"}}}
	}
	a := makeAuth(identityTenant)
	b := makeAuth("2558fd76-afc7-466e-9613-6b715296a526")
	cfg := &Config{DefaultCloudGRPC: "cloud:443"}
	cfg.AddAuth(a)
	cfg.AddAuth(b)
	if len(cfg.Auth) != 2 {
		t.Fatal("UUID organizations were merged")
	}
	cfg.DefaultTenantUUID = b.Certificates[0].TenantUUID()
	for _, endpoint := range []string{"", "cloud:443"} {
		got, err := ResolveAuth(cfg, endpoint, nil)
		if err != nil || got.OrganizationKey() != b.OrganizationKey() {
			t.Fatalf("default resolved wrong UUID: %v", err)
		}
	}
	cfg.DefaultTenantUUID = ""
	got, err := ResolveAuth(cfg, "", nil)
	if err != nil || got.OrganizationKey() != a.OrganizationKey() {
		t.Fatal("unselected UUID displaced first organization")
	}
	cfg.Auth = append([]AuthConfig{{CloudGRPC: "cloud:443", Certificates: []CertificateInfo{{OrganizationID: 0}}}}, cfg.Auth...)
	got, err = ResolveAuth(cfg, "cloud:443", nil)
	if err != nil || got.OrganizationKey() != a.OrganizationKey() {
		t.Fatal("empty legacy identity displaced PKI identity")
	}
}
