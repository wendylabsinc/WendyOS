package cloudenroll

import (
	"context"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/agent/acmeenroll"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
)

const testTenant = "2558fd76-afc7-466e-9613-6b715296a526"

// testAuth builds the operator credential MintEAB signs with: an ML-DSA-65
// leaf carrying a tenant-operator SPIFFE URI, same shape the CLI carries after
// 'wendy auth login'.
func testAuth(t *testing.T) *config.AuthConfig {
	t.Helper()
	key, err := mldsa.GenerateKey(mldsa.MLDSA65())
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
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
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
	}
}

func testEnrollmentConfig(t *testing.T, auth *config.AuthConfig, deviceID string) acmeenroll.Config {
	t.Helper()
	cfg, err := EnrollmentConfig(auth, deviceID, "https://acme.test/"+testTenant+"/acme/directory")
	if err != nil {
		t.Fatalf("EnrollmentConfig: %v", err)
	}
	return cfg
}

// meshSuffix is the last 10 bytes of a canonical UUID -- the part of the
// device identity WendyOS turns into a mesh address.
func meshSuffix(t *testing.T, deviceID string) string {
	t.Helper()
	u, err := uuid.Parse(deviceID)
	if err != nil {
		t.Fatalf("device id %q is not a UUID: %v", deviceID, err)
	}
	return strings.ToLower(u.String())[24:]
}

// fakeEnrollment stands in for Cloud's DeviceEnrollmentService. collide
// answers the field name Cloud would name in the wendy-colliding-field
// trailer, or "" to accept the enrollment and mint the EAB.
type fakeEnrollment struct {
	cloudpbv2.UnimplementedDeviceEnrollmentServiceServer
	collide func(deviceID string) string
	seen    []string
}

func (f *fakeEnrollment) EnrollDevice(ctx context.Context, envelope *cloudpbv2.SignedRequest) (*cloudpbv2.EnrollDeviceResponse, error) {
	req := &cloudpbv2.EnrollDeviceRequest{}
	if err := proto.Unmarshal(envelope.GetPayload(), req); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	f.seen = append(f.seen, req.GetDeviceId())
	if field := f.collide(req.GetDeviceId()); field != "" {
		if err := grpc.SetTrailer(ctx, metadata.Pairs("wendy-colliding-field", field)); err != nil {
			return nil, err
		}
		return nil, status.Error(codes.AlreadyExists, "device_id: this device UUID's mesh address is already claimed")
	}
	return &cloudpbv2.EnrollDeviceResponse{
		CredentialKind: "eab",
		EabKeyId:       "eab-key-1",
		EabHmacKey:     strings.Repeat("ab", 32),
		AssetId:        "11111111-2222-3333-4444-555555555555",
	}, nil
}

func dialFake(t *testing.T, fake *fakeEnrollment) *grpc.ClientConn {
	t.Helper()
	l := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	cloudpbv2.RegisterDeviceEnrollmentServiceServer(srv, fake)
	go srv.Serve(l)
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return l.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func TestMintEABMeshRetrySubstitutesFreshUUIDOnCollision(t *testing.T) {
	auth := testAuth(t)
	original := uuid.NewString()
	cfg := testEnrollmentConfig(t, auth, original)

	fake := &fakeEnrollment{collide: func(deviceID string) string {
		if deviceID == original {
			return "mesh_suffix"
		}
		return ""
	}}
	conn := dialFake(t, fake)

	var notices []string
	out, assetID, err := MintEABWithMeshRetry(context.Background(), conn, auth, cfg, "edge-one", nil, 3,
		func(format string, args ...any) { notices = append(notices, strings.TrimSpace(fmt.Sprintf(format, args...))) })
	if err != nil {
		t.Fatalf("retry after collision: %v", err)
	}
	if out.DeviceID == original {
		t.Fatalf("device UUID was not substituted after a mesh collision")
	}
	if _, err := uuid.Parse(out.DeviceID); err != nil {
		t.Fatalf("substituted device id %q is not a UUID", out.DeviceID)
	}
	if meshSuffix(t, out.DeviceID) == meshSuffix(t, original) {
		t.Fatalf("substitute %s carries the colliding suffix", out.DeviceID)
	}
	if assetID != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("asset id = %q", assetID)
	}
	if len(fake.seen) != 2 || fake.seen[0] != original || fake.seen[1] != out.DeviceID {
		t.Fatalf("Cloud saw %v, want the original then the substitute", fake.seen)
	}
	if len(notices) != 1 ||
		!strings.Contains(notices[0], original) ||
		!strings.Contains(notices[0], out.DeviceID) ||
		!strings.Contains(notices[0], "(1/3)") {
		t.Fatalf("substitution was not reported to the operator: %q", notices)
	}
}

