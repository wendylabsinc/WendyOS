//go:build linux

package bleprovider

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/url"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshoperatorlink"
	"go.uber.org/zap"
)

type operatorAdmissionNode struct {
	t           *testing.T
	attached    chan string
	credentials *localmesh.Credentials
}

func (n *operatorAdmissionNode) AttachStream(context.Context, meshid.Device, net.Conn, uint16) error {
	n.t.Error("operator reached the router-neighbour attachment path")
	return io.ErrUnexpectedEOF
}
func (n *operatorAdmissionNode) AttachOperatorStream(_ context.Context, stream *tls.Conn, cost uint16) error {
	state := stream.ConnectionState()
	chain := make([][]byte, len(state.PeerCertificates))
	for i, cert := range state.PeerCertificates {
		chain[i] = cert.Raw
	}
	identity, err := n.credentials.VerifyPrimaryOperator(chain, time.Now())
	if err != nil {
		return err
	}
	if cost != LinkCost || state.NegotiatedProtocol != meshoperatorlink.RouterALPN {
		n.t.Error("operator lost its BLE role or cost")
	}
	n.attached <- identity.Operator().Principal()
	var payload [4]byte
	if _, err := io.ReadFull(stream, payload[:]); err != nil {
		return err
	}
	_, err = stream.Write(payload[:])
	return err
}

func bleOperatorCredentials(t *testing.T, algorithm string) (*localmesh.Credentials, tls.Certificate, *x509.CertPool, string) {
	t.Helper()
	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	root := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	rootDER, err := x509.CreateCertificate(rand.Reader, root, root, rootKey.Public(), rootKey)
	if err != nil {
		t.Fatal(err)
	}
	root, err = x509.ParseCertificate(rootDER)
	if err != nil {
		t.Fatal(err)
	}
	issue := func(principal string, serial int64, key crypto.Signer) tls.Certificate {
		uri, err := url.Parse(principal)
		if err != nil {
			t.Fatal(err)
		}
		leaf := &x509.Certificate{SerialNumber: big.NewInt(serial), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), URIs: []*url.URL{uri}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}}
		der, err := x509.CreateCertificate(rand.Reader, leaf, root, key.Public(), rootKey)
		if err != nil {
			t.Fatal(err)
		}
		return tls.Certificate{Certificate: [][]byte{der, rootDER}, PrivateKey: key}
	}
	deviceKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	device := issue(selfFixture.Principal(), 2, deviceKey)
	keyDER, err := x509.MarshalPKCS8PrivateKey(deviceKey)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := localmesh.NewCredentials(string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: device.Certificate[0]})), string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER})), string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})))
	if err != nil {
		t.Fatal(err)
	}
	var operatorKey crypto.Signer
	if algorithm == "MLDSA65" {
		operatorKey, err = mldsa.GenerateKey(mldsa.MLDSA65())
	} else {
		operatorKey, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	}
	if err != nil {
		t.Fatal(err)
	}
	principal := "spiffe://wendy.sh/tenant/" + selfFixture.Tenant().String() + "/operator/auth0%7Calice"
	pool := x509.NewCertPool()
	pool.AddCert(root)
	return credentials, issue(principal, 3, operatorKey), pool, principal
}

// Normal operator attachment remains possible with every router-neighbour slot
// in use. It authenticates with the real primary policy and reuses that TLS
// stream for management, then releases its separate capacity for the next call.
func TestBLEOperatorAdmissionAlongsideFullRouterCapacity(t *testing.T) {
	for _, algorithm := range []string{"P256", "MLDSA65"} {
		t.Run(algorithm, func(t *testing.T) {
			credentials, certificate, roots, principal := bleOperatorCredentials(t, algorithm)
			node := &operatorAdmissionNode{t: t, attached: make(chan string, 1), credentials: credentials}
			r := &runtime{cfg: Config{Credentials: credentials, Node: node, NetworkName: "lab", Logger: zap.NewNop()}}
			if err := r.cfg.defaults(); err != nil {
				t.Fatal(err)
			}
			r.serverTLS = r.makeServerTLS()
			policy := r.connectionPolicy()
			var reservations []*localmesh.ConnectionLease
			for i := 0; i < localmesh.BLEMaxConnections; i++ {
				lease := policy.Reserve(string(rune('a'+i)), meshid.Device{}, false)
				if lease == nil {
					t.Fatal("could not fill normal router capacity")
				}
				reservations = append(reservations, lease)
			}
			defer func() {
				for _, lease := range reservations {
					lease.Release()
				}
			}()
			before := policy.Counts()
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			for range 2 {
				done := make(chan struct{})
				go func() {
					defer close(done)
					raw, err := listener.Accept()
					if err != nil {
						return
					}
					r.acceptLink(ctx, raw)
				}()
				client, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp4", listener.Addr().String(), &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate}, RootCAs: roots, ServerName: "unused", InsecureSkipVerify: true, NextProtos: []string{meshoperatorlink.RouterALPN}, CurvePreferences: []tls.CurveID{tls.X25519}, VerifyConnection: func(state tls.ConnectionState) error {
					chain := make([][]byte, len(state.PeerCertificates))
					for i, certificate := range state.PeerCertificates {
						chain[i] = certificate.Raw
					}
					_, err := credentials.Verify(chain, time.Now())
					return err
				}})
				if err != nil {
					t.Fatal(err)
				}
				if got := <-node.attached; got != principal {
					t.Fatalf("operator=%s", got)
				}
				if policy.Counts() != before {
					t.Fatal("operator consumed a router slot")
				}
				if _, err = client.Write([]byte("ping")); err != nil {
					t.Fatal(err)
				}
				var reply [4]byte
				if _, err = io.ReadFull(client, reply[:]); err != nil || string(reply[:]) != "ping" {
					t.Fatalf("same-TLS management %q: %v", reply, err)
				}
				client.Close()
				<-done
				if len(r.operatorAdmission()) != 0 {
					t.Fatal("operator capacity was not released")
				}
			}
		})
	}
}
