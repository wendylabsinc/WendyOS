package cloudrequest

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

const testTenant = "2558fd76-afc7-466e-9613-6b715296a526"

func testAuth(t *testing.T) (*config.AuthConfig, *ecdsa.PrivateKey, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	principal := "spiffe://wendy.sh/tenant/" + testTenant + "/operator/op-42"
	u, err := url.Parse(principal)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "op-42"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		URIs:         []*url.URL{u},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return &config.AuthConfig{
		OAuthIssuer: "https://auth.wendy.sh/realms/test",
		Certificates: []config.CertificateInfo{{
			PemCertificate: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
			PemPrivateKey:  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})),
			PrincipalURI:   principal,
		}},
	}, key, der
}

func TestSignerProducesCloudContractJWS(t *testing.T) {
	auth, key, leafDER := testAuth(t)
	signer, err := newSigner(auth)
	if err != nil {
		t.Fatalf("newSigner: %v", err)
	}
	fixed := time.Unix(1_784_659_200, 0)
	signer.now = func() time.Time { return fixed }

	interceptor := signer.unaryClientInterceptor()
	var gotEnvelope string
	ctx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs(
		"authorization", "Bearer access-token",
		metadataKey, "stale-envelope",
	))
	err = interceptor(
		ctx,
		cloudpb.CertificateService_CreateAssetEnrollmentToken_FullMethodName,
		&cloudpb.CreateAssetEnrollmentTokenRequest{OrganizationId: 7, Name: "edge-one"},
		nil,
		nil,
		func(ctx context.Context, _ string, _, _ any, _ *grpc.ClientConn, _ ...grpc.CallOption) error {
			md, ok := metadata.FromOutgoingContext(ctx)
			if !ok {
				t.Fatal("invoker received no outgoing metadata")
			}
			values := md.Get(metadataKey)
			if len(values) != 1 {
				t.Fatalf("signature metadata = %v", values)
			}
			if got := md.Get("authorization"); len(got) != 1 || got[0] != "Bearer access-token" {
				t.Fatalf("authorization metadata = %v", got)
			}
			gotEnvelope = values[0]
			return nil
		},
	)
	if err != nil {
		t.Fatalf("interceptor: %v", err)
	}

	segments := strings.Split(gotEnvelope, ".")
	if len(segments) != 3 {
		t.Fatalf("JWS has %d segments", len(segments))
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(segments[0])
	if err != nil {
		t.Fatalf("decode header: %v", err)
	}
	var header struct {
		Alg string   `json:"alg"`
		X5C []string `json:"x5c"`
	}
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		t.Fatalf("unmarshal header: %v", err)
	}
	if header.Alg != "ES256" || len(header.X5C) != 1 {
		t.Fatalf("header = %#v", header)
	}
	if header.X5C[0] != base64.StdEncoding.EncodeToString(leafDER) {
		t.Fatal("x5c does not contain the leaf DER")
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(segments[1])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if !strings.HasPrefix(string(payloadBytes), `{"aud":`) ||
		!strings.Contains(string(payloadBytes), `"target":{"resource":"org/7/enroll-asset-name/edge-one","tenant":"`+testTenant+`"}`) {
		t.Fatalf("payload is not in canonical member order: %s", payloadBytes)
	}
	var descriptor struct {
		Audience   string `json:"aud"`
		BodyDigest string `json:"body_sha256"`
		Expiry     int64  `json:"expiry"`
		IssuedAt   int64  `json:"iat"`
		Nonce      string `json:"nonce"`
		Operation  string `json:"operation"`
		Target     struct {
			Resource string `json:"resource"`
			Tenant   string `json:"tenant"`
		} `json:"target"`
	}
	if err := json.Unmarshal(payloadBytes, &descriptor); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if descriptor.Audience != brokerAudience || descriptor.BodyDigest != "47DEQpj8HBSa-_TImW-5JCeuQeRkm5NMpJWZG3hSuFU" {
		t.Fatalf("descriptor audience/body = %#v", descriptor)
	}
	if descriptor.IssuedAt != fixed.Unix() || descriptor.Expiry != fixed.Add(signatureTTL).Unix() {
		t.Fatalf("descriptor times = iat %d expiry %d", descriptor.IssuedAt, descriptor.Expiry)
	}
	if descriptor.Operation != "wendycloud.v1.CertificateService/CreateAssetEnrollmentToken" {
		t.Errorf("operation = %q", descriptor.Operation)
	}
	if descriptor.Target.Tenant != testTenant || descriptor.Target.Resource != "org/7/enroll-asset-name/edge-one" {
		t.Errorf("target = %#v", descriptor.Target)
	}
	if nonce, err := base64.RawURLEncoding.DecodeString(descriptor.Nonce); err != nil || len(nonce) != 32 {
		t.Errorf("nonce = %q, err %v", descriptor.Nonce, err)
	}

	sig, err := base64.RawURLEncoding.DecodeString(segments[2])
	if err != nil || len(sig) != 64 {
		t.Fatalf("signature length = %d, err %v", len(sig), err)
	}
	digest := sha256.Sum256([]byte(segments[0] + "." + segments[1]))
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(&key.PublicKey, digest[:], r, s) {
		t.Fatal("JWS signature does not verify")
	}
}