func TestMintEABMeshRetryIsBoundedAndReportsCollision(t *testing.T) {
	auth := testAuth(t)
	cfg := testEnrollmentConfig(t, auth, uuid.NewString())

	fake := &fakeEnrollment{collide: func(string) string { return "mesh_suffix" }}
	conn := dialFake(t, fake)

	notices := 0
	_, _, err := MintEABWithMeshRetry(context.Background(), conn, auth, cfg, "edge-one", nil, 2,
		func(string, ...any) { notices++ })
	var collision *MeshSuffixCollision
	if !errors.As(err, &collision) {
		t.Fatalf("exhausted retries must surface the collision, got %v", err)
	}
	if len(fake.seen) != 3 {
		t.Fatalf("attempts = %d, want 3 (initial + 2 retries)", len(fake.seen))
	}
	if notices != 2 {
		t.Fatalf("substitutions reported = %d, want 2", notices)
	}
	seen := map[string]bool{}
	for i, id := range fake.seen {
		if seen[id] {
			t.Fatalf("attempt %d reused device id %s; every attempt must carry a fresh UUID", i+1, id)
		}
		seen[id] = true
	}
}

func TestMintEABMeshRetryIgnoresOtherRefusals(t *testing.T) {
	auth := testAuth(t)
	cfg := testEnrollmentConfig(t, auth, uuid.NewString())

	// A NAME collision is not a mesh collision: the name is reserved by the
	// first attempt, so retrying with a new identity would orphan it. The
	// refusal must surface untouched.
	fake := &fakeEnrollment{collide: func(string) string { return "name" }}
	conn := dialFake(t, fake)

	_, _, err := MintEABWithMeshRetry(context.Background(), conn, auth, cfg, "edge-one", nil, 3,
		func(string, ...any) { t.Fatal("a name collision must not trigger a mesh retry") })
	var collision *MeshSuffixCollision
	if errors.As(err, &collision) {
		t.Fatal("name collision mistyped as a mesh collision")
	}
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("refusal was not surfaced: %v", err)
	}
	if len(fake.seen) != 1 {
		t.Fatalf("Cloud saw %d enrollments, want 1", len(fake.seen))
	}
}

func TestMintEABMeshRetryPassesCleanEnrollmentThrough(t *testing.T) {
	auth := testAuth(t)
	cfg := testEnrollmentConfig(t, auth, uuid.NewString())

	fake := &fakeEnrollment{collide: func(string) string { return "" }}
	conn := dialFake(t, fake)

	out, _, err := MintEABWithMeshRetry(context.Background(), conn, auth, cfg, "edge-one", nil, 3,
		func(string, ...any) { t.Fatal("a clean enrollment must not notify") })
	if err != nil {
		t.Fatal(err)
	}
	if out.DeviceID != cfg.DeviceID {
		t.Fatal("clean enrollment changed the device UUID")
	}
	if len(fake.seen) != 1 {
		t.Fatalf("Cloud saw %d enrollments, want 1", len(fake.seen))
	}
}
