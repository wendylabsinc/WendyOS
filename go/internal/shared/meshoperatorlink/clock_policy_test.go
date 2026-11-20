package meshoperatorlink

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"math/big"
	"net"
	"net/netip"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshoperator"
)

func TestExplicitLinkClockLifetimeUsesBaseWithoutExtendingResourceLease(t *testing.T) {
	for _, correction := range []string{"forward", "backward"} {
		t.Run(correction, func(t *testing.T) {
			base := time.Now()
			var clock atomic.Value
			clock.Store(base)
			a, b := net.Pipe()
			defer b.Close()
			l, err := readyWithClock(context.Background(), tls.Client(a, &tls.Config{InsecureSkipVerify: true}), netip.Addr{}, false, base.Add(300*time.Millisecond), func() time.Time { return clock.Load().(time.Time) })
			if err != nil {
				t.Fatal(err)
			}
			defer func() { l.Close(); l.Wait() }()
			if !l.Live() {
				t.Fatal("new link is not live")
			}
			if correction == "forward" {
				clock.Store(base.Add(time.Hour))
				if l.Live() {
					t.Fatal("forward correction left expired credential live")
				}
			} else {
				clock.Store(base.Add(-time.Hour))
			}
			select {
			case <-l.Done():
			case <-time.After(2 * time.Second):
				t.Fatal("clock correction extended fixed carrier lifetime")
			}
		})
	}
}

func operatorClockFixture(t testing.TB, nb, na time.Time) (tls.Certificate, *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	root := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: nb.Add(-48 * time.Hour), NotAfter: na.Add(48 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, root, root, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	root, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	principal, _ := url.Parse("spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/operator/clock-test")
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: nb, NotAfter: na, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}, URIs: []*url.URL{principal}}
	der, err = x509.CreateCertificate(rand.Reader, leaf, root, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, root
}

func TestAcceptRouterExplicitPolicyAndLegacyIsolation(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	certificate, root := operatorClockFixture(t, now.Add(12*time.Hour), now.Add(13*time.Hour))
	tenant, err := meshid.ParseTenant("11111111-1111-4111-8111-111111111111")
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"legacy", "uncertain", "certain", "expired-base"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			a, b := net.Pipe()
			server := tls.Server(a, &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate}, ClientAuth: tls.RequireAnyClientCert, NextProtos: []string{RouterALPN}})
			client := tls.Client(b, &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate}, InsecureSkipVerify: true, NextProtos: []string{RouterALPN}})
			defer a.Close()
			defer b.Close()
			result := make(chan error, 1)
			go func() {
				if err := server.HandshakeContext(ctx); err != nil {
					result <- err
					return
				}
				var link *RouterLink
				var err error
				if mode == "legacy" {
					link, err = AcceptRouter(ctx, server, tenant, []*x509.Certificate{root}, nil)
				} else {
					base := now
					if mode == "expired-base" {
						base = now.Add(14 * time.Hour)
					}
					policy := certs.CertificateTimePolicy{Now: func() time.Time { return now.Add(-598 * time.Second) }, Floor: func() time.Time { return base }, Uncertain: func() bool { return mode != "certain" }}
					link, err = AcceptRouterWithTimePolicy(ctx, server, tenant, []*x509.Certificate{root}, policy, nil)
				}
				if link != nil {
					if !link.Live() {
						err = net.ErrClosed
					}
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
			if mode == "uncertain" {
				if reply, err := readControl(client); err != nil || reply.Kind != "operator-router-ready" {
					t.Fatal("explicit policy did not finish readiness", reply, err)
				}
			}
			select {
			case err := <-result:
				if (err == nil) != (mode == "uncertain") {
					t.Fatal("unexpected admission", mode, err)
				}
			case <-ctx.Done():
				t.Fatal("router admission did not terminate", ctx.Err())
			}
		})
	}
}
