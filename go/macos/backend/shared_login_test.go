package backend

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"math/big"
	"testing"
	"time"
)

func sharedLoginDummy(t *testing.T) string {
	t.Helper()
	data := make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(data)
}
func sharedLoginFixture(t *testing.T, now time.Time) config.AuthConfig {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Test only"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(48 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	private, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return config.AuthConfig{Name: "fixture", CloudGRPC: "fixture.invalid:443", RefreshToken: sharedLoginDummy(t), RegistryPrivateKey: sharedLoginDummy(t), RegistryKeyID: "local-key", LitePrivateKey: sharedLoginDummy(t), APIKey: sharedLoginDummy(t), Certificates: []config.CertificateInfo{{PemCertificate: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), PemPrivateKey: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private}))}}}
}
func TestSharedLoginOmitsDeviceSecretsAndUnboundedTokens(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	value, err := makeSharedLogin(sharedLoginFixture(t, now), now)
	if err != nil {
		t.Fatal(err)
	}
	a := value.Account
	if a.RefreshToken != "" || a.RegistryPrivateKey != "" || a.RegistryKeyID != "" || a.LitePrivateKey != "" || a.APIKey != "" || a.ServiceAccount != "" {
		t.Fatal("non-shareable credential escaped allowlist")
	}
	if !value.ExpiresAt.Equal(now.Add(24 * time.Hour)) {
		t.Fatal("snapshot expiry exceeds 24 hours")
	}
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = parseSharedLogin(data, now); err != nil {
		t.Fatal(err)
	}
	if _, err = parseSharedLogin(data, now.Add(25*time.Hour)); err == nil {
		t.Fatal("expired snapshot accepted")
	}
}
func TestSharedLoginRejectsTamperingAndLocalKeyReferences(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	original, err := makeSharedLogin(sharedLoginFixture(t, now), now)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*sharedLogin)
	}{
		{"refresh token", func(v *sharedLogin) { v.Account.RefreshToken = sharedLoginDummy(t) }},
		{"service account", func(v *sharedLogin) { v.Account.ServiceAccount = "forbidden" }},
		{"registry key", func(v *sharedLogin) { v.Account.RegistryPrivateKey = sharedLoginDummy(t) }},
		{"local reference", func(v *sharedLogin) { v.Account.APIKey = "keychain:" + sharedLoginDummy(t) }},
		{"invalid key", func(v *sharedLogin) { v.Account.Certificates[0].PemPrivateKey = sharedLoginDummy(t) }},
		{"unbounded expiry", func(v *sharedLogin) { v.ExpiresAt = now.Add(48 * time.Hour) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, _ := json.Marshal(original)
			var v sharedLogin
			if err := json.Unmarshal(raw, &v); err != nil {
				t.Fatal(err)
			}
			tt.mutate(&v)
			raw, _ = json.Marshal(v)
			if _, err := parseSharedLogin(raw, now); err == nil {
				t.Fatal("invalid snapshot accepted")
			}
		})
	}
	raw, _ := json.Marshal(original)
	if _, err := parseSharedLogin(append(raw, []byte(" {}")...), now); err == nil {
		t.Fatal("trailing object accepted")
	}
}
func TestSharedLoginTokenExpiryBoundsSnapshot(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	auth := sharedLoginFixture(t, now)
	auth.OAuthExpiresAt = now.Add(10 * time.Minute).Format(time.RFC3339)
	value, err := makeSharedLogin(auth, now)
	if err != nil {
		t.Fatal(err)
	}
	if value.Account.APIKey != auth.APIKey || !value.ExpiresAt.Equal(now.Add(10*time.Minute)) {
		t.Fatal("token expiry not respected")
	}
}

func TestSharedLoginImportPreservesAccountsAndRefusesReplacement(t *testing.T) {
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
	t.Setenv("WENDY_SECRET_STORE", "file")
	now := time.Now().UTC().Truncate(time.Second)
	snapshot, err := makeSharedLogin(sharedLoginFixture(t, now), now)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	existing := config.AuthConfig{Name: "existing", CloudGRPC: "other.invalid:443"}
	if err := config.Save(&config.Config{Auth: []config.AuthConfig{existing}, CurrentContext: existing.Name, DefaultDevice: "preserved.local"}); err != nil {
		t.Fatal(err)
	}
	b := &Backend{}
	if err := b.ImportSharedLogin(data); err != nil {
		t.Fatal(err)
	}
	if err := b.ImportSharedLogin(data); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Auth) != 2 || loaded.Auth[0].Name != existing.Name || loaded.DefaultDevice != "preserved.local" {
		t.Fatal("import changed unrelated configuration or duplicated context")
	}
	protected := snapshot.Account
	protected.Name = "browser-login"
	protected.RefreshToken = sharedLoginDummy(t)
	if err := config.Save(&config.Config{Auth: []config.AuthConfig{protected}, CurrentContext: protected.Name}); err != nil {
		t.Fatal(err)
	}
	if err := b.ImportSharedLogin(data); err == nil {
		t.Fatal("import overwrote a device-local login")
	}
	loaded, err = config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Auth) != 1 || loaded.CurrentContext != protected.Name || loaded.Auth[0].RefreshToken != protected.RefreshToken {
		t.Fatal("rejected import mutated the existing login")
	}
}
