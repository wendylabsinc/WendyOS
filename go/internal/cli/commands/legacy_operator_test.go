package commands

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

func TestLegacyDeliveryAcceptsTransitionalOperator(t *testing.T) {
	actual := certs.WendyIdentity{OrgID: 64, EntityType: certs.EntityAsset, EntityID: "674"}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	legacy, _ := url.Parse("urn:wendy:org:64:user:operator")
	transitional, _ := url.Parse("spiffe://wendy.sh/tenant/22f35004-d638-4b5c-869f-b815974d6eae/service/user-operator")
	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), URIs: []*url.URL{legacy, transitional}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	operator := &config.CertificateInfo{OrganizationID: 64, UserID: "operator", PrincipalURI: transitional.String(), PemCertificate: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}
	conn, err := grpcclient.ConnectSessionProxy(context.Background(), filepath.Join(t.TempDir(), "unused.sock"), "target", "target:50052", operator, actual)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := verifiedLegacyBuildDelivery(conn, nil, &pb.ProvisionedResponse{OrganizationId: 64, AssetId: 674}); err != nil {
		t.Fatalf("same-org transitional legacy operator refused: %v", err)
	}
	cfg := &config.Config{Auth: []config.AuthConfig{{CloudGRPC: "legacy:443", Certificates: []config.CertificateInfo{*operator}}}}
	device := &pb.ProvisionedResponse{CloudHost: "legacy:443", OrganizationId: 64, AssetId: 674}
	selected, err := deploymentAuth(cfg, device)
	if err != nil {
		t.Fatal(err)
	}
	if selected.Certificates[0].PrincipalURI != "" || certXFCC(selected.Certificates[0]) != "URI=urn:wendy:org:64:user:operator" {
		t.Fatal("selected legacy registration did not retain numeric authentication")
	}
	if cfg.Auth[0].Certificates[0].PrincipalURI != transitional.String() {
		t.Fatal("saved session was modified")
	}
	template.URIs = []*url.URL{transitional}
	pkiOnly, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []config.CertificateInfo{
		{OrganizationID: 64, UserID: "operator", PemCertificate: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: pkiOnly}))},
		{OrganizationID: 65, UserID: "operator", PemCertificate: operator.PemCertificate},
		{OrganizationID: 64, UserID: "other", PemCertificate: operator.PemCertificate},
		{OrganizationID: 64, UserID: "operator", PrincipalURI: "spiffe://wendy.sh/tenant/22f35004-d638-4b5c-869f-b815974d6eae/operator/other"},
		{OrganizationID: 64, AssetID: 674, PemCertificate: operator.PemCertificate},
	} {
		if legacyOperatorForOrg(bad, 64) {
			t.Fatalf("accepted unrelated operator: org=%d user=%s", bad.OrganizationID, bad.UserID)
		}
	}

}
