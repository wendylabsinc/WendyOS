package services

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
)

type legacyRefreshContract struct {
	cloudpb.UnimplementedCertificateServiceServer
	refresh func(context.Context, *cloudpb.RefreshCertificateRequest) (*cloudpb.RefreshCertificateResponse, error)
}

func (f legacyRefreshContract) RefreshCertificate(ctx context.Context, req *cloudpb.RefreshCertificateRequest) (*cloudpb.RefreshCertificateResponse, error) {
	return f.refresh(ctx, req)
}

func TestLegacyRenewalRotatesKeyAndKeepsProtocol(t *testing.T) {
	for _, dual := range []bool{false, true} {
		t.Run(fmt.Sprint(dual), func(t *testing.T) {
			ca, caKey, chain := brokerTestCA(t)
			leaf, key := legacyBrokerTestLeaf(t, dual)
			parsed, err := storedDeviceLeaf(leaf)
			if err != nil {
				t.Fatal(err)
			}
			parsed.NotBefore = time.Now().Add(-90 * 24 * time.Hour)
			parsed.NotAfter = time.Now().Add(5 * 24 * time.Hour)
			der, err := x509.CreateCertificate(rand.Reader, parsed, ca, parsed.PublicKey, caKey)
			if err != nil {
				t.Fatal(err)
			}
			leaf = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			serverCert := brokerTestServer(t, ca, caKey, "cloud", nil, []net.IP{net.ParseIP("127.0.0.1")}, x509.ExtKeyUsageServerAuth)
			server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{serverCert}, MinVersion: tls.VersionTLS12})))
			cloudpb.RegisterCertificateServiceServer(server, legacyRefreshContract{refresh: func(ctx context.Context, req *cloudpb.RefreshCertificateRequest) (*cloudpb.RefreshCertificateResponse, error) {
				md, _ := metadata.FromIncomingContext(ctx)
				if got := md.Get("x-wendy-certificate-uri"); len(got) != 1 || got[0] != "urn:wendy:org:64:asset:674" {
					return nil, fmt.Errorf("missing legacy identity")
				}
				if len(md.Get("x-wendy-certificate-signature")) != 1 {
					return nil, fmt.Errorf("missing signed proof")
				}
				block, _ := pem.Decode([]byte(req.GetPemCsr()))
				if block == nil {
					return nil, fmt.Errorf("missing CSR")
				}
				csr, err := x509.ParseCertificateRequest(block.Bytes)
				if err != nil {
					return nil, err
				}
				if err := csr.CheckSignature(); err != nil {
					return nil, err
				}
				if csr.Subject.CommonName != parsed.Subject.CommonName || len(csr.URIs) != len(parsed.URIs) {
					return nil, fmt.Errorf("renewal lost identity")
				}
				tmpl := *parsed
				tmpl.SerialNumber = big.NewInt(123)
				tmpl.PublicKey = csr.PublicKey
				tmpl.NotBefore = time.Now().Add(-time.Minute)
				tmpl.NotAfter = time.Now().Add(180 * 24 * time.Hour)
				issued, err := x509.CreateCertificate(rand.Reader, &tmpl, ca, csr.PublicKey, caKey)
				if err != nil {
					return nil, err
				}
				return &cloudpb.RefreshCertificateResponse{Certificate: &cloudpb.Certificate{PemCertificate: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: issued})), PemCertificateChain: chain}}, nil
			}})
			go func() { _ = server.Serve(listener) }()
			defer server.Stop()
			dir := t.TempDir()
			state, _ := json.Marshal(provisioningState{Enrolled: true, CloudHost: listener.Addr().String(), OrgID: 64, AssetID: 674, CertPEM: leaf, ChainPEM: chain})
			if err := WriteDeviceKey(dir, []byte(key)); err != nil {
				t.Fatal(err)
			}
			if err := WriteProvisioningState(dir, state); err != nil {
				t.Fatal(err)
			}
			svc := NewProvisioningService(zap.NewNop(), dir)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			renewed, err := svc.RenewIfDue(ctx, time.Now())
			if err != nil || !renewed {
				t.Fatalf("renewed=%t: %v", renewed, err)
			}
			certificate, _, rotated := svc.ProvisioningCerts()
			if certificate == leaf || string(rotated) == key {
				t.Fatal("identity did not rotate")
			}
			reloaded := NewProvisioningService(zap.NewNop(), dir)
			restored, _, restoredKey := reloaded.ProvisioningCerts()
			if restored != certificate || string(restoredKey) != string(rotated) {
				t.Fatal("renewal did not survive restart")
			}
			enrollment, err := reloaded.CloudEnrollment()
			if err != nil || enrollment.Mode != CloudLegacy {
				t.Fatalf("protocol changed: %+v %v", enrollment, err)
			}
			if _, err := os.Stat(filepath.Join(dir, pendingDeviceKeyFile)); !os.IsNotExist(err) {
				t.Fatal("pending key was not retired")
			}
		})
	}
}

func TestLegacyRenewalRejectsIdentityChange(t *testing.T) {
	ca, caKey, chain := brokerTestCA(t)
	leaf, key := legacyBrokerTestLeaf(t, false)
	parsed, _ := storedDeviceLeaf(leaf)
	previous, _, _ := certs.IdentityFromCert(parsed)
	for _, identity := range []string{"urn:wendy:org:64:asset:675", "urn:wendy:org:65:asset:674"} {
		uri, _ := url.Parse(identity)
		parsed.URIs = []*url.URL{uri}
		der, err := x509.CreateCertificate(rand.Reader, parsed, ca, parsed.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		issued := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
		if _, _, err := checkLegacyRenewedIdentity(issued, chain, key, previous, chain, time.Now()); err == nil {
			t.Fatalf("accepted %s", identity)
		}
	}
	otherCA, otherKey, _ := brokerTestCA(t)
	uri, _ := url.Parse(previous.LegacyURN())
	parsed.URIs = []*url.URL{uri}
	der, err := x509.CreateCertificate(rand.Reader, parsed, otherCA, parsed.PublicKey, otherKey)
	if err != nil {
		t.Fatal(err)
	}
	issued := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	if _, _, err := checkLegacyRenewedIdentity(issued, chain, key, previous, chain, time.Now()); err == nil {
		t.Fatal("accepted untrusted issuer")
	}
}
