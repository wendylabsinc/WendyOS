package commands

import (
	"context"
	"crypto/mldsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/cloudenroll"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func oidcEnrollmentAuth(t *testing.T) *config.AuthConfig {
	t.Helper()
	auth := fakeAuth(t)
	// The operator credential is ML-DSA-65 (WDY-3032); fakeAuth still mints the
	// EC device-style key, so replace it here rather than weaken the fixture.
	keyPEM, err := certs.GenerateMLDSAKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	key, err := certs.ParseSigningPrivateKeyPEM([]byte(keyPEM))
	if err != nil {
		t.Fatal(err)
	}
	auth.Certificates[0].PemPrivateKey = keyPEM
	auth.Certificates[0].PemCertificate = testLeafPEM(t, key)
	auth.Certificates[0].PemCertificateChain = ""
	auth.Certificates[0].PrincipalURI = "spiffe://wendy.sh/tenant/" + testOperatorTenant + "/operator/" + testOperatorSubject
	auth.OAuthIssuer = "https://auth.dev.wendy.sh/realms/acme"
	auth.PKIEndpoint = "https://identity.dev.pki.wendy.sh/v1/identity/certificate"
	auth.OAuthExpiresAt = time.Now().Add(time.Hour).Format(time.RFC3339)
	auth.APIKey = "test-access-token"
	return auth
}

type acmeProvisioningServer struct {
	agentpbv2.UnimplementedWendyProvisioningServiceServer
	req           *agentpbv2.StartACMEProvisioningRequest
	enrolled      bool
	preflightErr  error
	startErr      error
	wrongIdentity bool
}

func (s *acmeProvisioningServer) IsProvisioned(context.Context, *agentpbv2.IsProvisionedRequest) (*agentpbv2.IsProvisionedResponse, error) {
	if s.preflightErr != nil {
		return nil, s.preflightErr
	}
	if s.enrolled {
		return &agentpbv2.IsProvisionedResponse{ResponseType: &agentpbv2.IsProvisionedResponse_Provisioned{Provisioned: &agentpbv2.ProvisionedResponse{}}}, nil
	}
	return &agentpbv2.IsProvisionedResponse{ResponseType: &agentpbv2.IsProvisionedResponse_NotProvisioned{NotProvisioned: &agentpbv2.NotProvisionedResponse{}}}, nil
}
func (s *acmeProvisioningServer) StartACMEProvisioning(_ context.Context, req *agentpbv2.StartACMEProvisioningRequest) (*agentpbv2.StartACMEProvisioningResponse, error) {
	s.req = req
	if s.startErr != nil {
		return nil, s.startErr
	}
	principal := "spiffe://wendy.sh/tenant/" + testOperatorTenant + "/device/" + req.DeviceId
	if s.wrongIdentity {
		principal += "-wrong"
	}
	return &agentpbv2.StartACMEProvisioningResponse{PrincipalUri: principal}, nil
}

type oidcEnrollmentServer struct {
	cloudpbv2.UnimplementedDeviceEnrollmentServiceServer
	req      *cloudpbv2.EnrollDeviceRequest
	md       metadata.MD
	err      error
	response *cloudpbv2.EnrollDeviceResponse
}

func (s *oidcEnrollmentServer) EnrollDevice(ctx context.Context, req *cloudpbv2.EnrollDeviceRequest) (*cloudpbv2.EnrollDeviceResponse, error) {
	s.req = req
	s.md, _ = metadata.FromIncomingContext(ctx)
	return s.response, s.err
}

func enrollmentServers(t *testing.T, cloud *oidcEnrollmentServer, agent *acmeProvisioningServer) (*grpcclient.AgentConnection, string) {
	t.Helper()
	// Minted-but-unredeemed credentials are kept under the config dir; a
	// fresh one per test stops one test's failure from resuming in the next.
	t.Setenv("HOME", t.TempDir())
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	cloudpbv2.RegisterDeviceEnrollmentServiceServer(srv, cloud)
	agentpbv2.RegisterWendyProvisioningServiceServer(srv, agent)
	go srv.Serve(lis) //nolint:errcheck
	t.Cleanup(srv.Stop)
	client, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	return &grpcclient.AgentConnection{Conn: client, Host: "sim.local"}, lis.Addr().String()
}

func verifyEnrollmentJWS(t *testing.T, compact string) map[string]any {
	t.Helper()
	parts := strings.Split(compact, ".")
	if len(parts) != 3 {
		t.Fatal("expected compact JWS")
	}
	decode := func(s string) []byte {
		b, err := base64.RawURLEncoding.DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	var header struct {
		Alg string
		X5C []string
	}
	if err := json.Unmarshal(decode(parts[0]), &header); err != nil {
		t.Fatal(err)
	}
	if header.Alg != "ML-DSA-65" || len(header.X5C) != 1 {
		t.Fatal("invalid signing header")
	}
	der, err := base64.StdEncoding.DecodeString(header.X5C[0])
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	key, ok := leaf.PublicKey.(*mldsa.PublicKey)
	if !ok {
		t.Fatalf("enrollment leaf carries %T, want an ML-DSA-65 operator key", leaf.PublicKey)
	}
	// Verified the way pki-core does: over the signing input, no pre-hash,
	// nil options.
	if err := mldsa.Verify(key, []byte(parts[0]+"."+parts[1]), decode(parts[2]), nil); err != nil {
		t.Fatalf("invalid enrollment signature: %v", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(decode(parts[1]), &claims); err != nil {
		t.Fatal(err)
	}
	return claims
}

func TestOIDCEnrollmentAutomaticCloudRelay(t *testing.T) {
	minted := map[string]bool{}
	for _, name := range []string{"sim", "box-01", ""} {
		t.Run(name, func(t *testing.T) {
			cloud := &oidcEnrollmentServer{response: &cloudpbv2.EnrollDeviceResponse{AssetId: "asset-uuid", CredentialKind: "eab", EabKeyId: "eab-id", EabHmacKey: strings.Repeat("ab", 32)}}
			agent := &acmeProvisioningServer{}
			conn, host := enrollmentServers(t, cloud, agent)
			auth := oidcEnrollmentAuth(t)
			auth.CloudGRPC = host
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := runEnrollDevice(ctx, conn, auth, name, 0); err != nil {
				t.Fatal(err)
			}
			if name == "" {
				name = "sim"
			}

			// The identity is a freshly minted UUID and the name is carried
			// beside it: the operator's label never becomes the permanent SAN.
			deviceID := cloud.req.GetDeviceId()
			if _, err := uuid.Parse(deviceID); err != nil {
				t.Fatalf("device_id %q is not a UUID", deviceID)
			}
			if deviceID == name {
				t.Fatal("device identity was derived from the name")
			}
			if minted[deviceID] {
				t.Fatal("device identity was reused across enrollments")
			}
			minted[deviceID] = true
			if cloud.req.GetName() != name || cloud.req.GetDeviceClass() != cloudpbv2.DeviceClass_DEVICE_CLASS_B {
				t.Fatal("incorrect Cloud enrollment request")
			}
			if got := cloud.md.Get("authorization"); len(got) != 1 || got[0] != "Bearer test-access-token" {
				t.Fatal("missing OIDC bearer")
			}
			artifact := verifyEnrollmentJWS(t, string(cloud.req.GetEnrollmentRequestJws()))
			// Cloud refuses on disagreement, so the signed id and the request
			// id have to be the same string.
			if artifact["tenant"] != testOperatorTenant || artifact["device_id"] != deviceID || artifact["device_class"] != "B" {
				t.Fatal("incorrect PKI claims")
			}
			if _, err := uuid.Parse(artifact["jti"].(string)); err != nil {
				t.Fatal("missing replay identifier")
			}
			iat, exp := int64(artifact["iat"].(float64)), int64(artifact["exp"].(float64))
			if exp-iat != 300 || time.Now().Unix()-iat > 5 {
				t.Fatal("incorrect enrollment validity")
			}
			envelope := cloud.md.Get("x-wendy-request-signature")
			if len(envelope) != 1 {
				t.Fatal("missing Cloud request signature")
			}
			descriptor := verifyEnrollmentJWS(t, envelope[0])
			target := descriptor["target"].(map[string]any)
			if descriptor["operation"] != "wendycloud.v2.DeviceEnrollmentService/EnrollDevice" || target["tenant"] != testOperatorTenant || target["resource"] != "org/"+testOperatorTenant+"/device/"+deviceID {
				t.Fatal("incorrect Cloud request scope")
			}
			if agent.req.GetDeviceId() != deviceID || agent.req.GetEabKeyId() != "eab-id" || agent.req.GetEabHmacKey() != strings.Repeat("ab", 32) || agent.req.GetCloudHost() != host {
				t.Fatal("credential handoff mismatch")
			}
			if agent.req.GetDirectoryUrl() != "https://acme.dev.pki.wendy.sh/"+testOperatorTenant+"/acme/directory" {
				t.Fatal("incorrect directory")
			}
		})
	}
}

// A name Cloud would refuse has to be refused here, before the RPC that mints
// a single-use credential is made at all.
func TestOIDCEnrollmentRejectsNamesCloudWouldRefuse(t *testing.T) {
	for _, name := range []string{"Box-01", "fleet-a/box-01", "box.01", "1box", "box-", strings.Repeat("b", 64)} {
		t.Run(name, func(t *testing.T) {
			cloud := &oidcEnrollmentServer{response: &cloudpbv2.EnrollDeviceResponse{AssetId: "asset-uuid", CredentialKind: "eab", EabKeyId: "eab-id", EabHmacKey: strings.Repeat("ab", 32)}}
			agent := &acmeProvisioningServer{}
			conn, host := enrollmentServers(t, cloud, agent)
			auth := oidcEnrollmentAuth(t)
			auth.CloudGRPC = host
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := runEnrollDevice(ctx, conn, auth, name, 0)
			if err == nil {
				t.Fatal("expected the enrollment to be refused")
			}
			if !strings.Contains(err.Error(), "not usable") {
				t.Fatalf("err = %v, want the device-name rule", err)
			}
			if cloud.req != nil {
				t.Fatal("a refused name still reached Cloud")
			}
		})
	}
}

func TestOIDCEnrollmentFailures(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		cloudErr, agentErr      error
		kind, key               string
		enrolled, wrongIdentity bool
		want                    string
		wantCloud, wantAgent    bool
	}{
		{name: "cloud unavailable", cloudErr: status.Error(codes.Unavailable, "relay unavailable"), want: "relay unavailable", wantCloud: true},
		{name: "old cloud", cloudErr: status.Error(codes.Unimplemented, "missing"), want: "needs DeviceEnrollmentService", wantCloud: true},
		{name: "wrong credential kind", kind: "enrollment_token", want: "unexpected enrollment credential kind", wantCloud: true},
		{name: "missing EAB", kind: "eab", want: "invalid enrollment credentials", wantCloud: true},
		{name: "malformed EAB", kind: "eab", key: "secret-not-hex", want: "hex-encoded", wantCloud: true},
		{name: "already enrolled", enrolled: true, want: "already enrolled"},
		{name: "old agent", kind: "eab", key: "abcd", agentErr: status.Error(codes.Unimplemented, "missing"), want: "update the agent", wantCloud: true, wantAgent: true},
		{name: "handoff failure", kind: "eab", key: "abcd", agentErr: status.Error(codes.Unavailable, "device disconnected"), want: "re-run this command", wantCloud: true, wantAgent: true},
		{name: "wrong identity", kind: "eab", key: "abcd", wrongIdentity: true, want: "unexpected enrollment identity", wantCloud: true, wantAgent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cloud := &oidcEnrollmentServer{err: tc.cloudErr, response: &cloudpbv2.EnrollDeviceResponse{AssetId: "asset-uuid", CredentialKind: tc.kind, EabKeyId: "id", EabHmacKey: tc.key}}
			agent := &acmeProvisioningServer{enrolled: tc.enrolled, startErr: tc.agentErr, wrongIdentity: tc.wrongIdentity}
			conn, host := enrollmentServers(t, cloud, agent)
			auth := oidcEnrollmentAuth(t)
			auth.CloudGRPC = host
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := runEnrollDevice(ctx, conn, auth, "sim", 0)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v, want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "secret-not-hex") {
				t.Fatal("error leaked EAB secret")
			}
			if (cloud.req != nil) != tc.wantCloud || (agent.req != nil) != tc.wantAgent {
				t.Fatal("unexpected RPC after failed enrollment step")
			}
		})
	}
}

func TestOIDCEnrollmentConfig(t *testing.T) {
	for _, tc := range []struct {
		name, device, directory string
		pkiEndpoint             string
		want, wantDirectory     string
	}{
		{name: "wrong tenant", device: "sim", directory: "https://acme.example/11111111-1111-4111-8111-111111111111/acme/directory", want: "tenant does not match"},
		// Any deployment derives its own ACME frontend from its own identity
		// frontend, not just the one this CLI was built knowing about.
		{name: "self-hosted deployment derives its own directory", device: "sim", pkiEndpoint: "https://identity.example/v1/identity/certificate", wantDirectory: "https://acme.example/" + testOperatorTenant + "/acme/directory"},
		{name: "port is carried across", device: "sim", pkiEndpoint: "https://identity.pki.example:8451/v1/identity/certificate", wantDirectory: "https://acme.pki.example:8451/" + testOperatorTenant + "/acme/directory"},
		{name: "a PKI host with no identity label needs the flag", device: "sim", pkiEndpoint: "https://pki.example/v1/identity/certificate", want: "--acme-directory-url"},
		{name: "custom directory", device: "fleet/sim", pkiEndpoint: "https://identity.example/v1/identity/certificate", directory: "https://acme.example/" + testOperatorTenant + "/acme/directory"},
		{name: "bad device", device: "../sim", want: "invalid ACME device ID"},
		{name: "insecure endpoint", device: "sim", directory: "http://acme.example/" + testOperatorTenant + "/acme/directory", want: "HTTPS"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auth := oidcEnrollmentAuth(t)
			if tc.pkiEndpoint != "" {
				auth.PKIEndpoint = tc.pkiEndpoint
			}
			cfg, err := cloudenroll.EnrollmentConfig(auth, tc.device, tc.directory)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v, want %q", err, tc.want)
			}
			if tc.wantDirectory != "" && cfg.DirectoryURL != tc.wantDirectory {
				t.Fatalf("directory=%q, want %q", cfg.DirectoryURL, tc.wantDirectory)
			}
		})
	}
	err := runEnrollDevice(context.Background(), nil, oidcEnrollmentAuth(t), "sim", 7)
	if err == nil || !strings.Contains(err.Error(), "--org is for legacy") {
		t.Fatalf("error=%v", err)
	}
	err = runEnrollDevice(context.Background(), nil, testAuth(), "sim", 0, "https://acme.example")
	if err == nil || !strings.Contains(err.Error(), "requires an OIDC") {
		t.Fatalf("error=%v", err)
	}
}

