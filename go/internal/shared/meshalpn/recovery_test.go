package meshalpn

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"io"
	"math/big"
	"net"
	"sync"
	"testing"
	"time"
)

func TestRecoveryDeadlineAndExclusions(t *testing.T) {
	now := time.Now()
	r := &Recovery{now: func() time.Time { return now }}
	for _, err := range []error{nil, errors.New("expired"), errors.New("wrong tenant"), errors.New("remote error: tls: bad certificate"), io.EOF} {
		r.failed(err, true)
		if r.active() {
			t.Fatalf("armed on unclassified error: %v", err)
		}
	}
	path := &certs.IssuerPathError{Err: errors.New("issuer missing")}
	r.failed(path, false)
	if r.active() {
		t.Fatal("full-chain failure armed recovery")
	}
	var workers sync.WaitGroup
	for range 32 {
		workers.Add(1)
		go func() { defer workers.Done(); r.failed(path, true) }()
	}
	workers.Wait()
	if !r.active() {
		t.Fatal("issuer failure did not arm recovery")
	}
	until := r.until
	now = now.Add(119 * time.Second)
	r.failed(path, true)
	if !r.until.Equal(until) {
		t.Fatal("failure extended cooldown")
	}
	now = until
	if r.active() {
		t.Fatal("cooldown did not expire")
	}
	// Recreated material and either TLS role share the same runtime controller.
	a, b := new(Certificates), new(Certificates)
	a.SetRecovery(r)
	b.SetRecovery(r)
	a.recoveryState().failed(path, true)
	if !b.recoveryState().active() {
		t.Fatal("runtime cooldown not shared")
	}
}

type recoveryFixture struct {
	pair tls.Certificate
	root *x509.Certificate
}

