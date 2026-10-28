package services

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/interceptor"
	"github.com/wendylabsinc/wendy/go/internal/agent/mtls"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshappstream"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	v2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"go.uber.org/zap"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

type operatorApplicationAgent struct {
	pb.UnimplementedWendyAgentServiceServer
	principal string
}

func (s operatorApplicationAgent) GetAgentVersion(ctx context.Context, _ *pb.GetAgentVersionRequest) (*pb.GetAgentVersionResponse, error) {
	remote, ok := peer.FromContext(ctx)
	if !ok {
		return nil, fmt.Errorf("missing TLS peer")
	}
	info, ok := remote.AuthInfo.(credentials.TLSInfo)
	if !ok || len(info.State.PeerCertificates) == 0 {
		return nil, fmt.Errorf("missing actual TLS certificate")
	}
	principal, ok := certs.TenantPrincipalFromCert(info.State.PeerCertificates[0])
	if !ok || principal != s.principal {
		return nil, fmt.Errorf("operator identity changed")
	}
	return &pb.GetAgentVersionResponse{Version: "normal-operator-app"}, nil
}

type countedAppListener struct {
	net.Listener
	accepted atomic.Int32
}

func (l *countedAppListener) Accept() (net.Conn, error) {
	c, e := l.Listener.Accept()
	if e == nil {
		l.accepted.Add(1)
	}
	return c, e
}

type partialAppSocket struct{ net.Conn }

func (c partialAppSocket) Write(p []byte) (int, error) { return c.Conn.Write(p[:min(len(p), 17)]) }
func (c partialAppSocket) CloseWrite() error           { return c.Conn.(*net.TCPConn).CloseWrite() }

func TestNormalOperatorApplicationStreamReusesPrimaryTLSAndKeepsAgentRPC(t *testing.T) {
	for _, algorithm := range []string{"P256", "MLDSA65"} {
		t.Run(algorithm, func(t *testing.T) {
			now := time.Now()
			rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			rootTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "normal app root"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
			rootDER, err := x509.CreateCertificate(rand.Reader, rootTemplate, rootTemplate, rootKey.Public(), rootKey)
			if err != nil {
				t.Fatal(err)
			}
			root, err := x509.ParseCertificate(rootDER)
			if err != nil {
				t.Fatal(err)
			}
			rootPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER}))
			issue := func(serial int64, principal string, key crypto.Signer, usage x509.ExtKeyUsage) (string, string) {
				t.Helper()
				uri, _ := url.Parse(principal)
				leaf := &x509.Certificate{SerialNumber: big.NewInt(serial), URIs: []*url.URL{uri}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(30 * time.Minute), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage}}
				der, err := x509.CreateCertificate(rand.Reader, leaf, root, key.Public(), rootKey)
				if err != nil {
					t.Fatal(err)
				}
				private, err := x509.MarshalPKCS8PrivateKey(key)
				if err != nil {
					t.Fatal(err)
				}
				return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private}))
			}
			deviceKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			devicePEM, devicePrivate := issue(2, meshTestDevice, deviceKey, x509.ExtKeyUsageServerAuth)
			var operatorKey crypto.Signer
			if algorithm == "MLDSA65" {
				operatorKey, err = mldsa.GenerateKey(mldsa.MLDSA65())
			} else {
				operatorKey, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			}
			if err != nil {
				t.Fatal(err)
			}
			principal := "spiffe://wendy.sh/tenant/" + meshTestTenant + "/operator/alice"
			operatorPEM, operatorPrivate := issue(3, principal, operatorKey, x509.ExtKeyUsageClientAuth)
			server, err := mtls.NewServer(devicePEM, rootPEM, devicePrivate, zap.NewNop(), time.Time{}, testScope, interceptor.OrgModeStrict)
			if err != nil {
				t.Fatal(err)
			}
			defer server.Stop()
			application, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer application.Close()
			payload := bytes.Repeat([]byte("normal operator application payload\n"), 2048)
			appDone := make(chan error, 1)
			go func() {
				conn, err := application.Accept()
				if err != nil {
					appDone <- err
					return
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
				got, err := io.ReadAll(conn)
				if err == nil && !bytes.Equal(got, payload) {
					err = fmt.Errorf("app upload differs")
				}
				if err == nil {
					_, err = conn.Write(got)
				}
				appDone <- err
			}()
			svc, _ := newMeshServiceForTest(t)
			svc.dialLocal = func(_ string, timeout time.Duration) (net.Conn, error) {
				conn, err := net.DialTimeout("tcp", application.Addr().String(), timeout)
				if err != nil {
					return nil, err
				}
				return partialAppSocket{Conn: conn}, nil
			}
			v2.RegisterWendyMeshServiceServer(server, svc)
			pb.RegisterWendyAgentServiceServer(server, operatorApplicationAgent{principal: principal})
			raw, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			listener := &countedAppListener{Listener: raw}
			defer listener.Close()
			go server.Serve(listener)
			certificate := config.CertificateInfo{PemCertificate: operatorPEM, PemCertificateChain: rootPEM, PemPrivateKey: operatorPrivate, PrincipalURI: principal}
			expected := certs.WendyIdentity{EntityType: certs.EntityAsset, EntityID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", TenantUUID: meshTestTenant, Principal: meshTestDevice}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			client, err := grpcclient.ConnectWithTLSExpecting(ctx, listener.Addr().String(), &certificate, nil, &expected)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			setup, stop := context.WithCancel(ctx)
			flow, err := meshappstream.Dial(setup, client.Conn, 8080)
			stop()
			if err != nil {
				t.Fatal(err)
			}
			defer flow.Close()
			_ = flow.SetDeadline(time.Now().Add(10 * time.Second))
			if _, err := flow.Write(payload); err != nil {
				t.Fatal(err)
			}
			if err := flow.(interface{ CloseWrite() error }).CloseWrite(); err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(flow)
			if err != nil || !bytes.Equal(got, payload) {
				t.Fatal("normal app echo", len(got), err)
			}
			flow.Close()
			if err := <-appDone; err != nil {
				t.Fatal(err)
			}
			if _, err := client.AgentService.GetAgentVersion(ctx, &pb.GetAgentVersionRequest{}); err != nil {
				t.Fatal("closing app stream closed management", err)
			}
			if listener.accepted.Load() != 1 {
				t.Fatal("app stream added a second device TLS connection", listener.accepted.Load())
			}
			until := time.Now().Add(time.Second)
			for svc.active.Load() != 0 && time.Now().Before(until) {
				time.Sleep(time.Millisecond)
			}
			if svc.active.Load() != 0 {
				t.Fatal("app admission lease leaked")
			}
		})
	}
}
