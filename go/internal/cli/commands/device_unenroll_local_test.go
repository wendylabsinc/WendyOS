package commands

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
)

type localUnenrollTestServer struct {
	agentpbv2.UnimplementedWendyProvisioningServiceServer
	calls    atomic.Int32
	requests chan *agentpbv2.UnprovisionRequest
	ack      bool
	err      error
}

func (s *localUnenrollTestServer) Unprovision(_ context.Context, req *agentpbv2.UnprovisionRequest) (*agentpbv2.UnprovisionResponse, error) {
	s.calls.Add(1)
	s.requests <- proto.Clone(req).(*agentpbv2.UnprovisionRequest)
	if s.err != nil {
		return nil, s.err
	}
	return &agentpbv2.UnprovisionResponse{LocalResetCompleted: s.ack}, nil
}

func localUnenrollOperatorCert(t *testing.T, principal string) *config.CertificateInfo {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	uri, err := url.Parse(principal)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), URIs: []*url.URL{uri}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return &config.CertificateInfo{PemCertificate: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}
}

func localUnenrollFixture(t *testing.T, server *localUnenrollTestServer) (*grpcclient.AgentConnection, *agentpbv2.ProvisionedResponse, *peer.Peer) {
	t.Helper()
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	server.requests = make(chan *agentpbv2.UnprovisionRequest, 1)
	agentpbv2.RegisterWendyProvisioningServiceServer(grpcServer, server)
	go grpcServer.Serve(listener)
	t.Cleanup(grpcServer.Stop)
	t.Cleanup(func() { listener.Close() })
	transport, err := grpc.NewClient("passthrough:///local-unenroll", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { transport.Close() })
	principal := testUnenrollProgress().Principal
	uri, _ := url.Parse(principal)
	remote := &peer.Peer{AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{HandshakeComplete: true, PeerCertificates: []*x509.Certificate{{Raw: []byte("verified installed device certificate"), SerialNumber: big.NewInt(1), URIs: []*url.URL{uri}}}}}}
	conn := &grpcclient.AgentConnection{Conn: transport, IsMTLS: true, CertInfo: localUnenrollOperatorCert(t, "spiffe://wendy.sh/tenant/"+unenrollTenant+"/operator/test-user")}
	provisioned := &agentpbv2.ProvisionedResponse{CloudHost: testUnenrollProgress().Cloud, PrincipalUri: principal, LocalResetSupported: true}
	return conn, provisioned, remote
}

type localUnenrollMissingCloudServer struct {
	unenrollConfirmationServer
	lookupErr error
}

func (s *localUnenrollMissingCloudServer) GetAsset(context.Context, *cloudpbv2.GetAssetRequest) (*cloudpbv2.Asset, error) {
	s.gets.Add(1)
	if s.lookupErr != nil {
		return nil, s.lookupErr
	}
	return s.asset, nil
}

