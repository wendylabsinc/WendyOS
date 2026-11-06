package commands

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"math/big"
	"net"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// Keep the local Cloud fixture plaintext while the agent RPC proves its actual
// server certificate over TLS, as the direct and Cloud-tunnel transports do.
func deploymentTLSConnection(t *testing.T, srv *deploymentV2Server, legacy *deploymentLegacyProvisioningStub, serverPrincipal string) *grpcclient.AgentConnection {
	t.Helper()
	_ = deploymentTestConnection(t, srv, legacy)
	key := testKey(t)
	principal, err := url.Parse(serverPrincipal)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), DNSNames: []string{"localhost"}, URIs: []*url.URL{principal},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
		MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
	})))
	agentpb.RegisterWendyProvisioningServiceServer(server, srv)
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
		MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: "localhost",
	})))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	// NewFromConn intentionally has neither IsMTLS nor an observer. Actual
	// grpc.Peer TLS evidence must work independently of those metadata fields.
	result := grpcclient.NewFromConn(conn)
	if legacy != nil {
		result.ProvisioningService = legacy
	}
	return result
}

func TestDeploymentEnrollmentBindsClaimsBeforeConfigOrCloudAccess(t *testing.T) {
	actual := deploymentV2Device().PrincipalUri
	otherTenant := "11111111-1111-4111-8111-111111111111"
	operator := config.CertificateInfo{PrincipalURI: "spiffe://wendy.sh/tenant/" + deploymentTenant + "/operator/test"}
	for _, tc := range []struct {
		name, reported, operatorTenant string
		plaintext                      bool
	}{
		{name: "another tenant with stored operator", reported: strings.Replace(actual, deploymentTenant, otherTenant, 1)},
		{name: "another device in same tenant", reported: "spiffe://wendy.sh/tenant/" + deploymentTenant + "/device/11111111-1111-4111-8111-111111111111"},
		{name: "connection operator from another tenant", reported: actual, operatorTenant: otherTenant},
		{name: "UUID connection cannot downgrade to numeric enrollment"},
		{name: "plaintext enrollment claim", reported: actual, plaintext: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			device := deploymentV2Device()
			device.PrincipalUri = tc.reported
			if tc.reported == "" {
				device.OrganizationId, device.AssetId = 42, 9
			}
			legacy := &deploymentLegacyProvisioningStub{}
			srv := &deploymentV2Server{device: device}
			var conn *grpcclient.AgentConnection
			if tc.plaintext {
				conn = deploymentTestConnection(t, srv, legacy)
			} else {
				conn = deploymentTLSConnection(t, srv, legacy, actual)
			}
			connectionCert := operator
			if tc.operatorTenant != "" {
				connectionCert.PrincipalURI = strings.Replace(connectionCert.PrincipalURI, deploymentTenant, tc.operatorTenant, 1)
			}
			conn.CertInfo = &connectionCert
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			loaded, cloudCalls := 0, 0
			err := registerDeviceApps(ctx, deploymentProvisioningService{conn}, []string{"app"}, func() (*config.Config, error) {
				loaded++
				other := config.CertificateInfo{PrincipalURI: strings.Replace(operator.PrincipalURI, deploymentTenant, otherTenant, 1)}
				return &config.Config{Auth: []config.AuthConfig{{CloudGRPC: device.CloudHost, Certificates: []config.CertificateInfo{operator, other}}}}, nil
			}, func(context.Context, *config.AuthConfig, *agentpb.ProvisionedResponse, []string) error {
				cloudCalls++
				return nil
			})
			if err == nil || loaded != 0 || cloudCalls != 0 || legacy.calls != 0 {
				t.Fatalf("unsafe enrollment reached side effects: err=%v config=%d Cloud=%d legacy=%d", err, loaded, cloudCalls, legacy.calls)
			}
		})
	}
}

