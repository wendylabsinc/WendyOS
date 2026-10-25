package liteenroll

import (
	"context"
	"crypto/sha256"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/wendy-core/core/cloud"
	cloudpb "github.com/wendylabsinc/WendyOS/go/wendy-core/core/lite/enrollment/internal/cloudpb"
	litepb "github.com/wendylabsinc/WendyOS/go/wendy-core/core/lite/pb"
	"github.com/wendylabsinc/WendyOS/go/wendy-core/platform"
	"google.golang.org/protobuf/proto"
)

const deviceID = "00000000-0000-4000-8000-000000000004"

func authFixture(t *testing.T) *Session {
	t.Helper()
	return &Session{HasCertificates: true, PKIEndpoint: "https://identity.dev.pki.wendy.sh/v1/identity/certificate", PrincipalURI: "spiffe://wendy.sh/tenant/" + tenant + "/operator/test"}
}

type testEntropy struct{ n byte }

func (e *testEntropy) RandomBytes(ctx context.Context, n int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	e.n++
	return bytesRepeat(e.n, n), nil
}
func bytesRepeat(b byte, n int) []byte {
	p := make([]byte, n)
	for i := range p {
		p[i] = b
	}
	return p
}

type testSigner struct{ platform.Signer }

func (testSigner) Algorithm() platform.SignatureAlgorithm { return platform.MLDSA65 }
func (testSigner) CertificateChain(context.Context) ([][]byte, error) {
	return [][]byte{[]byte("host-validated-leaf")}, nil
}
func (testSigner) Sign(context.Context, []byte) ([]byte, error) { return []byte("host-signature"), nil }
func testMint(ctx context.Context, t platform.Transport, a *Session, cfg *litepb.WendyConfEnrollment, name string, hw *DeviceHardware, p TrustProfile) (string, error) {
	e := &testEntropy{}
	signer, err := cloud.NewSigner(ctx, testSigner{}, tenant, e, cloud.SignerOptions{})
	if err != nil {
		return "", err
	}
	return Mint(ctx, t, signer, e, cfg, name, hw, p)
}
func rpcError(code int32, message string) error {
	return &platform.RPCStatusError{Code: platform.RPCStatusCode(code), Message: message}
}
func rpcCode(err error) int32 {
	var s *platform.RPCStatusError
	if errors.As(err, &s) {
		return int32(s.Code)
	}
	return 0
}

func testSignedTime(ctx context.Context, c *http.Client, url, nonce string) ([]byte, error) {
	return SignedTime(ctx, testHTTP{c}, url, nonce)
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
	auth.PrincipalURI = "spiffe://wendy.sh/tenant/" + tenant + "/device/other"
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
		body, err := testSignedTime(context.Background(), server.Client(), server.URL, hex.EncodeToString(nonce))
		if err != nil || string(body) != "signed-response" {
			t.Fatalf("response %q %v", body, err)
		}
		// A system-trust client must reject this untrusted HTTPS server.
		if _, err = testSignedTime(context.Background(), http.DefaultClient, server.URL, hex.EncodeToString(nonce)); err == nil {
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
			if _, err := testSignedTime(context.Background(), server.Client(), server.URL, nonce); err == nil {
				t.Fatal("bad response accepted")
			}
		})
	}
	if _, err := testSignedTime(context.Background(), http.DefaultClient, "https://unused.invalid", "ff"); err == nil {
		t.Fatal("bad nonce accepted")
	}
}

type fakeEnrollment struct {
	platform.Transport
	reply   *cloudpb.EnrollDeviceResponse
	request *cloudpb.EnrollDeviceRequest
	// collide, when set, answers the wendy-colliding-field Cloud would name
	// for a device UUID ("untagged" refuses with no trailer), or "" to accept.
	collide func(deviceID string) string
	seen    []string
}

