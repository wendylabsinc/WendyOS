package cloudrelay

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
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

func TestDeviceCloudMTLSChainAndHostname(t *testing.T) {
	rootKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Cloud test CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	rootDER, e := x509.CreateCertificate(rand.Reader, ca, ca, &rootKey.PublicKey, rootKey)
	if e != nil {
		t.Fatal(e)
	}
	rootPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER}))
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM([]byte(rootPEM))
	issue := func(serial int64, ips []net.IP) (string, string) {
		key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		template := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "test"}, NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, IPAddresses: ips, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
		cert, e := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, rootKey)
		if e != nil {
			t.Fatal(e)
		}
		raw, _ := x509.MarshalECPrivateKey(key)
		return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert})), string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: raw}))
	}
	clientCert, clientKey := issue(2, nil)
	for _, validName := range []bool{true, false} {
		t.Run(map[bool]string{true: "valid", false: "wrong hostname"}[validName], func(t *testing.T) {
			ip := net.ParseIP("127.0.0.1")
			if !validName {
				ip = net.ParseIP("192.0.2.1")
			}
			serverCert, serverKey := issue(3, []net.IP{ip})
			pair, e := tls.X509KeyPair([]byte(serverCert+rootPEM), []byte(serverKey))
			if e != nil {
				t.Fatal(e)
			}
			presented := make(chan int, 1)
			listener, e := net.Listen("tcp", "127.0.0.1:0")
			if e != nil {
				t.Fatal(e)
			}
			srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots, VerifyConnection: func(cs tls.ConnectionState) error {
				select {
				case presented <- len(cs.PeerCertificates):
				default:
				}
				return nil
			}})))
			healthpb.RegisterHealthServer(srv, health.NewServer())
			go srv.Serve(listener)
			defer srv.Stop()
			conn, e := DialCloud(listener.Addr().String(), clientCert, rootPEM, []byte(clientKey))
			if e != nil {
				t.Fatal(e)
			}
			defer conn.Close()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, e = healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
			if validName {
				if e != nil {
					t.Fatal(e)
				}
				select {
				case count := <-presented:
					if count != 2 {
						t.Fatalf("presented %d certificates, want complete leaf+CA chain", count)
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			} else if e == nil {
				t.Fatal("accepted Cloud certificate for wrong hostname")
			}
		})
	}
}
