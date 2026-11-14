package grpcclient

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/operatorpermission"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

type permissionAgent struct {
	pb.UnimplementedWendyAgentServiceServer
	supported bool
	reads     atomic.Int32
}

func (s *permissionAgent) GetAgentVersion(context.Context, *pb.GetAgentVersionRequest) (*pb.GetAgentVersionResponse, error) {
	s.reads.Add(1)
	r := new(pb.GetAgentVersionResponse)
	if s.supported {
		r.Featureset = []string{"operator-permissions-v1"}
	}
	return r, nil
}

type permissionContainers struct {
	pb.UnimplementedWendyContainerServiceServer
	writes atomic.Int32
}

func (s *permissionContainers) StopContainer(context.Context, *pb.StopContainerRequest) (*pb.StopContainerResponse, error) {
	s.writes.Add(1)
	return new(pb.StopContainerResponse), nil
}
func permissionTestConn(t *testing.T, supported bool) (*grpc.ClientConn, *permissionAgent, *permissionContainers) {
	t.Helper()
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	agent := &permissionAgent{supported: supported}
	containers := new(permissionContainers)
	pb.RegisterWendyAgentServiceServer(server, agent)
	pb.RegisterWendyContainerServiceServer(server, containers)
	go server.Serve(listener)
	conn, err := grpc.NewClient("passthrough:///permissions", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(); server.Stop(); listener.Close() })
	return conn, agent, containers
}
func permissionTestCert(t *testing.T, permission string) *config.CertificateInfo {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	u, _ := url.Parse("spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/operator/test")
	claim := "entitlement:" + permission + ":device:22222222-2222-4222-8222-222222222222:allow"
	ext, _ := asn1.Marshal([]asn1.RawValue{{Tag: 12, Bytes: []byte(claim)}})
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(4 * time.Minute), URIs: []*url.URL{u}, ExtraExtensions: []pkix.Extension{{Id: operatorpermission.OID, Value: ext}}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	return &config.CertificateInfo{PemCertificate: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}
}
func TestOperatorPermissionDialer(t *testing.T) {
	for _, tc := range []struct {
		name              string
		supported, refuse bool
		permission        string
	}{
		{"on demand and reuse", true, false, "deployment:control-container"},
		{"Cloud refusal never falls back", true, true, "deployment:control-container"},
		{"missing claim never falls back", true, false, "device:read"},
		{"older agent", false, false, "deployment:control-container"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base, agent, baseWrites := permissionTestConn(t, tc.supported)
			elevated, _, elevatedWrites := permissionTestConn(t, false)
			calls := 0
			issued := permissionTestCert(t, tc.permission)
			d := &operatorPermissionDialer{target: certs.WendyIdentity{TenantUUID: "11111111-1111-4111-8111-111111111111", EntityType: certs.EntityAsset, EntityID: "22222222-2222-4222-8222-222222222222"}, request: func(context.Context, *config.CertificateInfo, certs.WendyIdentity) (*config.CertificateInfo, error) {
				calls++
				if tc.refuse {
					return nil, fmt.Errorf("Cloud refused")
				}
				return issued, nil
			}, dial: func(context.Context, *config.CertificateInfo) (*AgentConnection, error) {
				c := newAgentConnection(elevated)
				c.CertInfo = issued
				return c, nil
			}}
			defer d.Close()
			invoke := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
				return cc.Invoke(ctx, method, req, reply, opts...)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := d.unary(ctx, pb.WendyAgentService_GetAgentVersion_FullMethodName, &pb.GetAgentVersionRequest{}, &pb.GetAgentVersionResponse{}, base, invoke); err != nil {
				t.Fatal(err)
			}
			if calls != 0 {
				t.Fatal("read requested a certificate")
			}
			for i := 0; i < 2; i++ {
				err := d.unary(ctx, pb.WendyContainerService_StopContainer_FullMethodName, &pb.StopContainerRequest{}, &pb.StopContainerResponse{}, base, invoke)
				wantErr := tc.supported && (tc.refuse || tc.permission == "device:read")
				if (err != nil) != wantErr {
					t.Fatalf("call err=%v", err)
				}
			}
			if tc.supported {
				if baseWrites.writes.Load() != 0 {
					t.Fatal("privileged call used login connection")
				}
				if !tc.refuse && calls != 1 {
					t.Fatalf("certificate requests=%d", calls)
				}
			} else if calls != 0 || baseWrites.writes.Load() != 2 {
				t.Fatal("older-agent compatibility changed")
			}
			if tc.supported && !tc.refuse && tc.permission != "device:read" && elevatedWrites.writes.Load() != 2 {
				t.Fatal("authorized calls not routed exactly once")
			}
			if agent.reads.Load() != 2 {
				t.Fatalf("unexpected capability probes=%d", agent.reads.Load())
			}
		})
	}
}
