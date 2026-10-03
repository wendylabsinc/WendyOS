//go:build linux

package bleprovider

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
)

type firstWriteConn struct {
	net.Conn
	mu    sync.Mutex
	first []byte
}

func (c *firstWriteConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	if c.first == nil {
		c.first = append([]byte(nil), p...)
	}
	c.mu.Unlock()
	return c.Conn.Write(p)
}

func (c *firstWriteConn) firstRecordBytes(t *testing.T) int {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.first) < 5 || c.first[0] != 0x16 {
		t.Fatalf("first client write is not a TLS handshake record: %x", c.first)
	}
	size := 5 + (int(c.first[3]) << 8) + int(c.first[4])
	if len(c.first) < size {
		t.Fatalf("first ClientHello record truncated: got %d want %d", len(c.first), size)
	}
	return size
}

func testTLSIdentity(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "mesh.test"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		DNSNames: []string{"mesh.test"}, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: cert}, pool
}

// A fresh and a resumed mTLS handshake both exchange the same ClientHello
// key share. Count the exact TLS record that packetConn would split into
// 240-byte CoC SDUs, independent of radio timing or certificate size.
func TestBLECurvePreferenceShrinksFullAndResumedClientHello(t *testing.T) {
	identity, roots := testTLSIdentity(t)
	type result struct{ cold, warm int }
	measure := func(curves []tls.CurveID) result {
		clientCfg := &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13,
			Certificates: []tls.Certificate{identity}, RootCAs: roots, ServerName: "mesh.test",
			NextProtos: []string{ALPN}, ClientSessionCache: tls.NewLRUClientSessionCache(2),
			CurvePreferences: curves}
		serverCfg := &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13,
			Certificates: []tls.Certificate{identity}, ClientAuth: tls.RequireAndVerifyClientCert,
			ClientCAs: roots, NextProtos: []string{ALPN}, CurvePreferences: curves}
		connect := func(wantResume bool) int {
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			clientRaw, err := net.Dial("tcp4", listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			serverRaw, err := listener.Accept()
			if err != nil {
				clientRaw.Close()
				t.Fatal(err)
			}
			defer serverRaw.Close()
			defer clientRaw.Close()
			_ = serverRaw.SetDeadline(time.Now().Add(5 * time.Second))
			_ = clientRaw.SetDeadline(time.Now().Add(5 * time.Second))
			recorded := &firstWriteConn{Conn: clientRaw}
			server := tls.Server(serverRaw, serverCfg)
			client := tls.Client(recorded, clientCfg)
			serverDone := make(chan error, 1)
			go func() {
				if err := server.Handshake(); err != nil {
					serverDone <- err
					return
				}
				_, err := server.Write([]byte{1})
				serverDone <- err
			}()
			if err := client.Handshake(); err != nil {
				t.Fatalf("client handshake: %v", err)
			}
			if client.ConnectionState().DidResume != wantResume {
				t.Fatalf("resumed=%v, want %v", client.ConnectionState().DidResume, wantResume)
			}
			var one [1]byte
			if _, err := io.ReadFull(client, one[:]); err != nil || one[0] != 1 {
				t.Fatalf("post-handshake ticket read: %v byte=%d", err, one[0])
			}
			if err := <-serverDone; err != nil {
				t.Fatalf("server handshake/write: %v", err)
			}
			return recorded.firstRecordBytes(t)
		}
		return result{cold: connect(false), warm: connect(true)}
	}
	defaultCurves := measure(nil)
	bleCurves := measure([]tls.CurveID{tls.X25519})
	t.Logf("ClientHello TLS bytes full: default=%d BLE-X25519=%d; resumed: default=%d BLE-X25519=%d", defaultCurves.cold, bleCurves.cold, defaultCurves.warm, bleCurves.warm)
	if defaultCurves.cold-bleCurves.cold < 1000 || defaultCurves.warm-bleCurves.warm < 1000 {
		t.Fatalf("BLE curve did not remove the hybrid key share: default=%+v BLE=%+v", defaultCurves, bleCurves)
	}
	if bleCurves.cold > 480 || bleCurves.warm > 960 {
		t.Fatalf("BLE ClientHello still needs too many CoC SDUs: %+v", bleCurves)
	}
}

func TestBLEServerCurveDoesNotChangePeerVerifier(t *testing.T) {
	r := &runtime{cfg: Config{Credentials: &localmesh.Credentials{Org: 64, Asset: 445}}}
	server := r.makeServerTLS()
	if len(server.CurvePreferences) != 1 || server.CurvePreferences[0] != tls.X25519 {
		t.Fatalf("BLE server curve preferences: %v", server.CurvePreferences)
	}
	if server.VerifyConnection == nil || server.ClientAuth != tls.RequireAnyClientCert {
		t.Fatal("BLE mutual asset verification was removed")
	}
}