func recoveryFixtures(t *testing.T) (recoveryFixture, recoveryFixture) {
	t.Helper()
	mint := func(name string, serial int64, ca bool, parent *x509.Certificate, signer ed25519.PrivateKey) (*x509.Certificate, ed25519.PrivateKey) {
		pub, key, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		template := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: name}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), BasicConstraintsValid: true, IsCA: ca, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}
		if ca {
			template.KeyUsage |= x509.KeyUsageCertSign
		}
		if parent == nil {
			parent = template
			signer = key
		}
		der, err := x509.CreateCertificate(rand.Reader, template, parent, pub, signer)
		if err != nil {
			t.Fatal(err)
		}
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		return cert, key
	}
	root, key := mint("root", 1, true, nil, nil)
	makePeer := func(name string, serial int64) recoveryFixture {
		issuer, ik := mint(name+" issuer", serial, true, root, key)
		leaf, lk := mint(name, serial+1, false, issuer, ik)
		return recoveryFixture{tls.Certificate{Certificate: [][]byte{leaf.Raw, issuer.Raw}, PrivateKey: lk}, root}
	}
	return makePeer("a", 2), makePeer("b", 4)
}
func (f recoveryFixture) material(r *Recovery) *Certificates {
	c := NewCertificates(f.pair, []*x509.Certificate{f.root})
	c.SetRecovery(r)
	return c
}
func (f recoveryFixture) config(peerCallback bool) *tls.Config {
	verify := func(raw [][]byte) error {
		leaf, err := x509.ParseCertificate(raw[0])
		if err != nil {
			return err
		}
		var parents []*x509.Certificate
		for _, der := range raw[1:] {
			p, e := x509.ParseCertificate(der)
			if e != nil {
				return e
			}
			parents = append(parents, p)
		}
		return certs.VerifyPeerCertificateChain(leaf, parents, []*x509.Certificate{f.root}, x509.ExtKeyUsageAny, time.Now(), time.Now())
	}
	c := &tls.Config{MinVersion: tls.VersionTLS13, InsecureSkipVerify: true, ClientAuth: tls.RequireAnyClientCert, NextProtos: []string{"test/1"}}
	if peerCallback {
		c.VerifyPeerCertificate = func(raw [][]byte, _ [][]*x509.Certificate) error { return verify(raw) }
	} else {
		c.VerifyConnection = func(s tls.ConnectionState) error {
			var raw [][]byte
			for _, p := range s.PeerCertificates {
				raw = append(raw, p.Raw)
			}
			return verify(raw)
		}
	}
	return c
}
func recoveryWire(client, server *tls.Config) (tls.ConnectionState, tls.ConnectionState, error) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return tls.ConnectionState{}, tls.ConnectionState{}, err
	}
	left, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		ln.Close()
		return tls.ConnectionState{}, tls.ConnectionState{}, err
	}
	right, err := ln.Accept()
	ln.Close()
	if err != nil {
		left.Close()
		return tls.ConnectionState{}, tls.ConnectionState{}, err
	}
	defer left.Close()
	defer right.Close()
	left.SetDeadline(time.Now().Add(3 * time.Second))
	right.SetDeadline(time.Now().Add(3 * time.Second))
	cc, sc := tls.Client(left, client), tls.Server(right, server)
	done := make(chan error, 1)
	go func() {
		e := sc.Handshake()
		var b [1]byte
		if e == nil {
			_, e = io.ReadFull(sc, b[:])
		}
		if e == nil {
			_, e = sc.Write(b[:])
		}
		done <- e
		right.Close()
	}()
	err = cc.Handshake()
	if err == nil {
		_, err = cc.Write([]byte{1})
	}
	var b [1]byte
	if err == nil {
		_, err = io.ReadFull(cc, b[:])
	}
	left.Close()
	if e := <-done; err == nil {
		err = e
	}
	return cc.ConnectionState(), sc.ConnectionState(), err
}
func TestFalseAvailabilityRecoversBothRoles(t *testing.T) {
	a, b := recoveryFixtures(t)
	for _, role := range []string{"client", "server"} {
		for _, peerCallback := range []bool{false, true} {
			t.Run(role+map[bool]string{false: "-connection-hook", true: "-certificate-hook"}[peerCallback], func(t *testing.T) {
				now := time.Now()
				ar, br := &Recovery{now: func() time.Time { return now }}, new(Recovery)
				am, bm := a.material(ar), b.material(br)
				client, server := a.config(peerCallback), b.config(peerCallback)
				if role == "client" {
					// A false positive models a collision: A advertises B's issuer while it
					// actually holds only its own distinct issuer. B then omits its issuer.
					am.path = []Fingerprint{Hash(a.root.Raw), Hash(b.pair.Certificate[1])}
				}
				am.ConfigureClient(client, "test/1")
				bm.ConfigureServer(server)
				if role == "server" {
					// Model the matching false acknowledgement in the opposite direction.
					previous := server.GetConfigForClient
					server.GetConfigForClient = func(h *tls.ClientHelloInfo) (*tls.Config, error) {
						c, e := previous(h)
						if e == nil {
							c.NextProtos = []string{h.SupportedProtos[0]}
						}
						return c, e
					}
				}
				_, _, err := recoveryWire(client, server)
				if err == nil {
					t.Fatal("false availability unexpectedly verified")
				}
				failed := ar
				if role == "server" {
					failed = br
				}
				if !failed.active() {
					t.Fatal("rejecting endpoint did not enter cooldown")
				}
				// Recreate material and reverse roles: the same controller must force full
				// chains whether its next connection is incoming or outgoing.
				for _, reverse := range []bool{false, true} {
					am, bm = a.material(ar), b.material(br)
					client, server = a.config(peerCallback), b.config(peerCallback)
					if reverse {
						am, bm = bm, am
						client, server = server, client
					}
					am.ConfigureClient(client, "test/1")
					bm.ConfigureServer(server)
					cs, ss, e := recoveryWire(client, server)
					if e != nil {
						t.Fatal(e)
					}
					if cs.NegotiatedProtocol != "test/1" || len(cs.PeerCertificates) != 2 || len(ss.PeerCertificates) != 2 || cs.DidResume || ss.DidResume {
						t.Fatalf("fallback not full: client=%+v server=%+v", cs, ss)
					}
				}
			})
		}
	}
}
func TestOrdinaryALPNIgnoresInventory(t *testing.T) {
	a, b := recoveryFixtures(t)
	am, bm := a.material(new(Recovery)), b.material(new(Recovery))
	client, server := a.config(false), b.config(false)
	am.ConfigureClient(client, "test/1")
	// No acknowledged ancestry, but auxiliary inventory claims the server CA.
	client.NextProtos = Offer("test/1", nil, []Fingerprint{Hash(b.pair.Certificate[1])})
	bm.ConfigureServer(server)
	cs, ss, err := recoveryWire(client, server)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs.PeerCertificates) != 2 || len(ss.PeerCertificates) != 2 {
		t.Fatal("ordinary ALPN omitted an inventoried issuer")
	}
}

func TestCooldownDeclinesExistingTickets(t *testing.T) {
	a, b := recoveryFixtures(t)
	for _, rejectingRole := range []string{"client", "server"} {
		t.Run(rejectingRole, func(t *testing.T) {
			ar, br := new(Recovery), new(Recovery)
			am, bm := a.material(ar), b.material(br)
			cache := tls.NewLRUClientSessionCache(2)
			key := [32]byte{1, 2, 3}
			for round := 0; round < 3; round++ {
				if round == 2 {
					r := ar
					if rejectingRole == "server" {
						r = br
					}
					r.failed(&certs.IssuerPathError{Err: errors.New("issuer missing")}, true)
				}
				client, server := a.config(false), b.config(false)
				client.ServerName = "test"
				client.ClientSessionCache = cache
				server.SetSessionTicketKeys([][32]byte{key})
				server.WrapSession = server.EncryptTicket
				server.UnwrapSession = server.DecryptTicket
				am.ConfigureClient(client, "test/1")
				bm.ConfigureServer(server)
				cs, ss, err := recoveryWire(client, server)
				if err != nil {
					t.Fatalf("round %d: %v", round, err)
				}
				if cs.DidResume != (round == 1) || ss.DidResume != (round == 1) {
					t.Fatalf("round %d: client resumed=%v server resumed=%v", round, cs.DidResume, ss.DidResume)
				}
				if round == 2 && (cs.NegotiatedProtocol != "test/1" || len(cs.PeerCertificates) != 2 || len(ss.PeerCertificates) != 2) {
					t.Fatal("cooldown reused a partial presentation")
				}
			}
		})
	}
}
