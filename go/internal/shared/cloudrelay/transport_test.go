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
	"google.golang.org/grpc/keepalive"
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
					// TLSKeyPair omits a proven self-signed root: this server already
					// trusts it locally. The successful required-mTLS RPC proves the
					// normalized leaf authenticates; required intermediates have a
					// separate wire-retention regression in localmesh.
					if count != 1 {
						t.Fatalf("presented %d certificates, want normalized leaf without self-signed root", count)
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

func TestLiteBrokerHost(t *testing.T) {
	for _, tc := range []struct{ cloud, want string }{
		{"api.wendy.sh", "tunnel.wendy.dev"},
		{"api.wendy.dev", "tunnel.wendy.dev"},
		{"api.dev.wendy.sh", "tunnel.dev.wendy.dev"},
		{"api.dev.wendy.dev", "tunnel.dev.wendy.dev"},
		{"cloud.example", "cloud.example"},
		{"localhost", "localhost"},
		{"devices.wendy.dev", "devices.wendy.dev"},
	} {
		for _, cloud := range []string{tc.cloud, tc.cloud + ":9443", "https://" + tc.cloud, "https://" + tc.cloud + ":9443/"} {
			t.Run(cloud, func(t *testing.T) {
				got, err := LiteBrokerHost(cloud)
				if err != nil || got != tc.want {
					t.Fatalf("LiteBrokerHost(%q) = %q, %v; want %q", cloud, got, err, tc.want)
				}
			})
		}
	}
	for _, cloud := range []string{"", "http://cloud.example", "https://user:pass@cloud.example", "https://cloud.example/path", "https://cloud.example?query=1", "https://cloud.example#fragment", "cloud example"} {
		t.Run("invalid/"+cloud, func(t *testing.T) {
			if _, err := LiteBrokerHost(cloud); err == nil {
				t.Fatalf("accepted invalid Cloud endpoint %q", cloud)
			}
		})
	}
}

// Both Cloud dials must pass brokerKeepalive to gRPC. Pings faster than
// Cloud's 10s floor are answered with GOAWAY (BrokerHTTP2Config.swift), and an
// idle presence stream must keep pinging or it is dropped as idle.
func TestBrokerDialsUseCloudKeepalive(t *testing.T) {
	want := keepalive.ClientParameters{Time: 30 * time.Second, Timeout: 20 * time.Second, PermitWithoutStream: true}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "device"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	der, e := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if e != nil {
		t.Fatal(e)
	}
	rawKey, _ := x509.MarshalECPrivateKey(key)
	certPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: rawKey})

	for name, dial := range map[string]func() (*grpc.ClientConn, error){
		"DialCloud":  func() (*grpc.ClientConn, error) { return DialCloud("devices.example:443", certPEM, certPEM, keyPEM) },
		"DialBroker": func() (*grpc.ClientConn, error) { return DialBroker("broker.example:443") },
	} {
		t.Run(name, func(t *testing.T) {
			var got []keepalive.ClientParameters
			defer func(orig func(keepalive.ClientParameters) grpc.DialOption) { withKeepalive = orig }(withKeepalive)
			withKeepalive = func(p keepalive.ClientParameters) grpc.DialOption {
				got = append(got, p)
				return grpc.WithKeepaliveParams(p)
			}
			conn, e := dial()
			if e != nil {
				t.Fatal(e)
			}
			conn.Close()
			if len(got) != 1 || got[0] != want {
				t.Fatalf("keepalive dial options = %+v, want exactly %+v", got, want)
			}
		})
	}
}