func TestSignedResourcesMatchCloudContract(t *testing.T) {
	tests := []struct {
		method string
		req    any
		want   string
	}{
		{cloudpb.AssetService_UpdateAsset_FullMethodName, &cloudpb.UpdateAssetRequest{Id: 41}, "asset/41"},
		{cloudpb.AssetService_DeleteAsset_FullMethodName, &cloudpb.DeleteAssetRequest{Id: 42}, "asset/42"},
		{cloudpb.CertificateService_RevokeCertificate_FullMethodName, &cloudpb.RevokeCertificateRequest{CertificateId: 43}, "certificate/43"},
		{cloudpb.CertificateService_CreateAssetEnrollmentToken_FullMethodName, &cloudpb.CreateAssetEnrollmentTokenRequest{OrganizationId: 7, Name: "pi-5"}, "org/7/enroll-asset-name/pi-5"},
	}
	for _, tt := range tests {
		got, required, err := signedResource(tt.method, tt.req)
		if err != nil {
			t.Errorf("signedResource(%s): %v", tt.method, err)
			continue
		}
		if !required || got != tt.want {
			t.Errorf("signedResource(%s) = %q, %v; want %q, true", tt.method, got, required, tt.want)
		}
	}
}

func TestInterceptorLeavesReadRPCUnsigned(t *testing.T) {
	auth, _, _ := testAuth(t)
	signer, err := newSigner(auth)
	if err != nil {
		t.Fatal(err)
	}
	err = signer.unaryClientInterceptor()(
		context.Background(),
		cloudpb.AssetService_GetAsset_FullMethodName,
		&cloudpb.GetAssetRequest{Id: 1},
		nil,
		nil,
		func(ctx context.Context, _ string, _, _ any, _ *grpc.ClientConn, _ ...grpc.CallOption) error {
			md, _ := metadata.FromOutgoingContext(ctx)
			if values := md.Get(metadataKey); len(values) != 0 {
				t.Fatalf("read RPC carried signature %v", values)
			}
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
}

func TestNewSignerSkipsNonOIDCSessions(t *testing.T) {
	auth, _, _ := testAuth(t)
	auth.OAuthIssuer = ""
	signer, err := newSigner(auth)
	if err != nil || signer != nil {
		t.Fatalf("newSigner = %#v, %v; want nil, nil", signer, err)
	}
}

func TestNewSignerRejectsNonOperatorPrincipal(t *testing.T) {
	auth, _, _ := testAuth(t)
	auth.Certificates[0].PrincipalURI = "urn:wendy:org:7:user:op-42"
	if _, err := newSigner(auth); err == nil {
		t.Fatal("newSigner accepted a non-operator principal")
	}
}

// A cloud-relayed session spells the same human "service/user-<id>" rather than
// "operator/<sub>" (AAA contract D17). Refusing it was WDY-2968's spelling bug:
// both are operator identities and both must sign.
func TestNewSignerAcceptsRelayedUserPrincipal(t *testing.T) {
	auth, _, _ := testAuth(t)
	auth.Certificates[0].PrincipalURI = "spiffe://wendy.sh/tenant/" + testTenant + "/service/user-42"
	signer, err := newSigner(auth)
	if err != nil {
		t.Fatalf("newSigner: %v", err)
	}
	if signer.tenantUUID != testTenant {
		t.Fatalf("tenantUUID = %q, want %q", signer.tenantUUID, testTenant)
	}
}

// A device leaf is not an actor that may sign a privileged cloud mutation.
func TestNewSignerRejectsDevicePrincipal(t *testing.T) {
	auth, _, _ := testAuth(t)
	auth.Certificates[0].PrincipalURI = "spiffe://wendy.sh/tenant/" + testTenant + "/device/thor-1"
	if _, err := newSigner(auth); err == nil {
		t.Fatal("newSigner accepted a device principal as an operator")
	}
}