// Invoke adopts the requested device UUID as the asset id, as Cloud does,
// unless reply names another asset.
func (f *fakeEnrollment) Unary(_ context.Context, req platform.RPCRequest) (platform.RPCResponse, error) {
	if req.Method == "/wendycloud.v2.OperatorSessionService/RegisterOperatorLeaf" {
		return platform.RPCResponse{}, rpcError(12, "unavailable")
	}
	if req.Method != "/wendycloud.v2.DeviceEnrollmentService/EnrollDevice" {
		return platform.RPCResponse{}, rpcError(12, "unexpected method")
	}
	signed := new(cloudpb.SignedRequest)
	if err := proto.Unmarshal(req.Message, signed); err != nil {
		return platform.RPCResponse{}, err
	}
	if len(signed.GetSignature()) == 0 || signed.GetPayloadType() != "wendycloud.v2.EnrollDeviceRequest" {
		return platform.RPCResponse{}, rpcError(3, "expected signed enrollment request")
	}
	f.request = new(cloudpb.EnrollDeviceRequest)
	if err := proto.Unmarshal(signed.GetPayload(), f.request); err != nil {
		return platform.RPCResponse{}, err
	}
	f.seen = append(f.seen, f.request.GetDeviceId())
	if f.collide != nil {
		if field := f.collide(f.request.GetDeviceId()); field != "" {
			var trailer platform.Metadata
			if field != "untagged" {
				trailer = platform.Metadata{{Name: "wendy-colliding-field", Value: []byte(field)}}
			}
			return platform.RPCResponse{TrailingMetadata: trailer}, rpcError(6, "already in use")
		}
	}
	reply := &cloudpb.EnrollDeviceResponse{AssetId: f.request.GetDeviceId()}
	proto.Merge(reply, f.reply)
	data, err := proto.Marshal(reply)
	return platform.RPCResponse{Message: data}, err
}
func TestMintTierCCredential(t *testing.T) {
	auth := authFixture(t)
	cfg, err := Config(auth, deviceID, "", "https://tsa.local/time", "broker", 5055)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeEnrollment{reply: &cloudpb.EnrollDeviceResponse{CredentialKind: "enrollment_token", TokenValue: "single-use-test", ExpiresAt: time.Now().Add(4 * time.Minute).Format(time.RFC3339)}}
	asset, err := testMint(context.Background(), f, auth, cfg, "lite-test", nil, testProfile())
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
		if asset, err := testMint(context.Background(), f, auth, cfg, "lite-test", nil, testProfile()); err == nil || cfg.Token != "" || asset != deviceID {
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
	asset, err := testMint(context.Background(), &fakeEnrollment{reply: reply}, auth, cfg, "desk-board", nil, testProfile())
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
			asset, err := testMint(context.Background(), f, auth, cfg, "desk-board", nil, testProfile())
			if err != nil || asset != cfg.DeviceId || cfg.Token != "single-use-test" {
				t.Fatalf("mint after collision: %s %v", asset, err)
			}
			if !canonicalUUID(cfg.DeviceId) || cfg.DeviceId == deviceID {
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
	if _, err := testMint(context.Background(), f, auth, cfg, "desk-board", nil, testProfile()); !errors.Is(err, ErrDeviceUUIDTaken) {
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
			_, err = testMint(context.Background(), f, auth, cfg, "desk-board", nil, testProfile())
			if errors.Is(err, ErrDeviceUUIDTaken) || rpcCode(err) != 6 {
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
			if _, err := testMint(context.Background(), f, auth, cfg, "lite-test", tc.hardware, testProfile()); err != nil {
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

func TestConfigPreservesServiceOperatorIdentities(t *testing.T) {
	for _, name := range []string{"build-bot", "user-ci", "operator/with/path"} {
		a := authFixture(t)
		a.PrincipalURI = "spiffe://wendy.sh/tenant/" + tenant + "/service/" + name
		if _, err := Config(a, deviceID, "", "", "broker", 5055); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"asset-device", "user-", ""} {
		a := authFixture(t)
		a.PrincipalURI = "spiffe://wendy.sh/tenant/" + tenant + "/service/" + name
		if _, err := Config(a, deviceID, "", "", "broker", 5055); err == nil {
			t.Fatal("accepted non-user service")
		}
	}
}