func TestEnrollmentCommandsExposeDirectoryOverride(t *testing.T) {
	for _, cmd := range []*cobra.Command{newDeviceEnrollCmd(), newCloudEnrollDeviceCmd()} {
		if cmd.Flags().Lookup("acme-directory-url") == nil || cmd.Flags().Lookup("acme-config") != nil {
			t.Fatal("expected directory override without manual EAB flag")
		}
	}
}

func TestOIDCEnrollmentPreflightFailureDoesNotMint(t *testing.T) {
	for _, code := range []codes.Code{codes.Unimplemented, codes.Unavailable} {
		t.Run(code.String(), func(t *testing.T) {
			cloud := &oidcEnrollmentServer{}
			agent := &acmeProvisioningServer{preflightErr: status.Error(code, "preflight failed")}
			conn, host := enrollmentServers(t, cloud, agent)
			auth := oidcEnrollmentAuth(t)
			auth.CloudGRPC = host
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := runEnrollDevice(ctx, conn, auth, "sim", 0)
			if err == nil || cloud.req != nil || agent.req != nil {
				t.Fatal("credential minted after failed agent preflight")
			}
			if code == codes.Unimplemented && !strings.Contains(err.Error(), "update the agent") {
				t.Fatalf("missing update hint: %v", err)
			}
		})
	}
}

