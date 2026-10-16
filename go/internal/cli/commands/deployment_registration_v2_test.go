package commands

import (
	"context"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const registrationTenantV2 = "11111111-1111-4111-8111-111111111111"
const registrationDeviceV2 = "22222222-2222-4222-8222-222222222222"

func registrationOperatorV2(t *testing.T, tenant, entity string) (config.CertificateInfo, *mldsa.PrivateKey) {
	t.Helper()
	key, err := mldsa.GenerateKey(mldsa.MLDSA65())
	if err != nil {
		t.Fatal(err)
	}
	uri, err := url.Parse("spiffe://wendy.sh/tenant/" + tenant + "/" + entity + "/33333333-3333-4333-8333-333333333333")
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), URIs: []*url.URL{uri}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	raw, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	priv, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return config.CertificateInfo{PemCertificate: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: raw})), PemPrivateKey: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: priv})), PrincipalURI: uri.String()}, key
}
func TestV2DeploymentAcceptsUUIDEnrollmentWithZeroLegacyIDs(t *testing.T) {
	device := &agentpbv2.ProvisionedResponse{CloudHost: "api.dev.example:443", PrincipalUri: "spiffe://wendy.sh/tenant/" + registrationTenantV2 + "/device/" + registrationDeviceV2}
	identity, err := deploymentV2Identity(device)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := registrationOperatorV2(t, registrationTenantV2, "operator")
	cfg := &config.Config{Auth: []config.AuthConfig{{CloudGRPC: device.CloudHost, Certificates: []config.CertificateInfo{cert}}}}
	if _, err := deploymentV2Auth(cfg, device.CloudHost, identity.TenantUUID); err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"", "attacker.example:443"} {
		if _, err := deploymentV2Auth(cfg, host, identity.TenantUUID); err == nil {
			t.Fatal("untrusted host accepted")
		}
	}
	if _, err := deploymentV2Auth(cfg, device.CloudHost, "44444444-4444-4444-8444-444444444444"); err == nil {
		t.Fatal("zero-org wildcard matched another tenant")
	}
	device.PrincipalUri = "spiffe://wendy.sh/tenant/" + registrationTenantV2 + "/operator/" + registrationDeviceV2
	if _, err := deploymentV2Identity(device); err == nil {
		t.Fatal("operator used as device")
	}
	deviceCert, _ := registrationOperatorV2(t, registrationTenantV2, "device")
	cfg.Auth[0].Certificates = []config.CertificateInfo{deviceCert}
	if _, err := deploymentV2Auth(cfg, device.CloudHost, registrationTenantV2); err == nil {
		t.Fatal("device used as operator")
	}
}

type v2CatalogFixture struct {
	app   *cloudpbv2.App
	err   error
	reads int
}

func (f *v2CatalogFixture) GetApp(context.Context, *cloudpbv2.GetAppRequest, ...grpc.CallOption) (*cloudpbv2.App, error) {
	f.reads++
	return f.app, f.err
}
func TestV2CatalogPreservesExistingGrantsAndFailsClosed(t *testing.T) {
	f := &v2CatalogFixture{app: &cloudpbv2.App{Id: "app", OrganizationId: registrationTenantV2, Name: "edited", CanSendNotifications: true}}
	writes := 0
	upsert := func(context.Context, *cloudpbv2.UpsertAppRequest) (*cloudpbv2.App, error) { writes++; return nil, nil }
	if err := registerV2AppCatalog(context.Background(), f, upsert, registrationTenantV2, []string{"app", "app"}); err != nil {
		t.Fatal(err)
	}
	if writes != 0 || f.reads != 1 {
		t.Fatal("existing metadata/grants were overwritten or duplicate registered")
	}
	f.app.OrganizationId = "another-tenant"
	if err := registerV2AppCatalog(context.Background(), f, upsert, registrationTenantV2, []string{"app"}); err == nil {
		t.Fatal("mismatched app accepted")
	}
	f.err = status.Error(codes.PermissionDenied, "denied")
	if err := registerV2AppCatalog(context.Background(), f, upsert, registrationTenantV2, []string{"app"}); err == nil || writes != 0 {
		t.Fatal("authorization failure bypassed")
	}
	f.err = status.Error(codes.NotFound, "missing")
	writes = 0
	upsert = func(_ context.Context, r *cloudpbv2.UpsertAppRequest) (*cloudpbv2.App, error) {
		writes++
		if r.GetOrganizationId() != registrationTenantV2 || r.GetName() != "campaign-name" || r.Details != nil {
			t.Fatal("creation changed identity or notification grant")
		}
		return &cloudpbv2.App{Id: r.Id, OrganizationId: r.OrganizationId}, nil
	}
	if err := registerV2AppCatalog(context.Background(), f, upsert, registrationTenantV2, []string{"campaign:campaign-name", "campaign:campaign-name"}); err != nil || writes != 1 {
		t.Fatalf("missing app: writes=%d err=%v", writes, err)
	}
}

