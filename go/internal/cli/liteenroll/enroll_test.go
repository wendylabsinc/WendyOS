package liteenroll

import (
	"context"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/hex"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const tenant = "2558fd76-afc7-466e-9613-6b715296a526"

func authFixture(t *testing.T) *config.AuthConfig {
	t.Helper()
	key, err := mldsa.GenerateKey(mldsa.MLDSA65())
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse("spiffe://wendy.sh/tenant/" + tenant + "/operator/test")
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, URIs: []*url.URL{u}}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	private, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return &config.AuthConfig{PKIEndpoint: "https://identity.dev.pki.wendy.sh/v1/identity/certificate", Certificates: []config.CertificateInfo{{PrincipalURI: u.String(), PemCertificate: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), PemPrivateKey: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private}))}}}
}
func TestConfigUsesSelectedPKI(t *testing.T) {
	auth := authFixture(t)
	cfg, err := Config(auth, "lite-test", "", "", "broker.dev.example", 5055)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CsrUrl != "https://csr.dev.pki.wendy.sh/v1/"+tenant || cfg.TimeUrl != "roughtime" {
		t.Fatalf("unexpected endpoint derivation: %v", cfg)
	}
	cfg, err = Config(auth, "lite-test", "https://csr.local:8443/v1/"+tenant, "https://tsa.local/time", "broker.local", 9443)
	if err != nil || cfg.BrokerPort != 9443 {
		t.Fatalf("self-hosted config: %v %v", cfg, err)
	}
}
func TestConfigDefaultsBrokerToAgentHostname(t *testing.T) {
	auth := authFixture(t)
	for _, tc := range []struct{ cloud, override, want string }{
		{"api.dev.wendy.sh:443", "", "devices.dev.wendy.sh"},
		{"https://api.wendy.sh", "", "devices.wendy.sh"},
		{"api.wendy.dev:443", "", "devices.wendy.dev"},
		{"https://cloud.example:9443", "", "cloud.example"},
		{"", "broker.example", "broker.example"},
	} {
		t.Run(tc.cloud+"/"+tc.override, func(t *testing.T) {
			auth.CloudGRPC = tc.cloud
			cfg, err := Config(auth, "lite-test", "", "", tc.override, 5055)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.BrokerHost != tc.want || cfg.BrokerPort != 5055 {
				t.Fatalf("broker = %s:%d, want %s:5055", cfg.BrokerHost, cfg.BrokerPort, tc.want)
			}
		})
	}
	auth.CloudGRPC = ""
	if _, err := Config(auth, "lite-test", "", "", "", 5055); err == nil {
		t.Fatal("accepted missing Cloud endpoint and broker override")
	}
}

func TestConfigRejectsUnsafeEndpointsAndIdentity(t *testing.T) {
	auth := authFixture(t)
	for _, csr := range []string{"http://csr.local/v1/" + tenant, "https://user@csr.local/v1/" + tenant, "https://csr.local/v1/other", "https://csr.local/v1/" + tenant + "?x=y"} {
		if _, err := Config(auth, "lite-test", csr, "", "broker", 5055); err == nil {
			t.Errorf("accepted %q", csr)
		}
	}
	for _, device := range []string{"", "../x/y", strings.Repeat("x", 65)} {
		if _, err := Config(auth, device, "", "", "broker", 5055); err == nil {
			t.Errorf("accepted device %q", device)
		}
	}
	auth.Certificates[0].PrincipalURI = "spiffe://wendy.sh/tenant/" + tenant + "/device/other"
	if _, err := Config(auth, "lite-test", "", "", "broker", 5055); err == nil {
		t.Fatal("device identity authorized enrollment")
	}
}
func TestSignedTimeBindsDeviceNonce(t *testing.T) {
	nonce := make([]byte, 32)
	for i := range nonce {
		nonce[i] = byte(i + 1)
	}
	for _, first := range []byte{0x01, 0x00, 0x80, 0xff} {
		nonce[0] = first
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				Version int
				Imprint struct {
					Algorithm pkix.AlgorithmIdentifier
					Digest    []byte
				}
				Nonce   *big.Int
				CertReq bool
			}
			data := make([]byte, r.ContentLength)
			_, _ = io.ReadFull(r.Body, data)
			rest, err := asn1.Unmarshal(data, &req)
			if err != nil || len(rest) != 0 {
				t.Errorf("malformed request: %v", err)
				w.WriteHeader(400)
				return
			}
			sum := sha256.Sum256(nonce)
			if req.Version != 1 || !req.CertReq || !req.Imprint.Algorithm.Algorithm.Equal(asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 1}) || string(req.Imprint.Digest) != string(sum[:]) || req.Nonce.Cmp(new(big.Int).SetBytes(nonce)) != 0 {
				t.Errorf("timestamp did not bind device nonce")
			}
			w.Write([]byte("signed-response"))
		}))
		defer server.Close()
		body, err := SignedTime(context.Background(), server.Client(), server.URL, hex.EncodeToString(nonce))
		if err != nil || string(body) != "signed-response" {
			t.Fatalf("response %q %v", body, err)
		}
		// A system-trust client must reject this untrusted HTTPS server.
		if _, err = SignedTime(context.Background(), http.DefaultClient, server.URL, hex.EncodeToString(nonce)); err == nil {
			t.Fatal("untrusted TLS accepted")
		}
	}
}
func TestSignedTimeRejectsRedirectOversizeAndBadNonce(t *testing.T) {
	nonce := strings.Repeat("01", 32)
	for _, mode := range []string{"redirect", "oversize", "empty", "failure"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch mode {
				case "redirect":
					http.Redirect(w, r, "https://elsewhere.invalid", 302)
				case "oversize":
					w.Write([]byte(strings.Repeat("x", 65537)))
				case "failure":
					w.WriteHeader(503)
				default:
				}
			}))
			defer server.Close()
			if _, err := SignedTime(context.Background(), server.Client(), server.URL, nonce); err == nil {
				t.Fatal("bad response accepted")
			}
		})
	}
	if _, err := SignedTime(context.Background(), http.DefaultClient, "https://unused.invalid", "ff"); err == nil {
		t.Fatal("bad nonce accepted")
	}
}

