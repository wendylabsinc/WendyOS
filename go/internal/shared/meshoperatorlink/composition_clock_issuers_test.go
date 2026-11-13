package meshoperatorlink

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshoperator"
)

// Exercise the integration of shortened known-CA TLS presentations and the
// explicit mesh clock, rather than testing either admission feature alone.
func TestExplicitRouterClockAndKnownIssuerCompletionCompose(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	identity, root := operatorClockFixture(t, now.Add(12*time.Hour), now.Add(13*time.Hour))
	key := identity.PrivateKey.(*ecdsa.PrivateKey)
	issuerTemplate := &x509.Certificate{SerialNumber: big.NewInt(3), Subject: pkix.Name{CommonName: "operator clock intermediate"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	issuerDER, err := x509.CreateCertificate(rand.Reader, issuerTemplate, root, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := x509.ParseCertificate(issuerDER)
	if err != nil {
		t.Fatal(err)
	}
	original, err := x509.ParseCertificate(identity.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	leafTemplate := &x509.Certificate{SerialNumber: big.NewInt(4), NotBefore: original.NotBefore, NotAfter: original.NotAfter, URIs: original.URIs, KeyUsage: original.KeyUsage, ExtKeyUsage: original.ExtKeyUsage}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, issuer, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	identity.Certificate = [][]byte{leafDER} // The intermediate is intentionally omitted.
	tenant, err := meshid.ParseTenant("11111111-1111-4111-8111-111111111111")
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"completed-uncertain", "missing-issuer", "mutated-presentation", "completed-certain", "completed-expired"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			a, b := net.Pipe()
			defer a.Close()
			defer b.Close()
			server := tls.Server(a, &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, Certificates: []tls.Certificate{identity}, ClientAuth: tls.RequireAnyClientCert, NextProtos: []string{RouterALPN}})
			client := tls.Client(b, &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, Certificates: []tls.Certificate{identity}, InsecureSkipVerify: true, NextProtos: []string{RouterALPN}})
			wall, base := now.Add(-598*time.Second), now
			if mode == "completed-expired" {
				base = now.Add(14 * time.Hour)
			}
			policy := certs.CertificateTimePolicy{Now: func() time.Time { return wall }, Floor: func() time.Time { return base }, Uncertain: func() bool { return mode != "completed-certain" }}
			var called atomic.Bool
			var candidates IssuerCandidates
			if mode != "missing-issuer" {
				candidates = func(wire [][]byte, at time.Time) [][]byte {
					called.Store(true)
					if !at.Equal(wall) {
						t.Error("issuer completion used a scheduling clock instead of certificate UTC")
					}
					if mode == "mutated-presentation" {
						wire[0][0] ^= 1
					}
					return append(wire, issuerDER)
				}
			}
			result := make(chan error, 1)
			go func() {
				defer a.Close()
				if err := server.HandshakeContext(ctx); err != nil {
					result <- err
					return
				}
				link, err := AcceptRouterWithTimePolicy(ctx, server, tenant, []*x509.Certificate{root}, policy, nil, candidates)
				if link != nil {
					link.Close()
					link.Wait()
				}
				result <- err
			}()
			if err := client.HandshakeContext(ctx); err != nil {
				t.Fatal(err)
			}
			_ = client.SetDeadline(time.Now().Add(time.Second))
			_ = writeControl(client, control{Kind: "operator-router-hello", Instance: meshoperator.InstanceID{1}})
			reply, readErr := readControl(client)
			want := mode == "completed-uncertain"
			if want && (readErr != nil || reply.Kind != "operator-router-ready") {
				t.Fatal("composed admission did not become ready", reply, readErr)
			}
			select {
			case err := <-result:
				if (err == nil) != want {
					t.Fatal("unexpected composed admission", mode, err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if mode != "missing-issuer" && !called.Load() {
				t.Fatal("known-issuer completion was bypassed")
			}
			if !bytes.Equal(server.ConnectionState().PeerCertificates[0].Raw, leafDER) {
				t.Fatal("candidate completion mutated actual TLS peer identity")
			}
		})
	}
}
