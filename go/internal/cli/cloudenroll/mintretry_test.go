package cloudenroll

import (
	"context"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
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

// fakeEnrollment stands in for Cloud's DeviceEnrollmentService. collide
// answers the field name Cloud would name in the wendy-colliding-field
// trailer ("untagged" refuses with no trailer), or "" to accept the enrollment
// and mint the EAB.
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
		if field != "untagged" {
			if err := grpc.SetTrailer(ctx, metadata.Pairs("wendy-colliding-field", field)); err != nil {
				return nil, err
			}
		}
		return nil, status.Error(codes.AlreadyExists, "already in use")
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

func TestMintEABRegeneratesTakenDeviceUUID(t *testing.T) {
	for _, field := range []string{"device_id", "mesh_suffix"} {
		t.Run(field, func(t *testing.T) {
			auth := testAuth(t)
			original := uuid.NewString()
			cfg := testEnrollmentConfig(t, auth, original)

			fake := &fakeEnrollment{collide: func(deviceID string) string {
				if deviceID == original {
					return field
				}
				return ""
			}}
			out, assetID, err := MintEAB(context.Background(), dialFake(t, fake), auth, cfg, "edge-one", nil)
			if err != nil {
				t.Fatalf("mint after collision: %v", err)
			}
			if out.DeviceID == original {
				t.Fatal("taken device UUID was reused")
			}
			if _, err := uuid.Parse(out.DeviceID); err != nil {
				t.Fatalf("regenerated device id %q is not a UUID", out.DeviceID)
			}
			if out.DirectoryURL != cfg.DirectoryURL {
				t.Fatalf("directory = %q, want %q", out.DirectoryURL, cfg.DirectoryURL)
			}
			if assetID != "11111111-2222-3333-4444-555555555555" {
				t.Fatalf("asset id = %q", assetID)
			}
			if len(fake.seen) != 2 || fake.seen[0] != original || fake.seen[1] != out.DeviceID {
				t.Fatalf("Cloud saw %v, want the original then the regenerated UUID", fake.seen)
			}
		})
	}
}

func TestMintEABCollisionRetryIsBoundedAndNeverReuses(t *testing.T) {
	auth := testAuth(t)
	cfg := testEnrollmentConfig(t, auth, uuid.NewString())

	fake := &fakeEnrollment{collide: func(string) string { return "device_id" }}
	_, _, err := MintEAB(context.Background(), dialFake(t, fake), auth, cfg, "edge-one", nil)
	if !errors.Is(err, ErrDeviceUUIDTaken) {
		t.Fatalf("exhausted retries must surface the collision, got %v", err)
	}
	if len(fake.seen) != maxMintAttempts {
		t.Fatalf("attempts = %d, want %d", len(fake.seen), maxMintAttempts)
	}
	seen := map[string]bool{}
	for i, id := range fake.seen {
		if seen[id] {
			t.Fatalf("attempt %d reused device id %s", i+1, id)
		}
		seen[id] = true
	}
}

func TestMintEABDoesNotRegenerateOnOtherRefusals(t *testing.T) {
	// A name duplicate carries no trailer (or "name"): a new UUID cannot clear
	// it, so the refusal surfaces after one attempt.
	for _, field := range []string{"name", "untagged"} {
		t.Run(field, func(t *testing.T) {
			auth := testAuth(t)
			cfg := testEnrollmentConfig(t, auth, uuid.NewString())
			fake := &fakeEnrollment{collide: func(string) string { return field }}
			_, _, err := MintEAB(context.Background(), dialFake(t, fake), auth, cfg, "edge-one", nil)
			if errors.Is(err, ErrDeviceUUIDTaken) {
				t.Fatal("name refusal mistyped as a UUID collision")
			}
			if status.Code(err) != codes.AlreadyExists {
				t.Fatalf("refusal was not surfaced: %v", err)
			}
			if len(fake.seen) != 1 {
				t.Fatalf("Cloud saw %d enrollments, want 1", len(fake.seen))
			}
		})
	}
}

func TestMintEABPassesCleanEnrollmentThrough(t *testing.T) {
	auth := testAuth(t)
	cfg := testEnrollmentConfig(t, auth, uuid.NewString())

	fake := &fakeEnrollment{collide: func(string) string { return "" }}
	out, _, err := MintEAB(context.Background(), dialFake(t, fake), auth, cfg, "edge-one", nil)
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
