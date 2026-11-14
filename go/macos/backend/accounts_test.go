package backend

import (
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"testing"
)

func TestSavedAccountOrganizationsAreScopedToEachCertificate(t *testing.T) {
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
	t.Setenv("WENDY_SECRET_STORE", "file")
	const first = "11111111-1111-4111-8111-111111111111"
	const second = "22222222-2222-4222-8222-222222222222"
	cfg := &config.Config{CurrentContext: "first", Auth: []config.AuthConfig{
		{Name: "first", CloudGRPC: "cloud.example:443", Certificates: []config.CertificateInfo{{PrincipalURI: "spiffe://wendy.sh/tenant/" + first + "/operator/one"}}},
		{Name: "second", CloudGRPC: "other.example:443", Certificates: []config.CertificateInfo{{PrincipalURI: "spiffe://wendy.sh/tenant/" + second + "/operator/two"}}},
		{Name: "no-certificate", CloudGRPC: "cloud.example:443"},
	}}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	config.SetOrganizationName("cloud.example:443", first, "First")
	config.SetOrganizationName("cloud.example:443", second, "Wrong cloud")
	rows, err := savedAccountInfos()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d accounts", len(rows))
	}
	if rows[0].OrganizationID != first || rows[0].OrganizationName != "First" {
		t.Fatal("first account lost its scope")
	}
	if rows[1].OrganizationID != second || rows[1].OrganizationName != "" {
		t.Fatal("second account used another cloud's metadata")
	}
	if rows[2].OrganizationID != "" {
		t.Fatal("account without certificate acquired a scope")
	}
}
