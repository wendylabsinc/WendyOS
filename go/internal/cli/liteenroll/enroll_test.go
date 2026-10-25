package liteenroll

import (
	"context"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/cli/cloudenroll"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const deviceID = "00000000-0000-4000-8000-000000000004"

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
	cfg, err := Config(auth, deviceID, "", "https://tsa.local/time", "broker.dev.example", 5055)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CsrUrl != "https://csr.dev.pki.wendy.sh/v1/"+tenant || cfg.TimeUrl != "https://tsa.local/time" {
		t.Fatalf("unexpected endpoint derivation: %v", cfg)
	}
	cfg, err = Config(auth, deviceID, "https://csr.local:8443/v1/"+tenant, "https://tsa.local/time", "broker.local", 9443)
	if err != nil || cfg.BrokerPort != 9443 {
		t.Fatalf("self-hosted config: %v %v", cfg, err)
	}
}
func TestConfigDefaultsLiteBrokerForSelectedCloud(t *testing.T) {
	auth := authFixture(t)
	for _, tc := range []struct {
		cloud, override, want string
		port                  uint32
	}{
		{"api.dev.wendy.sh", "", "tunnel.dev.wendy.dev", 5055},
		{"api.dev.wendy.dev", "", "tunnel.dev.wendy.dev", 5055},
		{"api.wendy.sh", "", "tunnel.wendy.dev", 5055},
		{"api.wendy.dev", "", "tunnel.wendy.dev", 5055},
		{"api.dev.wendy.sh:443", "", "tunnel.dev.wendy.dev", 5055},
		{"https://api.dev.wendy.sh", "", "tunnel.dev.wendy.dev", 5055},
		{"api.dev.wendy.dev:443", "", "tunnel.dev.wendy.dev", 5055},
		{"https://api.dev.wendy.dev", "", "tunnel.dev.wendy.dev", 5055},
		{"https://api.wendy.sh", "", "tunnel.wendy.dev", 5055},
		{"api.wendy.sh:443", "", "tunnel.wendy.dev", 5055},
		{"api.wendy.dev:443", "", "tunnel.wendy.dev", 5055},
		{"https://api.wendy.dev", "", "tunnel.wendy.dev", 5055},
		{"https://cloud.example:9443", "", "cloud.example", 5055},
		{"api.dev.wendy.sh:443", "broker.example", "broker.example", 5055},
		{"api.dev.wendy.sh:443", "devices.dev.wendy.sh", "devices.dev.wendy.sh", 5055},
		{"api.wendy.dev:443", "broker.example", "broker.example", 9443},
		{"api.wendy.dev:443", "devices.wendy.dev", "devices.wendy.dev", 5055},
		{"api.wendy.sh:443", "devices.wendy.sh", "devices.wendy.sh", 5055},
		{"api.wendy.dev:443", "", "tunnel.wendy.dev", 9443},
		{"https://cloud.example:9443", "", "cloud.example", 7443},
		{"", "broker.example", "broker.example", 5055},
	} {
		t.Run(tc.cloud+"/"+tc.override, func(t *testing.T) {
			auth.CloudGRPC = tc.cloud
			cfg, err := Config(auth, deviceID, "", "https://tsa.local/time", tc.override, tc.port)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.BrokerHost != tc.want || cfg.BrokerPort != tc.port {
				t.Fatalf("broker = %s:%d, want %s:%d", cfg.BrokerHost, cfg.BrokerPort, tc.want, tc.port)
			}
		})
	}
	auth.CloudGRPC = ""
	if _, err := Config(auth, deviceID, "", "https://tsa.local/time", "", 5055); err == nil {
		t.Fatal("accepted missing Cloud endpoint and broker override")
	}
}