// Cloud returns the same bare NotFound to a caller who cannot see the device,
// so only an explicit --force may clear the local enrollment.
func TestV2DeviceUnenrollForceAloneClearsLocalEnrollment(t *testing.T) {
	notFound := status.Error(codes.NotFound, "asset not found")
	for _, tc := range []struct {
		name                   string
		lookupErr              error
		force, check, oldAgent bool
		assetOverride          string
		wantGets               int32
		wantLocalCalls         int32
		wantErr                string
	}{
		{name: "not found without force", lookupErr: notFound, wantGets: 1, wantErr: "--force"},
		{name: "not found check without force", lookupErr: notFound, check: true, wantGets: 1, wantErr: "asset not found"},
		{name: "forced", lookupErr: notFound, force: true, wantGets: 1, wantLocalCalls: 1},
		{name: "forced check", lookupErr: notFound, force: true, check: true, wantGets: 1},
		{name: "forced old Agent", lookupErr: notFound, force: true, oldAgent: true, wantErr: "does not support forced local enrollment reset"},
		{name: "forced with asset ID", lookupErr: notFound, force: true, assetOverride: unenrollAsset, wantErr: "--asset-id"},
		{name: "forced active asset", force: true, wantGets: 1, wantErr: "Cloud still lists"},
		{name: "forced permission denied", lookupErr: status.Error(codes.PermissionDenied, "asset not found"), force: true, wantGets: 1, wantErr: "PermissionDenied"},
		{name: "forced Cloud unavailable", lookupErr: status.Error(codes.Unavailable, "asset not found"), force: true, wantGets: 1, wantErr: "Unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			preserveCloudUnenrollmentHooks(t)
			jsonOutput = true
			cloudServer := &localUnenrollMissingCloudServer{lookupErr: tc.lookupErr}
			cloudServer.asset = testUnenrollAsset()
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			server := grpc.NewServer()
			cloudpbv2.RegisterAssetServiceServer(server, cloudServer)
			go server.Serve(listener)
			t.Cleanup(server.Stop)
			auth := oidcEnrollmentAuth(t)
			auth.CloudGRPC = listener.Addr().String()
			ensureFreshCertificateFn = func(context.Context, *config.AuthConfig) error { return nil }
			setTempConfig(t, &config.Config{Auth: []config.AuthConfig{*auth}})
			localServer := &localUnenrollTestServer{ack: true}
			conn, prov, remote := localUnenrollFixture(t, localServer)
			conn.CertInfo = &auth.Certificates[0]
			prov.CloudHost = auth.CloudGRPC
			prov.PrincipalUri = "spiffe://wendy.sh/tenant/" + auth.Certificates[0].TenantUUID() + "/device/" + unenrollDevice
			prov.CloudAuthenticatedResetSupported = true
			prov.LocalResetSupported = !tc.oldAgent
			cloudServer.asset.OrganizationId = auth.Certificates[0].TenantUUID()
			deviceURI, _ := url.Parse(prov.PrincipalUri)
			remote.AuthInfo.(credentials.TLSInfo).State.PeerCertificates[0].URIs = []*url.URL{deviceURI}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			output, err := captureCommandStdout(t, func() error {
				return runV2DeviceUnenroll(ctx, conn, prov, remote, "", tc.assetOverride, true, tc.check, tc.force)
			})
			if tc.wantErr == "" && err != nil || tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("error = %v, want error containing %q", err, tc.wantErr)
			}
			if localServer.calls.Load() != tc.wantLocalCalls || cloudServer.gets.Load() != tc.wantGets {
				t.Fatalf("local reset calls = %d, Cloud reads = %d; want %d and %d", localServer.calls.Load(), cloudServer.gets.Load(), tc.wantLocalCalls, tc.wantGets)
			}
			if cloudServer.deletes.Load() != 0 || cloudServer.revokes.Load() != 0 {
				t.Fatal("forced local reset triggered a Cloud mutation")
			}
			if tc.wantErr == "" {
				var result map[string]any
				if err := json.Unmarshal([]byte(output), &result); err != nil {
					t.Fatalf("invalid JSON output %q: %v", output, err)
				}
				if result["cloudAssetNotFound"] != true || result["deviceReset"] != !tc.check || result["assetDeleted"] == true || result["certificateRevoked"] == true {
					t.Fatalf("incorrect forced reset result: %v", result)
				}
			}
		})
	}
}

func TestV2LocalUnenrollRequiresSupportedAuthenticatedOperator(t *testing.T) {
	for _, refusal := range []string{"old Agent", "Cloud capability only", "plaintext", "session proxy", "missing peer", "changed peer identity", "missing operator", "other tenant", "device caller"} {
		t.Run(refusal, func(t *testing.T) {
			preserveCloudUnenrollmentHooks(t)
			setTempConfig(t, &config.Config{})
			server := &localUnenrollTestServer{ack: true}
			conn, prov, remote := localUnenrollFixture(t, server)
			switch refusal {
			case "old Agent":
				prov.LocalResetSupported = false
			case "Cloud capability only":
				prov.LocalResetSupported = false
				prov.CloudAuthenticatedResetSupported = true
			case "plaintext":
				conn.IsMTLS = false
			case "session proxy":
				conn.IsSessionProxy = true
			case "missing peer":
				remote = nil
			case "changed peer identity":
				prov.PrincipalUri += "different"
			case "missing operator":
				conn.CertInfo = nil
			case "other tenant":
				conn.CertInfo = localUnenrollOperatorCert(t, "spiffe://wendy.sh/tenant/44444444-4444-4444-8444-444444444444/operator/test-user")
			case "device caller":
				conn.CertInfo = localUnenrollOperatorCert(t, prov.PrincipalUri)
			}
			for _, check := range []bool{false, true} {
				if err := runV2LocalDeviceUnenroll(context.Background(), conn, prov, remote, true, check); err == nil {
					t.Errorf("%s accepted with check=%v", refusal, check)
				}
			}
			if server.calls.Load() != 0 {
				t.Fatal("refused connection sent reset RPC")
			}
		})
	}
}

