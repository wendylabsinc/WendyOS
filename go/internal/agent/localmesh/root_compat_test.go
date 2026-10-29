package localmesh

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

type rootByteConn struct {
	net.Conn
	written atomic.Int64
}

func (c *rootByteConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.written.Add(int64(n))
	return n, err
}

type rootHandshakeResult struct {
	client, server tls.ConnectionState
	bytes          int64
	err            error
}

// Both peers finish authentication and an application roundtrip. For failures,
// wait for the other handshake worker too, so tests cannot hide a hanging peer.
func rootTLSRoundtrip(client, server *tls.Config) rootHandshakeResult {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return rootHandshakeResult{err: err}
	}
	rawClient, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		ln.Close()
		return rootHandshakeResult{err: err}
	}
	rawServer, err := ln.Accept()
	ln.Close()
	if err != nil {
		rawClient.Close()
		return rootHandshakeResult{err: err}
	}
	defer rawClient.Close()
	defer rawServer.Close()
	deadline, _ := ctx.Deadline()
	rawClient.SetDeadline(deadline)
	rawServer.SetDeadline(deadline)
	cm, sm := &rootByteConn{Conn: rawClient}, &rootByteConn{Conn: rawServer}
	cc, sc := tls.Client(cm, client), tls.Server(sm, server)
	done := make(chan error, 1)
	go func() {
		e := sc.HandshakeContext(ctx)
		var b [1]byte
		if e == nil {
			_, e = io.ReadFull(sc, b[:])
		}
		if e == nil {
			_, e = sc.Write(b[:])
		}
		done <- e
	}()
	err = cc.HandshakeContext(ctx)
	if err == nil {
		_, err = cc.Write([]byte{7})
	}
	var b [1]byte
	if err == nil {
		_, err = io.ReadFull(cc, b[:])
	}
	if err != nil {
		rawClient.Close()
	}
	serverErr := <-done
	return rootHandshakeResult{client: cc.ConnectionState(), server: sc.ConnectionState(), bytes: cm.written.Load() + sm.written.Load(), err: errors.Join(err, serverErr)}
}

func rootQUICRoundtrip(client, server *tls.Config) rootHandshakeResult {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ln, err := quic.ListenAddr("127.0.0.1:0", server, QUICConfig())
	if err != nil {
		return rootHandshakeResult{err: err}
	}
	defer ln.Close()
	type sr struct {
		state tls.ConnectionState
		err   error
	}
	done := make(chan sr, 1)
	go func() {
		c, e := ln.Accept(ctx)
		if e != nil {
			done <- sr{err: e}
			return
		}
		defer c.CloseWithError(0, "done")
		stream, e := c.AcceptStream(ctx)
		var b [1]byte
		if e == nil {
			_, e = io.ReadFull(stream, b[:])
		}
		if e == nil {
			_, e = stream.Write(b[:])
		}
		if e == nil {
			_, e = io.ReadFull(stream, b[:])
		}
		done <- sr{c.ConnectionState().TLS, e}
	}()
	c, err := quic.DialAddr(ctx, ln.Addr().String(), client, QUICConfig())
	var state tls.ConnectionState
	if err == nil {
		defer c.CloseWithError(0, "done")
		state = c.ConnectionState().TLS
		s, e := c.OpenStreamSync(ctx)
		err = e
		var b [1]byte
		if err == nil {
			_, err = s.Write([]byte{7})
		}
		if err == nil {
			_, err = io.ReadFull(s, b[:])
		}
		if err == nil {
			_, err = s.Write(b[:])
		}
	}
	if err != nil {
		cancel()
	}
	result := <-done
	return rootHandshakeResult{client: state, server: result.state, err: errors.Join(err, result.err)}
}

