package grpcclient_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/url"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// Exercise the actual TLS callback, rather than injecting an observed identity.
// All leaves share one trusted CA so tenant/device failures must come from the
// identity check rather than an incidental chain failure.
func TestVerifiedUUIDServerHandshakeAndRenewal(t *testing.T) {
	t.Setenv("WENDY_TLS_SESSION_STORE", "off")
	const tenant = "22f35004-d638-4b5c-869f-b815974d6eae"
	principal := certs.DeviceSPIFFEURI(tenant, "03ce308b-4c00-40c2-bc64-5f66a83d4f37")
	expected, err := certs.ParsePrincipal(principal)
	if err != nil {
		t.Fatal(err)
	}
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "UUID observation test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	caPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))
	issue := func(principal string, usage x509.ExtKeyUsage) (string, string) {
		t.Helper()
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		uri, err := url.Parse(principal)
		if err != nil {
			t.Fatal(err)
		}
		template := &x509.Certificate{
			SerialNumber: big.NewInt(time.Now().UnixNano()), URIs: []*url.URL{uri},
			NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
			KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage},
		}
		der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		keyDER, _ := x509.MarshalECPrivateKey(key)
		return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	}
	operator := "spiffe://wendy.sh/tenant/" + tenant + "/operator/test"
	clientPEM, clientKey := issue(operator, x509.ExtKeyUsageClientAuth)
	client := &config.CertificateInfo{PemCertificate: clientPEM, PemPrivateKey: clientKey, PemCertificateChain: caPEM, PrincipalURI: operator}
	for _, tc := range []struct {
		name, principal string
		expected        *certs.WendyIdentity
		want            bool
	}{
		{"first verified UUID observation", principal, nil, true},
		{"renewed leaf and key same principal", principal, &expected, true},
		{"wrong device", certs.DeviceSPIFFEURI(tenant, "11111111-1111-4111-8111-111111111111"), &expected, false},
		{"same device UUID different tenant", certs.DeviceSPIFFEURI("11111111-1111-4111-8111-111111111111", expected.EntityID), &expected, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			leafPEM, leafKey := issue(tc.principal, x509.ExtKeyUsageServerAuth)
			pair, err := tls.X509KeyPair([]byte(leafPEM), []byte(leafKey))
			if err != nil {
				t.Fatal(err)
			}
			roots := x509.NewCertPool()
			roots.AddCert(ca)
			server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
				MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}, ClientCAs: roots, ClientAuth: tls.RequireAndVerifyClientCert,
			})))
			agentpb.RegisterWendyAgentServiceServer(server, &versionService{})
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			go server.Serve(listener)
			defer server.Stop()
			conn, err := grpcclient.ConnectWithTLSExpecting(context.Background(), listener.Addr().String(), client, nil, tc.expected)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, err = conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
			if tc.want {
				if err != nil {
					t.Fatal(err)
				}
				if got, ok := conn.ObservedServerIdentity(); !ok || got != expected {
					t.Fatalf("verified UUID identity missing: (%+v, %v)", got, ok)
				}
			} else {
				if err == nil {
					t.Fatal("wrong tenant/device completed an authenticated RPC")
				}
				if got, ok := conn.ObservedServerIdentity(); ok {
					t.Fatalf("rejected handshake published trusted identity: %+v", got)
				}
				if _, ok := conn.IdentityMismatch(); !ok {
					t.Fatal("wrong identity did not record an identity mismatch")
				}
			}
		})
	}
}