func TestDeploymentEnrollmentAcceptsVerifiedDirectAndTunnelPeer(t *testing.T) {
	for _, metadata := range []bool{true, false} {
		name := "TLS tunnel via NewFromConn"
		if metadata {
			name = "direct with operator metadata"
		}
		t.Run(name, func(t *testing.T) {
			device := deploymentV2Device()
			srv := &deploymentV2Server{device: device}
			conn := deploymentTLSConnection(t, srv, nil, device.PrincipalUri)
			if metadata {
				conn.IsMTLS = true
				conn.CertInfo = &config.CertificateInfo{PrincipalURI: "spiffe://wendy.sh/tenant/" + deploymentTenant + "/operator/test"}
			}
			response, err := (deploymentProvisioningService{conn}).IsProvisioned(context.Background(), &agentpb.IsProvisionedRequest{})
			if err != nil || response.GetProvisioned().GetPrincipalUri() != device.PrincipalUri {
				t.Fatalf("verified peer rejected: %v %v", response, err)
			}
		})
	}
}

func TestDeploymentEnrollmentSessionProxyRetainsPinnedIdentityAndTenant(t *testing.T) {
	device := deploymentV2Device()
	identity, err := certs.ParsePrincipal(device.PrincipalUri)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		pinned    certs.WendyIdentity
		operator  string
		reported  string
		wantError bool
	}{
		{name: "exact pinned principal", pinned: identity, operator: deploymentTenant, reported: device.PrincipalUri},
		{name: "different pinned device", pinned: identity, operator: deploymentTenant, reported: "spiffe://wendy.sh/tenant/" + deploymentTenant + "/device/11111111-1111-4111-8111-111111111111", wantError: true},
		{name: "numeric broker identity with matching operator tenant", operator: deploymentTenant, reported: device.PrincipalUri},
		{name: "numeric broker identity cannot change tenant", operator: "11111111-1111-4111-8111-111111111111", reported: device.PrincipalUri, wantError: true},
		{name: "numeric broker identity without tenant evidence", reported: device.PrincipalUri, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			operator := &config.CertificateInfo{}
			if tc.operator != "" {
				operator.PrincipalURI = "spiffe://wendy.sh/tenant/" + tc.operator + "/operator/test"
			}
			pinned := tc.pinned
			if pinned.Principal == "" {
				pinned = certs.WendyIdentity{OrgID: 42, EntityType: certs.EntityAsset, EntityID: "9"}
			}
			conn, err := grpcclient.ConnectSessionProxy(context.Background(), filepath.Join(t.TempDir(), "unused.sock"), "device", "device:50052", operator, pinned)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			reported := &agentpb.ProvisionedResponse{PrincipalUri: tc.reported}
			err = validateDeploymentEnrollment(conn, nil, reported)
			if (err != nil) != tc.wantError {
				t.Fatalf("proxy enrollment validation: %v, wantError=%v", err, tc.wantError)
			}
		})
	}
}

func TestDeploymentEnrollmentBindsLegacyClaimsBeforeConfigOrCloudAccess(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		org, asset, operatorOrg int32
		plaintext               bool
	}{
		{name: "another organization", org: 99, asset: 9, operatorOrg: 42},
		{name: "another device", org: 42, asset: 10, operatorOrg: 42},
		{name: "another connection operator", org: 42, asset: 9, operatorOrg: 99},
		{name: "plaintext legacy enrollment", org: 42, asset: 9, plaintext: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			device := &agentpb.ProvisionedResponse{CloudHost: "cloud.example:443", OrganizationId: tc.org, AssetId: tc.asset}
			srv := &deploymentV2Server{device: device}
			var conn *grpcclient.AgentConnection
			if tc.plaintext {
				conn = deploymentTestConnection(t, srv, nil)
			} else {
				conn = deploymentTLSConnection(t, srv, nil, "urn:wendy:org:42:asset:9")
			}
			conn.CertInfo = &config.CertificateInfo{OrganizationID: int(tc.operatorOrg)}
			loaded, cloudCalls := 0, 0
			err := registerDeviceApps(context.Background(), deploymentProvisioningService{conn}, []string{"app"}, func() (*config.Config, error) {
				loaded++
				return &config.Config{Auth: []config.AuthConfig{{CloudGRPC: device.CloudHost, Certificates: []config.CertificateInfo{{OrganizationID: int(tc.org), UserID: "operator"}}}}}, nil
			}, func(context.Context, *config.AuthConfig, *agentpb.ProvisionedResponse, []string) error {
				cloudCalls++
				return nil
			})
			if err == nil || loaded != 0 || cloudCalls != 0 {
				t.Fatalf("unsafe legacy enrollment reached side effects: err=%v config=%d Cloud=%d", err, loaded, cloudCalls)
			}
		})
	}
}
