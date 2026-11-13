package commands

import (
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

func meshTestCert(tenant, kind string) config.CertificateInfo {
	return config.CertificateInfo{PrincipalURI: "spiffe://wendy.sh/tenant/" + tenant + "/" + kind + "/alice", PemCertificate: "leaf", PemCertificateChain: "chain", PemPrivateKey: "key"}
}
func TestMeshSelectedOrganizationOnly(t *testing.T) {
	const first = "11111111-1111-4111-8111-111111111111"
	const second = "22222222-2222-4222-8222-222222222222"
	a, b := meshTestCert(first, "operator"), meshTestCert(second, "operator")
	cfg := &config.Config{CurrentContext: "b", Auth: []config.AuthConfig{{Name: "a", CloudGRPC: "cloud", Certificates: []config.CertificateInfo{a}}, {Name: "b", CloudGRPC: "cloud", Certificates: []config.CertificateInfo{b}}}}
	if got, ok := selectedMeshCertificate(cfg, ""); !ok || got.PrincipalURI != b.PrincipalURI {
		t.Fatal("selected another organization")
	}
	cfg.CurrentContext = "missing"
	if _, ok := selectedMeshCertificate(cfg, ""); ok {
		t.Fatal("stale selection joined an organization")
	}
	cfg.CurrentContext = ""
	if _, ok := selectedMeshCertificate(cfg, ""); ok {
		t.Fatal("ambiguous organization joined")
	}
	for _, certificate := range []config.CertificateInfo{{}, meshTestCert(first, "service"), {OrganizationID: 7, PemCertificate: "leaf", PemPrivateKey: "key", PemCertificateChain: "chain"}} {
		cfg = &config.Config{Auth: []config.AuthConfig{{Certificates: []config.CertificateInfo{certificate}}}}
		if _, ok := selectedMeshCertificate(cfg, ""); ok {
			t.Fatal("non-operator organization joined")
		}
	}
	if _, ok := selectedMeshCertificate(&config.Config{}, ""); ok {
		t.Fatal("logged out computer joined")
	}
}
func TestMeshPickerUsesMeshSelector(t *testing.T) {
	d := models.LANDevice{ID: "id", DisplayName: "robot", Hostname: "mesh:id", IPAddress: "fd45:b891:567f::1", Port: 50052, Mesh: true}
	if got := preferredLANAddress(d); got != "mesh:id" {
		t.Fatalf("host-network fallback: %s", got)
	}
	item := lanPickerItem(d, false, 0)
	if item.Type != "Mesh" || item.Address != "mesh:id" {
		t.Fatalf("wrong picker: %+v", item)
	}
}