func TestConfigRejectsUnsafeEndpointsAndIdentity(t *testing.T) {
	auth := authFixture(t)
	for _, csr := range []string{"http://csr.local/v1/" + tenant, "https://user@csr.local/v1/" + tenant, "https://csr.local/v1/other", "https://csr.local/v1/" + tenant + "?x=y"} {
		if _, err := Config(auth, deviceID, csr, "https://tsa.local/time", "broker", 5055); err == nil {
			t.Errorf("accepted %q", csr)
		}
	}
	for _, device := range []string{"", "../x/y", strings.Repeat("x", 65), "lite-441bf6804824", "441bf6804824", "00000000-0000-4000-8000-00000000000A", "00000000000040008000000000000004", "urn:uuid:" + deviceID, "{" + deviceID + "}"} {
		if _, err := Config(auth, device, "", "", "broker", 5055); err == nil {
			t.Errorf("accepted device %q", device)
		}
	}
	auth.Certificates[0].PrincipalURI = "spiffe://wendy.sh/tenant/" + tenant + "/device/other"
	if _, err := Config(auth, deviceID, "", "https://tsa.local/time", "broker", 5055); err == nil {
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
	signed  *cloudpb.SignedRequest
	md      metadata.MD
	// collide, when set, answers the wendy-colliding-field Cloud would name
	// for a device UUID ("untagged" refuses with no trailer), or "" to accept.
	collide func(deviceID string) string
	seen    []string
}

// Invoke adopts the requested device UUID as the asset id, as Cloud does,
// unless reply names another asset.
func (f *fakeEnrollment) Invoke(ctx context.Context, method string, args, reply any, opts ...grpc.CallOption) error {
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
	f.signed = signed
	f.md, _ = metadata.FromOutgoingContext(ctx)
	f.request = new(cloudpb.EnrollDeviceRequest)
	if err := proto.Unmarshal(signed.GetPayload(), f.request); err != nil {
		return err
	}
	f.seen = append(f.seen, f.request.GetDeviceId())
	if f.collide != nil {
		if field := f.collide(f.request.GetDeviceId()); field != "" {
			for _, opt := range opts {
				if trailer, ok := opt.(grpc.TrailerCallOption); ok && field != "untagged" {
					*trailer.TrailerAddr = metadata.Pairs("wendy-colliding-field", field)
				}
			}
			return status.Error(codes.AlreadyExists, "already in use")
		}
	}
	out := reply.(*cloudpb.EnrollDeviceResponse)
	out.AssetId = f.request.GetDeviceId()
	proto.Merge(out, f.reply)
	return nil
}
func TestMintTierCCredential(t *testing.T) {
	auth := authFixture(t)
	cfg, err := Config(auth, deviceID, "", "https://tsa.local/time", "broker", 5055)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeEnrollment{reply: &cloudpb.EnrollDeviceResponse{CredentialKind: "enrollment_token", TokenValue: "single-use-test", ExpiresAt: time.Now().Add(4 * time.Minute).Format(time.RFC3339)}}
	asset, err := Mint(context.Background(), f, auth, cfg, "lite-test", nil, TrustProfile{})
	if err != nil || asset != deviceID || cfg.Token != "single-use-test" {
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
		if asset, err := Mint(context.Background(), f, auth, cfg, "lite-test", nil, TrustProfile{}); err == nil || cfg.Token != "" || asset != deviceID {
			t.Fatal("invalid credential accepted")
		}
	}
}

func validTierCReply() *cloudpb.EnrollDeviceResponse {
	return &cloudpb.EnrollDeviceResponse{CredentialKind: "enrollment_token", TokenValue: "single-use-test", ExpiresAt: time.Now().Add(4 * time.Minute).Format(time.RFC3339)}
}

// signedDeviceID reads device_id from the operator-signed enrollment JWS.
func signedDeviceID(t *testing.T, jws []byte) string {
	t.Helper()
	parts := strings.Split(string(jws), ".")
	if len(parts) != 3 {
		t.Fatal("enrollment request is not a compact JWS")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims struct {
		DeviceID string `json:"device_id"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	return claims.DeviceID
}

func TestMintRefusesAssetOtherThanMintedUUID(t *testing.T) {
	auth := authFixture(t)
	cfg, err := Config(auth, deviceID, "", "https://tsa.local/time", "broker", 5055)
	if err != nil {
		t.Fatal(err)
	}
	reply := validTierCReply()
	reply.AssetId = "11111111-1111-4111-8111-111111111111"
	asset, err := Mint(context.Background(), &fakeEnrollment{reply: reply}, auth, cfg, "desk-board", nil, TrustProfile{})
	if err == nil || asset != reply.AssetId || cfg.Token != "" {
		t.Fatalf("accepted asset %q for device %s: %v", asset, cfg.DeviceId, err)
	}
}

func TestMintRegeneratesTakenLiteDeviceUUID(t *testing.T) {
	for _, field := range []string{"device_id", "mesh_suffix"} {
		t.Run(field, func(t *testing.T) {
			auth := authFixture(t)
			cfg, err := Config(auth, deviceID, "", "https://tsa.local/time", "broker", 5055)
			if err != nil {
				t.Fatal(err)
			}
			f := &fakeEnrollment{reply: validTierCReply(), collide: func(id string) string {
				if id == deviceID {
					return field
				}
				return ""
			}}
			asset, err := Mint(context.Background(), f, auth, cfg, "desk-board", nil, TrustProfile{})
			if err != nil || asset != cfg.DeviceId || cfg.Token != "single-use-test" {
				t.Fatalf("mint after collision: %s %v", asset, err)
			}
			if id, err := uuid.Parse(cfg.DeviceId); err != nil || id.String() != cfg.DeviceId || cfg.DeviceId == deviceID {
				t.Fatalf("delivered device id %q is not a fresh canonical UUID", cfg.DeviceId)
			}
			if len(f.seen) != 2 || f.seen[0] != deviceID || f.seen[1] != cfg.DeviceId {
				t.Fatalf("Cloud saw %v, want the original then the delivered UUID", f.seen)
			}
			if got := signedDeviceID(t, f.request.GetEnrollmentRequestJws()); got != cfg.DeviceId {
				t.Fatalf("operator signed device_id %q, want the regenerated %q", got, cfg.DeviceId)
			}
		})
	}
}

func TestMintLiteCollisionRetryIsBoundedAndNeverReuses(t *testing.T) {
	auth := authFixture(t)
	cfg, err := Config(auth, deviceID, "", "https://tsa.local/time", "broker", 5055)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeEnrollment{reply: validTierCReply(), collide: func(string) string { return "device_id" }}
	if _, err := Mint(context.Background(), f, auth, cfg, "desk-board", nil, TrustProfile{}); !errors.Is(err, cloudenroll.ErrDeviceUUIDTaken) {
		t.Fatalf("exhausted retries must surface the collision, got %v", err)
	}
	// The bound is the shared guard's: the first mint plus three regenerations.
	if len(f.seen) != 4 {
		t.Fatalf("attempts = %d, want 4", len(f.seen))
	}
	seen := map[string]bool{}
	for _, id := range f.seen {
		if seen[id] {
			t.Fatalf("reused device id %s", id)
		}
		seen[id] = true
	}
	if cfg.Token != "" {
		t.Fatal("a refused enrollment left a credential")
	}
}

func TestMintLiteDoesNotRegenerateOnNameRefusal(t *testing.T) {
	for _, field := range []string{"name", "untagged"} {
		t.Run(field, func(t *testing.T) {
			auth := authFixture(t)
			cfg, err := Config(auth, deviceID, "", "https://tsa.local/time", "broker", 5055)
			if err != nil {
				t.Fatal(err)
			}
			f := &fakeEnrollment{reply: validTierCReply(), collide: func(string) string { return field }}
			_, err = Mint(context.Background(), f, auth, cfg, "desk-board", nil, TrustProfile{})
			if errors.Is(err, cloudenroll.ErrDeviceUUIDTaken) || status.Code(err) != codes.AlreadyExists {
				t.Fatalf("name refusal must surface as-is, got %v", err)
			}
			if len(f.seen) != 1 || cfg.DeviceId != deviceID {
				t.Fatalf("name refusal regenerated the UUID: saw %v", f.seen)
			}
		})
	}
}

func TestMintIncludesLiteHardwareInSignedRequest(t *testing.T) {
	auth := authFixture(t)
	for _, tc := range []struct {
		name     string
		hardware *cloudpb.DeviceHardware
		want     *cloudpb.DeviceHardware
	}{
		{"unknown metadata", nil, &cloudpb.DeviceHardware{Os: proto.String("wendy-lite")}},
		{"reported metadata", &cloudpb.DeviceHardware{OsVersion: proto.String("1.2.3"), Architecture: proto.String("xtensa")}, &cloudpb.DeviceHardware{Os: proto.String("wendy-lite"), OsVersion: proto.String("1.2.3"), Architecture: proto.String("xtensa")}},
		{"canonical Lite type", &cloudpb.DeviceHardware{Os: proto.String("other")}, &cloudpb.DeviceHardware{Os: proto.String("wendy-lite")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Config(auth, deviceID, "", "https://tsa.local/time", "broker", 5055)
			if err != nil {
				t.Fatal(err)
			}
			before := proto.Clone(tc.hardware)
			f := &fakeEnrollment{reply: &cloudpb.EnrollDeviceResponse{CredentialKind: "enrollment_token", TokenValue: "single-use-test", ExpiresAt: time.Now().Add(4 * time.Minute).Format(time.RFC3339)}}
			if _, err := Mint(context.Background(), f, auth, cfg, "lite-test", tc.hardware, TrustProfile{}); err != nil {
				t.Fatal(err)
			}
			if !proto.Equal(f.request.GetHardware(), tc.want) {
				t.Fatalf("signed hardware = %v, want %v", f.request.GetHardware(), tc.want)
			}
			if !proto.Equal(tc.hardware, before) {
				t.Fatal("mint mutated the supplied hardware metadata")
			}
		})
	}
}

func TestConfigRequiresRFC3161Time(t *testing.T) {
	for _, endpoint := range []string{"roughtime", "http://tsa.local/time"} {
		if _, err := Config(authFixture(t), deviceID, "", endpoint, "broker", 5055); err == nil {
			t.Fatalf("accepted unsupported time endpoint %q", endpoint)
		}
	}
}

func TestConfigDefaultsSignedTimeAndPreservesOverride(t *testing.T) {
	auth := authFixture(t)
	for _, tc := range []struct{ name, endpoint, want string }{
		{"default", "", DefaultTimeURL},
		{"explicit default", DefaultTimeURL, DefaultTimeURL},
		{"custom", "https://tsa.example:8443/time", "https://tsa.example:8443/time"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Config(auth, deviceID, "", tc.endpoint, "broker", 5055)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.TimeUrl != tc.want {
				t.Fatalf("time endpoint = %q, want %q", cfg.TimeUrl, tc.want)
			}
		})
	}
}

func TestMintRetainsCallerMetadataAndSignedCorrelation(t *testing.T) {
	auth := authFixture(t)
	cfg, err := Config(auth, deviceID, "", "https://tsa.local/time", "broker", 5055)
	if err != nil {
		t.Fatal(err)
	}
	conn := &fakeEnrollment{reply: validTierCReply()}
	ctx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("x-correlation-id", "lite-flow", "authorization", "test-host-session"))
	if _, err = Mint(ctx, conn, auth, cfg, "board", nil, TrustProfile{}); err != nil {
		t.Fatal(err)
	}
	if conn.md.Get("x-correlation-id")[0] != "lite-flow" || conn.md.Get("authorization")[0] != "test-host-session" {
		t.Fatalf("metadata=%v", conn.md)
	}
	parts := strings.Split(string(conn.signed.GetSignature()), ".")
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	if err = json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	if claims["correlation_id"] != "lite-flow" {
		t.Fatalf("signed correlation=%v", claims["correlation_id"])
	}
}