type fakeEnrollment struct {
	grpc.ClientConnInterface
	reply   *cloudpb.EnrollDeviceResponse
	request *cloudpb.EnrollDeviceRequest
}

func (f *fakeEnrollment) Invoke(_ context.Context, method string, args, reply any, _ ...grpc.CallOption) error {
	if method == cloudpb.OperatorSessionService_RegisterOperatorLeaf_FullMethodName {
		return status.Error(codes.Unimplemented, "registration unavailable")
	}
	if method != cloudpb.DeviceEnrollmentService_EnrollDevice_FullMethodName {
		return status.Error(codes.Unimplemented, "unexpected method")
	}
	signed, ok := args.(*cloudpb.SignedRequest)
	if !ok || len(signed.GetSignature()) == 0 || signed.GetPayloadType() != "wendycloud.v2.EnrollDeviceRequest" {
		return status.Error(codes.InvalidArgument, "expected signed enrollment request")
	}
	f.request = new(cloudpb.EnrollDeviceRequest)
	if err := proto.Unmarshal(signed.GetPayload(), f.request); err != nil {
		return err
	}
	proto.Merge(reply.(*cloudpb.EnrollDeviceResponse), f.reply)
	return nil
}
func TestMintTierCCredential(t *testing.T) {
	auth := authFixture(t)
	cfg, err := Config(auth, "lite-test", "", "", "broker", 5055)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeEnrollment{reply: &cloudpb.EnrollDeviceResponse{CredentialKind: "enrollment_token", TokenValue: "single-use-test", AssetId: "42", ExpiresAt: time.Now().Add(4 * time.Minute).Format(time.RFC3339)}}
	asset, err := Mint(context.Background(), f, auth, cfg, "lite-test")
	if err != nil || asset != "42" || cfg.Token != "single-use-test" {
		t.Fatalf("mint: %s %v", asset, err)
	}
	if f.request.DeviceClass != cloudpb.DeviceClass_DEVICE_CLASS_C || len(f.request.EnrollmentRequestJws) == 0 {
		t.Fatal("wrong enrollment authority")
	}
	for _, mutate := range []func(){func() { f.reply.CredentialKind = "eab" }, func() {
		f.reply.CredentialKind = "enrollment_token"
		f.reply.ExpiresAt = time.Now().Add(-time.Second).Format(time.RFC3339)
	}, func() { f.reply.ExpiresAt = time.Now().Add(time.Minute).Format(time.RFC3339); f.reply.TokenValue = "" }} {
		mutate()
		cfg.Token = ""
		if asset, err := Mint(context.Background(), f, auth, cfg, "lite-test"); err == nil || cfg.Token != "" || asset != "42" {
			t.Fatal("invalid credential accepted")
		}
	}
}