func TestV2LocalUnenrollConfirmationCompletionAndPinCleanup(t *testing.T) {
	for _, tc := range []struct {
		name                                        string
		yes, check, interactive, confirm, ack, json bool
		rpcErr                                      error
		wantCalls                                   int32
		wantReset, wantErr                          bool
	}{
		{name: "confirmed JSON", yes: true, ack: true, json: true, wantCalls: 1, wantReset: true},
		{name: "interactive confirmation", interactive: true, confirm: true, ack: true, wantCalls: 1, wantReset: true},
		{name: "declined", interactive: true},
		{name: "requires confirmation", wantErr: true},
		{name: "interactive JSON requires yes", interactive: true, confirm: true, json: true, wantErr: true},
		{name: "check is read only", check: true, json: true},
		{name: "old empty acknowledgement", yes: true, json: true, wantCalls: 1, wantErr: true},
		{name: "connection lost", yes: true, json: true, rpcErr: status.Error(codes.Unavailable, "connection lost"), wantCalls: 1, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			preserveCloudUnenrollmentHooks(t)
			isInteractiveTerminalFn = func() bool { return tc.interactive }
			confirmDefaultNoFn = func(string) bool { return tc.confirm }
			jsonOutput = tc.json
			principal := testUnenrollProgress().Principal
			otherPrincipal := strings.Replace(principal, unenrollDevice, "44444444-4444-4444-8444-444444444444", 1)
			readPins := writePinTestConfig(t, map[string]config.DevicePin{
				"board":       {Principal: principal, CloudGRPC: testUnenrollProgress().Cloud},
				"board-alias": {Principal: principal, CloudGRPC: testUnenrollProgress().Cloud},
				"other-board": {Principal: otherPrincipal, CloudGRPC: testUnenrollProgress().Cloud},
			})
			seedSPKIPins(t, principal, otherPrincipal)
			server := &localUnenrollTestServer{ack: tc.ack, err: tc.rpcErr}
			conn, prov, remote := localUnenrollFixture(t, server)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			output, err := captureCommandStdout(t, func() error {
				return runV2LocalDeviceUnenroll(ctx, conn, prov, remote, tc.yes, tc.check)
			})
			if err == nil && tc.wantErr || err != nil && !tc.wantErr {
				t.Fatalf("error = %v, want error %v", err, tc.wantErr)
			}
			if server.calls.Load() != tc.wantCalls {
				t.Fatalf("reset calls = %d, want %d", server.calls.Load(), tc.wantCalls)
			}
			if tc.wantCalls > 0 {
				req := <-server.requests
				identity, _ := certs.ParsePrincipal(principal)
				fingerprint, fpErr := directUnenrollPeerFingerprint(conn, remote, identity)
				if fpErr != nil || !req.GetLocalOnly() || req.GetExpectedPrincipalUri() != principal || req.GetExpectedCertificateSha256() != fingerprint {
					t.Fatalf("reset lost authenticated device binding: %v, %v", req, fpErr)
				}
				if len(req.GetCloudDeletion()) != 0 || len(req.GetCloudResetAuthorization()) != 0 || len(req.GetRevocationProof()) != 0 {
					t.Fatal("local reset manufactured Cloud lifecycle evidence")
				}
			}
			pins := readPins()
			for _, host := range []string{"board", "board-alias"} {
				if _, retained := pins[host]; retained == tc.wantReset {
					t.Errorf("%s pin retained = %v, reset = %v", host, retained, tc.wantReset)
				}
			}
			spkiPins := readSPKIPins(t)
			if _, retained := spkiPins[principal]; retained == tc.wantReset {
				t.Errorf("SPKI pin retained = %v, reset = %v", retained, tc.wantReset)
			}
			if _, retained := pins["other-board"]; !retained {
				t.Error("reset cleared another device's identity pin")
			}
			if _, retained := spkiPins[otherPrincipal]; !retained {
				t.Error("reset cleared another device's SPKI pin")
			}
			if tc.json && tc.wantCalls == 0 && !tc.check {
				if output != "" {
					t.Fatalf("confirmation refusal wrote to JSON stdout: %q", output)
				}
			} else if tc.json {
				var result map[string]any
				if err := json.Unmarshal([]byte(output), &result); err != nil {
					t.Fatalf("invalid JSON output %q: %v", output, err)
				}
				if result["deviceReset"] != tc.wantReset {
					t.Errorf("JSON reset = %v, want %v", result["deviceReset"], tc.wantReset)
				}
				if result["cloudAssetNotFound"] != true || tc.wantCalls > 0 && result["localOnly"] != true {
					t.Errorf("JSON omitted the local reset's Cloud absence status: %v", result)
				}
				for _, field := range []string{"certificateRevoked", "assetDeleted", "cloudBindingVerified"} {
					if result[field] == true {
						t.Errorf("local reset falsely claimed %s", field)
					}
				}
			}
		})
	}
}

func TestDeviceUnenrollForceExcludesCloudBindingFlags(t *testing.T) {
	for _, other := range []struct{ name, value string }{{"asset-id", unenrollAsset}, {"cloud-only", "true"}} {
		cmd := newDeviceUnenrollCmd()
		for name, value := range map[string]string{"force": "true", other.name: other.value} {
			if err := cmd.Flags().Set(name, value); err != nil {
				t.Fatal(err)
			}
		}
		if err := cmd.ValidateFlagGroups(); err == nil {
			t.Errorf("--force accepted together with --%s", other.name)
		}
	}
}