func TestMeshTrimmedRootFullHandshakeCompatibility(t *testing.T) {
	var root []byte
	a, b := testCredentialsWithPEM(t, func(_ meshid.Device, _, chain, _ string) {
		block, _ := pem.Decode([]byte(chain))
		root = append([]byte(nil), block.Bytes...)
	})
	config := func(c *Credentials, peer meshid.Device, old bool) *tls.Config {
		t.Helper()
		cfg, err := c.PeerTLS(peer)
		if err != nil {
			t.Fatal(err)
		}
		// This fixture compares legacy root presentation, independently of
		// the newer per-issuer availability negotiation exercised elsewhere.
		cfg.NextProtos = []string{LinkALPN}
		cfg.GetClientCertificate = nil
		if len(cfg.Certificates[0].Certificate) != 1 {
			t.Fatal("root not omitted")
		}
		if old {
			cert := cfg.Certificates[0]
			cert.Certificate = append(append([][]byte(nil), cert.Certificate...), root)
			cfg.Certificates = []tls.Certificate{cert}
		}
		return cfg
	}
	for _, transport := range []string{"TLS13", "QUIC"} {
		handshake := rootTLSRoundtrip
		if transport == "QUIC" {
			handshake = rootQUICRoundtrip
		}
		var coldOld, coldNew int64
		for _, pair := range [][2]bool{{false, false}, {false, true}, {true, false}, {true, true}} {
			name := fmt.Sprintf("%s/clientOld%v/serverOld%v", transport, pair[0], pair[1])
			t.Run(name, func(t *testing.T) {
				r := handshake(config(a, b.Device, pair[0]), config(b, a.Device, pair[1]))
				if r.err != nil {
					t.Fatal(r.err)
				}
				if r.client.DidResume || r.server.DidResume || r.client.Version != tls.VersionTLS13 || r.server.Version != tls.VersionTLS13 {
					t.Fatal("expected full TLS1.3 mutual authentication")
				}
				wantClient, wantServer := 1, 1
				if pair[1] {
					wantClient++
				}
				if pair[0] {
					wantServer++
				}
				if len(r.client.PeerCertificates) != wantClient || len(r.server.PeerCertificates) != wantServer {
					t.Fatalf("presented certificate counts client=%d server=%d", len(r.client.PeerCertificates), len(r.server.PeerCertificates))
				}
				if !pair[0] && !pair[1] {
					coldNew = r.bytes
				}
				if pair[0] && pair[1] {
					coldOld = r.bytes
				}
			})
		}
		if transport == "TLS13" {
			t.Logf("full TLS application roundtrip bytes old=%d trimmed=%d saved=%d rootDER=%d", coldOld, coldNew, coldOld-coldNew, len(root))
			if coldOld-coldNew < int64(len(root)) {
				t.Fatal("full TLS root omission did not reduce bytes")
			}
		}
		t.Run(transport+"/wrong-server-pin", func(t *testing.T) {
			if r := handshake(config(a, testDevice(t, testPrincipal(testTenantUUID, testDeviceCID)), false), config(b, a.Device, true)); r.err == nil {
				t.Fatal("wrong peer pin accepted")
			}
		})
		t.Run(transport+"/wrong-client-pin", func(t *testing.T) {
			if r := handshake(config(a, b.Device, true), config(b, testDevice(t, testPrincipal(testTenantUUID, testDeviceCID)), false)); r.err == nil {
				t.Fatal("wrong peer pin accepted")
			}
		})
		t.Run(transport+"/untrusted-root", func(t *testing.T) {
			other := testForeignCredentials(t, testPrincipal(testTenantUUID, testDeviceCID))
			cfg, err := other.PeerTLS(b.Device)
			if err != nil {
				t.Fatal(err)
			}
			if r := handshake(cfg, config(b, a.Device, false)); r.err == nil {
				t.Fatal("untrusted root accepted")
			}
		})
	}
	now := time.Now()
	oldChain := [][]byte{a.Certificate.Certificate[0], root}
	newChain := a.Certificate.Certificate
	// The cache key is the device principal, so trimming the root cannot move an
	// identity: both bundles must verify to the same one.
	oldID, err := b.Verify(oldChain, now)
	if err != nil {
		t.Fatal(err)
	}
	newID, err := b.Verify(newChain, now)
	if err != nil {
		t.Fatal(err)
	}
	if oldID.Device.Principal() != a.Device.Principal() || newID.Device.Principal() != a.Device.Principal() {
		t.Fatalf("trimmed bundle changed identity: %s vs %s", oldID.Device.Principal(), newID.Device.Principal())
	}
	var oldWire, newWire bytes.Buffer
	if err := WriteControl(&oldWire, ControlMessage{Kind: "bundle", Bundle: oldChain}); err != nil {
		t.Fatal(err)
	}
	if err := WriteControl(&newWire, ControlMessage{Kind: "bundle", Bundle: newChain}); err != nil {
		t.Fatal(err)
	}
	t.Logf("catalog bundle framed bytes old=%d trimmed=%d saved=%d", oldWire.Len(), newWire.Len(), oldWire.Len()-newWire.Len())
	if oldWire.Len()-newWire.Len() < len(root) {
		t.Fatal("catalog identity proof did not shrink")
	}
	for _, wire := range [][]byte{oldWire.Bytes(), newWire.Bytes()} {
		m, err := ReadControl(bytes.NewReader(wire))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = b.Verify(m.Bundle, now); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMeshTrimmedRootRetainsRequiredIntermediateOnWire(t *testing.T) {
	// makeCert mints one certificate in the chain; an empty principal marks a
	// CA, anything else is a device principal carried as the leaf's URI SAN.
	makeCert := func(name, principal string, parent *x509.Certificate, parentKey *ecdsa.PrivateKey) (*x509.Certificate, *ecdsa.PrivateKey, string, string) {
		t.Helper()
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		template := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), BasicConstraintsValid: true, IsCA: principal == "", KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}}
		if name == "intermediate" {
			template.NotAfter = time.Now().Add(30 * time.Minute)
		}
		if principal == "" {
			template.KeyUsage |= x509.KeyUsageCertSign
		} else {
			u, _ := url.Parse(principal)
			template.URIs = []*url.URL{u}
		}
		if parent == nil {
			parent = template
			parentKey = key
		}
		der, err := x509.CreateCertificate(rand.Reader, template, parent, &key.PublicKey, parentKey)
		if err != nil {
			t.Fatal(err)
		}
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		kd, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		return cert, key, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd}))
	}
	deviceA, deviceB := testPrincipal(testTenantUUID, testDeviceAID), testPrincipal(testTenantUUID, testDeviceBID)
	root, rk, rp, _ := makeCert("root", "", nil, nil)
	intermediate, ik, ip, _ := makeCert("intermediate", "", root, rk)
	_, _, ap, ak := makeCert("a", deviceA, root, rk)
	_, _, bp, bk := makeCert("b", deviceB, intermediate, ik)
	a, err := NewCredentials(ap, rp, ak)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewCredentials(bp, ip+rp, bk)
	if err != nil {
		t.Fatal(err)
	}
	if peer, err := a.Verify(b.Certificate.Certificate, time.Now()); err != nil || !peer.NotAfter.Equal(intermediate.NotAfter) {
		t.Fatalf("peer lifetime must stop at its presented intermediate: %s, %v", peer.NotAfter, err)
	}

	for name, handshake := range map[string]func(*tls.Config, *tls.Config) rootHandshakeResult{"TLS13": rootTLSRoundtrip, "QUIC": rootQUICRoundtrip} {
		for _, oldSide := range []string{"neither", "client", "server"} {
			t.Run(name+"/old-"+oldSide, func(t *testing.T) {
				ac, e := a.PeerTLS(b.Device)
				if e != nil {
					t.Fatal(e)
				}
				bc, e := b.PeerTLS(a.Device)
				if e != nil {
					t.Fatal(e)
				}
				if len(ac.Certificates[0].Certificate) != 1 || len(bc.Certificates[0].Certificate) != 2 || !bytes.Equal(bc.Certificates[0].Certificate[1], intermediate.Raw) {
					t.Fatal("incorrect trimmed chain")
				}
				old := ac
				if oldSide == "server" {
					old = bc
				}
				if oldSide != "neither" {
					cert := old.Certificates[0]
					cert.Certificate = append(append([][]byte(nil), cert.Certificate...), root.Raw)
					old.Certificates = []tls.Certificate{cert}
				}
				result := handshake(ac, bc)
				if result.err != nil {
					t.Fatal(result.err)
				}
				if result.client.DidResume || result.server.DidResume {
					t.Fatal("expected full authentication")
				}
				want := 2
				if oldSide == "server" {
					want = 3
				}
				if len(result.client.PeerCertificates) != want {
					t.Fatal("intermediate missing from actual TLS peer certificate list")
				}
			})
		}
		t.Run(name+"/missing-intermediate-rejected", func(t *testing.T) {
			a.issuerMaterial.Clear()
			ac, _ := a.PeerTLS(b.Device)
			bc, _ := b.PeerTLS(a.Device)
			cert := bc.Certificates[0]
			cert.Certificate = cert.Certificate[:1]
			bc.Certificates = []tls.Certificate{cert}
			if result := handshake(ac, bc); result.err == nil {
				t.Fatal("omitting required intermediate was accepted")
			}
		})
	}
}