// Cloud reserves the name and returns the EAB once. A re-run after the agent
// step failed must finish with that same device and credential, without asking
// cloud again (which would refuse the reserved name).
func TestOIDCEnrollmentResumesAfterAgentFailure(t *testing.T) {
	cloud := &oidcEnrollmentServer{response: &cloudpbv2.EnrollDeviceResponse{AssetId: "asset-uuid", CredentialKind: "eab", EabKeyId: "eab-id", EabHmacKey: strings.Repeat("ab", 32)}}
	agent := &acmeProvisioningServer{startErr: status.Error(codes.FailedPrecondition, "direct ACME enrollment is experimental")}
	conn, host := enrollmentServers(t, cloud, agent)
	auth := oidcEnrollmentAuth(t)
	auth.CloudGRPC = host
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := runEnrollDevice(ctx, conn, auth, "sim", 0)
	if err == nil || agent.req == nil {
		t.Fatalf("first run: error=%v, agent called=%v", err, agent.req != nil)
	}
	first := agent.req
	if !strings.Contains(err.Error(), first.DeviceId) || !strings.Contains(err.Error(), "re-run this command") || strings.Contains(err.Error(), "asset") {
		t.Fatalf("failure should name the device id and the next step, never the asset: %v", err)
	}

	cloud.req, agent.req, agent.startErr = nil, nil, nil
	cloud.err = status.Error(codes.AlreadyExists, "another device in this organization is already named sim")
	if err := runEnrollDevice(ctx, conn, auth, "sim", 0); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if cloud.req != nil {
		t.Fatal("resume asked cloud to mint again for a reserved name")
	}
	if agent.req.DeviceId != first.DeviceId || agent.req.EabKeyId != first.EabKeyId || agent.req.EabHmacKey != first.EabHmacKey || agent.req.DirectoryUrl != first.DirectoryUrl {
		t.Fatalf("resume used different enrollment material: %+v vs %+v", agent.req, first)
	}

	// Redeemed: the credential is gone, so the next enrollment mints afresh.
	if err := runEnrollDevice(ctx, conn, auth, "sim", 0); err == nil || cloud.req == nil {
		t.Fatalf("after success, want a fresh mint; error=%v, cloud called=%v", err, cloud.req != nil)
	}
}

// A kept credential belongs to one name; another device must not pick it up.
func TestOIDCEnrollmentPendingIsPerName(t *testing.T) {
	cloud := &oidcEnrollmentServer{response: &cloudpbv2.EnrollDeviceResponse{AssetId: "asset-uuid", CredentialKind: "eab", EabKeyId: "eab-id", EabHmacKey: strings.Repeat("ab", 32)}}
	agent := &acmeProvisioningServer{startErr: status.Error(codes.Unavailable, "device disconnected")}
	conn, host := enrollmentServers(t, cloud, agent)
	auth := oidcEnrollmentAuth(t)
	auth.CloudGRPC = host
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := runEnrollDevice(ctx, conn, auth, "sim", 0); err == nil {
		t.Fatal("want agent failure")
	}
	cloud.req, agent.startErr = nil, nil
	if err := runEnrollDevice(ctx, conn, auth, "other", 0); err != nil {
		t.Fatal(err)
	}
	if cloud.req == nil {
		t.Fatal("a different name reused another device's pending credential")
	}
}
