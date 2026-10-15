//go:build linux

package bleprovider

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"net"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestPacketConnWriteStopsAfterNoProgress(t *testing.T) {
	a, _ := packetPair(t)
	a.writeIdleTimeout = 150 * time.Millisecond
	if err := boundL2CAPSendBuffer(a.fd); err != nil {
		t.Fatal(err)
	}
	_ = a.SetWriteDeadline(time.Now().Add(5 * time.Second))
	start := time.Now()
	n, err := a.Write(make([]byte, 1024*1024))
	var timeout net.Error
	if !errors.As(err, &timeout) || !timeout.Timeout() {
		t.Fatalf("write returned %d bytes, %v; want timeout", n, err)
	}
	if n == 0 || time.Since(start) > 2*time.Second {
		t.Fatalf("no-progress timeout not enforced: wrote %d bytes in %s", n, time.Since(start))
	}
}

func TestBoundedL2CAPSendBufferBackpressuresAndMakesProgress(t *testing.T) {
	a, b := packetPair(t)
	if err := boundL2CAPSendBuffer(a.fd); err != nil {
		t.Fatal(err)
	}
	actual, err := unix.GetsockoptInt(a.fd, unix.SOL_SOCKET, unix.SO_SNDBUF)
	if err != nil || actual < maxWriteSDU*2 || actual > max(meshSendBuffer*4, 4608) {
		t.Fatalf("L2CAP send buffer = %d, %v", actual, err)
	}
	a.writeIdleTimeout = 2 * time.Second
	_ = a.SetWriteDeadline(time.Now().Add(6 * time.Second))
	_ = b.SetReadDeadline(time.Now().Add(6 * time.Second))
	want := bytes.Repeat([]byte("ble-mesh"), 2048) // 16 KiB, larger than the kernel FIFO.
	type result struct {
		n   int
		err error
	}
	done := make(chan result, 1)
	go func() { n, err := a.Write(want); done <- result{n, err} }()
	select {
	case r := <-done:
		t.Fatalf("write finished without a reader: %+v", r)
	case <-time.After(100 * time.Millisecond):
	}
	got := make([]byte, len(want))
	for off := 0; off < len(got); {
		end := min(off+maxWriteSDU, len(got))
		n, err := b.Read(got[off:end])
		if err != nil {
			t.Fatal(err)
		}
		off += n
		time.Sleep(15 * time.Millisecond) // A slower-than-host, credit-limited link.
	}
	if r := <-done; r.err != nil || r.n != len(want) {
		t.Fatalf("bounded write = %+v; want %d bytes", r, len(want))
	}
	if !bytes.Equal(got, want) {
		t.Fatal("bounded L2CAP socket changed the byte stream")
	}
}

func packetPair(t *testing.T) (*packetConn, *packetConn) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	a := newPacketConn(fds[0], l2addr{}, l2addr{})
	b := newPacketConn(fds[1], l2addr{}, l2addr{})
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	return a, b
}

func TestPacketConnPreservesBytesAcrossSDUBoundaries(t *testing.T) {
	a, b := packetPair(t)
	_ = a.SetDeadline(time.Now().Add(3 * time.Second))
	_ = b.SetDeadline(time.Now().Add(3 * time.Second))
	want := bytes.Repeat([]byte("wendy"), 700)
	done := make(chan error, 1)
	go func() { _, err := a.Write(want); done <- err }()
	got := make([]byte, len(want))
	// TLS first reads the five-byte record header; a seqpacket adapter must
	// retain the rest of that SDU rather than discard it.
	if _, err := io.ReadFull(b, got[:5]); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(b, got[5:]); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("L2CAP SDU boundaries changed byte stream")
	}
}

func TestTLS13MutualAuthOverPacketConn(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "mesh.test"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		DNSNames: []string{"mesh.test"}, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}, &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "mesh.test"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		DNSNames: []string{"mesh.test"}, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	identity := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: cert}
	rawServer, rawClient := packetPair(t)
	for _, raw := range []*packetConn{rawServer, rawClient} {
		if err := boundL2CAPSendBuffer(raw.fd); err != nil {
			t.Fatal(err)
		}
	}
	_ = rawServer.SetDeadline(time.Now().Add(5 * time.Second))
	_ = rawClient.SetDeadline(time.Now().Add(5 * time.Second))
	server := tls.Server(rawServer, &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, Certificates: []tls.Certificate{identity}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool, NextProtos: []string{ALPN}})
	client := tls.Client(rawClient, &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, Certificates: []tls.Certificate{identity}, RootCAs: pool, ServerName: "mesh.test", NextProtos: []string{ALPN}})
	t.Cleanup(func() { _ = server.Close(); _ = client.Close() })
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Handshake() }()
	if err := client.Handshake(); err != nil {
		t.Fatalf("client handshake: %v", err)
	}
	if err := <-serverDone; err != nil {
		t.Fatalf("server handshake: %v", err)
	}
	if client.ConnectionState().Version != tls.VersionTLS13 || server.ConnectionState().NegotiatedProtocol != ALPN || len(server.ConnectionState().PeerCertificates) != 1 {
		t.Fatal("TLS 1.3 mTLS/ALPN not negotiated")
	}
	want := bytes.Repeat([]byte("ble"), 1200)
	done := make(chan error, 1)
	go func() { _, err := client.Write(want); done <- err }()
	got := make([]byte, len(want))
	if _, err := io.ReadFull(server, got); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("TLS payload changed")
	}
}
