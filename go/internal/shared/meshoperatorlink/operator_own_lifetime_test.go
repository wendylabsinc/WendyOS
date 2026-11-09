package meshoperatorlink

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshoperator"
)

func TestOperatorOwnPathCapsAndRevalidatesLifetime(t *testing.T) {
	for _, mode := range []string{"expiry", "not-before"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Now().Truncate(time.Second)
			certificate, root := operatorClockFixture(t, now.Add(-time.Minute), now.Add(2*time.Hour))
			tenant, _ := meshid.ParseTenant("11111111-1111-4111-8111-111111111111")
			var clock atomic.Value
			clock.Store(now)
			var invalid atomic.Bool
			policy := certs.CertificateTimePolicy{Now: func() time.Time { return now }, Floor: func() time.Time { return clock.Load().(time.Time) }, Uncertain: func() bool { return true }}
			own := func() (time.Time, error) {
				if invalid.Load() {
					return time.Time{}, errors.New("own path no longer valid")
				}
				return now.Add(time.Minute), nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			a, b := net.Pipe()
			defer a.Close()
			defer b.Close()
			server := tls.Server(a, &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate}, ClientAuth: tls.RequireAnyClientCert, NextProtos: []string{RouterALPN}})
			client := tls.Client(b, &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate}, InsecureSkipVerify: true, NextProtos: []string{RouterALPN}})
			type result struct {
				link *RouterLink
				err  error
			}
			resultC := make(chan result, 1)
			go func() {
				if err := server.HandshakeContext(ctx); err != nil {
					resultC <- result{err: err}
					return
				}
				link, err := AcceptRouterWithTimePolicyAndOwn(ctx, server, tenant, []*x509.Certificate{root}, policy, own, nil)
				resultC <- result{link, err}
			}()
			if err := client.HandshakeContext(ctx); err != nil {
				t.Fatal(err)
			}
			if err := writeControl(client, control{Kind: "operator-router-hello", Instance: meshoperator.InstanceID{1}}); err != nil {
				t.Fatal(err)
			}
			if _, err := readControl(client); err != nil {
				t.Fatal(err)
			}
			r := <-resultC
			if r.err != nil {
				t.Fatal(r.err)
			}
			defer func() { r.link.Close(); r.link.Wait() }()
			if remaining := time.Until(r.link.currentExpiry()); remaining > 61*time.Second || remaining < 58*time.Second {
				t.Fatal("own selected path did not cap peer lease", remaining)
			}
			if mode == "expiry" {
				clock.Store(now.Add(2 * time.Minute))
			} else {
				invalid.Store(true)
			}
			if r.link.Live() {
				t.Fatal("invalid own path stayed live")
			}
			clock.Store(now)
			invalid.Store(false)
			if r.link.Live() {
				t.Fatal("own path invalidity resurrected")
			}
		})
	}
}
