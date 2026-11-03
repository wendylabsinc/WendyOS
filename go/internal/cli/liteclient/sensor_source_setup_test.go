package liteclient

import (
	"context"
	"crypto/tls"
	"encoding/pem"
	"errors"
	"net"
	"testing"
	"time"
)

func TestNormalSensorSourceAllowsSlowAuthenticatedSetup(t *testing.T) {
	const base = "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/device/"
	root, rootKey := issueRelayCertificate(t, 1, "root", "", true, nil, nil)
	consumer, consumerKey := issueRelayCertificate(t, 2, "consumer", base+"consumer", false, root, rootKey)
	source, sourceKey := issueRelayCertificate(t, 3, "source", base+"source", false, root, rootKey)
	cert := tls.Certificate{Certificate: [][]byte{consumer.Raw}, PrivateKey: consumerKey}
	chain := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Raw}))
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan error, 1)
	go func() {
		raw, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer raw.Close()
		raw.SetDeadline(time.Now().Add(3 * time.Second))
		// Scaled normal radio delays exceed the old combined dial ceiling in
		// both TLS delivery and the source's post-certificate protocol reply.
		time.Sleep(150 * time.Millisecond)
		secure := tls.Server(raw, &tls.Config{MinVersion: tls.VersionTLS13,
			Certificates: []tls.Certificate{{Certificate: [][]byte{source.Raw}, PrivateKey: sourceKey}},
			ClientAuth:   tls.RequireAnyClientCert})
		if err := secure.Handshake(); err != nil {
			done <- err
			return
		}
		link := newDirectLink(secure)
		request, err := link.recv(time.Second)
		if err == nil {
			time.Sleep(150 * time.Millisecond)
			err = link.send(request)
		}
		done <- err
	}()
	client := NewWendyLiteClient()
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.connectSensorSourceWithBudgets(ctx, listener.Addr().String(), cert, chain, base+"source", 100*time.Millisecond, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestNormalSensorSourceTLSSetupRemainsBounded(t *testing.T) {
	const base = "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/device/"
	root, rootKey := issueRelayCertificate(t, 1, "root", "", true, nil, nil)
	consumer, key := issueRelayCertificate(t, 2, "consumer", base+"consumer", false, root, rootKey)
	cert := tls.Certificate{Certificate: [][]byte{consumer.Raw}, PrivateKey: key}
	chain := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Raw}))
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	client := NewWendyLiteClient()
	defer client.Close()
	// Model a source that has accepted TCP but has not yet become ready to
	// process its certificate flight. The longer TLS phase is still finite.
	started := time.Now()
	err = client.connectSensorSourceWithBudgets(context.Background(), listener.Addr().String(), cert, chain, base+"source", 100*time.Millisecond, 75*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("setup error = %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("native TLS setup exceeded its finite phase budget")
	}
	select {
	case conn := <-accepted:
		defer conn.Close()
	case <-time.After(time.Second):
		t.Fatal("source did not accept TCP")
	}
}
