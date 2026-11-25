package meshoperatorlink

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshoperator"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

func TestOperatorCertaintyWithdrawsNotBeforeGrace(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	cert, root := operatorClockFixture(t, now.Add(time.Hour), now.Add(2*time.Hour))
	tenant, _ := meshid.ParseTenant("11111111-1111-4111-8111-111111111111")
	var uncertain atomic.Bool
	uncertain.Store(true)
	policy := certs.CertificateTimePolicy{Now: func() time.Time { return now }, Uncertain: uncertain.Load}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	server := tls.Server(a, &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}, ClientAuth: tls.RequireAnyClientCert, NextProtos: []string{RouterALPN}})
	client := tls.Client(b, &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}, InsecureSkipVerify: true, NextProtos: []string{RouterALPN}})
	type result struct {
		l *RouterLink
		e error
	}
	done := make(chan result, 1)
	go func() {
		if e := server.HandshakeContext(ctx); e != nil {
			done <- result{e: e}
			return
		}
		l, e := AcceptRouterWithTimePolicy(ctx, server, tenant, []*x509.Certificate{root}, policy, nil)
		done <- result{l, e}
	}()
	if e := client.HandshakeContext(ctx); e != nil {
		t.Fatal(e)
	}
	if e := writeControl(client, control{Kind: "operator-router-hello", Instance: meshoperator.InstanceID{1}}); e != nil {
		t.Fatal(e)
	}
	if reply, e := readControl(client); e != nil || reply.Kind != "operator-router-ready" {
		t.Fatal(reply, e)
	}
	r := <-done
	if r.e != nil {
		t.Fatal(r.e)
	}
	defer func() { r.l.Close(); r.l.Wait() }()
	if !r.l.Live() {
		t.Fatal("expected initial boot grace")
	}
	uncertain.Store(false)
	select {
	case <-r.l.Done():
	case <-time.After(1300 * time.Millisecond):
		t.Fatal("operator lease remains active after local certainty withdraws leaf NotBefore grace")
	}
}