type signedUpsertFixture struct {
	t      *testing.T
	key    *mldsa.PrivateKey
	called bool
}

func (f *signedUpsertFixture) Invoke(_ context.Context, method string, args, reply any, _ ...grpc.CallOption) error {
	if method != cloudpbv2.AppService_UpsertApp_FullMethodName {
		return status.Error(codes.Unimplemented, "fixture has no leaf-registration cache")
	}
	signed, ok := args.(*cloudpbv2.SignedRequest)
	if !ok {
		f.t.Fatal("unsigned UpsertApp")
	}
	if signed.PayloadType != "wendycloud.v2.UpsertAppRequest" {
		f.t.Fatal("wrong signed payload type")
	}
	parts := strings.Split(string(signed.Signature), ".")
	if len(parts) != 3 {
		f.t.Fatal("missing JWS")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		f.t.Fatal(err)
	}
	if err := mldsa.Verify(f.key.Public().(*mldsa.PublicKey), []byte(parts[0]+"."+parts[1]), sig, nil); err != nil {
		f.t.Fatal(err)
	}
	claimsRaw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		f.t.Fatal(err)
	}
	var claims struct {
		Operation string `json:"operation"`
		Target    struct {
			Tenant   string `json:"tenant"`
			Resource string `json:"resource"`
		} `json:"target"`
	}
	if err := json.Unmarshal(claimsRaw, &claims); err != nil {
		f.t.Fatal(err)
	}
	if claims.Operation != "wendycloud.v2.AppService/UpsertApp" || claims.Target.Tenant != registrationTenantV2 || claims.Target.Resource != "org/"+registrationTenantV2+"/app/app" {
		f.t.Fatal("wrong signed authority")
	}
	var request cloudpbv2.UpsertAppRequest
	if err := proto.Unmarshal(signed.Payload, &request); err != nil {
		f.t.Fatal(err)
	}
	if request.GetOrganizationId() != registrationTenantV2 {
		f.t.Fatal("wrong signed tenant")
	}
	out := reply.(*cloudpbv2.App)
	out.Id = request.Id
	out.OrganizationId = request.OrganizationId
	f.called = true
	return nil
}
func (*signedUpsertFixture) NewStream(context.Context, *grpc.StreamDesc, string, ...grpc.CallOption) (grpc.ClientStream, error) {
	return nil, status.Error(codes.Unimplemented, "unused")
}
func TestV2CatalogUsesSignedOperatorUpsert(t *testing.T) {
	cert, key := registrationOperatorV2(t, registrationTenantV2, "operator")
	auth := &config.AuthConfig{Certificates: []config.CertificateInfo{cert}}
	f := &signedUpsertFixture{t: t, key: key}
	_, err := upsertV2DeploymentApp(context.Background(), f, auth, &cloudpbv2.UpsertAppRequest{Id: "app", OrganizationId: registrationTenantV2})
	if err != nil || !f.called {
		t.Fatalf("signed upsert: %v", err)
	}
	f.called = false
	if _, err := upsertV2DeploymentApp(context.Background(), f, auth, &cloudpbv2.UpsertAppRequest{Id: "app", OrganizationId: "44444444-4444-4444-8444-444444444444"}); err == nil || f.called {
		t.Fatal("signed cross-tenant mutation permitted")
	}
}
